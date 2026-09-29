"""A fake Docker host and dashmate helper for worker tests.

The helper starts from dashmate 4.2.0-beta.3's own created config (captured
from the real release) and renders the files the worker reads, with fresh
rpcauth and Tor salts on every render, as dashmate does. Compose honours the
worker's override (labels, sidecar images, container names, binds) and, like
Compose, recreates a container exactly when its definition changes. Real
rendering and Compose are covered by the helper contract in CI."""

import copy
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets

FIXTURE = Path(__file__).parent / "dashmate-4.2.0-beta.3.json"


def tor_hash(password, salt=None):
    """tor --hash-password, as dashmate's hashTorControlPassword computes it."""
    salt = salt or os.urandom(8)
    data = salt + password.encode()
    whole, rest = divmod(16 << 12, len(data))
    digest = hashlib.sha1(data * whole + data[:rest]).hexdigest()
    return ("16:" + salt.hex() + "60" + digest).upper()


def write(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)


class FakeHost:
    """Mixin preceding the worker class. Records every host command."""

    version = "4.2.0-beta.3"
    # Extra gateway configuration a later release would render.
    gateway_template = ""

    def setup_host(self):
        self.containers = {}
        self.commands = []
        self.missing_images = set()
        self.sequence = 0
        self.units = {}

    # dashmate's helper ------------------------------------------------------

    def helper(self, stage, mode):
        self.commands.append(("helper", mode))
        path = stage / "config.json"
        if mode == "create":
            config = json.loads(FIXTURE.read_text())
            config["configs"] = {self.config_name: config["configs"]["node"]}
            path.write_text(json.dumps(config))
        elif mode == "migrate":
            json.loads(path.read_text())
        else:
            self.fake_render(stage, json.loads(path.read_text())["configs"][self.config_name])

    def fake_render(self, stage, d):
        root = stage / self.config_name
        lines = ["server=1"]
        for user, value in d["core"]["rpc"]["users"].items():
            salt = secrets.token_hex(16)
            digest = hmac.new(salt.encode(), value["password"].encode(), hashlib.sha256).hexdigest()
            lines.append("rpcauth=%s:%s$%s" % (user, salt, digest))
        lines += ["externalip=%s" % d["externalIp"], "devnet=%s" % d["core"]["devnet"]["name"]]
        lines += ["%sindex=1" % x for x in d["core"]["indexes"]]
        if d["core"]["masternode"]["enable"]:
            lines.append("masternodeblsprivkey=" + d["core"]["masternode"]["operator"]["privateKey"])
        if d["core"]["spork"]["address"]:
            lines.append("sporkaddr=" + d["core"]["spork"]["address"])
        lines += ["addnode=%s:%d" % (s["host"], s["port"]) for s in d["core"]["p2p"]["seeds"]]
        write(root / "core/dash.conf", "\n".join(lines) + "\n")
        write(root / "core/tor/torrc", "SocksPort 127.0.0.1:9050\nHashedControlPassword %s\n"
              % tor_hash(d["core"]["tor"]["control"]["password"]))
        td = d["platform"]["drive"]["tenderdash"]
        write(root / "platform/drive/tenderdash/genesis.json", json.dumps(td["genesis"], sort_keys=True))
        write(root / "platform/drive/tenderdash/node_key.json", json.dumps(
            dict(id=td["node"]["id"], priv_key=dict(type="tendermint/PrivKeyEd25519", value=td["node"]["key"]))))
        peers = ",".join("%s@%s:%d" % (p["id"], p["host"], p["port"]) for p in td["p2p"]["persistentPeers"])
        write(root / "platform/drive/tenderdash/config.toml", 'mode = "%s"\nmoniker = "%s"\npersistent-peers = "%s"\n'
              % (td["mode"], td["moniker"], peers))
        write(root / "platform/drive/abci/logger.env", "ABCI_LOG_STDOUT_LEVEL=info\n")
        gateway = d["platform"]["gateway"]
        write(root / "platform/gateway/envoy.yaml", "port: %d\nprovider: %s\n%s"
              % (gateway["listeners"]["dapiAndDrive"]["port"], gateway["ssl"]["provider"], self.gateway_template))
        write(root / "platform/gateway/rate_limiter/rate_limiter.yaml", json.dumps(gateway["rateLimiter"]["whitelist"]))
        write(root / "dynamic-compose.yml", "services: {}\n")
        files = ["docker-compose.yml", str(self.home / self.config_name / "dynamic-compose.yml")]
        if d["core"]["tor"]["enabled"]:
            files.append("docker-compose.tor.yml")
        profiles = ["core"]
        if d["platform"]["enable"]:
            files.append("docker-compose.rate_limiter.yml")
            profiles += ["platform", "platform-dapi-rs"]
        envs = dict(DASHMATE_HOME_DIR=str(self.home), CONFIG_NAME=self.config_name,
                    COMPOSE_PROJECT_NAME="dashmate_00000000_" + self.config_name,
                    COMPOSE_FILE=":".join(files), COMPOSE_PROFILES=",".join(profiles), COMPOSE_PATH_SEPARATOR=":",
                    CORE_DOCKER_IMAGE=d["core"]["docker"]["image"], LOCAL_UID="1000", DESCRIPTION=d["description"])
        write(stage / ".envs", "".join("%s=%s\n" % kv for kv in envs.items()))
        for name in ["docker-compose.yml", "docker-compose.tor.yml", "docker-compose.rate_limiter.yml"]:
            write(stage / ".compose" / name, "services: {}\n")
        write(stage / ".version", self.version + "\n")

    # Compose ----------------------------------------------------------------

    def compose_model(self, args, env):
        base = Path(args[args.index("--project-directory") + 1]).parent
        files = [args[i + 1] for i, a in enumerate(args) if a == "--file"]
        profiles = {args[i + 1] for i, a in enumerate(args) if a == "--profile"}
        for f in files:
            assert Path(f).is_file(), f
        d = json.loads((base / "config.json").read_text())["configs"][self.config_name]
        home = env["DASHMATE_HOME_DIR"]
        services = {}

        def service(name, image, binds, profile, **extra):
            if profile is None or profile in profiles:
                services[name] = dict(
                    image=image,
                    labels={"org.dashmate.service.title": name},
                    volumes=[dict(type="bind", source=home + "/" + self.config_name + "/" + b, target="/etc/" + b, read_only=True) for b in binds],
                    **extra)

        service("dashmate_helper", "dashpay/dashmate-helper:" + self.version, [], None)
        service("core", d["core"]["docker"]["image"], ["core/dash.conf"], "core")
        if any(f.endswith("docker-compose.tor.yml") for f in files):
            service("core_tor", d["core"]["tor"]["docker"]["image"], ["core/tor/torrc"], "core", network_mode="service:core")
        logger = base / self.config_name / "platform/drive/abci/logger.env"
        if "platform" in profiles:
            # Compose reads env files while it evaluates the model.
            assert logger.is_file(), "env file not found"
            environment = dict(x.split("=", 1) for x in logger.read_text().split())
            environment["EPOCH_TIME_LENGTH_S"] = str(d["platform"]["drive"]["abci"]["epochTime"])
            service("drive_abci", d["platform"]["drive"]["abci"]["docker"]["image"], [], "platform", environment=environment)
            service("drive_tenderdash", d["platform"]["drive"]["tenderdash"]["docker"]["image"], ["platform/drive/tenderdash"], "platform")
            service("rs_dapi", d["platform"]["dapi"]["rsDapi"]["docker"]["image"], [], "platform")
            if any(f.endswith("docker-compose.rate_limiter.yml") for f in files):
                service("gateway_rate_limiter", d["platform"]["gateway"]["rateLimiter"]["docker"]["image"],
                        ["platform/gateway/rate_limiter/rate_limiter.yaml"], "platform")
                service("gateway_rate_limiter_redis", "redis:alpine", [], "platform")
            service("gateway", d["platform"]["gateway"]["docker"]["image"],
                    ["platform/gateway/envoy.yaml", "platform/gateway/ssl/bundle.crt", "platform/gateway/ssl/private.key"], "platform")
        for f in files:
            if f.endswith(".json"):
                for name, extra in json.loads(Path(f).read_text())["services"].items():
                    assert name in services, "override defines a service outside the model: " + name
                    s = services[name]
                    s["labels"].update(extra.get("labels", {}))
                    for key in ["image", "container_name"]:
                        if key in extra:
                            s[key] = extra[key]
                    for v in extra.get("volumes", []):
                        s["volumes"].append(dict(v, source=v["source"].replace("${DASHMATE_HOME_DIR}", home)))
        return services

    def docker(self, *args, timeout=120, env=None):
        self.commands.append(args)
        if args[0] == "compose" and "--project-directory" in args:
            services = self.compose_model(args, env)
            if "config" in args:
                return json.dumps(dict(services=services)).encode()
            if "up" in args:
                for name in args[args.index("never") + 1:]:
                    self.converge(name, services[name])
                return b""
            if "stop" in args:
                for name in args[args.index("stop") + 1:]:
                    c = self.containers.get(self.container_name(name))
                    if c:
                        c["State"]["Running"] = False
                return b""
            raise AssertionError(args)
        if args[0] == "compose":  # this tool's own services
            path = Path(args[args.index("-f") + 1])
            if "up" in args:
                for name, s in json.loads(path.read_text())["services"].items():
                    self.converge(name, dict(image=s["image"], labels=s["labels"], container_name=s["container_name"]))
            return b""
        if args[:2] == ("container", "ls"):
            if "--filter" in args:
                name = args[args.index("--filter") + 1][len("name=^/"):-1]
                return (self.containers[name]["Id"] if name in self.containers else "").encode()
            return "".join("%s\t%s\n" % (n, c["Config"]["Labels"].get("dashnet.auxiliary", ""))
                           for n, c in self.containers.items()).encode()
        if args[0] == "inspect":
            return json.dumps([self.containers[args[1]]]).encode()
        if args[:2] == ("image", "inspect"):
            if args[2] in self.missing_images:
                raise self.failure("command-exit-1")
            return json.dumps([dict(Id="image-" + args[2], Architecture="amd64", Os="linux", RepoDigests=[args[2]])]).encode()
        if args[0] == "pull":
            self.missing_images.discard(args[-1])
            return b""
        if args[0] in ["stop", "start", "kill", "rm"]:
            c = self.containers.get(args[-1])
            if args[0] == "rm":
                self.containers.pop(args[-1], None)
            elif c:
                c["State"]["Running"] = args[0] == "start"
                if args[0] == "start":
                    c["RestartCount"] = 0
            return b""
        raise AssertionError(args)

    def converge(self, name, definition):
        container = definition.get("container_name") or name
        current = self.containers.get(container)
        if current is None or current["definition"] != definition:
            self.sequence += 1
            self.containers[container] = dict(
                Id=hashlib.sha256(("%s-%d" % (container, self.sequence)).encode()).hexdigest(),
                Image="image-" + definition["image"], definition=copy.deepcopy(definition),
                Config=dict(Labels=dict(definition["labels"])), State=dict(Running=True), RestartCount=0,
                NetworkSettings=dict(Networks={self.project + "_default": dict(IPAddress="172.24.24.%d" % self.sequence)}))
        self.containers[container]["State"]["Running"] = True

    def systemd(self, units, enable):
        self.units.update(units)
        self.commands.append(("systemctl", "enable", enable))

    def service_container(self, name):
        return self.containers.get(self.container_name(name))
