"""Fixed in-place image rollout. Never renders new configuration or touches Core.

Only the image fields in the existing owned Platform Compose document may change.
An on-host write-ahead marker reconciles lost SSH responses with the same plan.
There is deliberately no downgrade/rollback/reset action.
"""
import copy
import socket
import time


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

    def execute(self):
        self.require(self.q["action"] in ["upgrade-stage", "upgrade-apply"],
                     "upgrade-action-refused")
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
