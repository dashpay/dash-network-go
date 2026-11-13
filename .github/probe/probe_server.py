"""Throwaway probe: what does the test's listener see when renew.peer disconnects?

renew.peer completes the handshake, reads the peer certificate and closes the
socket. The test's server thread stops at its first exception, so anything it
raises on connection one makes connection two (the IP identity) fail.
Each variant serves two connections the way the test does, many times over.
"""
import collections
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import traceback

ROUNDS = 40


def openssl(root, *args):
    subprocess.run(['openssl', *args], cwd=root, check=True, capture_output=True)


def fixture(root):
    openssl(root, 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=Test CA', '-days', '10', '-addext', 'keyUsage=critical,keyCertSign,cRLSign', '-keyout', 'ca.key', '-out', 'ca.crt')
    openssl(root, 'req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=seed.example.org', '-keyout', 'leaf.key', '-out', 'leaf.csr')
    (root / 'san').write_text('subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nauthorityKeyIdentifier=keyid:always\n')
    openssl(root, 'x509', '-req', '-in', 'leaf.csr', '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial', '-days', '5', '-extfile', 'san', '-out', 'leaf.crt')


def one_round(root, configure_server, samples):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(str(root / 'leaf.crt'), str(root / 'leaf.key'))
    configure_server(context)
    listener = socket.socket()
    listener.bind(('127.0.0.1', 0))
    listener.listen(4)
    listener.settimeout(5)
    address = listener.getsockname()
    server = []

    def serve():
        phase, n = 'accept', 0
        try:
            for n in range(2):
                phase = 'accept'
                client, _ = listener.accept()
                phase = 'handshake'
                with context.wrap_socket(client, server_side=True, do_handshake_on_connect=False) as tls:
                    tls.do_handshake()
                    phase = 'recv'
                    tls.recv(1)
                    phase = 'close'
        except Exception as e:
            key = 'connection %d, %s: %s' % (n + 1, phase, type(e).__name__)
            server.append(key)
            samples.setdefault(key, traceback.format_exc())
        finally:
            listener.close()

    thread = threading.Thread(target=serve, daemon=True)
    thread.start()
    client = []
    version = None
    for name in ['seed.example.org', '8.8.8.8']:
        ctx = ssl.create_default_context(cafile=str(root / 'ca.crt'))
        ctx.verify_flags |= ssl.VERIFY_X509_STRICT
        try:
            with socket.create_connection(address, timeout=5) as s:
                with ctx.wrap_socket(s, server_hostname=name) as t:
                    t.getpeercert(binary_form=True)
                    version = t.version()
            client.append('ok')
        except Exception as e:
            client.append(type(e).__name__)
    thread.join(6)
    return tuple(client), tuple(server), version


def nothing(_):
    pass


def no_tickets(context):
    context.num_tickets = 0


def tls12(context):
    context.maximum_version = ssl.TLSVersion.TLSv1_2


VARIANTS = [
    ('as the test serves today', nothing),
    ('server context with num_tickets = 0', no_tickets),
    ('server limited to TLS 1.2 (for comparison only)', tls12),
]


def main():
    print('python', sys.version.replace('\n', ' '))
    print('openssl', ssl.OPENSSL_VERSION)
    print('server default num_tickets', ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).num_tickets)
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        fixture(root)
        for title, configure_server in VARIANTS:
            outcomes = collections.Counter()
            samples = {}
            for _ in range(ROUNDS):
                outcomes[one_round(root, configure_server, samples)] += 1
            print('\n=== %s: %d rounds ===' % (title, ROUNDS))
            for (client, server, version), count in outcomes.most_common():
                print('%3d x  %s  client(hostname, ip)=%s  server errors=%s' % (count, version, client, list(server) or 'none'))
            for key, text in samples.items():
                print('-- first traceback for [%s]' % key)
                print(text.rstrip())


if __name__ == '__main__':
    main()
