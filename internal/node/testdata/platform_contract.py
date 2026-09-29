"""A validator rendered by the release's real dashmate and run from its compose
files: Core, Tor, Drive, Tenderdash, rs-dapi, the gateway and its rate limiter.
No chain claim: one Core forms no quorum, so Drive must hold at its ChainLock
gate and the worker must not report Platform healthy, while the gateway still
serves DAPI over TLS, HTTP/2, gRPC and gRPC-Web. Disposable Docker CI only.
"""

import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from test_worker import worker, request


class Disposable(worker.Worker):
    def verify_instance(self):
        self.require(os.environ.get("DASHNET_DISPOSABLE_CI") == "1", "disposable-ci-only")


def openssl_identity(root, q):
    """The executor's Ed25519 certificate profile, and a Tenderdash node key."""
    subprocess.run(
        ["openssl", "req", "-new", "-x509", "-newkey", "ed25519", "-nodes",
         "-keyout", str(root / "key.pem"), "-out", str(root / "cert.pem"), "-days", "1", "-subj", "/CN=localhost",
         "-addext", "subjectAltName=IP:127.0.0.1,IP:" + q["target"]["peerAddress"]],
        check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    key_der = subprocess.check_output(["openssl", "pkey", "-in", str(root / "key.pem"), "-outform", "DER"])
    public_der = subprocess.check_output(["openssl", "pkey", "-in", str(root / "key.pem"), "-pubout", "-outform", "DER"])
    return base64.b64encode(key_der[-32:] + public_der[-32:]).decode()


def served(w):
    pem = subprocess.run(["openssl", "s_client", "-connect", "127.0.0.1:" + str(w.ports["gateway"]), "-showcerts"],
                         input=b"", capture_output=True, timeout=20).stdout
    return subprocess.run(["openssl", "x509", "-noout", "-fingerprint", "-sha256"], input=pem, capture_output=True, timeout=20).stdout.strip()


def fingerprint(path):
    return subprocess.run(["openssl", "x509", "-noout", "-fingerprint", "-sha256", "-in", str(path)], capture_output=True, check=True).stdout.strip()


def until(check, what, seconds=60):
    deadline = time.monotonic() + seconds
    while True:
        try:
            value = check()
            if value:
                return value
        except Exception:
            if time.monotonic() > deadline:
                raise
        assert time.monotonic() < deadline, what
        time.sleep(1)


def main():
    images = json.loads(os.environ["PLATFORM_IMAGES"])
    q = request()
    q["target"].update(role="validator", name="validator-1", peerAddress="198.51.100.1")
    q["target"]["images"] = [dict(component=k, pinned=v) for k, v in images["components"].items()]
    q["context"].update(advertise="public", corePeers=["198.51.100.1:20001"], sidecarImages=images["sidecars"])
    with tempfile.TemporaryDirectory(prefix="dashnet-platform-ci-") as tmp:
        root = Path(tmp)
        root.chmod(0o700)
        (root / "owner").write_text(q["context"]["computePlanId"] + ":" + q["target"]["instanceId"])
        (root / "ready").write_text(q["context"]["bootstrapId"])
        w = Disposable(q, root, root / "lock")

        def call(action):
            q["action"] = action
            result = w.execute()
            print(action + " passed", flush=True)
            return result

        try:
            call("inspect")
            info = call("render")["render"]
            assert set(info["sidecars"]) == {"core_tor", "gateway_rate_limiter", "gateway_rate_limiter_redis"}, info
            call("core-start")
            q.update(nodePrivateKey=openssl_identity(root, q), tlsCertificate=(root / "cert.pem").read_text(),
                     tlsPrivateKey=(root / "key.pem").read_text())
            identity = call("identity")
            w.rpc("createwallet", ["dashnet"])
            q["sporkAddress"] = w.rpc("getnewaddress", [], True)
            call("core-finalize")
            q["peers"] = [dict(name="validator-1", address="198.51.100.1", nodeId=identity["platformNodeId"],
                               operatorPublicKey=identity["operatorPublicKey"], proTxHash="e" * 64)]
            q["genesisCoreHeight"] = 1
            call("platform-start")
            home = w.home / w.config_name
            # Tenderdash accepts dashmate's rendered configuration and node key.
            node = w.docker("run", "--rm", "--network", "none", "--entrypoint", "tenderdash",
                            "-v", str(home / "platform/drive/tenderdash") + ":/tenderdash/config:ro",
                            w.images["tenderdash"], "show-node-id", "--home", "/tenderdash").decode().strip()
            assert node == identity["platformNodeId"], "Tenderdash node-key readback"
            # Drive holds at its ChainLock gate: one Core forms no quorum.
            def gated():
                logs = w.docker("logs", "--tail", "80", w.container_name("drive_abci"))
                return b"cannot get best chain lock" in logs or b"waiting for core to sync" in logs
            until(gated, "Drive did not reach its Core-readiness gate")
            drive = w.inspect_container("drive_abci")
            address = drive["NetworkSettings"]["Networks"][w.project + "_default"]["IPAddress"]
            try:
                with socket.create_connection((address, 26670), timeout=1):
                    raise AssertionError("Drive bypassed its missing-ChainLock gate")
            except OSError:
                pass
            print("Real Drive runs dashmate's configuration and waits for Core readiness.", flush=True)
            # DAPI through dashmate's gateway: TLS, HTTP/2 and gRPC.
            result = until(w.dapi_status, "DAPI unreachable through the gateway")
            software = worker.protobuf(worker.protobuf(result[1])[1])
            assert software.get(1), "DAPI software version missing"
            # Browser path: a doubled slash, grpc-web trailers in the body, CORS.
            url = "https://127.0.0.1:" + str(w.ports["gateway"]) + "//org.dash.platform.dapi.v0.Platform/getStatus"
            curl = ["/usr/bin/curl", "--silent", "--show-error", "--noproxy", "*", "--max-time", "20",
                    "--cacert", str(root / "cert.pem"), "--dump-header", "-"]
            web = w.run(curl + ["--http1.1", "-H", "content-type: application/grpc-web+proto", "-H", "x-grpc-web: 1",
                                "-H", "origin: https://explorer.example", "--data-binary", "@-", "--output", "-", url],
                        stdin=b"\x00\x00\x00\x00\x02\x0a\x00", timeout=25)
            head, _, body = web.partition(b"\r\n\r\n")
            assert b"content-type: application/grpc-web" in head.lower(), "grpc-web content type"
            assert b"access-control-allow-origin" in head.lower(), "CORS on grpc-web reply"
            assert body[:1] == b"\x00" and b"\x80" in body and b"grpc-status:0" in body, "grpc-web trailer frame"
            print("dashmate's gateway serves gRPC, grpc-web and CORS.", flush=True)
            try:
                w.platform_status()
            except Exception:  # Tenderdash restarts until Drive opens ABCI
                pass
            else:
                raise AssertionError("Platform without consensus reported healthy")
            # A renewed certificate, installed in place, is served after the
            # reload signal dashmate itself sends.
            ssl = home / "platform/gateway/ssl"
            assert served(w) == fingerprint(ssl / "bundle.crt"), "the self-signed pair is served first"
            renewed = root / "renewed"
            renewed.mkdir()
            subprocess.run(["openssl", "req", "-new", "-x509", "-newkey", "rsa:2048", "-nodes",
                            "-keyout", str(renewed / "key.pem"), "-out", str(renewed / "cert.pem"), "-days", "1",
                            "-subj", "/CN=renewed", "-addext", "subjectAltName=IP:127.0.0.1"],
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            (ssl / "private.key").write_text((renewed / "key.pem").read_text())
            (ssl / "bundle.crt").write_text((renewed / "cert.pem").read_text())
            _, units = w.reload_units()
            reload = next(v for k, v in units.items() if k.endswith(".service")).split("ExecStart=/usr/bin/docker ")[1].split()
            w.docker(*reload)
            want = fingerprint(renewed / "cert.pem")
            until(lambda: served(w) == want, "Envoy did not reload the renewed certificate")
            print("A renewed certificate is served after Envoy's hot restart.", flush=True)
            # Converged: an identical render recreates nothing; a new image
            # recreates exactly its service.
            before = {s: w.inspect_container(s)["Id"] for s in w.selection()}
            call("platform-start")
            assert {s: w.inspect_container(s)["Id"] for s in w.selection()} == before, "identical render recreated services"
            w.images["dapi"] = images["upgrade"]["dapi"]
            r = w.render()
            try:
                w.install(r, [s for s in r.selection if s in worker.PLATFORM_SERVICES])
                w.up(r, ["rs_dapi"])
            finally:
                w.discard(r)
            after = {s: w.inspect_container(s)["Id"] for s in w.selection()}
            assert [s for s in before if after[s] != before[s]] == ["rs_dapi"], (before, after)
            w.verify_image(w.inspect_container("rs_dapi"), images["upgrade"]["dapi"])
            print("Compose recreates exactly the services a render changes.", flush=True)
            call("stop")
            assert not any(w.inspect_container(s)["State"]["Running"] for s in w.selection())
            print("dashmate's own stop withdraws every service.", flush=True)
        finally:
            try:
                w.dm_compose(w.home, "down", "--volumes", timeout=300)
            except Exception:
                pass
            for name in worker.CORE_SERVICES + worker.PLATFORM_SERVICES + ["render"]:
                subprocess.run(["docker", "rm", "-f", w.container_name(name)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


if __name__ == "__main__":
    main()
