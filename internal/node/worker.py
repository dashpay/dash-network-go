"""Fixed Dash host operations. No user shell, generic RPC proxy or reset API.

Inputs arrive on stdin over authenticated SSH. Only typed public facts leave
stdout. Private identities and pre-broadcast transactions stay on the node.

Dash services are dashmate's own, rendered by the release being deployed. Each
render runs that release's pinned dashmate-helper image, without network,
Docker socket or its entrypoint, to create (or migrate, as `dashmate update`
does) this node's dashmate config, apply this devnet's settings, and render the
service configs and Compose environment. Compose then runs that release's
dashmate compose files. dashmate never starts, stops or reconfigures a service.
"""

import base64
import copy
import datetime
from decimal import Decimal
import fcntl
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


ACME_ISSUERS = {
    "letsencrypt": "https://acme-v02.api.letsencrypt.org/directory",
    "letsencrypt-staging": "https://acme-staging-v02.api.letsencrypt.org/directory",
}

# dashmate runs as the host's operator; uid 1000 is Ubuntu's default user. The
# helper renders as this user, so every rendered file, and dashmate's LOCAL_UID
# (the gateway's Envoy user), belong to it rather than to root.
SERVICE_UID = 1000
# dashmate's own default network, 0.0.0.0/0, is only a placeholder.
DOCKER_SUBNET = "172.24.24.0/24"
# The dashmate services this tool runs, in start order. dashmate_helper, which
# has no profile, is never started: this tool drives the lifecycle.
CORE_SERVICES = ["core", "core_tor"]
PLATFORM_SERVICES = [
    "drive_abci",
    "drive_tenderdash",
    "rs_dapi",
    "gateway_rate_limiter_redis",
    "gateway_rate_limiter",
    "gateway",
]
NOT_STARTED = ["dashmate_helper"]
# Release components that are dashmate services. Every other selected service
# is a sidecar whose image dashmate chooses and the controller pins.
COMPONENTS = {
    "core": "core",
    "drive_abci": "drive",
    "drive_tenderdash": "tenderdash",
    "rs_dapi": "dapi",
    "gateway": "gateway",
}
# This tool's own services, in their own Compose project.
AUXILIARY = ["miner", "acme"]
# The wallet host's explorer, Insight and faucet read these indexes.
WALLET_INDEXES = ["address", "spent", "timestamp", "tx"]
# Each service carries a digest of its Compose definition and rendered files, so
# Compose recreates exactly the services whose configuration changed.
FINGERPRINT = "dashnet.config"
# The helper release (and config source) a render was made with.
RENDERED = ".dashnet-render.json"

# Runs dashmate's CLI inside the helper image. Nothing but config commands.
RENDER = r"""set -eu
cfg=$1
dm() { yarn dashmate "$@" >/dev/null; }
case "$2" in
create) dm config create "$cfg" base ;;
migrate) dm config get network --config "$cfg" ;;
render)
  dm config render --config "$cfg"
  dm config envs --config "$cfg" --output-file "$DASHMATE_HOME_DIR/.envs"
  rm -rf "$DASHMATE_HOME_DIR/.compose"
  mkdir "$DASHMATE_HOME_DIR/.compose"
  cp /platform/packages/dashmate/docker-compose*.yml "$DASHMATE_HOME_DIR/.compose/"
  node -p "require('/platform/packages/dashmate/package.json').version" >"$DASHMATE_HOME_DIR/.version"
  ;;
*) exit 64 ;;
esac
"""


def protobuf(raw):
    """Bounded decoder for the small, versioned getStatus observation only."""
    fields, pos = {}, 0

    def varint():
        nonlocal pos
        value = 0
        for shift in range(0, 70, 7):
            if pos >= len(raw):
                raise Failure("protobuf-truncated")
            b = raw[pos]
            pos += 1
            value |= (b & 127) << shift
            if b < 128:
                return value
        raise Failure("protobuf-varint")

    while pos < len(raw):
        tag = varint()
        number, wire = tag >> 3, tag & 7
        if number == 0 or number in fields:
            raise Failure("protobuf-field")
        if wire == 0:
            value = varint()
        elif wire in [1, 2, 5]:
            size = varint() if wire == 2 else (8 if wire == 1 else 4)
            if size > len(raw) - pos:
                raise Failure("protobuf-truncated")
            value = raw[pos : pos + size]
            pos += size
        else:
            raise Failure("protobuf-wire")
        fields[number] = value
    return fields


def tor_hash_matches(spec, password):
    """Verifies a Tor HashedControlPassword (RFC 2440 iterated, salted S2K)."""
    if not re.fullmatch(r"16:[0-9A-Fa-f]{58}", spec):
        return False
    raw = bytes.fromhex(spec[3:])
    salt, indicator, digest = raw[:8], raw[8], raw[9:]
    count = (16 + (indicator & 15)) << ((indicator >> 4) + 6)
    data = salt + password.encode()
    whole, rest = divmod(count, len(data))
    return hmac.compare_digest(hashlib.sha1(data * whole + data[:rest]).digest(), digest)


def block_age(stamp, now=None):
    """Seconds since a Tenderdash block time (RFC 3339, nanoseconds), never negative."""
    m = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)", stamp or "")
    if not m:
        raise Failure("tenderdash-block-time")
    zone = "+00:00" if m[3] == "Z" else m[3]
    when = datetime.datetime.fromisoformat(m[1] + (m[2] or "")[:7] + zone)
    now = now or datetime.datetime.now(datetime.timezone.utc)
    return max(0, int((now - when).total_seconds()))


class Failure(Exception):
    pass


class RPCFailure(Failure):
    def __init__(self, method, code):
        self.code = int(code)
        super().__init__("rpc-" + method.replace(" ", "-") + ":" + str(self.code))


class Render:
    """A dashmate render staged beside the live home, not yet installed."""

    def __init__(self, stage, config, envs, services, selection, fingerprints, version, requested):
        self.stage, self.config, self.envs = stage, config, envs
        self.services, self.selection = services, selection
        self.fingerprints, self.version = fingerprints, version
        # dashmate's own image for each sidecar, before any pin.
        self.requested = requested

    def sidecars(self):
        return dict(self.requested)


class Worker:
    def __init__(
        self,
        request,
        root=Path("/var/lib/dashnet"),
        lock=Path("/run/dashnet-bootstrap.lock"),
    ):
        self.q, self.c, self.t = request, request["context"], request["target"]
        self.root, self.lock = root, lock
        self.stage = "preflight"
        self.ports = self.c["ports"]
        self.images = {v["component"]: v["pinned"] for v in self.t["images"]}
        self.project = "dashnet-" + self.c["computePlanId"][:10] + "-" + self.t["name"]
        # dashmate's home: its config.json and rendered service configs.
        self.home = self.root / "dashmate"
        self.config_name = self.t["name"]
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def require(self, condition, code):
        if not condition:
            raise Failure(code)

    def atomic(self, relative, value, private=True):
        target = self.root / relative
        self.require(not target.is_symlink(), "symlink-refused")
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        tmp = target.with_name("." + target.name + "." + secrets.token_hex(8))
        data = value if isinstance(value, str) else json.dumps(value, sort_keys=True)
        fd = os.open(
            tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600 if private else 0o644
        )
        try:
            with os.fdopen(fd, "w") as stream:
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(tmp, target)
            parent = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(parent)
            finally:
                os.close(parent)
        finally:
            if tmp.exists():
                tmp.unlink()

    def read(self, path, default=None):
        p = self.root / path
        if not p.exists():
            return default
        self.require(not p.is_symlink(), "symlink-refused")
        return json.loads(p.read_text())

    def verify_instance(self):
        token_request = urllib.request.Request(
            "http://169.254.169.254/latest/api/token",
            method="PUT",
            headers={"X-aws-ec2-metadata-token-ttl-seconds": "60"},
        )
        with self.opener.open(token_request, timeout=5) as response:
            token = response.read(1024).decode()
        request = urllib.request.Request(
            "http://169.254.169.254/latest/meta-data/instance-id",
            headers={"X-aws-ec2-metadata-token": token},
        )
        with self.opener.open(request, timeout=5) as response:
            actual = response.read(1024).decode()
        self.require(actual == self.t["instanceId"], "instance-mismatch")
        self.require(os.geteuid() == 0, "root-required")

    def log(self, data):
        """Keeps bounded command diagnostics on the host; never in output."""
        try:
            path = self.root / "worker.log"
            if path.exists() and path.stat().st_size > 4 * 1024 * 1024:
                path.unlink()
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            with os.fdopen(fd, "ab") as stream:
                stream.write(data[-65536:] + b"\n")
        except OSError:
            pass

    def run(self, args, stdin=None, timeout=120, env=None):
        try:
            result = subprocess.run(
                args,
                input=stdin,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
                env=env,
            )
        except subprocess.TimeoutExpired:
            raise Failure("command-timeout") from None
        if result.returncode != 0:
            self.log(result.stderr)
        self.require(result.returncode == 0, "command-exit-" + str(result.returncode))
        self.require(len(result.stdout) <= 4 * 1024 * 1024, "command-output-too-large")
        return result.stdout

    def docker(self, *args, timeout=120, env=None):
        if env is not None:
            env = dict(env, PATH="/usr/sbin:/usr/bin:/sbin:/bin", HOME="/root")
        return self.run(
            [
                "docker",
                "--host",
                "unix:///var/run/docker.sock",
                "--config",
                str(self.root / "docker-config"),
                *args,
            ],
            timeout=timeout,
            env=env,
        )

    def container_name(self, service):
        return self.project + "-" + service

    def inspect_container(self, service):
        raw = (
            self.docker(
                "container",
                "ls",
                "-a",
                "--filter",
                "name=^/" + self.container_name(service) + "$",
                "--format",
                "{{.ID}}",
            )
            .decode()
            .strip()
        )
        if not raw:
            return None
        values = json.loads(self.docker("inspect", self.container_name(service)))
        self.require(len(values) == 1, "container-scope")
        value = values[0]
        labels = value["Config"].get("Labels") or {}
        self.require(
            labels.get("dashnet.compute") == self.c["computePlanId"]
            and labels.get("dashnet.node") == self.t["name"]
            and labels.get("dashnet.network") == self.c["network"],
            "container-ownership",
        )
        return value

    def labels(self):
        return {
            "dashnet.compute": self.c["computePlanId"],
            "dashnet.node": self.t["name"],
            "dashnet.network": self.c["network"],
        }

    def service(self, name, image):
        """This tool's own services: the miner, the ACME client, join nodes."""
        return dict(
            image=image,
            container_name=self.container_name(name),
            network_mode="host",
            user="0:0",
            restart="unless-stopped",
            labels=self.labels(),
            logging=dict(driver="local", options={"max-size": "25m", "max-file": "3"}),
        )

    def compose(self, component, services):
        # The miner and ACME client are a separate project from dashmate's.
        project = self.project + "-aux" if component in AUXILIARY else self.project
        path = self.root / component / "compose.json"
        value = dict(name=project, services=services)
        existing = self.read(component + "/compose.json")
        if existing != value:
            self.atomic(component + "/compose.json", value)
        self.docker("compose", "-p", project, "-f", str(path), "config", "--quiet")
        self.docker(
            "compose",
            "-p",
            project,
            "-f",
            str(path),
            "up",
            "-d",
            "--no-deps",
            "--pull",
            "never",
            *services.keys()
        )

    def owned(self):
        self.require(
            self.root.is_dir() and not self.root.is_symlink(), "missing-bootstrap-root"
        )
        self.require(
            (self.root.stat().st_mode & 0o777) == 0o700, "bootstrap-root-permissions"
        )
        self.require(
            (self.root / "owner").read_text().strip()
            == self.c["computePlanId"] + ":" + self.t["instanceId"],
            "bootstrap-owner",
        )
        self.require(
            (self.root / "ready").read_text().strip() == self.c["bootstrapId"],
            "bootstrap-not-ready",
        )
        self.require(
            b"HTTP2" in self.run(["/usr/bin/curl", "--version"]), "curl-http2-required"
        )
        prior = self.read("deployment.json")
        if prior:
            self.require(prior["planId"] == self.c["planId"], "deployment-plan-changed")
        for name in [
            "core",
            "platform",
            "secrets.json",
            "data",
            "transactions",
            "deployment.json",
            "dashmate",
            "dashmate-stage",
        ]:
            self.require(not (self.root / name).is_symlink(), "symlink-refused")
        self.check_containers()
        for service in self.known_services():
            self.inspect_container(service)

    def known_services(self):
        return CORE_SERVICES + PLATFORM_SERVICES + AUXILIARY + ["render"]

    def auxiliary_label(self):
        return self.c["network"] + "/" + self.t["name"]

    def check_containers(self):
        # Reject unknown containers, including stopped ones. Never adopt by name.
        # Operator services that share the host (for example a devnet's quorum
        # list server) are ignored only when explicitly labelled
        # dashnet.auxiliary=<network>/<node> for this exact host, and they can
        # never occupy dashnet's own container namespace.
        own = {self.container_name(x) for x in self.known_services()}
        listing = self.docker(
            "container",
            "ls",
            "-a",
            "--format",
            '{{.Names}}\t{{.Label "dashnet.auxiliary"}}',
        ).decode()
        for line in listing.splitlines():
            name, _, auxiliary = line.partition("\t")
            self.require(
                name in own
                or (
                    auxiliary == self.auxiliary_label()
                    and not name.startswith(self.project + "-")
                ),
                "unexpected-container",
            )

    def secret(self):
        value = self.read("secrets.json")
        if value is None:
            value = {"rpcPassword": secrets.token_hex(32)}
            self.atomic("secrets.json", value)
        return value

    def rpc(self, method, params=None, wallet=False):
        secret = self.read("secrets.json")
        self.require(secret is not None, "missing-rpc-identity")
        url = (
            "http://127.0.0.1:"
            + str(self.ports["coreRPC"])
            + ("/wallet/dashnet" if wallet else "/")
        )
        payload = json.dumps(
            dict(jsonrpc="1.0", id="dashnet", method=method, params=params or [])
        ).encode()
        auth = base64.b64encode(("dashnet:" + secret["rpcPassword"]).encode()).decode()
        request = urllib.request.Request(
            url,
            data=payload,
            headers={
                "Authorization": "Basic " + auth,
                "Content-Type": "application/json",
            },
        )
        try:
            response = self.opener.open(request, timeout=90)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(4 * 1024 * 1024 + 1)
        self.require(len(raw) <= 4 * 1024 * 1024, "rpc-response-size")
        data = json.loads(raw)
        if data.get("error"):
            raise RPCFailure(method, data["error"]["code"])
        return data["result"]

    # dashmate configuration and rendering ------------------------------------

    def own(self, path):
        if os.geteuid() == 0:
            os.chown(path, SERVICE_UID, SERVICE_UID)

    def inputs(self):
        """What Core has been finalized with and what Platform started with.

        Every later render reproduces them; neither is ever replaced."""
        state = self.read("core/state.json", {})
        return state.get("sporkAddress") if state.get("final") else None, self.read(
            "platform/inputs.json"
        )

    def configure(self, config, spork):
        """Applies this devnet's settings to the node's dashmate config.

        Only options: every other value is the release's own default, or the
        value its migrations produced."""
        s, p, t, c = self.secret(), self.ports, self.t, self.c
        d = config["configs"][self.config_name]
        validator, final = t["role"] == "validator", spork is not None

        def put(path, value, create=False):
            node, keys = d, path.split(".")
            for key in keys[:-1]:
                self.require(isinstance(node.get(key), dict), "dashmate-option-missing")
                node = node[key]
            self.require(create or keys[-1] in node, "dashmate-option-missing")
            node[keys[-1]] = value

        # RPC: dashmate's per-service users, each with its own password, plus
        # the user this tool (and the wallet host's services) authenticate as.
        users = d["core"]["rpc"]["users"]
        passwords = s.setdefault("rpcUsers", {})
        for user in users:
            if user not in passwords:
                passwords[user] = secrets.token_hex(32)
        if s != self.read("secrets.json"):
            self.atomic("secrets.json", s)
        for user in users:
            users[user]["password"] = passwords[user]
        users["dashnet"] = dict(password=s["rpcPassword"], whitelist=None, lowPriority=False)

        own = t["peerAddress"] + ":" + str(p["coreP2P"])
        seeds = []
        for peer in c["corePeers"]:
            if peer != own:
                host, port = peer.rsplit(":", 1)
                seeds.append(dict(host=host, port=int(port)))
        put("network", "devnet")
        put("description", "dashnet " + c["network"] + "/" + t["name"])
        put("externalIp", t["peerAddress"])
        put("docker.network.subnet", DOCKER_SUBNET)
        put("core.docker.image", self.images["core"])
        put("core.p2p.port", p["coreP2P"])
        put("core.p2p.seeds", seeds)
        put("core.rpc.port", p["coreRPC"])
        put("core.zmq.port", p["coreZMQ"])
        put("core.devnet.name", c["coreNetwork"])
        put("core.devnet.powTargetSpacing", self.block_seconds())
        # Blocks are paced by the miner, not by difficulty.
        put("core.devnet.minimumDifficultyBlocks", 1000000)
        put("core.indexes", WALLET_INDEXES if t["role"] == "wallet" else [])
        put("core.miner.enable", False)
        put("core.spork.address", spork)
        put("core.spork.privateKey", s["sporkKey"] if final and t["role"] == "wallet" else None)
        put("core.masternode.enable", validator and final)
        put(
            "core.masternode.operator.privateKey",
            s["operatorPrivateKey"] if validator and final else None,
        )
        put("platform.enable", validator)
        if not validator:
            return
        put("platform.drive.abci.docker.image", self.images["drive"])
        put("platform.drive.abci.epochTime", int(c.get("platformEpochSeconds") or 3600))
        # dashmate has no devnet preset: these are Core's llmq_devnet* settings.
        for name, kind, window, signers, rotation in [
            ("validatorSet", 107, 24, 4, False),
            ("chainLock", 101, 24, 4, False),
            ("instantLock", 105, 48, 2, True),
        ]:
            put(
                "platform.drive.abci." + name + ".quorum",
                dict(llmqType=kind, dkgInterval=window, activeSigners=signers, rotation=rotation),
            )
        put("platform.dapi.rsDapi.docker.image", self.images["dapi"])
        put("platform.gateway.docker.image", self.images["gateway"])
        put("platform.gateway.listeners.dapiAndDrive.port", p["gateway"])
        put("platform.gateway.ssl.enabled", True)
        tls = c.get("gatewayTls")
        put("platform.gateway.ssl.provider", "letsencrypt" if tls else "self-signed")
        if tls:
            put("platform.gateway.ssl.providerConfigs.letsencrypt.email", tls["email"])
            put(
                "platform.gateway.ssl.providerConfigs.letsencrypt.acmeDirectoryUrl",
                ACME_ISSUERS[tls["issuer"]],
            )
        # The fleet (including the wallet host's reverse proxy) is not limited.
        put(
            "platform.gateway.rateLimiter.whitelist",
            sorted({x.rsplit(":", 1)[0] for x in c["corePeers"]}),
        )
        td = "platform.drive.tenderdash."
        put(td + "docker.image", self.images["tenderdash"])
        put(td + "mode", "validator")
        put(td + "moniker", t["name"])
        put(td + "p2p.port", p["platformP2P"])
        put(td + "rpc.port", p["platformRPC"])
        if "platformNodeID" in s:
            put(td + "node.id", s["platformNodeID"])
            put(td + "node.key", s["nodePrivateKey"])
        genesis = d["platform"]["drive"]["tenderdash"]["genesis"]
        genesis.update(
            chain_id=c["platformChainId"],
            genesis_time=c["genesisTime"],
            validator_quorum_type=107,
        )
        genesis["consensus_params"]["version"] = dict(
            app_version=str(c["initialProtocolVersion"])
        )
        _, platform = self.inputs()
        if platform:
            genesis["initial_core_chain_locked_height"] = platform["genesisCoreHeight"]
            put(
                td + "p2p.persistentPeers",
                [
                    dict(id=v["nodeId"], host=v["address"], port=p["platformP2P"])
                    for v in platform["peers"]
                    if v["name"] != t["name"]
                ],
            )

    def helper(self, stage, mode):
        """dashmate's own CLI from the release's helper image: no network, no
        Docker socket, not its entrypoint (which expects the socket), and not
        root. The stage is mounted at the live home's path, so every path
        dashmate renders is the one the services will mount."""
        if self.inspect_container("render"):  # left by an interrupted render
            self.docker("rm", "--force", self.container_name("render"))
        labels = []
        for key, value in self.labels().items():
            labels += ["--label", key + "=" + value]
        self.docker(
            "run",
            "--rm",
            "--name",
            self.container_name("render"),
            *labels,
            "--network",
            "none",
            "--pull",
            "never",
            "--user",
            "%d:%d" % (SERVICE_UID, SERVICE_UID),
            "--entrypoint",
            "/bin/sh",
            "--workdir",
            "/platform",
            "--env",
            "DASHMATE_HOME_DIR=" + str(self.home),
            "--env",
            "HOME=/tmp",
            "--env",
            "YARN_ENABLE_TELEMETRY=0",
            "--volume",
            str(stage) + ":" + str(self.home),
            self.images["helper"],
            "-c",
            RENDER,
            "dashnet",
            self.config_name,
            mode,
            timeout=600,
        )

    def envs(self, base):
        values = {}
        for line in (base / ".envs").read_text().splitlines():
            if line:
                key, sep, value = line.partition("=")
                self.require(sep and re.fullmatch(r"[A-Z][A-Z0-9_]*", key), "dashmate-envs")
                values[key] = value
        return values

    def project_files(self, base):
        """dashmate's compose files and profiles from its Compose environment,
        read from base (the stage or the live home)."""
        envs = self.envs(base)
        files = []
        for name in envs["COMPOSE_FILE"].split(envs.get("COMPOSE_PATH_SEPARATOR") or ":"):
            if name.startswith("/"):
                path = Path(name)
                self.require(path.is_relative_to(self.home), "dashmate-compose-file")
                files.append(base / path.relative_to(self.home))
            else:
                self.require(re.fullmatch(r"docker-compose[a-z0-9_.-]*\.yml", name), "dashmate-compose-file")
                files.append(base / ".compose" / name)
        for path in files:
            self.require(path.is_file() and not path.is_symlink(), "dashmate-compose-file")
        profiles = [x for x in envs.get("COMPOSE_PROFILES", "").split(",") if x]
        # Compose reads the environment as dashmate passes it: process variables.
        environment = {k: v for k, v in envs.items() if not k.startswith("COMPOSE_")}
        return files, profiles, environment

    def dm_compose(self, base, *args, override=True, timeout=300):
        files, profiles, environment = self.project_files(base)
        # dashmate's compose files reach its home only through
        # DASHMATE_HOME_DIR, so a stage is evaluated where it lies.
        environment["DASHMATE_HOME_DIR"] = str(base)
        command = ["compose", "--project-name", self.project, "--project-directory", str(base / ".compose")]
        if override:
            files.append(base / ".dashnet-compose.json")
        for path in files:
            command += ["--file", str(path)]
        for profile in profiles:
            command += ["--profile", profile]
        return self.docker(*command, *args, timeout=timeout, env=environment)

    def compose_services(self, base, override=True):
        """The Compose model as the live home will run it."""
        text = self.dm_compose(base, "config", "--format", "json", override=override).decode()
        if base != self.home:
            text = text.replace(json.dumps(str(base))[1:-1], json.dumps(str(self.home))[1:-1])
        return json.loads(text)["services"]

    def select(self, services):
        names = set(services) - set(NOT_STARTED)
        self.require(names <= set(CORE_SERVICES + PLATFORM_SERVICES), "unsupported-dashmate-service")
        for service, component in COMPONENTS.items():
            if service in names:
                self.require(services[service]["image"] == self.images[component], "dashmate-image-not-pinned")
        return [s for s in CORE_SERVICES + PLATFORM_SERVICES if s in names]

    def override(self, selection, fingerprints=None):
        """This tool's Compose additions: ownership labels, its container names,
        pinned sidecar images, and a client-only dash.conf in Core for dash-cli
        (dashd reads its own). No service configuration."""
        pins = self.c.get("sidecarImages") or {}
        services = {}
        for name in selection:
            labels = self.labels()
            if fingerprints:
                labels[FINGERPRINT] = fingerprints[name]
            value = dict(container_name=self.container_name(name), labels=labels)
            if name not in COMPONENTS and name in pins:
                value["image"] = pins[name]
            services[name] = value
        if "core" in services:
            services["core"]["volumes"] = [
                dict(type="bind", source="${DASHMATE_HOME_DIR}/.client/dash.conf", target="/etc/dash/dash.conf", read_only=True)
            ]
        return dict(services=services)

    def write(self, path, data, mode=0o644):
        """Rewrites a file in place, as dashmate does: a running container's
        bind mount of it keeps seeing it."""
        self.require(not path.is_symlink(), "symlink-refused")
        path.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(path, os.O_WRONLY | os.O_CREAT, mode)
        try:
            os.ftruncate(fd, 0)
            os.write(fd, data)
            os.fsync(fd)
        finally:
            os.close(fd)
        os.chmod(path, mode)
        self.own(path)

    def normalized(self, path, data, config):
        """File content without the salts dashmate draws on every render.

        Each salted line is first verified against the configured secret, so a
        changed credential still changes the digest."""
        d = config["configs"][self.config_name]
        if path.name == "dash.conf" and path.parent.name == "core":
            users = d["core"]["rpc"]["users"]
            lines = []
            for line in data.decode().splitlines(keepends=True):
                if line.startswith("rpcauth="):
                    user, _, auth = line[len("rpcauth="):].strip().partition(":")
                    salt, _, digest = auth.partition("$")
                    password = users.get(user, {}).get("password", "")
                    expected = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
                    self.require(password and hmac.compare_digest(expected, digest), "rpcauth-mismatch")
                    line = "rpcauth=" + user + ":verified\n"
                lines.append(line)
            return "".join(lines).encode()
        if path.name == "torrc":
            password = d["core"]["tor"]["control"]["password"]
            lines = []
            for line in data.decode().splitlines(keepends=True):
                if line.startswith("HashedControlPassword "):
                    self.require(tor_hash_matches(line.split()[1], password), "tor-password-mismatch")
                    line = "HashedControlPassword verified\n"
                lines.append(line)
            return "".join(lines).encode()
        return data

    def content(self, path, config):
        self.require(not path.is_symlink(), "symlink-refused")
        if path.is_dir():
            return {
                str(p.relative_to(path)): self.content(p, config)
                for p in sorted(path.rglob("*"))
                if not p.is_dir()
            }
        if not path.exists():
            return None
        return hashlib.sha256(self.normalized(path, path.read_bytes(), config)).hexdigest()

    def mounts(self, service):
        """The rendered configuration a service bind-mounts from the home.
        Gateway certificates are not configuration: renewals reload them."""
        ssl = self.home / self.config_name / "platform/gateway/ssl"
        out = []
        for volume in service.get("volumes") or []:
            source = Path(volume.get("source") or "/")
            if (
                volume.get("type") == "bind"
                and source.is_relative_to(self.home)
                and not source.is_relative_to(ssl)
            ):
                out.append(source.relative_to(self.home))
        return out

    def fingerprints(self, base, services, selection, config):
        out = {}
        for name in selection:
            value = copy.deepcopy(services[name])
            (value.get("labels") or {}).pop(FINGERPRINT, None)
            files = {str(m): self.content(base / m, config) for m in self.mounts(value)}
            entry = [value, files]
            if name == "core_tor":
                # It shares Core's network namespace: a new Core means a new Tor.
                entry.append(out.get("core"))
            out[name] = hashlib.sha256(json.dumps(entry, sort_keys=True).encode()).hexdigest()
        return out

    def rendered(self, directory):
        """The release and source a render records, and its config."""
        meta, config = directory / RENDERED, directory / "config.json"
        if not meta.is_file() or not config.is_file():
            return None
        return json.loads(meta.read_text()), config.read_bytes()

    def fresh(self, stage):
        if stage.exists():
            shutil.rmtree(stage)
        stage.mkdir(mode=0o700)
        self.own(stage)

    def render(self):
        """Renders this node with its release's dashmate into the stage.

        dashmate's output follows from its config and release alone, so the
        last stage is reused when both are unchanged (a resume, a start after
        the render stage, an apply after staging), and dashmate migrates the
        config only for a new release."""
        self.stage = "dashmate-render"
        spork, platform = self.inputs()
        helper = self.images["helper"]
        stage = self.root / "dashmate-stage"
        self.require(not stage.is_symlink(), "symlink-refused")
        live = self.home / "config.json"
        source = hashlib.sha256(live.read_bytes()).hexdigest() if live.exists() else ""
        saved, installed = self.rendered(stage), self.rendered(self.home)
        fresh = False
        if saved and saved[0] == dict(helper=helper, source=source):
            config = json.loads(saved[1])  # already created or migrated
        elif installed and installed[0]["helper"] == helper:
            config = json.loads(installed[1])  # already this release's format
        else:
            self.fresh(stage)
            fresh = True
            if live.exists():
                shutil.copyfile(live, stage / "config.json")
                self.own(stage / "config.json")
                self.helper(stage, "migrate")
            else:
                self.helper(stage, "create")
            config = json.loads((stage / "config.json").read_text())
        self.configure(config, spork)
        data = json.dumps(config, indent=2).encode()
        if fresh or not (saved and saved[0]["helper"] == helper and saved[1] == data):
            if not fresh:
                self.fresh(stage)
            self.write(stage / "config.json", data, 0o600)
            self.helper(stage, "render")
            self.write(stage / RENDERED, json.dumps(dict(helper=helper, source=source)).encode(), 0o600)
        version = (stage / ".version").read_text().strip()
        self.require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?", version), "dashmate-version")
        client = [
            "devnet=" + self.c["coreNetwork"],
            "rpcuser=dashnet",
            "rpcpassword=" + self.secret()["rpcPassword"],
            "[devnet]",
            "rpcconnect=127.0.0.1",
            "rpcport=" + str(self.ports["coreRPC"]),
        ]
        self.write(stage / ".client/dash.conf", ("\n".join(client) + "\n").encode(), 0o600)
        # Selection, then this tool's additions, then their fingerprints.
        raw = self.compose_services(stage, override=False)
        selection = self.select(raw)
        requested = {s: raw[s]["image"] for s in selection if s not in COMPONENTS}
        self.write(stage / ".dashnet-compose.json", json.dumps(self.override(selection)).encode())
        services = self.compose_services(stage)
        fingerprints = self.fingerprints(stage, services, selection, config)
        self.write(stage / ".dashnet-compose.json", json.dumps(self.override(selection, fingerprints)).encode())
        self.require(self.select(self.compose_services(stage)) == selection, "dashmate-selection-changed")
        r = Render(stage, config, self.envs(stage), services, selection, fingerprints, version, requested)
        if platform:
            self.guard_identity(r)
        return r

    def guard_identity(self, r):
        """Platform genesis and node identity, once started, are never replaced."""
        tenderdash = r.stage / self.config_name / "platform/drive/tenderdash"
        identity = {
            name: self.content(tenderdash / name, r.config)
            for name in ["genesis.json", "node_key.json"]
        }
        self.require(all(identity.values()), "missing-platform-identity")
        self.immutable("platform/identity.json", identity)

    def install(self, r, services):
        """Installs the staged render for the given services: dashmate's own
        state and Compose environment, and the rendered trees those services
        use (mounted files and env files alike). Rendered files of services
        outside the scope keep their live content; they are only created when
        missing, since Compose reads every enabled service's files."""
        trees = []
        if any(s in CORE_SERVICES for s in services):
            trees += [Path(self.config_name) / "core", Path(".client")]
        if any(s in PLATFORM_SERVICES for s in services):
            trees.append(Path(self.config_name) / "platform")
        if not self.home.exists():
            self.home.mkdir(mode=0o700)
            self.own(self.home)
        paths = [p.relative_to(r.stage) for p in sorted(r.stage.rglob("*")) if not p.is_dir()]
        for name in ["config.json", ".envs", ".version", ".dashnet-compose.json", RENDERED]:
            self.require(Path(name) in paths, "dashmate-render-incomplete")
        for path in paths:
            source = r.stage / path
            self.require(source.is_file() and not source.is_symlink(), "dashmate-render-incomplete")
            scoped = len(path.parts) < 2 or path.parts[0] == ".compose" or path == Path(self.config_name) / "dynamic-compose.yml" \
                or any(path.is_relative_to(tree) for tree in trees)
            if scoped or not (self.home / path).exists():
                private = path.name in ["config.json", ".envs", RENDERED] or path.parts[0] == ".client"
                self.write(self.home / path, source.read_bytes(), 0o600 if private else 0o644)
        # Parent directories created above belong to the service user too.
        for directory in sorted({(self.home / p).parent for p in paths}):
            if directory.exists():
                self.own(directory)

    def discard(self, r):
        """The stage is kept for the next render to reuse."""

    def selection(self):
        """The services the live render runs."""
        path = self.home / ".dashnet-compose.json"
        if not path.exists():
            return []
        return [s for s in CORE_SERVICES + PLATFORM_SERVICES if s in json.loads(path.read_text())["services"]]

    def label(self, container):
        return (container["Config"].get("Labels") or {}).get(FINGERPRINT, "")

    def verify_pin(self, pin):
        """A pinned image is present, for this architecture and digest."""
        try:
            image = json.loads(self.docker("image", "inspect", pin))[0]
        except Failure:
            self.docker("pull", "--platform", "linux/" + self.t["architecture"], pin, timeout=600)
            image = json.loads(self.docker("image", "inspect", pin))[0]
        self.require(
            image["Architecture"] == self.t["architecture"]
            and image["Os"] == "linux"
            and any(v.split("@")[-1] == pin.split("@")[-1] for v in image.get("RepoDigests", [])),
            "image-proof",
        )

    def up(self, r, services):
        """Converges the given services with the installed render. Compose
        recreates exactly those whose configuration fingerprint changed."""
        for name in services:
            if name not in COMPONENTS:
                pin = (self.c.get("sidecarImages") or {}).get(name)
                self.require(pin and r.services[name]["image"] == pin, "sidecar-image-not-pinned")
                self.verify_pin(pin)
        self.dm_compose(
            self.home, "up", "--detach", "--no-deps", "--no-build", "--pull", "never", *services, timeout=600
        )
        # Converged means running this render. Readiness is the health gates'
        # job: Tenderdash, for one, restarts until Drive opens ABCI.
        for name in services:
            value = self.inspect_container(name)
            self.require(
                value
                and (value["State"]["Running"] or value["State"].get("Restarting"))
                and self.label(value) == r.fingerprints[name],
                "dashmate-service-not-converged",
            )

    def core_config(self):
        """Digest of Core's live dash.conf, without dashmate's per-render salts."""
        config = json.loads((self.home / "config.json").read_text())
        path = self.home / self.config_name / "core/dash.conf"
        return hashlib.sha256(self.normalized(path, path.read_bytes(), config)).hexdigest()

    # Core ---------------------------------------------------------------------

    def ensure_core(self, final=False):
        self.stage = "core-config"
        # A resume must never revert the final BLS/spork configuration.
        state = self.read("core/state.json", {})
        if final:
            self.require(self.q.get("sporkAddress"), "missing-spork-address")
            if state.get("final"):
                self.require(state["sporkAddress"] == self.q["sporkAddress"], "spork-address-changed")
            else:
                self.atomic("core/state.json", {"final": True, "sporkAddress": self.q["sporkAddress"]})
        r = self.render()
        try:
            services = [s for s in r.selection if s in CORE_SERVICES]
            self.install(r, services)
            self.stage = "core-start"
            self.up(r, services)
        finally:
            self.discard(r)
        # Each caller has a deadline; a bounded readiness wait avoids racing RPC
        # initialization without launching a persistent control-plane daemon.
        deadline = time.monotonic() + 90
        while True:
            try:
                return self.core_status()
            except (RPCFailure, urllib.error.URLError, ConnectionError):
                if time.monotonic() >= deadline:
                    raise Failure("core-rpc-not-ready") from None
                time.sleep(1)

    def core_status(self):
        value = self.inspect_container("core")
        self.require(value and value["State"]["Running"], "core-not-running")
        self.verify_image(value, self.images["core"])
        if "core_tor" in self.selection():
            tor = self.inspect_container("core_tor")
            self.require(tor and tor["State"]["Running"], "core-tor-not-running")
        info = self.rpc("getblockchaininfo")
        self.require(info["chain"] == "devnet-" + self.c["coreNetwork"], "wrong-chain")
        sync = self.rpc("mnsync", ["status"])
        genesis = self.rpc("getblockhash", [1])
        quorums = self.rpc("quorum", ["list"])
        chainlock = 0
        try:
            chainlock = self.rpc("getbestchainlock")["height"]
        except RPCFailure as error:
            if error.code not in [-32603, -5, -8]:
                raise
        state, protx = "", ""
        if self.t["role"] == "validator":
            try:
                mn = self.rpc("masternode", ["status"])
                state, protx = mn.get("state", ""), mn.get("proTxHash", "")
            except RPCFailure as error:
                if error.code not in [-32603, -1, -8]:
                    raise
        mining = None
        if self.c.get("miningNodeName") == self.t["name"]:
            miner = self.inspect_container("miner")
            if miner:
                self.verify_image(miner, self.images["core"])
                mining = dict(
                    running=miner["State"]["Running"],
                    containerId=miner["Id"],
                    restarts=miner["RestartCount"],
                )
        return dict(
            mining=mining,
            genesis=genesis,
            height=info["blocks"],
            headers=info["headers"],
            synced=sync["IsSynced"] and sync["IsBlockchainSynced"],
            ibd=info["initialblockdownload"],
            peers=self.rpc("getconnectioncount"),
            containerId=value["Id"],
            restarts=value["RestartCount"],
            configSha256=self.core_config(),
            masternodeState=state,
            proTxHash=protx,
            chainLockHeight=chainlock,
            quorums={k: len(v) for k, v in quorums.items()},
        )

    def verify_image(self, container, pinned):
        image = json.loads(self.docker("image", "inspect", pinned))[0]
        self.require(container["Image"] == image["Id"], "running-image-drift")
        self.require(
            image["Architecture"] == self.t["architecture"] and image["Os"] == "linux",
            "image-platform-drift",
        )

    def render_info(self):
        """The release's dashmate version and the sidecar images it selects,
        for the controller to pin. Nothing is installed or started."""
        r = self.render()
        try:
            return dict(render=dict(version=r.version, sidecars=r.sidecars()))
        finally:
            self.discard(r)

    def identity(self):
        self.require(self.t["role"] == "validator", "validator-only")
        secret = self.secret()
        if "operatorPrivateKey" not in secret:
            pair = self.rpc("bls", ["generate"])
            secret.update(
                operatorPrivateKey=pair["secret"], operatorPublicKey=pair["public"]
            )
            raw = base64.b64decode(self.q["nodePrivateKey"], validate=True)
            self.require(len(raw) == 64, "invalid-node-key")
            secret["nodePrivateKey"] = self.q["nodePrivateKey"]
            secret["platformNodeID"] = hashlib.sha256(raw[32:]).hexdigest()[:40]
            secret["tlsCertificate"], secret["tlsPrivateKey"] = (
                self.q["tlsCertificate"],
                self.q["tlsPrivateKey"],
            )
            self.atomic("secrets.json", secret)
        return dict(
            operatorPublicKey=secret["operatorPublicKey"],
            platformNodeId=secret["platformNodeID"],
        )

    def address(self, label):
        try:
            found = self.rpc("getaddressesbylabel", [label], True)
            self.require(len(found) == 1, "ambiguous-wallet-label")
            return next(iter(found))
        except RPCFailure as error:
            if error.code != -11:
                raise
        return self.rpc("getnewaddress", [label], True)

    def wallet(self):
        self.require(self.t["role"] == "wallet", "wallet-only")
        if "dashnet" not in self.rpc("listwallets"):
            names = [v["name"] for v in self.rpc("listwalletdir")["wallets"]]
            if "dashnet" in names:
                self.rpc("loadwallet", ["dashnet"])
            else:
                self.rpc("createwallet", ["dashnet"])
        secret = self.secret()
        payout, spork = self.address("dashnet:payout"), self.address("dashnet:spork")
        if "sporkKey" not in secret:
            secret["sporkKey"] = self.rpc("dumpprivkey", [spork], True)
            self.atomic("secrets.json", secret)
        # Core can restart between broadcast and the controller checkpoint. Re-lock
        # every locally recorded collateral before any subsequent wallet funding.
        for path in sorted((self.root / "transactions").glob("*.json")):
            saved = self.read(str(path.relative_to(self.root)))
            try:
                self.rpc("getrawtransaction", [saved["txid"], True])
            except RPCFailure as error:
                if error.code == -5:
                    continue  # signed, not yet submitted
                raise
            self.lock_collateral(saved)
        return dict(payoutAddress=payout, sporkAddress=spork)

    def lock_collateral(self, saved):
        output = dict(txid=saved["txid"], vout=saved["collateralIndex"])
        unspent = self.rpc("gettxout", [output["txid"], output["vout"]])
        self.require(
            unspent is not None and Decimal(str(unspent["value"])) == 4000,
            "collateral-spent-or-changed",
        )
        self.require(
            self.rpc("lockunspent", [False, [output], True], True),
            "collateral-lock-failed",
        )
        self.require(
            output in self.rpc("listlockunspent", [], True),
            "collateral-lock-not-observed",
        )

    def fund(self):
        self.require(self.t["role"] == "wallet", "wallet-only")
        target = self.q["requiredBalance"]
        self.require(0 < target <= 150000, "funding-target")
        address = self.address("dashnet:payout")
        # Mining is an explicit create-devnet action. No faucet/testnet funds are
        # used. First advance to the premine height at minimum difficulty, before
        # any EvoNode exists, so quorums form on a mature chain (legacy devnets
        # use minimumdifficultyblocks=4032). Then observe balance each retry.
        premine = int(self.c.get("premineHeight", 0))
        self.require(0 <= premine <= 20000, "premine-height")
        while self.rpc("getblockcount") < premine:
            self.stage = "premine"
            self.rpc("generatetoaddress", [min(250, premine - self.rpc("getblockcount")), address, 100000000])
        limit = max(1000, premine + 1000)
        for _ in range(1000):
            locked = {
                (v["txid"], v["vout"]) for v in self.rpc("listlockunspent", [], True)
            }
            balance = int(
                sum(
                    (
                        Decimal(str(v["amount"]))
                        for v in self.rpc("listunspent", [1], True)
                        if v["spendable"]
                        and v.get("safe", True)
                        and (v["txid"], v["vout"]) not in locked
                    ),
                    Decimal(0),
                )
            )
            if balance >= target:
                return dict(balance=balance)
            self.require(self.rpc("getblockcount") < limit, "bootstrap-mining-limit")
            # Never burst-mine once DKG runs (a resumed deploy): members would
            # be PoSe-punished for sessions they had no time to complete.
            self.require(self.rpc("spork", ["active"]).get("SPORK_17_QUORUM_DKG_ENABLED", True) is False, "funding-needs-mining-after-dkg")
            self.rpc("generatetoaddress", [1, address, 1000000])
        raise Failure("funding-not-reached")

    def register(self):
        self.require(self.t["role"] == "wallet", "wallet-only")
        target = self.q["registration"]
        name = target["name"]
        collateral, owner = self.address(
            "dashnet:" + name + ":collateral"
        ), self.address("dashnet:" + name + ":owner")
        path = "transactions/" + name + ".json"
        saved = self.read(path)
        if saved is not None:
            self.require(saved["target"] == target, "registration-intent-changed")
        else:
            self.stage = "registration-prepare"
            raw = self.rpc(
                "protx",
                [
                    "register_fund_evo",
                    collateral,
                    target["address"] + ":" + str(self.ports["coreP2P"]),
                    owner,
                    target["operatorPublicKey"],
                    owner,
                    0,
                    self.address("dashnet:payout"),
                    target["nodeId"],
                    self.ports["platformP2P"],
                    self.ports["gateway"],
                    None,
                    False,
                ],
                True,
            )
            decoded = self.rpc("decoderawtransaction", [raw])
            outputs = [
                v["n"]
                for v in decoded["vout"]
                if Decimal(str(v["value"])) == 4000
                and collateral
                in v["scriptPubKey"].get(
                    "addresses", [v["scriptPubKey"].get("address")]
                )
            ]
            self.require(len(outputs) == 1, "ambiguous-collateral-output")
            saved = dict(
                target=target, txid=decoded["txid"], hex=raw, collateralIndex=outputs[0]
            )
            # Durable signed bytes BEFORE submission. A lost response or runner
            # can never cause a second registration/funding transaction.
            self.atomic(path, saved)
        self.stage = "registration-submit"
        try:
            tx = self.rpc("getrawtransaction", [saved["txid"], True])
        except RPCFailure as error:
            if error.code != -5:
                raise
            returned = self.rpc("sendrawtransaction", [saved["hex"]])
            self.require(returned == saved["txid"], "registration-txid-mismatch")
            tx = self.rpc("getrawtransaction", [saved["txid"], True])
        self.lock_collateral(saved)
        required = self.q.get("requiredConfirmations", 1)
        self.require(1 <= required <= 6, "registration-confirmations")
        for _ in range(required + 1):
            if tx.get("confirmations", 0) >= required:
                break
            self.require(self.rpc("getblockcount") < max(2000, int(self.c.get("premineHeight", 0)) + 2000), "registration-mining-limit")
            self.rpc("generatetoaddress", [1, self.address("dashnet:payout"), 1000000])
            tx = self.rpc("getrawtransaction", [saved["txid"], True])
        info = self.rpc("protx", ["info", saved["txid"]])
        state = info["state"]
        self.require(
            state["pubKeyOperator"] == target["operatorPublicKey"]
            and state["ownerAddress"] == owner
            and state["platformNodeID"] == target["nodeId"],
            "registration-readback-mismatch",
        )
        self.require(tx.get("confirmations", 0) >= required, "registration-unconfirmed")
        return dict(proTxHash=saved["txid"], confirmations=tx["confirmations"])

    def activate(self):
        self.require(self.t["role"] == "wallet", "wallet-only")
        # As long-running devnets do: InstantSend (2, 3) and superblocks (9)
        # as well as DKG, ChainLocks and all-connected quorums. Core 23 sporks
        # default to off, so without 2 no transaction is ever InstantSend-locked.
        required = [
            "SPORK_2_INSTANTSEND_ENABLED",
            "SPORK_3_INSTANTSEND_BLOCK_FILTERING",
            "SPORK_9_SUPERBLOCKS_ENABLED",
            "SPORK_17_QUORUM_DKG_ENABLED",
            "SPORK_19_CHAINLOCKS_ENABLED",
            "SPORK_21_QUORUM_ALL_CONNECTED",
        ]
        active = self.rpc("spork", ["active"])
        self.require(all(key in active for key in required), "unsupported-spork-profile")
        for key in required:
            if not active[key]:
                self.require(
                    self.rpc("sporkupdate", [key, 0]) == "success",
                    "spork-update-not-accepted",
                )
        observed = self.rpc("spork", ["active"])
        self.require(all(observed.get(key) is True for key in required), "spork-not-active")
        return {}

    def block_seconds(self):
        interval = self.c["miningIntervalSeconds"]
        self.require(isinstance(interval, int) and 8 <= interval <= 600, "mining-interval")
        return interval

    def fast_forward(self):
        """Mine the cycles a rotated quorum waits for at minimum difficulty.

        A DIP-0024 quorum is assembled from quarters picked at the three previous
        cycle bases, each from the masternode list 8 blocks earlier in which a
        masternode counts once confirmed (so from 10 blocks after it registered), so the first
        full llmq_devnet_dip0024 (48-block cycle) forms three cycles after the
        EvoNodes registered. Block processing records those picks whether or not
        DKG runs. Before activation SPORK_17 is off: no DKG session exists and no
        member can be PoSe-punished, so those cycles need not take ten seconds
        per block. Stop about a minute short of the forming cycle, which DKG then runs
        at the normal pace (with every quorum type). Idempotent: the target
        follows from registration heights, and nothing is mined once DKG is on."""
        self.require(self.t["role"] == "wallet", "wallet-only")
        # Once DKG is on (a resume, or a redeploy after users registered their
        # own masternodes), never mine and never judge later registrations.
        if self.rpc("spork", ["active"]).get("SPORK_17_QUORUM_DKG_ENABLED", True) is not False:
            return self.core_status()
        # Leave about a minute of normal blocks for sporks and mnsync.
        cycle, depth, quarters, lead = 48, 8 + 2, 3, max(2, -(-60 // self.block_seconds()))
        registered = max(m["state"]["registeredHeight"] for m in self.rpc("protx", ["list", "registered", True]))
        forming = -(-(registered + depth + quarters * cycle) // cycle) * cycle
        target = forming - lead
        premine = int(self.c.get("premineHeight", 0))
        self.require(0 < target <= premine + 1000, "fast-forward-height")
        address = self.address("dashnet:payout")
        while (height := self.rpc("getblockcount")) < target:
            self.stage = "fast-forward"
            self.rpc("generatetoaddress", [min(250, target - height), address, 100000000])
        return self.core_status()

    def mine_pause(self):
        self.require(self.t["role"] in ["miner", "wallet"], "miner-only")
        value = self.inspect_container("miner")
        if value:
            self.verify_image(value, self.images["core"])
            self.docker("stop", "-t", "20", self.container_name("miner"), timeout=30)
            observed = self.inspect_container("miner")
            self.require(observed and observed["Id"] == value["Id"] and not observed["State"]["Running"], "miner-pause-unverified")
        return {}

    def mine_start(self):
        self.require(self.t["role"] in ["miner", "wallet"], "miner-only")
        address = self.q["payoutAddress"]
        self.require(address.isalnum() and 26 <= len(address) <= 40, "mining-payout")
        interval = self.block_seconds()
        secret = self.read("secrets.json")
        rpc_config = (
            "\n".join(
                [
                    "devnet=" + self.c["coreNetwork"],
                    "rpcuser=dashnet",
                    "rpcpassword=" + secret["rpcPassword"],
                    "[devnet]",
                    "rpcconnect=127.0.0.1",
                    "rpcport=" + str(self.ports["coreRPC"]),
                ]
            )
            + "\n"
        )
        self.atomic("miner/rpc.conf", rpc_config)
        service = self.service("miner", self.images["core"])
        service.update(
            entrypoint=["/bin/sh", "-c"],
            # The shell is PID 1 and would ignore SIGTERM: without the trap a
            # pause waits out the stop timeout and mines two more blocks.
            command=[
                "trap 'exit 0' TERM INT; while true; do dash-cli -datadir=/tmp -conf=/etc/dash/dash.conf generatetoaddress 1 "
                + address
                + " 1000000 >/dev/null 2>&1; sleep " + str(interval) + " & wait $!; done"
            ],
            volumes=[str(self.root / "miner/rpc.conf") + ":/etc/dash/dash.conf:ro"],
        )
        self.compose("miner", {"miner": service})
        return {}

    def immutable(self, path, value):
        previous = self.read(path)
        self.require(
            previous is None or previous == value, "immutable-platform-config-changed"
        )
        if previous is None:
            self.atomic(path, value)

    # Platform -----------------------------------------------------------------

    def platform_start(self):
        self.require(self.t["role"] == "validator", "validator-only")
        secret = self.read("secrets.json")
        own = [v for v in self.q["peers"] if v["name"] == self.t["name"]]
        self.require(
            len(own) == 1
            and own[0]["nodeId"] == secret["platformNodeID"]
            and own[0]["operatorPublicKey"] == secret["operatorPublicKey"],
            "platform-identity-mismatch",
        )
        self.require(self.q["genesisCoreHeight"] > 0, "missing-genesis-chainlock")
        self.require(self.inputs()[0], "core-not-finalized")
        peers = [
            dict(name=v["name"], address=v["address"], nodeId=v["nodeId"])
            for v in sorted(self.q["peers"], key=lambda v: v["name"])
        ]
        self.immutable("platform/inputs.json", dict(genesisCoreHeight=self.q["genesisCoreHeight"], peers=peers))
        r = self.render()
        try:
            # Core was finalized with exactly this configuration: never restart it here.
            core = self.inspect_container("core")
            self.require(core and self.label(core) == r.fingerprints["core"], "core-config-changed")
            services = [s for s in r.selection if s in PLATFORM_SERVICES]
            self.install(r, services)
            self.certificates()
            self.stage = "platform-start"
            self.up(r, services)
        finally:
            self.discard(r)
        if self.c.get("gatewayTls"):
            self.acme_start()
        return {}

    def ssl(self):
        return self.home / self.config_name / "platform/gateway/ssl"

    def certificates(self):
        """The gateway starts on this node's persisted self-signed pair. Trusted
        certificates, once issued, replace it in place and are kept."""
        secret = self.read("secrets.json")
        for name, value, mode in [
            ("private.key", secret["tlsPrivateKey"], 0o600),
            ("bundle.crt", secret["tlsCertificate"], 0o644),
        ]:
            if not (self.ssl() / name).exists():
                self.write(self.ssl() / name, value.encode(), mode)
        self.own(self.ssl())

    def acme_script(self):
        tls = self.c["gatewayTls"]
        ip, server, email = self.t["peerAddress"], ACME_ISSUERS[tls["issuer"]], tls["email"]
        self.require(re.fullmatch(r"[0-9.]{7,15}", ip) and re.fullmatch(r"[A-Za-z0-9._%+@-]{3,300}", email), "acme-parameters")
        return "\n".join([
            "#!/bin/sh",
            "# Obtains and renews a publicly trusted certificate for this validator's",
            "# public IP (ACME HTTP-01 on port 80) and installs it where dashmate's",
            "# gateway reads it. The host then signals Envoy to reload, as dashmate does.",
            "set -u",
            "ip=" + ip, "server=" + server, "email=" + email,
            "path=/acme/lego",
            "install() {",
            '  crt="$path/certificates/$ip.crt"; key="$path/certificates/$ip.key"',
            '  [ -s "$crt" ] && [ -s "$key" ] || return 1',
            '  cmp -s "$crt" /ssl/bundle.crt && cmp -s "$key" /ssl/private.key && return 0',
            "  # In place: the gateway bind-mounts these files.",
            '  cat "$key" >/ssl/private.key && cat "$crt" >/ssl/bundle.crt || return 1',
            "  date +%s >/acme/reload",
            '  echo "installed certificate for $ip"',
            "}",
            "while :; do",
            '  if /lego run --server "$server" --accept-tos --email "$email" --path "$path" --domains "$ip" \\',
            '      --http --http.address :80 --profile shortlived --key-type rsa2048 --renew-days 3 && install; then',
            "    sleep 21600",
            "  else",
            "    sleep 900",
            "  fi",
            "done",
            "",
        ])

    def reload_units(self):
        """A host path unit that signals Envoy's hot restarter after each
        certificate installation: dashmate's own reload, without Docker access
        inside a container."""
        unit = "dashnet-" + self.project + "-gateway-reload"
        return unit, {
            unit + ".path": "\n".join([
                "[Unit]",
                "Description=Reload " + self.container_name("gateway") + " after a certificate renewal",
                "[Path]",
                "PathChanged=" + str(self.root / "acme/reload"),
                "[Install]",
                "WantedBy=multi-user.target",
                "",
            ]),
            unit + ".service": "\n".join([
                "[Unit]",
                "Description=Reload " + self.container_name("gateway") + " certificates",
                "[Service]",
                "Type=oneshot",
                "ExecStart=/usr/bin/docker kill --signal HUP " + self.container_name("gateway"),
                "",
            ]),
        }

    def systemd(self, units, enable):
        changed = False
        for name, text in units.items():
            path = Path("/etc/systemd/system") / name
            if not path.exists() or path.read_text() != text:
                path.write_text(text)
                changed = True
        if changed:
            self.run(["/usr/bin/systemctl", "daemon-reload"])
        self.run(["/usr/bin/systemctl", "enable", "--now", enable])

    def acme_start(self):
        tls = self.c["gatewayTls"]
        image = tls["images"][self.t["architecture"]]
        self.docker("pull", "--platform", "linux/" + self.t["architecture"], image, timeout=600)
        self.atomic("acme/renew.sh", self.acme_script())
        (self.root / "acme/lego").mkdir(parents=True, exist_ok=True, mode=0o700)
        unit, units = self.reload_units()
        self.systemd(units, unit + ".path")
        service = self.service("acme", image)
        service.update(
            entrypoint=["/bin/sh", "/acme/renew.sh"],
            volumes=[str(self.root / "acme") + ":/acme", str(self.ssl()) + ":/ssl"],
            stop_grace_period="10s",
        )
        self.compose("acme", {"acme": service})
        self.verify_image(self.inspect_container("acme"), image)

    def tenderdash(self, method):
        with self.opener.open(
            "http://127.0.0.1:" + str(self.ports["platformRPC"]) + "/" + method,
            timeout=15,
        ) as response:
            raw = response.read(1024 * 1024 + 1)
        self.require(len(raw) <= 1024 * 1024, "tenderdash-response-size")
        value = json.loads(raw)
        self.require(
            isinstance(value, dict) and value.get("error") is None,
            "tenderdash-rpc-failed",
        )
        # Tenderdash 1.8 GET endpoints return bare objects; JSON-RPC callers and
        # earlier endpoints may wrap the same object in a result envelope.
        result = value.get("result", value)
        fields = {"status": ["node_info", "sync_info", "validator_info"],
                  "block": ["block_id", "block"]}.get(method.split("?", 1)[0])
        self.require(
            fields is not None and isinstance(result, dict)
            and all(isinstance(result.get(key), dict) for key in fields),
            "tenderdash-response-shape",
        )
        return result

    def dapi_status(self):
        # Exercise the real TLS -> HTTP/2 -> gRPC -> DAPI -> Drive/TD path.
        # Trust only the persisted per-node certificate, not --insecure. With
        # trusted gateway certificates, also accept the system CAs for this
        # node's public IP, which is what clients see.
        port = str(self.ports["gateway"])
        secret = self.read("secrets.json")
        with tempfile.TemporaryDirectory(dir=self.root) as tmp:
            headers = Path(tmp) / "headers"
            trust, url, route = Path(tmp) / "trust.pem", "https://127.0.0.1:" + port, []
            pem = secret["tlsCertificate"]
            if self.c.get("gatewayTls"):
                pem += "\n" + Path("/etc/ssl/certs/ca-certificates.crt").read_text()
                peer = self.t["peerAddress"]
                url, route = "https://" + peer + ":" + port, ["--connect-to", peer + ":" + port + ":127.0.0.1:" + port]
            trust.write_text(pem)
            raw = self.run(
                [
                    "/usr/bin/curl",
                    "--silent",
                    "--show-error",
                    "--fail",
                    "--noproxy",
                    "*",
                    "--http2",
                    "--max-time",
                    "20",
                    "--cacert",
                    str(trust),
                    *route,
                    "--dump-header",
                    str(headers),
                    "-H",
                    "content-type: application/grpc",
                    "-H",
                    "te: trailers",
                    "--data-binary",
                    "@-",
                    url + "/org.dash.platform.dapi.v0.Platform/getStatus",
                ],
                stdin=b"\x00\x00\x00\x00\x02\x0a\x00",
                timeout=25,
            )
            values = headers.read_text().lower().splitlines()
            self.require("grpc-status: 0" in values, "dapi-grpc-failed")
        self.require(
            len(raw) >= 5
            and raw[0] == 0
            and int.from_bytes(raw[1:5], "big") == len(raw) - 5,
            "dapi-grpc-frame",
        )
        return protobuf(protobuf(raw[5:])[1])

    def platform_containers(self):
        """Every running Platform service, keyed by release component (drive,
        tenderdash, dapi, gateway) or by sidecar service name."""
        pins = json.loads((self.home / ".dashnet-compose.json").read_text())["services"]
        containers, restarts = {}, {}
        for name in [s for s in self.selection() if s in PLATFORM_SERVICES]:
            value = self.inspect_container(name)
            self.require(
                value
                and value["State"]["Running"]
                and not value["State"].get("Restarting"),
                "platform-not-running",
            )
            key = COMPONENTS.get(name, name)
            pin = self.images[key] if name in COMPONENTS else pins[name].get("image")
            self.require(pin, "sidecar-image-not-pinned")
            self.verify_image(value, pin)
            containers[key] = value["Id"]
            restarts[key] = value["RestartCount"]
        self.require(all(k in containers for k in ["drive", "tenderdash", "dapi", "gateway"]), "platform-not-running")
        return containers, restarts

    def platform_status(self):
        self.require(self.t["role"] == "validator", "validator-only")
        containers, restarts = self.platform_containers()
        status = self.tenderdash("status")
        dapi = self.dapi_status()
        software = protobuf(protobuf(dapi.get(1, b"")).get(1, b""))
        chain, network, identity = (
            protobuf(dapi.get(3, b"")),
            protobuf(dapi.get(4, b"")),
            protobuf(dapi.get(2, b"")),
        )
        self.require(software.get(2) and software.get(3), "dapi-upstream-unavailable")
        # While Drive starts, getStatus can omit chain or node identity fields.
        self.require(network.get(1) and identity.get(1) and identity.get(2), "dapi-status-incomplete")
        reference = ""
        if self.q.get("referenceHeight", 0) > 0:
            reference = self.tenderdash(
                "block?height=" + str(self.q["referenceHeight"])
            )["block_id"]["hash"].lower()
        return dict(
            height=int(status["sync_info"]["latest_block_height"]),
            # An idle chain makes a block only every createEmptyBlocksInterval.
            blockAge=block_age(status["sync_info"].get("latest_block_time")),
            dapiHeight=chain.get(4, 0),
            catchingUp=status["sync_info"]["catching_up"] or bool(chain.get(1, 0)),
            chainId=network[1].decode(),
            nodeId=identity[1].hex(),
            proTxHash=identity[2].hex(),
            driveVersion=software[2].decode(),
            referenceBlockHash=reference,
            containers=containers,
            restarts=restarts,
        )

    def execute(self):
        self.verify_instance()
        self.require(
            self.q["action"]
            in [
                "inspect",
                "render",
                "core-start",
                "core-status",
                "identity",
                "wallet",
                "core-finalize",
                "fund",
                "register",
                "activate",
                "fast-forward",
                "mine-start",
                "mine-pause",
                "platform-start",
                "platform-status",
                "stop",
            ],
            "action-refused",
        )
        with self.lock.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Failure("host-busy") from None
            self.owned()
            action = self.q["action"]
            if (
                action not in ["inspect", "core-status", "platform-status"]
                and self.read("deployment.json") is None
            ):
                self.atomic("deployment.json", {"planId": self.c["planId"]})
            self.stage = action
            result = dict(
                instanceId=self.t["instanceId"], planId=self.c["planId"], action=action
            )
            if action == "inspect":
                return result
            if action == "render":
                result.update(self.render_info())
            elif action in ["core-start", "core-finalize"]:
                result["core"] = self.ensure_core(action == "core-finalize")
            elif action == "core-status":
                result["core"] = self.core_status()
            elif action == "identity":
                result.update(self.identity())
            elif action == "wallet":
                result.update(self.wallet())
            elif action == "fund":
                result.update(self.fund())
            elif action == "register":
                result.update(self.register())
            elif action == "activate":
                result.update(self.activate())
            elif action == "fast-forward":
                result["core"] = self.fast_forward()
            elif action == "mine-start":
                result.update(self.mine_start())
            elif action == "mine-pause":
                result.update(self.mine_pause())
            elif action == "platform-start":
                result.update(self.platform_start())
            elif action == "platform-status":
                result["platform"] = self.platform_status()
            elif action == "stop":
                self.stop()
            return result

    def stop(self):
        # Only stop owned containers, preserving all volumes and identities.
        for name in AUXILIARY:
            if self.inspect_container(name):
                self.docker("stop", "-t", "20", self.container_name(name), timeout=60)
        if self.selection():
            # dashmate's own stop: dependants first, each with its grace period.
            self.dm_compose(self.home, "stop", *self.selection(), timeout=600)
        for name in self.known_services():
            value = self.inspect_container(name)
            self.require(
                value is None or not value["State"]["Running"], "stop-not-observed"
            )


def main():
    worker = None
    try:
        raw = sys.stdin.buffer.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise Failure("input-size")
        worker = Worker(json.loads(raw))
        print(json.dumps(worker.execute(), separators=(",", ":")))
    except Exception as error:
        # No raw RPC/Docker output or Python repr may escape to a CLI/CI journal.
        stage = worker.stage if worker else "input"
        reason = (
            str(error) if isinstance(error, Failure) else type(error).__name__.lower()
        )
        allowed = "".join(
            c
            for c in (stage + ":" + reason)
            if c in "abcdefghijklmnopqrstuvwxyz0123456789:_-"
        )[:120]
        print(json.dumps(dict(error=allowed)))
        sys.exit(1)


if __name__ == "__main__":
    main()
