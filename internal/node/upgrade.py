"""Fixed in-place rollouts through the target release's own dashmate.

The target release's dashmate renders the node again (migrating its config, as
`dashmate update` does); Compose then recreates exactly the services whose image
or rendered configuration changed. A Platform rollout never touches Core and
never installs Core's rendered files. A Core rollout ("core" scope) changes
Core's image only; dash.conf, genesis, keys, wallets and chain data are kept.
An on-host write-ahead marker reconciles lost SSH responses with the same plan.
There is deliberately no downgrade/rollback/reset action.
"""
import copy
import socket
import time


# Failures a retry cannot cure: stop waiting and report them.
PERMANENT = ["wrong-chain", "running-image-drift", "image-platform-drift"]


class UpgradeWorker(Worker):
    def abci_address(self):
        """Drive's ABCI listener, on dashmate's Compose network."""
        value = self.inspect_container("drive_abci")
        self.require(value is not None, "upgrade-missing-service")
        networks = value["NetworkSettings"].get("Networks") or {}
        address = (networks.get(self.project + "_default") or {}).get("IPAddress")
        self.require(address, "upgrade-drive-abci-address")
        return address, 26658

    def wait_abci(self):
        # Tenderdash exits on a missing ABCI listener: start it after Drive's.
        deadline = time.monotonic() + 150
        while True:
            try:
                with socket.create_connection(self.abci_address(), timeout=2):
                    return
            except OSError:
                if time.monotonic() > deadline:
                    raise Failure("upgrade-drive-abci-timeout") from None
                time.sleep(1)

    def preserve_core(self):
        expected = self.q["upgrade"]["preserve"]
        actual = self.core_status()
        self.require(actual["containerId"] == expected["coreId"]
                     and actual["startedAt"] == expected["coreStarted"]
                     and actual["configSha256"] == expected["coreConfig"]
                     and actual["genesis"] == expected["coreGenesis"],
                     "upgrade-core-changed")
        return actual

    def core_observe(self, pin):
        """Core status with the running image checked against the given pin.

        The miner is withdrawn and replaced separately during a Core upgrade,
        so it is verified explicitly by the caller, not here."""
        saved, miner = self.images["core"], self.c.get("miningNodeName")
        self.images["core"], self.c["miningNodeName"] = pin, None
        try:
            return self.core_status()
        finally:
            self.images["core"], self.c["miningNodeName"] = saved, miner

    def running(self, name, pin):
        value = self.inspect_container(name)
        if not value or not value["State"]["Running"]:
            return False
        try:
            self.verify_image(value, pin)
            return True
        except Failure:
            return False

    def wait_core(self, pin, synced):
        deadline = time.monotonic() + (1800 if synced else 180)
        while True:
            try:
                status = self.core_observe(pin)
                if not synced or (status["synced"] and not status["ibd"] and status["height"] >= status["headers"]):
                    return status
            except Failure as error:
                if str(error) in PERMANENT:
                    raise
            except (RPCFailure, urllib.error.URLError, ConnectionError, OSError):
                pass
            if time.monotonic() >= deadline:
                raise Failure("core-upgrade-not-ready")
            time.sleep(3)

    def wait_dkg_quiet(self, pin):
        """Restart a validator's Core only in the quiet part of the 24-block DKG
        cycle: every session has finalized by block 13 and the next starts at
        24. The runner pauses mining first, so no block follows until the
        replaced node is back and connected (a session it missed means PoSe)."""
        deadline = time.monotonic() + max(600, 26 * self.block_seconds())
        while True:
            try:
                height = self.core_observe(pin)["height"]
                if 13 <= height % 24 <= 23:
                    return height
            except Failure as error:
                if str(error) in PERMANENT:
                    raise
            except (RPCFailure, urllib.error.URLError, ConnectionError, OSError):
                pass
            if time.monotonic() >= deadline:
                raise Failure("core-upgrade-no-quiet-dkg-window")
            time.sleep(2)

    def missing_quorum_links(self):
        """Valid quorum members this masternode has no MNAUTH-verified link to."""
        valid = set(self.rpc("protx", ["list", "valid"]))
        missing = set()
        for quorum in self.rpc("quorum", ["dkgstatus"]).get("quorumConnections", []):
            for member in quorum.get("quorumConnections", []):
                if not member.get("connected") and member.get("proTxHash") in valid:
                    missing.add(member["proTxHash"])
        return missing

    def refresh_quorum_links(self):
        """Reconnect a restarted masternode's peers once it is synced.

        Core ignores MNAUTH until its own blockchain sync completes, and a
        connection carries only one, so peers that reconnect during startup
        verify this node while it never verifies them. Masternode
        de-duplication then keeps those one-sided links and the node misses
        quorum connections (lost DKG contributions, then PoSe). Reconnecting
        once synced makes every link verify both ways."""
        for peer in self.rpc("getpeerinfo"):
            try:
                self.rpc("disconnectnode", ["", peer["id"]])
            except RPCFailure as error:
                if error.code != -29:  # already disconnected
                    raise
        deadline = time.monotonic() + 300
        while True:
            try:
                if self.rpc("getconnectioncount") > 0 and not self.missing_quorum_links():
                    return
            except (RPCFailure, urllib.error.URLError, ConnectionError, OSError):
                pass
            if time.monotonic() >= deadline:
                raise Failure("core-upgrade-quorum-links-missing")
            time.sleep(3)

    def locked(self):
        self.verify_instance()
        lock = self.lock.open("a")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            lock.close()
            raise Failure("host-busy") from None
        return lock

    def execute_core(self):
        """Replace Core's image on this node, withdrawing its dependants first.

        Steps are journaled in upgrade.json before they run, so a lost response
        or interrupted run resumes at the same step with the same plan."""
        change = self.q["upgrade"]
        before, after = change["from"], change["to"]
        self.require(set(before) == set(after) and "core" in before
                     and all(before[k] == after[k] for k in before if k != "core"), "core-upgrade-scope")
        with self.locked():
            self.owned()
            self.require(self.read("deployment.json") == {"planId": self.c["planId"]}, "upgrade-missing-deployment")
            self.stage = self.q["action"]
            expected = change["preserve"]
            marker = self.read("upgrade.json")
            same = bool(marker and marker.get("id") == change["id"])
            if same:
                self.require(marker["from"] == before and marker["to"] == after and marker.get("scope") == "core"
                             and marker.get("phase") in ["applying", "applied"], "upgrade-marker-drift")
            else:
                # Platform upgrades leave no marker on non-validators, so a node's
                # last marker may be older than previousId; only an unfinished
                # different operation blocks.
                self.require(marker is None or marker.get("phase") == "applied", "upgrade-previous-operation")
            self.verify_pin(after["core"])
            fresh = not same or marker.get("step") == "start"
            if fresh:
                core = self.core_observe(before["core"])
                self.require(core["containerId"] == expected["coreId"] and core["configSha256"] == expected["coreConfig"]
                             and core["genesis"] == expected["coreGenesis"], "upgrade-core-changed")
            if self.q["action"] == "upgrade-stage":
                # A node already in progress is reconciled by its apply.
                return dict(instanceId=self.t["instanceId"], planId=self.c["planId"], action=self.q["action"],
                            **({"core": core} if fresh else {}))
            validator = self.t["role"] == "validator"
            miner = self.read("miner/compose.json")
            if not same:
                marker = dict(id=change["id"], previousId=change.get("previousId", ""), scope="core",
                              **{"from": before}, to=after, preserve=expected, phase="applying", step="start")
                self.atomic("upgrade.json", marker)
            step = lambda name: (marker.update(step=name), self.atomic("upgrade.json", marker))
            if marker["step"] == "start":
                # Mining and Platform depend on Core RPC/ZMQ: withdraw them
                # gracefully (Tenderdash before Drive) instead of letting them crash.
                if miner and self.inspect_container("miner"):
                    self.docker("stop", "-t", "20", self.container_name("miner"), timeout=30)
                if validator:
                    for name in ["drive_tenderdash", "rs_dapi", "drive_abci"]:
                        self.docker("stop", "--time", "120", self.container_name(name), timeout=150)
                step("withdrawn")
            if marker["step"] == "withdrawn":
                if not self.running("core", after["core"]):
                    # Core itself stops only in the quiet part of the DKG cycle.
                    if validator and self.running("core", before["core"]):
                        self.wait_dkg_quiet(before["core"])
                    # The release's dashmate renders Core's new image; its
                    # configuration is otherwise exactly what was preserved.
                    self.images = dict(self.images, **after)
                    r = self.render()
                    try:
                        services = [s for s in r.selection if s in CORE_SERVICES]
                        self.install(r, services)
                        # Stop Core (it may take up to its grace period) before the
                        # recreate, so Compose never times out mid-replacement.
                        if self.inspect_container("core"):
                            self.docker("stop", "-t", "120", self.container_name("core"), timeout=150)
                        self.up(r, services)
                    finally:
                        self.discard(r)
                self.wait_core(after["core"], synced=False)
                step("replaced")
            if marker["step"] == "replaced":
                core = self.wait_core(after["core"], synced=True)
                self.require(core["configSha256"] == expected["coreConfig"] and core["genesis"] == expected["coreGenesis"],
                             "core-upgrade-identity-changed")
                if validator:
                    self.refresh_quorum_links()
                    self.docker("start", self.container_name("drive_abci"))
                    self.wait_abci()
                    for name in ["drive_tenderdash", "rs_dapi"]:
                        self.docker("start", self.container_name(name))
                if miner:
                    services = miner["services"]
                    services["miner"]["image"] = after["core"]
                    self.compose("miner", services)
                marker["phase"] = "applied"
                step("done")
            core = self.core_observe(after["core"])
            self.require(core["configSha256"] == expected["coreConfig"] and core["genesis"] == expected["coreGenesis"],
                         "core-upgrade-identity-changed")
            if validator:
                for component, name in [("drive", "drive_abci"), ("tenderdash", "drive_tenderdash"),
                                        ("dapi", "rs_dapi"), ("gateway", "gateway")]:
                    value = self.inspect_container(name)
                    self.require(value and value["State"]["Running"] and value["Id"] == expected["containers"][component],
                                 "core-upgrade-platform-not-restored")
            if miner:
                self.require(self.running("miner", after["core"]) and self.inspect_container("miner")["State"]["Running"],
                             "core-upgrade-miner-not-restored")
            return dict(instanceId=self.t["instanceId"], planId=self.c["planId"], action=self.q["action"], core=core)

    def execute(self):
        self.require(self.q["action"] in ["upgrade-stage", "upgrade-apply"],
                     "upgrade-action-refused")
        if self.q["upgrade"].get("scope") == "core":
            return self.execute_core()
        self.require(self.t["role"] == "validator", "upgrade-validator-only")
        change = self.q["upgrade"]
        before, after = change["from"], change["to"]
        components = {"core", "drive", "dapi", "gateway", "tenderdash", "helper"}
        self.require(set(before) == components and set(after) == components
                     and before["core"] == after["core"], "upgrade-core-refused")
        with self.locked():
            self.owned()
            self.require(self.read("deployment.json") == {"planId": self.c["planId"]},
                         "upgrade-missing-deployment")
            self.stage = self.q["action"]
            core = self.preserve_core()
            marker = self.read("upgrade.json")
            same = marker and marker.get("id") == change["id"]
            if same:
                self.require(marker["from"] == before and marker["to"] == after
                             and marker["preserve"] == change["preserve"]
                             and marker.get("phase") in ["applying", "applied"],
                             "upgrade-marker-drift")
            else:
                self.require((marker is None and not change.get("previousId"))
                             or (marker and marker.get("id") == change.get("previousId")
                                 and marker.get("phase") == "applied"),
                             "upgrade-previous-operation")
            for component, pin in after.items():
                if before[component] != pin:
                    self.verify_pin(pin)
            # The target release's dashmate renders this node; Compose then
            # recreates exactly the Platform services whose fingerprint changed.
            self.images = dict(self.images, **after)
            r = self.render()
            try:
                return self.apply_platform(change, marker if same else None, r, core)
            finally:
                self.discard(r)

    def apply_platform(self, change, marker, r, core):
        before, after, preserve = change["from"], change["to"], change["preserve"]
        platform = [s for s in r.selection if s in PLATFORM_SERVICES]
        desired = {s: r.fingerprints[s] for s in platform}
        current = {s: self.inspect_container(s) for s in platform}
        changes = [s for s in platform if current[s] is None or self.label(current[s]) != desired[s]]
        # Rendered Core changes wait for a Core rollout: this one keeps Core.
        deferred = [s for s in r.selection if s in CORE_SERVICES
                    and self.label(self.inspect_container(s) or {"Config": {}}) != r.fingerprints[s]]
        pins = self.c.get("sidecarImages") or {}
        for name in platform:
            if name not in COMPONENTS and name in pins:
                self.verify_pin(pins[name])
        if self.q["action"] == "upgrade-stage":
            # Sidecars are those the release requests: the controller pins any
            # it has not, then stages again. Changes follow the given pins.
            return dict(instanceId=self.t["instanceId"], planId=self.c["planId"], action=self.q["action"], core=core,
                        render=dict(version=r.version, sidecars=r.sidecars(), changes=changes, deferred=deferred))
        drain = "drive_abci" in (marker["changes"] if marker else changes)
        if marker is None:
            marker = dict(id=change["id"], previousId=change.get("previousId", ""),
                          **{"from": before}, to=after, preserve=preserve,
                          desired=desired, changes=changes, phase="applying")
            if drain:
                marker["dependency"] = "pending"
            self.atomic("upgrade.json", marker)
        else:
            self.require(marker["desired"] == desired, "upgrade-config-changed")
        if marker["phase"] == "applying":
            # Services outside this change keep their exact containers. A
            # Tenderdash withdrawn around Drive's replacement keeps its container.
            dependency = marker.get("dependency")
            for name in platform:
                if name in marker["changes"]:
                    continue
                key = COMPONENTS.get(name, name)
                value = current[name]
                self.require(value is not None, "upgrade-missing-service")
                if key in preserve["containers"]:
                    self.require(value["Id"] == preserve["containers"][key]
                                 and (value["RestartCount"] == preserve["restarts"][key]
                                      or (name == "drive_tenderdash" and dependency in ["start-requested", "started"]
                                          and value["RestartCount"] == 0))
                                 and (value["State"]["Running"]
                                      or (name == "drive_tenderdash" and dependency in ["stop-requested", "stopped", "start-requested"])),
                                 "upgrade-unselected-service-changed")
            self.install(r, platform)
            self.certificates()
            if drain and dependency not in ["start-requested", "started"]:
                # Tenderdash exits on an ABCI EOF. Withdraw it gracefully before
                # Drive, rather than relying on Docker's crash/restart policy.
                # Save intent first so disconnects after stop are retry-safe.
                marker["dependency"] = "stop-requested"
                self.atomic("upgrade.json", marker)
                self.docker("stop", "--time", "120", self.container_name("drive_tenderdash"), timeout=150)
                value = self.inspect_container("drive_tenderdash")
                self.require(value and not value["State"]["Running"], "upgrade-dependency-not-stopped")
                marker["dependency"] = "stopped"
                self.atomic("upgrade.json", marker)
            immediate = [s for s in marker["changes"] if not (drain and s == "drive_tenderdash")]
            if immediate:
                self.up(r, immediate)
            if drain:
                self.wait_abci()
                if marker["dependency"] != "started":
                    marker["dependency"] = "start-requested"
                    self.atomic("upgrade.json", marker)
                if "drive_tenderdash" in marker["changes"]:
                    self.up(r, ["drive_tenderdash"])
                elif not self.inspect_container("drive_tenderdash")["State"]["Running"]:
                    self.docker("start", self.container_name("drive_tenderdash"))
                marker["dependency"] = "started"
                self.atomic("upgrade.json", marker)
        core = self.preserve_core()
        for name in platform:
            value = self.inspect_container(name)
            self.require(value and value["State"]["Running"] and self.label(value) == desired[name],
                         "upgrade-service-not-running")
            key = COMPONENTS.get(name)
            if key:
                self.verify_image(value, after[key])
            if key in preserve["containers"] and name not in marker["changes"]:
                self.require(value["Id"] == preserve["containers"][key]
                             and value["RestartCount"] == (0 if name == "drive_tenderdash" and drain
                                                           else preserve["restarts"][key]),
                             "upgrade-unselected-service-changed")
        marker["phase"] = "applied"
        self.atomic("upgrade.json", marker)
        return dict(instanceId=self.t["instanceId"], planId=self.c["planId"],
                    action=self.q["action"], core=core,
                    render=dict(version=r.version, sidecars=r.sidecars(), changes=marker["changes"], deferred=deferred))


Worker = UpgradeWorker
