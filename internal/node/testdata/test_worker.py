import base64
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "worker", Path(__file__).parents[1] / "worker.py"
)
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)


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
