import base64
import copy
import hashlib
import importlib.util
import json
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "worker", Path(__file__).parents[1] / "worker.py"
)
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)

observer_scope = vars(worker).copy()
exec((Path(__file__).parents[1] / "observer.py").read_text(), observer_scope)
Observer = observer_scope["ReadOnlyWorker"]


def request():
    return dict(
        context=dict(
            planId="a" * 64,
            computePlanId="b" * 64,
            bootstrapId="c" * 64,
            network="devnet-ci",
            coreNetwork="ci-g1",
            platformChainId="dash-devnet-ci-g1",
            genesisTime="2026-09-24T00:00:00Z",
            initialProtocolVersion=14,
            miningIntervalSeconds=10,
            miningNodeName="wallet-1",
            corePeers=["10.0.0.1:20001"],
            ports=dict(
                coreP2P=20001,
                coreRPC=20002,
                coreZMQ=29998,
                platformP2P=26656,
                platformRPC=26657,
                driveABCI=26658,
                driveGRPC=26670,
                dapiGRPC=3010,
                dapiJSON=3009,
                gateway=1443,
            ),
        ),
        target=dict(
            name="wallet-1",
            role="wallet",
            architecture="amd64",
            instanceId="i-00000001",
            sshAddress="10.0.0.1",
            peerAddress="10.0.0.1",
            images=[
                dict(
                    component=x, pinned="docker.io/example/" + x + "@sha256:" + "d" * 64
                )
                for x in ["core", "drive", "dapi", "tenderdash", "gateway", "helper"]
            ],
        ),
        action="inspect",
    )


class Registration(worker.Worker):
    def __init__(self, q, root):
        super().__init__(q, root, root / "lock")
        self.prepared = 0
        self.sent = 0
        self.accepted = False
        self.lose = False

    def address(self, label):
        return "y" + "1" * 33

    def rpc(self, method, params=None, wallet=False):
        if method == "protx" and params[0] == "register_fund_evo":
            assert params[-1] is False and params[-2] is None
            self.prepared += 1
            return "signed-private-transaction"
        if method == "decoderawtransaction":
            return dict(
                txid="e" * 64,
                vout=[
                    dict(
                        n=0, value=4000, scriptPubKey=dict(addresses=[self.address("")])
                    )
                ],
            )
        if method == "gettxout":
            return dict(value=4000)
        if method == "lockunspent":
            assert params == [False, [dict(txid="e" * 64, vout=0)], True]
            return True
        if method == "listlockunspent":
            return [dict(txid="e" * 64, vout=0)]
        if method == "getrawtransaction":
            if not self.accepted:
                raise worker.RPCFailure(method, -5)
            return dict(confirmations=1)
        if method == "sendrawtransaction":
            self.sent += 1
            assert self.read("transactions/validator-1.json")["hex"] == params[0]
            self.accepted = True
            if self.lose:
                raise TimeoutError()
            return "e" * 64
        if method == "protx":
            return dict(
                state=dict(
                    pubKeyOperator="f" * 96,
                    ownerAddress=self.address(""),
                    platformNodeID="a" * 40,
                )
            )
        raise AssertionError(method)


class Tests(unittest.TestCase):
    def test_tenderdash_bare_and_jsonrpc_enveloped_reads(self):
        class Response:
            def __init__(self, value):
                self.raw = json.dumps(value).encode()

            def open(self, url, timeout):
                return io.BytesIO(self.raw)

        status = dict(node_info={}, sync_info=dict(latest_block_height="32"),
                      validator_info={})
        block = dict(block_id=dict(hash="a" * 64), block={})
        for cls in [worker.Worker, Observer]:
            w = cls(request())
            for method, result in [("status", status), ("block?height=32", block)]:
                for value in [result, dict(jsonrpc="2.0", result=result),
                              dict(result=result, error=None)]:
                    w.opener = Response(value)
                    self.assertEqual(w.tenderdash(method), result)
            for value in [[], {}, dict(error={}), dict(error=False),
                          dict(error=dict(code=-1)), dict(result=None),
                          dict(result=status, error=dict(code=-1)),
                          dict(sync_info="not an object"),
                          dict(result=status, padding="x" * 1024 * 1024)]:
                w.opener = Response(value)
                with self.assertRaises(worker.Failure):
                    w.tenderdash("status")

    def test_read_only_adapter_refuses_every_mutating_action(self):
        for action in ["core-start", "core-finalize", "wallet", "identity",
                       "fund", "register", "activate", "fast-forward", "mine-start", "mine-pause",
                       "platform-start", "stop", "upgrade-stage", "upgrade-apply"]:
            q = request()
            q["action"] = action
            w = Observer(q)
            with self.assertRaisesRegex(worker.Failure, "observation-only"):
                w.execute()

    def test_observer_reports_live_protocol_and_complete_membership(self):
        class Response:
            def __init__(self, value):
                self.raw = json.dumps(value).encode()

            def open(self, url, timeout):
                return io.BytesIO(self.raw)

        w = Observer(request())
        w.tenderdash = lambda method: dict(node_info=dict(protocol_version=dict(app="14")))
        value = dict(quorum_type=107, total="2", validators=[
            dict(pro_tx_hash="A" * 64, voting_power="100"),
            dict(pro_tx_hash="B" * 64, voting_power="100")])
        with patch.object(worker.Worker, "platform_status", return_value={}):
            for wrapped in [value, dict(result=value)]:
                w.opener = Response(wrapped)
                observed = w.platform_status()
                self.assertEqual(observed["protocol"], 14)
                self.assertEqual(observed["validators"], ["a" * 64, "b" * 64])
            for invalid in [dict(value, total="3"), dict(value, quorum_type=101),
                            dict(error=dict(code=-1)), []]:
                w.opener = Response(invalid)
                with self.assertRaises(worker.Failure):
                    w.platform_status()

    def test_activation_uses_core23_update_rpc_and_verifies_readback(self):
        class Sporks(worker.Worker):
            active = {"SPORK_2_INSTANTSEND_ENABLED": False,
                      "SPORK_3_INSTANTSEND_BLOCK_FILTERING": False,
                      "SPORK_9_SUPERBLOCKS_ENABLED": False,
                      "SPORK_17_QUORUM_DKG_ENABLED": False,
                      "SPORK_19_CHAINLOCKS_ENABLED": False,
                      "SPORK_21_QUORUM_ALL_CONNECTED": False}
            updates = 0
            accept = True

            def rpc(self, method, params=None, wallet=False):
                if method == "spork":
                    assert params == ["active"], "spork is read-only in Core 23"
                    return self.active.copy()
                assert method == "sporkupdate" and params[1] == 0
                self.updates += 1
                if self.accept:
                    self.active[params[0]] = True
                return "success"

        w = Sporks(request())
        w.activate()
        w.activate()
        self.assertEqual(w.updates, 6)
        w.accept = False
        w.active["SPORK_19_CHAINLOCKS_ENABLED"] = False
        with self.assertRaisesRegex(worker.Failure, "spork-not-active"):
            w.activate()
        del w.active["SPORK_19_CHAINLOCKS_ENABLED"]
        with self.assertRaisesRegex(worker.Failure, "unsupported-spork-profile"):
            w.activate()

    def test_block_time_sets_core_spacing_and_miner_cadence(self):
        class Miner(worker.Worker):
            def __init__(self, q, root):
                super().__init__(q, root, root / "lock")
                self.services = None

            def read(self, path, default=None):
                return dict(rpcPassword="p") if path == "secrets.json" else default

            def atomic(self, relative, value, private=True):
                pass

            def compose(self, component, services):
                self.services = services

        with tempfile.TemporaryDirectory() as tmp:
            for seconds in [10, 150]:
                q = request()
                q["target"].update(role="wallet", name="wallet-1")
                q["context"]["miningIntervalSeconds"] = seconds
                q["payoutAddress"] = "y" + "1" * 33
                w = Miner(q, Path(tmp))
                w.mine_start()
                command = w.services["miner"]["command"][0]
                self.assertIn("sleep %d & wait $!" % seconds, command)
                self.assertTrue(command.startswith("trap 'exit 0' TERM INT;"))
                self.assertEqual(w.block_seconds(), seconds)
            for seconds in [4, 601, "10"]:
                w.c["miningIntervalSeconds"] = seconds
                with self.assertRaisesRegex(worker.Failure, "mining-interval"):
                    w.block_seconds()

    def test_fast_forward_mines_rotation_cycles_only_before_dkg(self):
        class Chain(worker.Worker):
            height, dkg, batches = 4050, False, []
            registered = [4040, 4046, 4044]

            def address(self, label):
                return "y" + "1" * 33

            def core_status(self):
                return dict(height=self.height)

            def rpc(self, method, params=None, wallet=False):
                if method == "protx":
                    assert params == ["list", "registered", True]
                    return [dict(state=dict(registeredHeight=h)) for h in self.registered]
                if method == "spork":
                    return {"SPORK_17_QUORUM_DKG_ENABLED": self.dkg}
                if method == "getblockcount":
                    return self.height
                assert method == "generatetoaddress" and params[1] == self.address("")
                self.batches.append(params[0])
                self.height += params[0]
                return []

        q = request()
        q["target"].update(role="wallet", name="wallet-1")
        q["context"]["premineHeight"] = 4032
        w = Chain(q)
        # Last registration 4046: its quarters are picked at 4080, 4128 and 4176
        # (each 8 blocks after it), so the full quorum forms at 4224.
        self.assertEqual(w.fast_forward()["height"], 4218)
        self.assertTrue(all(0 < b <= 250 for b in w.batches))
        self.assertEqual(w.fast_forward()["height"], 4218, "a resume mines nothing more")
        w.height, w.batches, w.dkg = 4100, [], True
        self.assertEqual(w.fast_forward()["height"], 4100)
        self.assertEqual(w.batches, [], "never fast-forward once DKG runs")
        w.registered = [6000]  # a user's masternode, registered long after creation
        self.assertEqual(w.fast_forward()["height"], 4100, "DKG on: later registrations never block a redeploy")
        w.dkg = False
        with self.assertRaisesRegex(worker.Failure, "fast-forward-height"):
            w.fast_forward()

    def test_fund_premines_in_batches_before_registration(self):
        class Chain(worker.Worker):
            def __init__(self, q, root):
                super().__init__(q, root, root / "lock")
                self.height, self.calls = 0, []

            def address(self, label):
                return "y" + "1" * 33

            def rpc(self, method, params=None, wallet=False):
                if method == "getblockcount":
                    return self.height
                if method == "generatetoaddress":
                    self.calls.append(params[0])
                    self.height += params[0]
                    return ["00" * 32] * params[0]
                if method == "listlockunspent":
                    return []
                if method == "listunspent":
                    return [dict(txid="t", vout=0, amount=500000, spendable=True, safe=True)] if self.height >= 4032 else []
                raise AssertionError(method)

        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["context"]["premineHeight"] = 4032
            q["requiredBalance"] = 60000
            w = Chain(q, Path(tmp))
            self.assertEqual(w.fund()["balance"], 500000)
            self.assertEqual(w.height, 4032)
            self.assertTrue(all(n <= 250 for n in w.calls))
            # An already advanced chain (resume) mines nothing further.
            w.calls = []
            w.fund()
            self.assertEqual(w.calls, [])
            # Short of funds once DKG runs: refuse rather than burst-mine.
            w.q["requiredBalance"] = 4001
            nearly_empty = [dict(txid="t", vout=0, amount=500, spendable=True, safe=True)]
            w.rpc = lambda method, params=None, wallet=False: (
                {"SPORK_17_QUORUM_DKG_ENABLED": True} if method == "spork" else
                nearly_empty if method == "listunspent" else Chain.rpc(w, method, params, wallet))
            with self.assertRaisesRegex(worker.Failure, "funding-needs-mining-after-dkg"):
                w.fund()
            self.assertEqual(w.calls, [])

    def test_lost_registration_response_resends_no_new_funding(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["registration"] = dict(
                name="validator-1",
                address="10.0.0.2",
                operatorPublicKey="f" * 96,
                nodeId="a" * 40,
            )
            w = Registration(q, Path(tmp))
            w.lose = True
            with self.assertRaises(TimeoutError):
                w.register()
            w.lose = False
            self.assertEqual(w.register()["proTxHash"], "e" * 64)
            self.assertEqual((w.prepared, w.sent), (1, 1))
            q["registration"]["nodeId"] = "b" * 40
            with self.assertRaises(worker.Failure):
                w.register()
            self.assertEqual(w.prepared, 1)

    def test_lost_presubmit_retry_uses_saved_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["registration"] = dict(
                name="validator-1",
                address="10.0.0.2",
                operatorPublicKey="f" * 96,
                nodeId="a" * 40,
            )
            w = Registration(q, Path(tmp))
            w.register()
            w.accepted = False
            w.register()
            self.assertEqual((w.prepared, w.sent), (1, 2))

    def test_protobuf_malformed_and_missing_fields(self):
        self.assertEqual(worker.protobuf(b"\x0a\x02hi\x20\x05"), {1: b"hi", 4: 5})
        for raw in [b"\x0a\x04x", b"\x00", b"\x0f", b"\x80" * 12, b"\x08\x00\x08\x00"]:
            with self.assertRaises(worker.Failure):
                worker.protobuf(raw)


import fakehost

SPORK = "yZ9ffZ5NjHqFyRAgcJCvXX1kHUh9yBHa4K"


def pin(name):
    return "index.docker.io/example/" + name + "@sha256:" + hashlib.sha256(name.encode()).hexdigest()


class Host(fakehost.FakeHost, worker.Worker):
    failure = worker.Failure

    def __init__(self, q, root):
        worker.Worker.__init__(self, q, root, root / "lock")
        self.setup_host()
        self.status_calls = 0

    def core_status(self):
        self.status_calls += 1
        return dict(containerId=self.service_container("core")["Id"])


def node_request(role="validator", name="validator-1", pins=True):
    q = request()
    peer = "198.51.100.1"
    q["target"].update(role=role, name=name, peerAddress=peer)
    if role != "validator":
        q["target"]["images"] = [x for x in q["target"]["images"] if x["component"] in ["core", "helper"]]
    q["context"].update(advertise="public", corePeers=["198.51.100.%d:20001" % i for i in range(1, 5)])
    if pins:
        q["context"]["sidecarImages"] = {s: pin(s) for s in ["core_tor", "gateway_rate_limiter", "gateway_rate_limiter_redis"]}
    return q


def host(tmp, role="validator", **kw):
    w = Host(node_request(role, role + "-1", **kw), Path(tmp))
    key = bytes(range(64))
    secret = dict(rpcPassword="p" * 64)
    if role == "validator":
        secret.update(operatorPrivateKey="11" * 32, operatorPublicKey="22" * 48,
                      nodePrivateKey=base64.b64encode(key).decode(),
                      platformNodeID=hashlib.sha256(key[32:]).hexdigest()[:40],
                      tlsCertificate="SELF-SIGNED", tlsPrivateKey="SELF-KEY")
    if role == "wallet":
        secret["sporkKey"] = "cSporkKey"
    w.atomic("secrets.json", secret)
    return w


def platform_request(w):
    peers = [dict(name="validator-%d" % i, address="198.51.100.%d" % i, nodeId="%040x" % i,
                  operatorPublicKey="%096x" % i) for i in range(1, 5)]
    secret = w.read("secrets.json")
    peers[0].update(nodeId=secret["platformNodeID"], operatorPublicKey=secret["operatorPublicKey"])
    w.q.update(peers=peers, genesisCoreHeight=4500, sporkAddress=SPORK)


class DashmateTests(unittest.TestCase):
    def test_dashmate_config_carries_the_devnet_settings(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            config = json.loads(fakehost.FIXTURE.read_text())
            config["configs"] = {w.config_name: config["configs"]["node"]}
            w.configure(config, None)
            d = config["configs"][w.config_name]
            secret = w.read("secrets.json")
            self.assertEqual((d["network"], d["externalIp"]), ("devnet", "198.51.100.1"))
            self.assertEqual(d["core"]["p2p"]["seeds"], [dict(host="198.51.100.%d" % i, port=20001) for i in range(2, 5)])
            users = d["core"]["rpc"]["users"]
            self.assertEqual(users["dashnet"], dict(password="p" * 64, whitelist=None, lowPriority=False))
            self.assertEqual(users["tenderdash"]["whitelist"][:2], ["quoruminfo", "quorumverify"], "dashmate's whitelists kept")
            self.assertEqual({u: users[u]["password"] for u in secret["rpcUsers"]}, secret["rpcUsers"])
            self.assertEqual(len({v["password"] for v in users.values()}), len(users), "a password per user")
            self.assertNotIn("rpcpassword", json.dumps(users))
            self.assertIs(d["core"]["masternode"]["enable"], False, "no BLS key before finalization")
            self.assertIsNone(d["core"]["spork"]["address"])
            self.assertEqual(d["core"]["devnet"]["powTargetSpacing"], 10)
            self.assertEqual(d["core"]["indexes"], [])
            self.assertIs(d["core"]["tor"]["enabled"], True, "dashmate's default is kept")
            abci = d["platform"]["drive"]["abci"]
            self.assertEqual(abci["validatorSet"]["quorum"]["llmqType"], 107)
            self.assertEqual(abci["instantLock"]["quorum"], dict(llmqType=105, dkgInterval=48, activeSigners=2, rotation=True))
            self.assertEqual(abci["epochTime"], 3600)
            td = d["platform"]["drive"]["tenderdash"]
            self.assertEqual((td["mode"], td["moniker"], td["node"]["id"]), ("validator", "validator-1", secret["platformNodeID"]))
            self.assertEqual(td["genesis"]["validator_quorum_type"], 107)
            self.assertEqual(td["genesis"]["consensus_params"]["version"], dict(app_version="14"))
            self.assertEqual(td["genesis"]["consensus_params"]["timeout"]["propose"], "50000000000", "dashmate's own consensus params")
            self.assertNotIn("initial_core_chain_locked_height", td["genesis"])
            self.assertEqual(td["p2p"]["persistentPeers"], [])
            self.assertEqual(td["consensus"]["createEmptyBlocksInterval"], "3m")
            self.assertEqual(d["platform"]["gateway"]["ssl"]["provider"], "self-signed")
            self.assertEqual(d["platform"]["gateway"]["rateLimiter"]["whitelist"], ["198.51.100.%d" % i for i in range(1, 5)])
            self.assertEqual(d["platform"]["drive"]["abci"]["docker"]["image"], w.images["drive"])
            # Finalized, with Platform started.
            platform_request(w)
            w.atomic("platform/inputs.json", dict(genesisCoreHeight=4500, peers=[
                dict(name=v["name"], address=v["address"], nodeId=v["nodeId"]) for v in w.q["peers"]]))
            w.configure(config, SPORK)
            self.assertEqual(d["core"]["masternode"]["operator"]["privateKey"], "11" * 32)
            self.assertEqual(d["core"]["spork"]["address"], SPORK)
            self.assertIsNone(d["core"]["spork"]["privateKey"], "only the wallet signs sporks")
            self.assertEqual(td["genesis"]["initial_core_chain_locked_height"], 4500)
            self.assertEqual([p["host"] for p in td["p2p"]["persistentPeers"]], ["198.51.100.%d" % i for i in range(2, 5)])
            # Trusted certificates use dashmate's own Let's Encrypt provider.
            w.c["gatewayTls"] = dict(issuer="letsencrypt-staging", email="ops@example.org", images={})
            w.configure(config, SPORK)
            ssl = d["platform"]["gateway"]["ssl"]
            self.assertEqual(ssl["provider"], "letsencrypt")
            self.assertIn("staging", ssl["providerConfigs"]["letsencrypt"]["acmeDirectoryUrl"])
            # An option a release no longer has fails closed.
            del d["core"]["zmq"]
            with self.assertRaisesRegex(worker.Failure, "dashmate-option-missing"):
                w.configure(config, SPORK)
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp, "wallet")
            config = json.loads(fakehost.FIXTURE.read_text())
            config["configs"] = {w.config_name: config["configs"]["node"]}
            w.configure(config, SPORK)
            d = config["configs"][w.config_name]
            self.assertEqual(d["core"]["indexes"], ["address", "spent", "timestamp", "tx"])
            self.assertEqual(d["core"]["spork"]["privateKey"], "cSporkKey")
            self.assertIs(d["platform"]["enable"], False)
            self.assertIs(d["core"]["masternode"]["enable"], False)

    def test_salted_render_lines_are_verified_then_normalized(self):
        password = "torControlFixture"
        self.assertTrue(worker.tor_hash_matches(fakehost.tor_hash(password), password))
        self.assertFalse(worker.tor_hash_matches(fakehost.tor_hash(password), "other"))
        self.assertFalse(worker.tor_hash_matches("16:ZZ", password))
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            first = w.render()
            second = w.render()
            self.assertEqual(first.fingerprints, second.fingerprints, "fresh salts are not changes")
            conf = second.stage / w.config_name / "core/dash.conf"
            config = copy.deepcopy(second.config)
            config["configs"][w.config_name]["core"]["rpc"]["users"]["dapi"]["password"] = "changed"
            with self.assertRaisesRegex(worker.Failure, "rpcauth-mismatch"):
                w.normalized(conf, conf.read_bytes(), config)
            config = copy.deepcopy(second.config)
            config["configs"][w.config_name]["core"]["tor"]["control"]["password"] = "changed"
            torrc = second.stage / w.config_name / "core/tor/torrc"
            with self.assertRaisesRegex(worker.Failure, "tor-password-mismatch"):
                w.normalized(torrc, torrc.read_bytes(), config)

    def test_core_start_then_finalize_recreates_core_and_tor_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            w.ensure_core(False)
            self.assertEqual(set(w.containers), {w.container_name("core"), w.container_name("core_tor")})
            conf = w.home / w.config_name / "core/dash.conf"
            self.assertNotIn("masternodeblsprivkey", conf.read_text())
            before = {k: v["Id"] for k, v in w.containers.items()}
            inode = conf.stat().st_ino
            w.ensure_core(False)
            self.assertEqual({k: v["Id"] for k, v in w.containers.items()}, before, "an identical render restarts nothing")
            self.assertEqual(conf.stat().st_ino, inode, "rendered files are rewritten in place")
            w.q["sporkAddress"] = SPORK
            w.ensure_core(True)
            self.assertIn("masternodeblsprivkey=" + "11" * 32, conf.read_text())
            after = {k: v["Id"] for k, v in w.containers.items()}
            self.assertTrue(all(after[k] != before[k] for k in before), "Core and its Tor namespace are recreated")
            self.assertEqual(w.read("core/state.json"), dict(final=True, sporkAddress=SPORK))
            # A resume never reverts finalization, and never changes the spork.
            w.q.pop("sporkAddress")
            w.ensure_core(False)
            self.assertEqual({k: v["Id"] for k, v in w.containers.items()}, after)
            w.q["sporkAddress"] = "yOther1111111111111111111111111111"
            with self.assertRaisesRegex(worker.Failure, "spork-address-changed"):
                w.ensure_core(True)
            client = (w.home / ".client/dash.conf").read_text()
            self.assertIn("rpcuser=dashnet\n", client)
            core = w.service_container("core")["definition"]
            self.assertIn(dict(type="bind", source=str(w.home / ".client/dash.conf"), target="/etc/dash/dash.conf", read_only=True),
                          core["volumes"], "dash-cli in Core authenticates as before")
            self.assertFalse((w.root / "dashmate-stage").exists(), "stages are discarded")

    def test_platform_start_keeps_core_and_platform_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            platform_request(w)
            w.ensure_core(True)
            core = w.service_container("core")["Id"]
            w.platform_start()
            self.assertEqual(w.service_container("core")["Id"], core, "Platform never restarts Core")
            for name in ["drive_abci", "drive_tenderdash", "rs_dapi", "gateway", "gateway_rate_limiter", "gateway_rate_limiter_redis"]:
                self.assertTrue(w.service_container(name)["State"]["Running"], name)
            self.assertEqual(w.service_container("gateway_rate_limiter_redis")["Image"], "image-" + pin("gateway_rate_limiter_redis"))
            ssl = w.home / w.config_name / "platform/gateway/ssl"
            self.assertEqual((ssl / "bundle.crt").read_text(), "SELF-SIGNED")
            self.assertEqual(oct((ssl / "private.key").stat().st_mode & 0o777), "0o600")
            genesis = json.loads((w.home / w.config_name / "platform/drive/tenderdash/genesis.json").read_text())
            self.assertEqual(genesis["initial_core_chain_locked_height"], 4500)
            # A certificate issued since is kept; a second start changes nothing.
            (ssl / "bundle.crt").write_text("TRUSTED")
            ids = {k: v["Id"] for k, v in w.containers.items()}
            w.platform_start()
            self.assertEqual((ssl / "bundle.crt").read_text(), "TRUSTED")
            self.assertEqual({k: v["Id"] for k, v in w.containers.items()}, ids)
            # Genesis never changes once Platform started.
            w.q["genesisCoreHeight"] = 4501
            with self.assertRaisesRegex(worker.Failure, "immutable-platform-config-changed"):
                w.platform_start()
            w.q["genesisCoreHeight"] = 4500
            w.c["genesisTime"] = "2026-09-25T00:00:00Z"
            with self.assertRaisesRegex(worker.Failure, "immutable-platform-config-changed"):
                w.platform_start()
            w.c["genesisTime"] = "2026-09-24T00:00:00Z"
            w.service_container("core")["Config"]["Labels"][worker.FINGERPRINT] = "other"
            with self.assertRaisesRegex(worker.Failure, "core-config-changed"):
                w.platform_start()

    def test_dashmate_selects_services_and_the_tool_pins_their_images(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp, "wallet", pins=False)
            info = w.render_info()["render"]
            fixture = json.loads(fakehost.FIXTURE.read_text())["configs"]["node"]
            self.assertEqual(info, dict(version="4.2.0-beta.3", sidecars={"core_tor": fixture["core"]["tor"]["docker"]["image"]}))
            self.assertEqual(w.containers, {}, "a render starts nothing")
            with self.assertRaisesRegex(worker.Failure, "sidecar-image-not-pinned"):
                w.ensure_core(False)
            w.c["sidecarImages"] = {"core_tor": pin("core_tor")}
            self.assertEqual(w.render_info()["render"]["sidecars"]["core_tor"], fixture["core"]["tor"]["docker"]["image"],
                             "requested images are dashmate's own, whatever the pins")
            w.ensure_core(False)
            self.assertEqual(w.service_container("core_tor")["Image"], "image-" + pin("core_tor"))
            self.assertNotIn("dashmate_helper", str(w.containers), "dashmate's helper never runs")
        for change, code in [
            (lambda m: m.update(quorum_list=dict(image="x", labels={}, volumes=[])), "unsupported-dashmate-service"),
            (lambda m: m["core"].update(image="dashpay/dashd:23"), "dashmate-image-not-pinned"),
        ]:
            with tempfile.TemporaryDirectory() as tmp:
                w = host(tmp, "wallet")
                model = w.compose_model
                w.compose_model = lambda args, env: (lambda m: (change(m), m)[1])(model(args, env))
                with self.assertRaisesRegex(worker.Failure, code):
                    w.render()

    def test_stop_withdraws_this_tools_services_then_dashmates(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            platform_request(w)
            w.ensure_core(True)
            w.platform_start()
            w.q["payoutAddress"] = "y" + "1" * 33
            w.mine_start = worker.Worker.mine_start.__get__(w)
            w.stop()
            self.assertTrue(all(not c["State"]["Running"] for c in w.containers.values()))
            stops = [c for c in w.commands if c[0] == "stop" or (c[0] == "compose" and "stop" in c)]
            self.assertEqual(len(stops), 1, "dashmate's own stop, one Compose call")
            self.assertEqual(set(stops[0][stops[0].index("stop") + 1:]), set(w.selection()))

    def test_only_owned_or_labelled_containers_share_the_host(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            w.containers = {w.container_name("drive_abci"): dict(Config=dict(Labels={})),
                            "devnet-services-quorums-1": dict(Config=dict(Labels={"dashnet.auxiliary": "devnet-ci/validator-1"}))}
            w.check_containers()
            for name, labels in [("stray", {}), ("other", {"dashnet.auxiliary": "devnet-other/validator-1"}),
                                 (w.container_name("drive") + "x", {"dashnet.auxiliary": "devnet-ci/validator-1"}),
                                 (w.container_name("dashmate_helper"), {})]:
                w.containers[name] = dict(Config=dict(Labels=labels))
                with self.assertRaises(worker.Failure):
                    w.check_containers()
                del w.containers[name]

    def test_trusted_certificates_install_in_place_and_reload_envoy(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = host(tmp)
            w.t["peerAddress"] = "198.51.100.10"
            w.c["gatewayTls"] = {"issuer": "letsencrypt", "email": "ops@example.org", "images": {"amd64": pin("lego")}}
            script = w.acme_script()
            self.assertIn("ip=198.51.100.10\n", script)
            self.assertIn("server=https://acme-v02.api.letsencrypt.org/directory\n", script)
            self.assertIn("--key-type rsa2048", script, "dashmate's key type")
            self.assertIn('cat "$crt" >/ssl/bundle.crt', script, "in place: the gateway bind-mounts the files")
            self.assertIn("date +%s >/acme/reload", script)
            unit, units = w.reload_units()
            self.assertIn("PathChanged=" + str(w.root / "acme/reload"), units[unit + ".path"])
            self.assertIn("ExecStart=/usr/bin/docker kill --signal HUP " + w.container_name("gateway"), units[unit + ".service"])
            w.acme_start()
            self.assertEqual(w.units, units)
            acme = w.service_container("acme")
            self.assertEqual(acme["definition"]["image"], pin("lego"))
            w.c["gatewayTls"]["email"] = "x@y.org; rm -rf /"
            with self.assertRaises(worker.Failure):
                w.acme_script()

    def test_incomplete_dapi_status_is_a_named_failure(self):
        def field(n, value):
            data = value if isinstance(value, bytes) else value.encode()
            return bytes([n << 3 | 2, len(data)]) + data
        software = field(1, "4.2.0") + field(2, "4.2.0") + field(3, "1.8.1")
        v0 = field(1, field(1, software)) + field(3, b"") + field(4, field(2, b"x"))

        class Starting(worker.Worker):
            def platform_containers(self):
                return {}, {}

            def tenderdash(self, method):
                return {"sync_info": {"latest_block_height": "3", "catching_up": False}}

            def dapi_status(self):
                return worker.protobuf(v0)

        q = request()
        q["target"]["role"] = "validator"
        with self.assertRaisesRegex(worker.Failure, "dapi-status-incomplete"):
            Starting(q).platform_status()


if __name__ == "__main__":
    unittest.main()
