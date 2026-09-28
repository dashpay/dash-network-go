import base64
import copy
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
                for x in ["core", "drive", "dapi", "tenderdash", "gateway"]
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
        w.dkg, w.registered = False, [6000]
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

    def test_public_advertising_disables_private_addresses(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["target"]["peerAddress"] = "198.51.100.10"
            q["context"]["corePeers"] = ["198.51.100.10:20001", "198.51.100.11:20001"]
            w = worker.Worker(q, Path(tmp), Path(tmp) / "lock")
            self.assertIn("allowprivatenet=1\n", w.core_config())
            q["context"]["advertise"] = "public"
            config = worker.Worker(q, Path(tmp), Path(tmp) / "lock").core_config()
            self.assertIn("allowprivatenet=0\n", config)
            self.assertIn("externalip=198.51.100.10:20001\n", config)
            self.assertIn("addnode=198.51.100.11:20001\n", config)
            self.assertNotIn("addnode=198.51.100.10:20001", config)

    def test_trusted_gateway_certificates_use_watched_sds(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["target"]["peerAddress"] = "198.51.100.10"
            w = worker.Worker(q, Path(tmp), Path(tmp) / "lock")
            context = w.envoy()["static_resources"]["listeners"][0]["filter_chains"][0]["transport_socket"]["typed_config"]["common_tls_context"]
            self.assertIn("tls_certificates", context)
            q["context"]["gatewayTls"] = {"issuer": "letsencrypt", "email": "ops@example.org", "images": {}}
            w = worker.Worker(q, Path(tmp), Path(tmp) / "lock")
            context = w.envoy()["static_resources"]["listeners"][0]["filter_chains"][0]["transport_socket"]["typed_config"]["common_tls_context"]
            self.assertNotIn("tls_certificates", context)
            source = context["tls_certificate_sds_secret_configs"][0]["sds_config"]["path_config_source"]
            self.assertEqual(source, {"path": "/tls/sds.json", "watched_directory": {"path": "/tls"}})
            self.assertEqual(w.envoy()["node"]["id"], q["target"]["name"], "SDS needs a node identity")
            script = w.acme_script()
            self.assertIn("ip=198.51.100.10\n", script)
            self.assertIn("server=https://acme-v02.api.letsencrypt.org/directory\n", script)
            self.assertIn("--profile shortlived", script)
            self.assertNotIn("staging", script)
            # Injection-shaped parameters are refused.
            q["context"]["gatewayTls"]["email"] = "x@y.org; rm -rf /"
            with self.assertRaises(worker.Failure):
                worker.Worker(q, Path(tmp), Path(tmp) / "lock").acme_script()

    def test_stop_includes_the_acme_client(self):
        class Stopping(worker.Worker):
            stopped = []
            running = {"acme", "gateway", "core"}

            def inspect_container(self, name):
                return {"State": {"Running": name in self.running}} if name in {"acme", "gateway", "core"} else None

            def docker(self, *args, timeout=120):
                assert args[0] == "stop"
                self.stopped.append(args[-1])
                self.running.discard(args[-1].rsplit("-", 1)[-1])
                return b""

        w = Stopping(request())
        w.stop()
        self.assertEqual([n.rsplit("-", 1)[-1] for n in w.stopped], ["acme", "gateway", "core"])

    def test_incomplete_dapi_status_is_a_named_failure(self):
        def field(n, value):
            data = value if isinstance(value, bytes) else value.encode()
            return bytes([n << 3 | 2, len(data)]) + data
        software = field(1, "4.2.0") + field(2, "4.2.0") + field(3, "1.8.1")
        v0 = field(1, field(1, software)) + field(3, b"") + field(4, field(2, b"x"))

        class Starting(worker.Worker):
            def inspect_container(self, name):
                return {"Id": "i" * 64, "RestartCount": 0, "State": {"Running": True}}

            def verify_image(self, value, pinned):
                pass

            def tenderdash(self, method):
                return {"sync_info": {"latest_block_height": "3", "catching_up": False}}

            def dapi_status(self):
                return worker.protobuf(v0)

        q = request()
        q["target"]["role"] = "validator"
        with self.assertRaisesRegex(worker.Failure, "dapi-status-incomplete"):
            Starting(q).platform_status()

    def test_only_labelled_auxiliary_containers_share_the_host(self):
        class Listing(worker.Worker):
            names = ""

            def docker(self, *args, timeout=120):
                assert args[:3] == ("container", "ls", "-a")
                return self.names.encode()

        with tempfile.TemporaryDirectory() as tmp:
            w = Listing(request(), Path(tmp), Path(tmp) / "lock")
            core = w.container_name("core")
            w.names = core + "\t\ndevnet-services-quorums-1\tdevnet-ci/wallet-1\n"
            w.check_containers()
            for names in [
                core + "\t\nstray\t\n",
                "other\tdevnet-other/wallet-1\n",
                "other\tdevnet-ci/validator-1\n",
                w.container_name("drive") + "x\tdevnet-ci/wallet-1\n",
            ]:
                w.names = names
                with self.assertRaises(worker.Failure):
                    w.check_containers()

    def test_platform_genesis_never_replaced(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = request()
            q["target"]["role"] = "validator"
            q["target"]["name"] = "validator-1"
            q["peers"] = [
                dict(
                    name="validator-1",
                    address="10.0.0.1",
                    nodeId="a" * 40,
                    operatorPublicKey="b" * 96,
                    proTxHash="e" * 64,
                )
            ]
            q["genesisCoreHeight"] = 160
            w = worker.Worker(q, Path(tmp), Path(tmp) / "lock")
            w.atomic(
                "secrets.json",
                dict(
                    rpcPassword="private-password",
                    platformNodeID="a" * 40,
                    operatorPublicKey="b" * 96,
                    nodePrivateKey=base64.b64encode(bytes(64)).decode(),
                    tlsCertificate="certificate",
                    tlsPrivateKey="private-key",
                ),
            )
            w.platform_files()
            before = (
                Path(tmp) / "platform/tenderdash/config/genesis.json"
            ).read_bytes()
            w.platform_files()
            self.assertEqual(
                before,
                (Path(tmp) / "platform/tenderdash/config/genesis.json").read_bytes(),
            )
            q["genesisCoreHeight"] = 161
            with self.assertRaises(worker.Failure):
                w.platform_files()
            self.assertEqual(
                before,
                (Path(tmp) / "platform/tenderdash/config/genesis.json").read_bytes(),
            )
            services = w.platform_services()
            self.assertNotIn("core", services)
            self.assertEqual(
                services["drive"]["environment"]["VALIDATOR_SET_QUORUM_TYPE"], "107"
            )
            self.assertEqual(
                services["dapi"]["environment"]["DAPI_BIND_ADDRESS"], "127.0.0.1"
            )

    def test_protobuf_malformed_and_missing_fields(self):
        self.assertEqual(worker.protobuf(b"\x0a\x02hi\x20\x05"), {1: b"hi", 4: 5})
        for raw in [b"\x0a\x04x", b"\x00", b"\x0f", b"\x80" * 12, b"\x08\x00\x08\x00"]:
            with self.assertRaises(worker.Failure):
                worker.protobuf(raw)


if __name__ == "__main__":
    unittest.main()
