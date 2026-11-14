"""The worker's Core-facing code against a real dashd, one image per run.

Fixtures cannot show that a new Core version changed an RPC, a default or an
error code. This starts the image named by CORE_COMPAT_IMAGE as a lone regtest
node and runs the real worker against it: the wallet and spork identity, the
funding, the EvoNode registration and the spork activation of a deployment,
with nothing scripted. CI runs it once per supported Core major
(.github/workflows/core-compat.yml), and it can be dispatched by hand against
any other tag, such as a release candidate, before that release is deployed.

Without CORE_COMPAT_IMAGE the whole module is skipped, so the ordinary test
run needs neither Docker nor a network. A lone regtest node has no masternode
of its own, no quorums and no ChainLocks: those reads are checked only as far
as "the RPC exists and fails with a code the worker expects". Core as dashmate
renders it, on a devnet, is core_integration.py's subject.
"""

import base64
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time
import unittest
import urllib.error
import urllib.request

from test_worker import worker, request

IMAGE = os.environ.get("CORE_COMPAT_IMAGE", "")
IMAGE_OK = re.fullmatch(
    r"(docker\.io/)?dashpay/dashd(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(@sha256:[0-9a-f]{64})?", IMAGE
)
CONF = "/etc/dash/dash.conf"  # where the image reads it
NODE = Path(__file__).parents[1]
# Regtest accepts ProTx transactions from height 432 (DIP3) in Core 23 and 24,
# and its block reward halves every 150 blocks: the wallet is funded early.
PREMINE = 600
# Every Core has these. Core 23 also has sporks 3 and 9, which Core 24 removed.
SPORKS = [
    "SPORK_2_INSTANTSEND_ENABLED",
    "SPORK_17_QUORUM_DKG_ENABLED",
    "SPORK_19_CHAINLOCKS_ENABLED",
    "SPORK_21_QUORUM_ALL_CONNECTED",
]
OLDER_SPORKS = ["SPORK_3_INSTANTSEND_BLOCK_FILTERING", "SPORK_9_SUPERBLOCKS_ENABLED"]


def sh(*args, timeout=180):
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout)


def rpcs_used():
    """Every Core RPC the node workers name, read from the scripts themselves so
    a newly added call is covered without touching this test."""
    names = set()
    for path in [NODE / "worker.py", NODE / "upgrade.py", NODE / "join.py", NODE / "observer.py"]:
        names.update(re.findall(r"""\.rpc\(\s*["']([a-z]+)["']""", path.read_text()))
    managed = (NODE.parent / "managed" / "worker.py").read_text()
    names.update(re.findall(r"""\.rpc\(\s*selected,\s*["']([a-z]+)["']""", managed))
    return sorted(names)


class Node:
    """A lone regtest dashd from the image under test."""

    ports = iter(range(19898, 19998))

    def __init__(self):
        self.port = next(self.ports)
        self.password = secrets.token_hex(16)
        self.work = tempfile.TemporaryDirectory(prefix="dashnet-core-compat-")
        # The node runs as the image's own user and reads this through the mount.
        os.chmod(self.work.name, 0o755)
        self.conf = Path(self.work.name) / "dash.conf"
        self.configure([])
        self.conf.chmod(0o644)
        self.name = "core-compat-" + secrets.token_hex(4)
        started = sh(
            "docker", "run", "-d", "--name", self.name, "-p", "127.0.0.1:%d:%d" % (self.port, self.port),
            "-v", "%s:%s:ro" % (self.conf, CONF), IMAGE, "dashd", "-conf=" + CONF,
        )
        if started.returncode:
            raise AssertionError("could not start %s: %s" % (IMAGE, started.stderr.strip()[-500:]))
        self.wait()

    def configure(self, extra):
        # Rewritten in place: the container mounts this one file.
        with self.conf.open("w") as stream:
            stream.write("\n".join([
                "regtest=1", "server=1", "listen=0", "dnsseed=0", "printtoconsole=1", "fallbackfee=0.0001",
                "txindex=1", "rpcuser=dashnet", "rpcpassword=" + self.password, "rpcallowip=0.0.0.0/0",
                *extra, "[regtest]", "rpcbind=0.0.0.0", "rpcport=%d" % self.port, "",
            ]))

    def wait(self):
        deadline = time.time() + 120
        while sh("docker", "exec", self.name, "dash-cli", "-conf=" + CONF, "getblockchaininfo").returncode:
            if time.time() > deadline:
                logs = sh("docker", "logs", "--tail", "40", self.name)
                raise AssertionError(
                    "%s did not answer RPC within two minutes:\n%s%s" % (IMAGE, logs.stdout[-3000:], logs.stderr[-3000:])
                )
            time.sleep(2)

    def restart(self, extra):
        """As core-finalize does: the same data, a configuration with more in it."""
        self.configure(extra)
        restarted = sh("docker", "restart", self.name)
        if restarted.returncode:
            raise AssertionError("could not restart %s: %s" % (IMAGE, restarted.stderr.strip()[-500:]))
        self.wait()

    def cli(self, *args, wallet=None):
        """dash-cli in the container. Returns (value, None) or (None, error text)."""
        command = ["docker", "exec", self.name, "dash-cli", "-conf=" + CONF]
        command += ["-rpcwallet=" + wallet] if wallet else []
        p = sh(*command, *[str(a) for a in args])
        if p.returncode:
            return None, (p.stderr or p.stdout).strip()[-400:]
        try:
            return json.loads(p.stdout), None
        except ValueError:
            return p.stdout.strip(), None

    def message(self, method, params, wallet):
        """Core's own error for a call. The worker's client keeps only the
        code, which is all it acts on; whoever reads a failed run of this test
        needs the reason."""
        url = "http://127.0.0.1:%d/" % self.port + ("wallet/dashnet" if wallet else "")
        auth = base64.b64encode(("dashnet:" + self.password).encode()).decode()
        body = json.dumps(dict(jsonrpc="1.0", id="why", method=method, params=params or [])).encode()
        call = urllib.request.Request(url, data=body, headers={"Authorization": "Basic " + auth, "Content-Type": "application/json"})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            try:
                response = opener.open(call, timeout=90)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return json.dumps(json.loads(response.read(1024 * 1024)).get("error"))
        except (OSError, ValueError) as error:
            return "no answer (%s)" % type(error).__name__

    def worker(self):
        """The real worker, with this node as its host's Core."""
        q = request()
        q["context"]["ports"]["coreRPC"] = self.port
        root = Path(tempfile.mkdtemp(prefix="state-", dir=self.work.name))
        w = worker.Worker(q, root, root / "lock")
        w.atomic("secrets.json", {"rpcPassword": self.password})
        return w

    def remove(self):
        sh("docker", "rm", "-f", self.name)
        self.work.cleanup()


@unittest.skipUnless(IMAGE, "set CORE_COMPAT_IMAGE to a dashpay/dashd image to run against a real node")
class CoreCompat(unittest.TestCase):
    summary = {}
    nodes = []

    @classmethod
    def setUpClass(cls):
        if not IMAGE_OK:
            raise AssertionError("CORE_COMPAT_IMAGE must be a dashpay/dashd image reference, got %r" % IMAGE)
        cls.node = cls.start()
        cls.w = cls.node.worker()

    @classmethod
    def start(cls):
        node = Node()
        cls.nodes.append(node)
        return node

    @classmethod
    def tearDownClass(cls):
        for node in cls.nodes:
            node.remove()
        if cls.summary:
            print("\ncore-compat " + json.dumps(cls.summary, sort_keys=True), flush=True)

    def ok(self, node, *args, wallet=None):
        value, error = node.cli(*args, wallet=wallet)
        self.assertIsNone(error, "%s failed on %s: %s" % (" ".join(map(str, args)), IMAGE, error))
        return value

    def step(self, node, w, action):
        """One worker action. When Core refuses a call, the same call is asked
        once more for the message the worker's client drops."""
        calls, client = [], w.rpc

        def recording(method, params=None, wallet=False):
            calls.append((method, params, wallet))
            return client(method, params, wallet)

        w.rpc = recording
        try:
            return action()
        except worker.RPCFailure as error:
            self.fail("%s on %s: Core answered %s" % (error, IMAGE, node.message(*calls[-1])))
        finally:
            del w.rpc

    def needs_identity(self):
        if not hasattr(self, "identity"):
            self.skipTest("needs the wallet identity that test_3 did not produce")

    def code(self, w, method, params=None, wallet=False):
        """Core's error code for a call, as the worker's own client reports it."""
        try:
            w.rpc(method, params, wallet)
        except worker.RPCFailure as error:
            return error.code
        return None

    def key_address(self, node, key):
        """What Core itself says a private key's address is."""
        info = self.ok(node, "getdescriptorinfo", "pkh(%s)" % key)
        return self.ok(node, "deriveaddresses", info["descriptor"])[0]

    def test_1_image_is_the_version_it_claims(self):
        net = self.ok(self.node, "getnetworkinfo")
        self.assertIsInstance(net.get("version"), int)
        self.summary.update(image=IMAGE, version=net["version"], subversion=net.get("subversion"))
        major = re.search(r"dashd:(\d+)(?:[.-]|$)", IMAGE)
        if major:
            self.assertEqual(net["version"] // 10000, int(major.group(1)), "the image tag and the running Core disagree")
        self.assertEqual(self.w.rpc("getblockchaininfo")["chain"], "regtest")

    def test_2_every_rpc_the_workers_call_still_exists(self):
        names = rpcs_used()
        # A guard on the scan itself: an empty list would pass vacuously.
        for expected in ["createwallet", "getaddressesbylabel", "getbestchainlock", "getblockchaininfo", "lockunspent",
                         "masternode", "mnsync", "protx", "quorum", "spork", "sporkupdate"]:
            self.assertIn(expected, names)
        missing = [n for n in names if str(self.ok(self.node, "help", n)).startswith("help: unknown command")]
        self.assertEqual(missing, [], "RPCs the workers call that %s does not have" % IMAGE)
        self.summary["rpcs"] = len(names)
        # Reads every role makes, in the argument forms the workers pass.
        self.assertIsInstance(self.w.rpc("mnsync", ["status"]).get("IsSynced"), bool)
        self.assertIsInstance(self.w.rpc("mnsync", ["status"]).get("IsBlockchainSynced"), bool)
        self.assertIsInstance(self.w.rpc("quorum", ["list"]), dict)
        self.assertIsInstance(self.w.rpc("quorum", ["dkgstatus"]), dict)
        self.assertIsInstance(self.w.rpc("getconnectioncount"), int)
        self.assertIsInstance(self.w.rpc("getpeerinfo"), list)
        self.assertIsInstance(self.w.rpc("getnetworkinfo").get("subversion"), str)
        self.assertIsInstance(self.w.rpc("spork", ["active"]), dict)
        # These need a ChainLock or a registered masternode. They must fail with
        # a code the worker treats as "not yet", not as an unknown method.
        self.assertIn(self.code(self.w, "getbestchainlock"), [-32603, -5, -8])
        self.assertIn(self.code(self.w, "masternode", ["status"]), [-32603, -1, -8])
        # Before submission, a signed registration is "not found" by this code.
        self.assertEqual(self.code(self.w, "getrawtransaction", ["0" * 64, True]), -5)

    def test_3_wallet_and_spork_identity(self):
        w, node = self.w, self.node
        first = self.step(node, w, w.wallet)
        info = self.ok(node, "getwalletinfo", wallet="dashnet")
        # Core's own default: legacy up to Core 23, descriptor from Core 24.
        self.summary["defaultWalletDescriptors"] = info.get("descriptors", False)
        for address in [first["payoutAddress"], first["sporkAddress"]]:
            self.assertTrue(self.ok(node, "validateaddress", address)["isvalid"], address)
        self.assertNotEqual(first["payoutAddress"], first["sporkAddress"])
        key = w.read("secrets.json")["sporkKey"]
        self.assertEqual(self.key_address(node, key), first["sporkAddress"], "the recorded spork key is not the spork address's key")
        self.assertTrue(self.ok(node, "getaddressinfo", first["payoutAddress"], wallet="dashnet")["ismine"])
        # A retry, and a resume on a Core that restarted without the wallet loaded.
        self.assertEqual(self.step(node, w, w.wallet), first)
        self.ok(node, "unloadwallet", "dashnet")
        self.assertEqual(self.step(node, w, w.wallet), first)
        self.assertEqual(w.read("secrets.json")["sporkKey"], key)
        # How the worker learns that the wallet does not have a label yet.
        self.assertEqual(self.code(w, "getaddressesbylabel", ["dashnet:absent"], True), -11)
        type(self).identity = first

    def test_4_core_restarts_with_the_spork_key(self):
        # core-finalize: Core is configured with the spork address every node
        # trusts and, on the wallet host, the key that signs for it.
        self.needs_identity()
        key = self.w.read("secrets.json")["sporkKey"]
        self.node.restart(["sporkaddr=" + self.identity["sporkAddress"], "sporkkey=" + key])
        self.assertEqual(self.step(self.node, self.w, self.w.wallet), self.identity, "the wallet identity changed across a Core restart")

    def test_5_fund_and_register_an_evonode(self):
        self.needs_identity()
        w, node = self.w, self.node
        w.c["premineHeight"] = PREMINE
        w.q["requiredBalance"] = 4001
        self.assertGreaterEqual(self.step(node, w, w.fund)["balance"], 4001)
        self.assertGreaterEqual(w.rpc("getblockcount"), PREMINE)
        # The masternode list is defined from the first block on.
        self.assertIsInstance(w.rpc("protx", ["list", "valid"]), list)
        w.q["registration"] = dict(
            name="validator-1", address="1.2.3.4", operatorPublicKey=w.rpc("bls", ["generate"])["public"], nodeId="a" * 40
        )
        w.q["requiredConfirmations"] = 1
        registration = self.step(node, w, w.register)
        self.assertRegex(registration["proTxHash"], r"^[0-9a-f]{64}$")
        self.assertGreaterEqual(registration["confirmations"], 1)
        saved = (w.root / "transactions/validator-1.json").read_bytes()
        balance = w.rpc("getbalance", [], True)
        # A replay submits nothing and spends nothing.
        self.assertEqual(self.step(node, w, w.register)["proTxHash"], registration["proTxHash"])
        self.assertEqual((w.root / "transactions/validator-1.json").read_bytes(), saved)
        self.assertEqual(w.rpc("getbalance", [], True), balance)
        self.assertEqual(len(w.rpc("listlockunspent", [], True)), 1, "the collateral is not locked")
        # What fast-forward and the upgrade gate read.
        listed = w.rpc("protx", ["list", "registered", True])
        self.assertEqual([m["proTxHash"] for m in listed], [registration["proTxHash"]])
        self.assertIsInstance(listed[0]["state"]["registeredHeight"], int)
        # The lock is the wallet's own record: it survives the wallet's reload.
        self.ok(node, "unloadwallet", "dashnet")
        self.step(node, w, w.wallet)
        self.assertEqual(len(w.rpc("listlockunspent", [], True)), 1, "the collateral lock was lost on reload")

    def test_6_spork_key_activates_the_network(self):
        self.needs_identity()
        w = self.w
        before = w.rpc("spork", ["active"])
        self.summary["sporks"] = sorted(before)
        wanted = SPORKS + [k for k in OLDER_SPORKS if k in before]
        self.assertEqual([before.get(k) for k in wanted], [False] * len(wanted), "regtest sporks start off; nothing here would exercise the key")
        # Core signs each update with the configured key and accepts it only
        # if that key is the configured spork address's.
        self.step(self.node, w, w.activate)
        after = w.rpc("spork", ["active"])
        self.assertEqual([after.get(k) for k in wanted], [True] * len(wanted))
        # Only what a devnet needs was switched on, and a replay changes nothing.
        self.assertEqual({k: v for k, v in after.items() if k not in wanted}, {k: v for k, v in before.items() if k not in wanted})
        self.step(self.node, w, w.activate)
        self.assertEqual(w.rpc("spork", ["active"]), after)

    def test_7_deployment_made_with_an_exported_wallet_key(self):
        """Deployments made by earlier versions of this tool, on Core 23, hold a
        legacy wallet and the spork key they exported from it. Resuming one, on
        the same Core or after its upgrade, must keep that spork identity."""
        node = self.start()
        w = node.worker()
        # descriptors=false, which Core 24 accepts only with load_on_startup.
        _, error = node.cli("createwallet", "dashnet", "false", "false", "", "false", "false", "true")
        if error is not None:
            self.summary["legacyWallet"] = error[:200]
            # Core 23 and 24 create legacy wallets. A later Core that does not
            # can no longer be given this state: say so, loudly, and go on.
            self.assertGreaterEqual(self.ok(node, "getnetworkinfo")["version"], 250000, "no legacy wallet on %s: %s" % (IMAGE, error))
            self.skipTest("%s no longer creates legacy wallets: %s" % (IMAGE, error[:200]))
        self.assertIs(self.ok(node, "getwalletinfo", wallet="dashnet").get("descriptors", False), False)
        payout = self.ok(node, "getnewaddress", "dashnet:payout", wallet="dashnet")
        spork = self.ok(node, "getnewaddress", "dashnet:spork", wallet="dashnet")
        key = self.ok(node, "dumpprivkey", spork, wallet="dashnet")
        w.atomic("secrets.json", dict(w.read("secrets.json"), sporkKey=key))
        self.assertEqual(self.step(node, w, w.wallet), dict(payoutAddress=payout, sporkAddress=spork))
        self.assertEqual(w.read("secrets.json")["sporkKey"], key)
        self.summary["legacyWallet"] = "ok"

    def test_8_deployment_interrupted_before_the_spork_key(self):
        """A deployment that stopped where Core 24 first broke it: the wallet
        and its labelled addresses exist, no spork key was recorded."""
        node = self.start()
        w = node.worker()
        self.ok(node, "createwallet", "dashnet")
        payout = self.ok(node, "getnewaddress", "dashnet:payout", wallet="dashnet")
        self.ok(node, "getnewaddress", "dashnet:spork", wallet="dashnet")
        self.assertNotIn("sporkKey", w.read("secrets.json"))
        resumed = self.step(node, w, w.wallet)
        self.assertEqual(resumed["payoutAddress"], payout)
        self.assertEqual(self.key_address(node, w.read("secrets.json")["sporkKey"]), resumed["sporkAddress"])
        self.assertEqual(self.step(node, w, w.wallet), resumed)


if __name__ == "__main__":
    unittest.main()
