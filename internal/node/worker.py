"""Fixed Dash host operations. No user shell, generic RPC proxy or reset API.

Inputs arrive on stdin over authenticated SSH. Only typed public facts leave
stdout. Private identities and pre-broadcast transactions stay on the node.
"""

import base64
from decimal import Decimal
import fcntl
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


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


class Failure(Exception):
    pass


class RPCFailure(Failure):
    def __init__(self, method, code):
        self.code = int(code)
        super().__init__("rpc-" + method.replace(" ", "-") + ":" + str(self.code))


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

    def run(self, args, stdin=None, timeout=120):
        try:
            result = subprocess.run(
                args,
                input=stdin,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired:
            raise Failure("command-timeout") from None
        self.require(result.returncode == 0, "command-exit-" + str(result.returncode))
        self.require(len(result.stdout) <= 4 * 1024 * 1024, "command-output-too-large")
        return result.stdout

    def docker(self, *args, timeout=120):
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
        path = self.root / component / "compose.json"
        value = dict(name=self.project, services=services)
        existing = self.read(component + "/compose.json")
        if existing != value:
            self.atomic(component + "/compose.json", value)
        self.docker("compose", "-p", self.project, "-f", str(path), "config", "--quiet")
        self.docker(
            "compose",
            "-p",
            self.project,
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
        ]:
            self.require(not (self.root / name).is_symlink(), "symlink-refused")
        # Reject unknown containers, including stopped ones. Never adopt by name.
        for line in (
            self.docker("container", "ls", "-a", "--format", "{{.Names}}")
            .decode()
            .splitlines()
        ):
            self.require(
                line
                in {
                    self.container_name(x)
                    for x in ["core", "miner", "drive", "tenderdash", "dapi", "gateway"]
                },
                "unexpected-container",
            )
        for service in ["core", "miner", "drive", "tenderdash", "dapi", "gateway"]:
            if self.inspect_container(service):
                pass

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

    def core_config(self, final=False):
        secret = self.secret()
        p = self.ports
        # Configured only for this private managed devnet. RPC and ZMQ are always
        # loopback; no credentials are ever placed in process arguments.
        lines = [
            "devnet=" + self.c["coreNetwork"],
            "daemon=0",
            "server=1",
            "txindex=1",
            "addressindex=1",
            "spentindex=1",
            "timestampindex=1",
            "dnsseed=0",
            "discover=0",
            "allowprivatenet=1",
            "listen=1",
            "maxconnections=256",
            "fallbackfee=0.00001",
            "rpcuser=dashnet",
            "rpcpassword=" + secret["rpcPassword"],
            "rpcallowip=127.0.0.1",
            "rpcworkqueue=128",
            "rpcthreads=32",
            "deprecatedrpc=hpmn",
            "llmqchainlocks=llmq_devnet",
            "llmqinstantsenddip0024=llmq_devnet_dip0024",
            "llmqplatform=llmq_devnet_platform",
            "llmqmnhf=llmq_devnet",
        ]
        if final:
            self.require(self.q.get("sporkAddress"), "missing-spork-address")
            lines.append("sporkaddr=" + self.q["sporkAddress"])
            if self.t["role"] == "wallet":
                lines.append("sporkkey=" + secret["sporkKey"])
            if self.t["role"] == "validator":
                lines.append("masternodeblsprivkey=" + secret["operatorPrivateKey"])
        lines += [
            "[devnet]",
            "port=" + str(p["coreP2P"]),
            "rpcport=" + str(p["coreRPC"]),
            "rpcbind=127.0.0.1",
            "bind=0.0.0.0",
            "externalip=" + self.t["peerAddress"] + ":" + str(p["coreP2P"]),
            "minimumdifficultyblocks=1000000",
            "highsubsidyblocks=500",
            "highsubsidyfactor=100",
            "powtargetspacing=10",
        ]
        for name in [
            "rawtx",
            "rawtxlock",
            "rawblock",
            "hashblock",
            "rawchainlocksig",
            "rawtxlocksig",
        ]:
            lines.append("zmqpub" + name + "=tcp://127.0.0.1:" + str(p["coreZMQ"]))
        lines += [
            "addnode=" + peer
            for peer in self.c["corePeers"]
            if peer != self.t["peerAddress"] + ":" + str(p["coreP2P"])
        ]
        return "\n".join(lines) + "\n"

    def ensure_core(self, final=False):
        self.stage = "core-config"
        # A resume must never revert the final BLS/spork configuration.
        state = self.read("core/state.json", {})
        if state.get("final") and not final:
            self.q["sporkAddress"] = state["sporkAddress"]
            final = True
        config = self.core_config(final)
        config_path = self.root / "core/dash.conf"
        changed = not config_path.exists() or config_path.read_text() != config
        existing = self.inspect_container("core")
        if changed and existing:
            self.stage = "core-stop-for-config"
            self.docker("stop", "-t", "120", self.container_name("core"), timeout=150)
        if changed:
            self.atomic("core/dash.conf", config)
        (self.root / "data/core").mkdir(parents=True, exist_ok=True, mode=0o700)
        service = self.service("core", self.images["core"])
        service.update(
            entrypoint=["dashd"],
            command=[
                "-conf=/etc/dash/dash.conf",
                "-datadir=/data",
                "-printtoconsole=1",
            ],
            volumes=[
                str(config_path) + ":/etc/dash/dash.conf:ro",
                str(self.root / "data/core") + ":/data",
            ],
            stop_grace_period="120s",
        )
        self.stage = "core-start"
        self.compose("core", {"core": service})
        # Each caller has a deadline; a bounded readiness wait avoids racing RPC
        # initialization without launching a persistent control-plane daemon.
        deadline = time.monotonic() + 90
        while True:
            try:
                result = self.core_status()
                break
            except (RPCFailure, urllib.error.URLError, ConnectionError):
                if time.monotonic() >= deadline:
                    raise Failure("core-rpc-not-ready") from None
                time.sleep(1)
        if final:
            self.atomic(
                "core/state.json",
                {"final": True, "sporkAddress": self.q["sporkAddress"]},
            )
        return result

    def core_status(self):
        value = self.inspect_container("core")
        self.require(value and value["State"]["Running"], "core-not-running")
        self.verify_image(value, self.images["core"])
        info = self.rpc("getblockchaininfo")
        self.require(info["chain"] == "devnet-" + self.c["coreNetwork"], "wrong-chain")
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
            ibd=info["initialblockdownload"],
            peers=self.rpc("getconnectioncount"),
            containerId=value["Id"],
            restarts=value["RestartCount"],
            configSha256=hashlib.sha256(
                (self.root / "core/dash.conf").read_bytes()
            ).hexdigest(),
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
        # used. Observe balance each retry; cap initial mining to 1,000 blocks.
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
            self.require(self.rpc("getblockcount") < 1000, "bootstrap-mining-limit")
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
            self.require(self.rpc("getblockcount") < 2000, "registration-mining-limit")
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
        active = self.rpc("spork", ["active"])
        for key in [
            "SPORK_17_QUORUM_DKG_ENABLED",
            "SPORK_19_CHAINLOCKS_ENABLED",
            "SPORK_21_QUORUM_ALL_CONNECTED",
        ]:
            if key in active and not active[key]:
                self.rpc("spork", [key, 0])
        return {}

    def mine_start(self):
        self.require(self.t["role"] in ["miner", "wallet"], "miner-only")
        address = self.q["payoutAddress"]
        self.require(address.isalnum() and 26 <= len(address) <= 40, "mining-payout")
        interval = self.c["miningIntervalSeconds"]
        self.require(interval == 10, "mining-interval")
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
            command=[
                "while true; do dash-cli -datadir=/tmp -conf=/etc/dash/dash.conf generatetoaddress 1 "
                + address
                + " 1000000 >/dev/null 2>&1; sleep 10; done"
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

    def platform_files(self):
        self.require(self.t["role"] == "validator", "validator-only")
        secret, p = self.read("secrets.json"), self.ports
        own = [v for v in self.q["peers"] if v["name"] == self.t["name"]]
        self.require(
            len(own) == 1
            and own[0]["nodeId"] == secret["platformNodeID"]
            and own[0]["operatorPublicKey"] == secret["operatorPublicKey"],
            "platform-identity-mismatch",
        )
        self.require(self.q["genesisCoreHeight"] > 0, "missing-genesis-chainlock")
        genesis = dict(
            genesis_time=self.c["genesisTime"],
            chain_id=self.c["platformChainId"],
            initial_height="1",
            initial_core_chain_locked_height=self.q["genesisCoreHeight"],
            validator_quorum_type=107,
            consensus_params=dict(
                block=dict(
                    max_bytes="2097152", max_gas="57631392000", time_iota_ms="5000"
                ),
                evidence=dict(
                    max_age_num_blocks="100000",
                    max_age_duration="172800000000000",
                    max_bytes="1048576",
                ),
                validator=dict(pub_key_types=["bls12381"]),
                version=dict(app_version=str(self.c["initialProtocolVersion"])),
                timeout=dict(
                    propose="50000000000",
                    propose_delta="5000000000",
                    vote="10000000000",
                    vote_delta="1000000000",
                ),
                synchrony=dict(message_delay="70000000000", precision="1000000000"),
                abci=dict(recheck_tx=True),
            ),
        )
        self.immutable("platform/tenderdash/config/genesis.json", genesis)
        self.immutable(
            "platform/tenderdash/config/node_key.json",
            dict(
                id=secret["platformNodeID"],
                priv_key=dict(
                    type="tendermint/PrivKeyEd25519", value=secret["nodePrivateKey"]
                ),
            ),
        )
        peers = ",".join(
            v["nodeId"] + "@" + v["address"] + ":" + str(p["platformP2P"])
            for v in self.q["peers"]
            if v["name"] != self.t["name"]
        )
        # TOML string values use JSON escaping (the common TOML basic-string subset).
        lines = [
            'mode = "validator"',
            "moniker = " + json.dumps(self.t["name"]),
            'genesis-file = "config/genesis.json"',
            'node-key-file = "config/node_key.json"',
            'db-dir = "data"',
            'log-format = "json"',
            "[abci]",
            'transport = "routed"',
            "address = "
            + json.dumps(
                "CheckTx:grpc:127.0.0.1:"
                + str(p["driveGRPC"])
                + ",*:socket:tcp://127.0.0.1:"
                + str(p["driveABCI"])
            ),
            "[priv-validator]",
            'key-file = "data/priv_validator_key.json"',
            'state-file = "data/priv_validator_state.json"',
            'core-rpc-host = "127.0.0.1:' + str(p["coreRPC"]) + '"',
            'core-rpc-username = "dashnet"',
            "core-rpc-password = " + json.dumps(secret["rpcPassword"]),
            "[rpc]",
            'laddr = "tcp://127.0.0.1:' + str(p["platformRPC"]) + '"',
            "unsafe = false",
            "[p2p]",
            'laddr = "tcp://0.0.0.0:' + str(p["platformP2P"]) + '"',
            "external-address = "
            + json.dumps(self.t["peerAddress"] + ":" + str(p["platformP2P"])),
            "persistent-peers = " + json.dumps(peers),
            "max-connections = 64",
            "max-outgoing-connections = 32",
            "[consensus]",
            "create-empty-blocks = true",
            'create-empty-blocks-interval = "5s"',
            "[tx-index]",
            'indexer = ["kv"]',
        ]
        config = "\n".join(lines) + "\n"
        path = self.root / "platform/tenderdash/config/config.toml"
        self.require(
            not path.exists() or path.read_text() == config, "tenderdash-config-drift"
        )
        if not path.exists():
            self.atomic("platform/tenderdash/config/config.toml", config)
        for name in ["drive", "tenderdash/data"]:
            (self.root / "platform" / name).mkdir(
                parents=True, exist_ok=True, mode=0o700
            )
        self.atomic("platform/tls/cert.pem", secret["tlsCertificate"])
        self.atomic("platform/tls/key.pem", secret["tlsPrivateKey"])
        self.atomic("platform/envoy.json", self.envoy())

    def envoy(self):
        def address(port):
            return dict(socket_address=dict(address="127.0.0.1", port_value=port))

        def cluster(name, port, http2=False):
            value = dict(
                name=name,
                connect_timeout="5s",
                type="STATIC",
                load_assignment=dict(
                    cluster_name=name,
                    endpoints=[
                        dict(lb_endpoints=[dict(endpoint=dict(address=address(port)))])
                    ],
                ),
            )
            if http2:
                value["typed_extension_protocol_options"] = {
                    "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
                        "@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
                        "explicit_http_config": {"http2_protocol_options": {}},
                    }
                }
            return value

        manager = {
            "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
            "stat_prefix": "dashnet",
            "codec_type": "AUTO",
            "route_config": dict(
                name="dapi",
                virtual_hosts=[
                    dict(
                        name="dapi",
                        domains=["*"],
                        routes=[
                            dict(
                                match=dict(prefix="/org.dash.platform.dapi."),
                                route=dict(cluster="grpc", timeout="120s"),
                            ),
                            dict(
                                match=dict(prefix="/"),
                                route=dict(cluster="json", timeout="30s"),
                            ),
                        ],
                    )
                ],
            ),
            "http_filters": [
                {
                    "name": "envoy.filters.http.router",
                    "typed_config": {
                        "@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"
                    },
                }
            ],
        }
        tls = {
            "name": "envoy.transport_sockets.tls",
            "typed_config": {
                "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
                "common_tls_context": {
                    "alpn_protocols": ["h2", "http/1.1"],
                    "tls_certificates": [
                        {
                            "certificate_chain": {"filename": "/tls/cert.pem"},
                            "private_key": {"filename": "/tls/key.pem"},
                        }
                    ],
                },
            },
        }
        return dict(
            static_resources=dict(
                listeners=[
                    dict(
                        name="dapi_tls",
                        address=dict(
                            socket_address=dict(
                                address="0.0.0.0", port_value=self.ports["gateway"]
                            )
                        ),
                        filter_chains=[
                            dict(
                                transport_socket=tls,
                                filters=[
                                    dict(
                                        name="envoy.filters.network.http_connection_manager",
                                        typed_config=manager,
                                    )
                                ],
                            )
                        ],
                    )
                ],
                clusters=[
                    cluster("grpc", self.ports["dapiGRPC"], True),
                    cluster("json", self.ports["dapiJSON"]),
                ],
            )
        )

    def platform_services(self):
        secret, p = self.read("secrets.json"), self.ports
        environment = dict(
            CHAIN_ID=self.c["platformChainId"],
            NETWORK="devnet",
            DB_PATH="/db",
            EPOCH_TIME_LENGTH_S="3600",
            ABCI_CONSENSUS_BIND_ADDRESS="tcp://127.0.0.1:" + str(p["driveABCI"]),
            GRPC_BIND_ADDRESS="127.0.0.1:" + str(p["driveGRPC"]),
            TOKIO_CONSOLE_ENABLED="false",
            GROVEDB_VISUALIZER_ENABLED="false",
            LOG_LEVEL="info",
        )
        for prefix in ["CORE_CONSENSUS", "CORE_CHECK_TX"]:
            environment.update(
                {
                    prefix + "_JSON_RPC_USERNAME": "dashnet",
                    prefix + "_JSON_RPC_PASSWORD": secret["rpcPassword"],
                    prefix + "_JSON_RPC_HOST": "127.0.0.1",
                    prefix + "_JSON_RPC_PORT": str(p["coreRPC"]),
                }
            )
        for prefix, kind, size, window, signers, rotation in [
            ("VALIDATOR_SET", 107, 12, 24, 4, False),
            ("CHAIN_LOCK", 101, 12, 24, 4, False),
            ("INSTANT_LOCK", 105, 8, 48, 2, True),
        ]:
            environment.update(
                {
                    prefix + "_QUORUM_SIZE": str(size),
                    prefix + "_QUORUM_TYPE": str(kind),
                    prefix + "_QUORUM_WINDOW": str(window),
                    prefix + "_QUORUM_ACTIVE_SIGNERS": str(signers),
                    prefix + "_QUORUM_ROTATION": str(rotation).lower(),
                }
            )
        drive = self.service("drive", self.images["drive"])
        drive.update(
            environment=environment,
            volumes=[str(self.root / "platform/drive") + ":/db"],
            stop_grace_period="120s",
        )
        td = self.service("tenderdash", self.images["tenderdash"])
        td.update(
            entrypoint=["tenderdash"],
            command=["node", "--home", "/tenderdash"],
            volumes=[str(self.root / "platform/tenderdash") + ":/tenderdash"],
            stop_grace_period="120s",
        )
        dapi = self.service("dapi", self.images["dapi"])
        dapi.update(
            environment=dict(
                DAPI_GRPC_SERVER_PORT=str(p["dapiGRPC"]),
                DAPI_JSON_RPC_PORT=str(p["dapiJSON"]),
                DAPI_BIND_ADDRESS="127.0.0.1",
                DAPI_DRIVE_URI="http://127.0.0.1:" + str(p["driveGRPC"]),
                DAPI_TENDERDASH_URI="http://127.0.0.1:" + str(p["platformRPC"]),
                DAPI_TENDERDASH_WEBSOCKET_URI="ws://127.0.0.1:"
                + str(p["platformRPC"])
                + "/websocket",
                DAPI_CORE_ZMQ_URL="tcp://127.0.0.1:" + str(p["coreZMQ"]),
                DAPI_CORE_RPC_URL="http://127.0.0.1:" + str(p["coreRPC"]),
                DAPI_CORE_RPC_USER="dashnet",
                DAPI_CORE_RPC_PASS=secret["rpcPassword"],
                DAPI_STATE_TRANSITION_WAIT_TIMEOUT="120000",
                DAPI_LOG_LEVEL="info",
            )
        )
        gateway = self.service("gateway", self.images["gateway"])
        gateway.update(
            entrypoint=["envoy"],
            command=["-c", "/etc/envoy/config.json", "--log-level", "warning"],
            volumes=[
                str(self.root / "platform/envoy.json") + ":/etc/envoy/config.json:ro",
                str(self.root / "platform/tls") + ":/tls:ro",
            ],
        )
        return dict(drive=drive, tenderdash=td, dapi=dapi, gateway=gateway)

    def platform_start(self):
        self.platform_files()
        self.compose("platform", self.platform_services())
        return {}

    def tenderdash(self, method):
        with self.opener.open(
            "http://127.0.0.1:" + str(self.ports["platformRPC"]) + "/" + method,
            timeout=15,
        ) as response:
            value = json.loads(response.read(1024 * 1024))
        self.require(
            "result" in value and "error" not in value, "tenderdash-rpc-failed"
        )
        return value["result"]

    def dapi_status(self):
        # Exercise the real TLS -> HTTP/2 -> gRPC -> DAPI -> Drive/TD path.
        # Trust only the persisted per-node certificate, not --insecure.
        with tempfile.TemporaryDirectory(dir=self.root) as tmp:
            headers = Path(tmp) / "headers"
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
                    str(self.root / "platform/tls/cert.pem"),
                    "--dump-header",
                    str(headers),
                    "-H",
                    "content-type: application/grpc",
                    "-H",
                    "te: trailers",
                    "--data-binary",
                    "@-",
                    "https://127.0.0.1:"
                    + str(self.ports["gateway"])
                    + "/org.dash.platform.dapi.v0.Platform/getStatus",
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

    def platform_status(self):
        self.require(self.t["role"] == "validator", "validator-only")
        containers, restarts = {}, {}
        for name in ["drive", "tenderdash", "dapi", "gateway"]:
            value = self.inspect_container(name)
            self.require(
                value
                and value["State"]["Running"]
                and not value["State"].get("Restarting"),
                "platform-not-running",
            )
            self.verify_image(value, self.images[name])
            containers[name] = value["Id"]
            restarts[name] = value["RestartCount"]
        status = self.tenderdash("status")
        dapi = self.dapi_status()
        software = protobuf(protobuf(dapi[1])[1])
        chain, network, identity = (
            protobuf(dapi[3]),
            protobuf(dapi[4]),
            protobuf(dapi[2]),
        )
        self.require(software.get(2) and software.get(3), "dapi-upstream-unavailable")
        reference = ""
        if self.q.get("referenceHeight", 0) > 0:
            reference = self.tenderdash(
                "block?height=" + str(self.q["referenceHeight"])
            )["block_id"]["hash"].lower()
        return dict(
            height=int(status["sync_info"]["latest_block_height"]),
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
                "core-start",
                "core-status",
                "identity",
                "wallet",
                "core-finalize",
                "fund",
                "register",
                "activate",
                "mine-start",
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
            if action in ["core-start", "core-finalize"]:
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
            elif action == "mine-start":
                result.update(self.mine_start())
            elif action == "platform-start":
                result.update(self.platform_start())
            elif action == "platform-status":
                result["platform"] = self.platform_status()
            elif action == "stop":
                self.stop()
            return result

    def stop(self):
        # Only stop owned containers, preserving all volumes and identities.
        for name in ["miner", "gateway", "dapi", "tenderdash", "drive", "core"]:
            if self.inspect_container(name):
                self.docker("stop", "-t", "120", self.container_name(name), timeout=150)
        for name in ["miner", "gateway", "dapi", "tenderdash", "drive", "core"]:
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
