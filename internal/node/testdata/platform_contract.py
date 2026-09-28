"""Real Tenderdash/Envoy configuration and TLS/gRPC contract, no chain claim.

DAPI intentionally has no Drive/Core backend: its successful getStatus response
must NOT be accepted as healthy by our worker. Disposable Docker CI only.
"""

import base64
import json
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import time
from test_worker import worker, request


class Disposable(worker.Worker):
    def verify_instance(self):
        self.require(
            os.environ.get("DASHNET_DISPOSABLE_CI") == "1", "disposable-ci-only"
        )


def main():
    q = request()
    q["target"]["role"] = "validator"
    q["target"]["images"] = json.loads(os.environ["PLATFORM_IMAGES"])
    node_id = hashlib.sha256(bytes(32)).hexdigest()[:40]
    q["peers"] = [
        dict(
            name=q["target"]["name"],
            address=q["target"]["peerAddress"],
            nodeId=node_id,
            operatorPublicKey="b" * 96,
            proTxHash="e" * 64,
        )
    ]
    q["genesisCoreHeight"] = 160
    with tempfile.TemporaryDirectory(prefix="dashnet-platform-ci-") as tmp:
        root = Path(tmp)
        root.chmod(0o700)
        (root / "owner").write_text(
            q["context"]["computePlanId"] + ":" + q["target"]["instanceId"]
        )
        (root / "ready").write_text(q["context"]["bootstrapId"])
        w = Disposable(q, root, root / "lock")
        # Match the executor's Ed25519 certificate profile and loopback SAN.
        subprocess.run(
            [
                "openssl",
                "req",
                "-new",
                "-x509",
                "-newkey",
                "ed25519",
                "-nodes",
                "-keyout",
                str(root / "key.pem"),
                "-out",
                str(root / "cert.pem"),
                "-days",
                "1",
                "-subj",
                "/CN=localhost",
                "-addext",
                "subjectAltName=IP:127.0.0.1,IP:" + q["target"]["peerAddress"],
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        key_der = subprocess.check_output(
            ["openssl", "pkey", "-in", str(root / "key.pem"), "-outform", "DER"]
        )
        public_der = subprocess.check_output(
            [
                "openssl",
                "pkey",
                "-in",
                str(root / "key.pem"),
                "-pubout",
                "-outform",
                "DER",
            ]
        )
        private = key_der[-32:] + public_der[-32:]
        node_id = hashlib.sha256(public_der[-32:]).hexdigest()[:40]
        q["peers"][0]["nodeId"] = node_id
        w.atomic(
            "secrets.json",
            dict(
                rpcPassword="private-ci-only",
                platformNodeID=node_id,
                operatorPublicKey="b" * 96,
                nodePrivateKey=base64.b64encode(private).decode(),
                tlsCertificate=(root / "cert.pem").read_text(),
                tlsPrivateKey=(root / "key.pem").read_text(),
            ),
        )
        w.platform_files()
        try:
            td = (
                w.run(
                    [
                        "docker",
                        "run",
                        "--rm",
                        "--user",
                        "0:0",
                        "--network",
                        "none",
                        "--entrypoint",
                        "tenderdash",
                        "-v",
                        str(root / "platform/tenderdash") + ":/tenderdash",
                        w.images["tenderdash"],
                        "show-node-id",
                        "--home",
                        "/tenderdash",
                    ]
                )
                .decode()
                .strip()
            )
            assert td == node_id, "Tenderdash node-key readback"
            print("Tenderdash accepts native configuration and node key.", flush=True)
            w.run(
                [
                    "docker",
                    "run",
                    "--rm",
                    "--user",
                    "0:0",
                    "--network",
                    "none",
                    "--entrypoint",
                    "envoy",
                    "-v",
                    str(root / "platform/envoy.json") + ":/etc/envoy/config.json:ro",
                    "-v",
                    str(root / "platform/tls") + ":/tls:ro",
                    w.images["gateway"],
                    "-c",
                    "/etc/envoy/config.json",
                    "--mode",
                    "validate",
                ]
            )
            print("Envoy accepts native config and generated TLS identity.", flush=True)
            services = w.platform_services()
            w.compose("platform", {k: services[k] for k in ["dapi", "gateway"]})
            deadline = time.monotonic() + 45
            while True:
                try:
                    result = w.dapi_status()
                    break
                except Exception:
                    if time.monotonic() > deadline:
                        raise
                    time.sleep(1)
            # Browser path: grpc-web framing with trailers in the body, and CORS.
            url = "https://127.0.0.1:" + str(w.ports["gateway"]) + "/org.dash.platform.dapi.v0.Platform/getStatus"
            curl = ["/usr/bin/curl", "--silent", "--show-error", "--noproxy", "*", "--max-time", "20",
                    "--cacert", str(root / "platform/tls/cert.pem"), "--dump-header", "-"]
            web = w.run(curl + ["--http1.1", "-H", "content-type: application/grpc-web+proto", "-H", "x-grpc-web: 1",
                                "-H", "origin: https://explorer.example", "--data-binary", "@-", "--output", "-", url],
                        stdin=b"\x00\x00\x00\x00\x02\x0a\x00", timeout=25)
            head, _, body = web.partition(b"\r\n\r\n")
            assert b"content-type: application/grpc-web" in head.lower(), "grpc-web content type"
            assert b"access-control-allow-origin" in head.lower(), "CORS on grpc-web reply"
            assert body[:1] == b"\x00" and b"\x80" in body and b"grpc-status:0" in body, "grpc-web trailer frame"
            preflight = w.run(curl + ["--http1.1", "-X", "OPTIONS", "-o", "/dev/null", "-H", "origin: https://explorer.example",
                                      "-H", "access-control-request-method: POST", "-H", "access-control-request-headers: content-type,x-grpc-web", url],
                              timeout=25).lower()
            assert b"access-control-allow-origin" in preflight and b"x-grpc-web" in preflight, "CORS preflight"
            print("grpc-web getStatus with body trailers and CORS preflight succeed.", flush=True)
            software = worker.protobuf(worker.protobuf(result[1])[1])
            assert software.get(1), "DAPI software version missing"
            assert not software.get(2), "Fixture unexpectedly has Drive"
            try:
                w.platform_status()
            except worker.Failure:
                pass
            else:
                raise AssertionError("Missing Drive/consensus was reported healthy")
            print(
                "Real TLS/HTTP2/gRPC getStatus succeeds; absent consensus is correctly unhealthy.",
                flush=True,
            )
            sds_rotation(w, root, q)
        finally:
            for name in ["drive", "tenderdash", "dapi", "gateway"]:
                subprocess.run(
                    ["docker", "rm", "-f", w.container_name(name)],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                )


def served_fingerprint(w):
    pem = subprocess.run(
        ["openssl", "s_client", "-connect", "127.0.0.1:" + str(w.ports["gateway"]), "-showcerts"],
        input=b"", capture_output=True, timeout=20,
    ).stdout
    cert = subprocess.run(["openssl", "x509", "-noout", "-fingerprint", "-sha256"], input=pem, capture_output=True, timeout=20).stdout
    return cert.strip()


def file_fingerprint(path):
    return subprocess.run(["openssl", "x509", "-noout", "-fingerprint", "-sha256", "-in", str(path)], capture_output=True, check=True).stdout.strip()


def sds_rotation(w, root, q):
    """Trusted-certificate mode: Envoy loads the pair through a watched SDS file
    and hot-reloads a rotated certificate without a restart."""
    w.c["gatewayTls"] = {"issuer": "letsencrypt-staging", "email": "ci@example.org", "images": {}}
    w.platform_files()
    config = json.loads((root / "platform/envoy.json").read_text())
    assert "tls_certificate_sds_secret_configs" in json.dumps(config), "SDS certificate config"
    w.run(["docker", "run", "--rm", "--user", "0:0", "--network", "none", "--entrypoint", "envoy",
           "-v", str(root / "platform/envoy.json") + ":/etc/envoy/config.json:ro",
           "-v", str(root / "platform/tls") + ":/tls:ro",
           w.images["gateway"], "-c", "/etc/envoy/config.json", "--mode", "validate"])
    w.docker("restart", w.container_name("gateway"))
    deadline = time.monotonic() + 45
    while True:
        try:
            w.dapi_status()
            break
        except Exception:
            if time.monotonic() > deadline:
                raise
            time.sleep(1)
    assert served_fingerprint(w) == file_fingerprint(root / "platform/tls/cert.pem"), "SDS serves the self-signed pair first"
    # Rotate exactly as the ACME client does: new directory, then swap sds.json.
    rotated = root / "platform/tls/acme-ci"
    rotated.mkdir(mode=0o700)
    subprocess.run(["openssl", "req", "-new", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
                    "-keyout", str(rotated / "key.pem"), "-out", str(rotated / "cert.pem"), "-days", "1",
                    "-subj", "/CN=rotated", "-addext", "subjectAltName=IP:" + q["target"]["peerAddress"]],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    tmp = root / "platform/tls/.sds.json"
    tmp.write_text(json.dumps(w.sds("/tls/acme-ci/cert.pem", "/tls/acme-ci/key.pem")))
    os.replace(tmp, root / "platform/tls/sds.json")
    want = file_fingerprint(rotated / "cert.pem")
    deadline = time.monotonic() + 30
    while served_fingerprint(w) != want:
        assert time.monotonic() < deadline, "Envoy did not hot-reload the rotated certificate"
        time.sleep(1)
    # A later platform start keeps the installed certificate.
    w.platform_files()
    assert "acme-ci" in (root / "platform/tls/sds.json").read_text(), "installed certificate kept"
    print("Watched SDS certificate: served, hot-reloaded after rotation, and kept across restarts.", flush=True)


if __name__ == "__main__":
    main()
