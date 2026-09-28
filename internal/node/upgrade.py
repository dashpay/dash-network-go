"""Fixed in-place image rollout. Never renders new configuration.

A Platform rollout changes only image fields in the owned Platform Compose
document and never touches Core. A Core rollout ("core" scope) changes only the
Core (and miner) image in their owned Compose documents; dash.conf, genesis,
keys, wallets and chain data are kept.
An on-host write-ahead marker reconciles lost SSH responses with the same plan.
There is deliberately no downgrade/rollback/reset action.
"""
import copy
import socket
import time


# Failures a retry cannot cure: stop waiting and report them.
PERMANENT = ["wrong-chain", "running-image-drift", "image-platform-drift"]


class UpgradeWorker(Worker):
    def wait_abci(self):
        deadline = time.monotonic() + 150
        while time.monotonic() < deadline:
            self.preserve_core()
            try:
                with socket.create_connection(('127.0.0.1', self.ports['driveABCI']), timeout=2):
                    return
            except OSError:
                time.sleep(1)
        raise Failure('upgrade-drive-abci-timeout')

    def preserve_core(self):
        expected = self.q["upgrade"]["preserve"]
        actual = self.core_status()
        self.require(actual["containerId"] == expected["coreId"]
                     and actual["startedAt"] == expected["coreStarted"]
                     and actual["configSha256"] == expected["coreConfig"]
                     and actual["genesis"] == expected["coreGenesis"],
                     "upgrade-core-changed")
        return actual

    def check_images(self, compose, pins):
        self.require(compose.get("name") == self.project
                     and set(compose.get("services", {})) ==
                         {"drive", "tenderdash", "dapi", "gateway"},
                     "upgrade-compose-scope")
        for service, value in compose["services"].items():
            self.require(value["image"] == pins[service], "upgrade-compose-drift")
            self.require(value["container_name"] == self.container_name(service)
                         and value["labels"] == self.labels(), "upgrade-compose-owner")

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

    def stage_image(self, pin):
        try:
            image = json.loads(self.docker("image", "inspect", pin))[0]
        except Failure:
            self.docker("pull", "--platform", "linux/" + self.t["architecture"], pin, timeout=600)
            image = json.loads(self.docker("image", "inspect", pin))[0]
        self.require(image["Architecture"] == self.t["architecture"] and image["Os"] == "linux"
                     and any(v.split("@")[-1] == pin.split("@")[-1] for v in image.get("RepoDigests", [])),
                     "upgrade-image-proof")

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
        """Restart a validator's Core only early in the quiet part of the
        24-block DKG cycle (sessions end by block 12; the next starts at 24), so
        the node is back before its quorums' next contribution phase (PoSe)."""
        deadline = time.monotonic() + 600
        while True:
            try:
                height = self.core_observe(pin)["height"]
                if 13 <= height % 24 <= 14:
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

    def wait_drive(self):
        # Tenderdash exits on a missing ABCI listener: start it after Drive's.
        deadline = time.monotonic() + 150
        while True:
            try:
                with socket.create_connection(("127.0.0.1", self.ports["driveABCI"]), timeout=2):
                    return
            except OSError:
                if time.monotonic() > deadline:
                    raise Failure("upgrade-drive-abci-timeout") from None
                time.sleep(1)

    def execute_core(self):
        """Replace Core's image on this node, withdrawing its dependants first.

        Steps are journaled in upgrade.json before they run, so a lost response
        or interrupted run resumes at the same step with the same plan."""
        change = self.q["upgrade"]
        before, after = change["from"], change["to"]
        self.require(set(before) == set(after) and "core" in before
                     and all(before[k] == after[k] for k in before if k != "core"), "core-upgrade-scope")
        self.verify_instance()
        with self.lock.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Failure("host-busy") from None
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
            self.stage_image(after["core"])
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
                    for name in ["tenderdash", "dapi", "drive"]:
                        self.docker("stop", "--time", "120", self.container_name(name), timeout=150)
                step("withdrawn")
            if marker["step"] == "withdrawn":
                if not self.running("core", after["core"]):
                    # Core itself stops only in the quiet part of the DKG cycle.
                    if validator and self.running("core", before["core"]):
                        self.wait_dkg_quiet(before["core"])
                    # Stop Core (it may take up to its grace period) before the
                    # recreate, so Compose never times out mid-replacement.
                    if self.inspect_container("core"):
                        self.docker("stop", "-t", "120", self.container_name("core"), timeout=150)
                    services = self.read("core/compose.json")["services"]
                    services["core"]["image"] = after["core"]
                    self.compose("core", services)
                self.wait_core(after["core"], synced=False)
                step("replaced")
            if marker["step"] == "replaced":
                core = self.wait_core(after["core"], synced=True)
                self.require(core["configSha256"] == expected["coreConfig"] and core["genesis"] == expected["coreGenesis"],
                             "core-upgrade-identity-changed")
                if validator:
                    self.refresh_quorum_links()
                    self.docker("start", self.container_name("drive"))
                    self.wait_drive()
                    for name in ["tenderdash", "dapi"]:
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
                for name in ["drive", "tenderdash", "dapi", "gateway"]:
                    value = self.inspect_container(name)
                    self.require(value and value["State"]["Running"] and value["Id"] == expected["containers"][name],
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
        self.verify_instance()
        with self.lock.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Failure("host-busy") from None
            self.owned()
            self.require(self.read("deployment.json") == {"planId": self.c["planId"]},
                         "upgrade-missing-deployment")
            self.stage = self.q["action"]
            core = self.preserve_core()
            compose = self.read("platform/compose.json")
            self.require(compose is not None, "upgrade-missing-compose")
            marker = self.read("upgrade.json")
            same = marker and marker.get("id") == change["id"]
            drain = before['drive'] != after['drive']
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
                self.check_images(compose, before)
            desired = copy.deepcopy(compose)
            for service in desired["services"]:
                desired["services"][service]["image"] = after[service]
            # A saved marker contains hashes, not the secret-bearing document.
            config_hash = hashlib.sha256(json.dumps(desired, sort_keys=True).encode()).hexdigest()
            if same:
                self.require(marker["desiredComposeHash"] == config_hash,
                             "upgrade-config-changed")
                for service, value in compose["services"].items():
                    self.require(value["image"] in [before[service], after[service]],
                                 "upgrade-unexpected-image")
            for service in desired["services"]:
                value = self.inspect_container(service)
                # Compose can remove an old container before replacement fails.
                # Only this exact unfinished, journaled image change may repair
                # that absence. Missing unrelated/applied services remain drift.
                if value is None and same and marker["phase"] == "applying" \
                        and before[service] != after[service]:
                    continue
                self.require(value is not None, "upgrade-missing-service")
                allowed = [before[service], after[service]] if same else [before[service]]
                matches = False
                for pin in allowed:
                    try:
                        self.verify_image(value, pin)
                        matches = True
                        break
                    except Failure:
                        pass
                self.require(matches, "upgrade-running-image-drift")
                if before[service] == after[service]:
                    dependency = service == 'tenderdash' and drain and same and marker.get('dependency')
                    self.require(value["Id"] == change["preserve"]["containers"][service]
                                 and (value["RestartCount"] == change["preserve"]["restarts"][service]
                                      or (dependency in ['start-requested', 'started'] and value["RestartCount"] == 0))
                                 and (value["State"]["Running"] or
                                      (dependency in ['stop-requested', 'stopped', 'start-requested'] and marker['phase'] == 'applying')),
                                 "upgrade-unselected-service-changed")
            for component, pin in after.items():
                if before[component] != pin:
                    try:
                        image = json.loads(self.docker("image", "inspect", pin))[0]
                    except Failure:
                        self.docker("pull", "--platform", "linux/" + self.t["architecture"],
                                    pin, timeout=600)
                        image = json.loads(self.docker("image", "inspect", pin))[0]
                    self.require(image["Architecture"] == self.t["architecture"]
                                 and image["Os"] == "linux"
                                 and any(v.split("@")[-1] == pin.split("@")[-1]
                                         for v in image.get("RepoDigests", [])),
                                 "upgrade-image-proof")
            self.preserve_core()
            if self.q["action"] == "upgrade-stage":
                return dict(instanceId=self.t["instanceId"], planId=self.c["planId"],
                            action=self.q["action"], core=core)
            if not same:
                marker = dict(id=change["id"], previousId=change.get("previousId", ""),
                              **{"from": before}, to=after, preserve=change["preserve"],
                              desiredComposeHash=config_hash, phase="applying")
                self.atomic("upgrade.json", marker)
            self.atomic("platform/compose.json", desired)
            path = str(self.root / "platform/compose.json")
            self.docker("compose", "-p", self.project, "-f", path, "config", "--quiet")
            changed = [k for k in desired["services"] if before[k] != after[k]]
            # Tenderdash exits on an ABCI EOF. Gracefully withdraw it before
            # Drive, rather than relying on Docker's crash/restart policy.
            # Save intent first so disconnects after stop are retry-safe.
            if drain and marker['phase'] == 'applying' and marker.get('dependency') not in ['start-requested', 'started']:
                marker['dependency'] = 'stop-requested'
                self.atomic('upgrade.json', marker)
                self.docker('stop', '--time', '120', self.container_name('tenderdash'), timeout=150)
                value = self.inspect_container('tenderdash')
                self.require(value and not value['State']['Running'], 'upgrade-dependency-not-stopped')
                marker['dependency'] = 'stopped'
                self.atomic('upgrade.json', marker)
            immediate = [k for k in changed if not (drain and k == 'tenderdash')]
            if immediate:
                self.docker("compose", "-p", self.project, "-f", path, "up", "-d",
                            "--no-deps", "--pull", "never", *immediate, timeout=300)
            if drain:
                self.wait_abci()
                if marker.get('dependency') != 'started':
                    marker['dependency'] = 'start-requested'
                    self.atomic('upgrade.json', marker)
                if 'tenderdash' in changed:
                    self.docker('compose', '-p', self.project, '-f', path, 'up', '-d',
                                '--no-deps', '--pull', 'never', 'tenderdash', timeout=300)
                elif not self.inspect_container('tenderdash')['State']['Running']:
                    self.docker('start', self.container_name('tenderdash'))
                marker['dependency'] = 'started'
                self.atomic('upgrade.json', marker)
            core = self.preserve_core()
            for service in desired["services"]:
                value = self.inspect_container(service)
                self.require(value and value["State"]["Running"], "upgrade-service-not-running")
                self.verify_image(value, after[service])
                if before[service] == after[service]:
                    self.require(value["Id"] == change["preserve"]["containers"][service]
                                 and value["RestartCount"] ==
                                     (0 if service == 'tenderdash' and drain else change["preserve"]["restarts"][service]),
                                 "upgrade-unselected-service-changed")
            marker["phase"] = "applied"
            self.atomic("upgrade.json", marker)
            return dict(instanceId=self.t["instanceId"], planId=self.c["planId"],
                        action=self.q["action"], core=core)


Worker = UpgradeWorker
