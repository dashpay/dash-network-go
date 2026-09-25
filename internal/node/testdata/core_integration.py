"""Real Core RPC contract proof, in an ephemeral CI runner only. No AWS calls.

Test subclass substitutes instance identity and filesystem location; all Core
configuration, Compose, wallet, registration and reconciliation code is real.
No wallet/config/key/transaction artifacts or raw container logs are published.
"""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import socket
from test_worker import worker, request


class Disposable(worker.Worker):
    def verify_instance(self):
        self.require(
            os.environ.get("DASHNET_DISPOSABLE_CI") == "1", "disposable-ci-only"
        )


def main():
    image = os.environ["CORE_IMAGE"]
    q = request()
    q["target"]["images"] = [dict(component="core", pinned=image)]
    with tempfile.TemporaryDirectory(prefix="dashnet-core-ci-") as tmp:
        root = Path(tmp)
        root.chmod(0o700)
        (root / "owner").write_text(
            q["context"]["computePlanId"] + ":" + q["target"]["instanceId"]
        )
        (root / "ready").write_text(q["context"]["bootstrapId"])
        w = Disposable(q, root, root / "lock")

        def call(action):
            q["action"] = action
            result = w.execute()
            print(action + " passed", flush=True)
            return result

        try:
            call("inspect")
            first = call("core-start")["core"]
            wallet = call("wallet")
            q["sporkAddress"] = wallet["sporkAddress"]
            call("core-finalize")
            # Reload after Core restart before funding (wallet load is explicit).
            call("wallet")
            call("activate")
            # Exercise the live update RPC as well as readback/idempotent resume.
            call("activate")
            q["requiredBalance"] = 4001
            call("fund")
            pair = w.rpc("bls", ["generate"])
            q["registration"] = dict(
                name="validator-1",
                address="10.0.0.2",
                operatorPublicKey=pair["public"],
                nodeId="a" * 40,
            )
            q["requiredConfirmations"] = 1
            registration = call("register")
            saved = (root / "transactions/validator-1.json").read_bytes()
            balance = w.rpc("getbalance", [], True)
            again = call("register")
            assert registration["proTxHash"] == again["proTxHash"]
            assert saved == (root / "transactions/validator-1.json").read_bytes()
            assert balance == w.rpc("getbalance", [], True)
            assert w.rpc("listlockunspent", [], True), "Collateral was not locked"
            resumed = call("core-start")["core"]
            assert first["genesis"] == resumed["genesis"]
            w.images.update(
                drive=os.environ["DRIVE_IMAGE"],
                tenderdash=image,
                dapi=image,
                gateway=image,
            )
            (root / "platform/drive").mkdir(parents=True, mode=0o700)
            w.compose("drive-contract", {"drive": w.platform_services()["drive"]})
            # Drive deliberately waits for a ChainLock and mnsync before opening
            # ABCI/gRPC. This one-Core fixture cannot form a quorum: verify the
            # actual waiting gate, rather than falsely asserting consensus startup.
            drive_deadline = time.monotonic() + 30
            while True:
                logs = w.docker("logs", "--tail", "50", w.container_name("drive"))
                if (
                    b"cannot get best chain lock" in logs
                    or b"waiting for core to sync" in logs
                ):
                    break
                assert (
                    time.monotonic() < drive_deadline
                ), "Drive did not reach its Core-readiness gate"
                time.sleep(1)
            running = w.inspect_container("drive")
            assert running["State"]["Running"] and running["RestartCount"] == 0
            w.verify_image(running, w.images["drive"])
            try:
                with socket.create_connection(
                    ("127.0.0.1", q["context"]["ports"]["driveGRPC"]), timeout=1
                ):
                    raise AssertionError("Drive bypassed its missing-ChainLock gate")
            except OSError:
                pass
            print(
                "Real Drive accepts native configuration and waits for Core readiness; no consensus claim.",
                flush=True,
            )
            q["payoutAddress"] = wallet["payoutAddress"]
            height = w.rpc("getblockcount")
            call("mine-start")
            deadline = time.monotonic() + 25
            while w.rpc("getblockcount") <= height:
                assert (
                    time.monotonic() < deadline
                ), "Persistent miner did not advance Core"
                time.sleep(1)
            miner_before = w.inspect_container("miner")["Id"]
            core_before = w.inspect_container("core")["Id"]
            call("mine-pause")
            assert not w.inspect_container("miner")["State"]["Running"]
            assert w.inspect_container("core")["Id"] == core_before
            assert w.inspect_container("core")["State"]["Running"]
            call("mine-start")
            assert w.inspect_container("miner")["Id"] == miner_before
            call("stop")
            call("core-start")
            call("wallet")
            assert w.rpc("listlockunspent", [], True), "Collateral lock lost on restart"
            assert call("register")["proTxHash"] == registration["proTxHash"]
            q["registration"] = dict(
                name="validator-2",
                address="10.0.0.3",
                operatorPublicKey=w.rpc("bls", ["generate"])["public"],
                nodeId="b" * 40,
            )
            call("fund")
            call("register")
            assert (
                w.rpc("protx", ["info", registration["proTxHash"]])["proTxHash"]
                == registration["proTxHash"]
            )
            assert (
                len(w.rpc("listlockunspent", [], True)) == 2
            ), "Second registration spent first collateral"
            print(
                "Real Core: config, wallet, signed EvoNode registration, idempotent replay, stop/resume passed.",
                flush=True,
            )
        finally:
            for name in ["core", "miner", "drive"]:
                subprocess.run(
                    ["docker", "rm", "-f", w.container_name(name)],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                )


if __name__ == "__main__":
    main()
