"""Throwaway probe: why does the seed TLS test handshake fail under strict X.509?

Rebuilds the test's fixture exactly as main's setUpClass does, serves the leaf
from a local listener and connects the way renew.peer does, printing the real
exception. Then repeats with candidate fixtures, and (with --public) runs
renew.peer's exact context against real public Let's Encrypt endpoints.
"""
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading

FORCE_STRICT = '--force-strict' in sys.argv


def openssl(root, *args):
    p = subprocess.run(['openssl', *args], cwd=root, capture_output=True, text=True)
    if p.returncode != 0:
        print('openssl', args, 'failed:', p.stderr)
        raise SystemExit(2)
    return p.stdout


def fixture(root, ca_args, leaf_ext):
    openssl(root, 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=Test CA', '-days', '10', '-keyout', 'ca.key', '-out', 'ca.crt', *ca_args)
    openssl(root, 'req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=seed.example.org', '-keyout', 'leaf.key', '-out', 'leaf.csr')
    (root / 'san').write_text(leaf_ext)
    openssl(root, 'x509', '-req', '-in', 'leaf.csr', '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial', '-days', '5', '-extfile', 'san', '-out', 'leaf.crt')


def extensions(root, name):
    text = openssl(root, 'x509', '-in', name, '-noout', '-text')
    start = text.find('X509v3 extensions')
    end = text.find('Signature Algorithm', start)
    return text[start:end].rstrip() if start >= 0 else '(no extensions)'


def handshake(root, name, flags_note=True):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(str(root / 'leaf.crt'), str(root / 'leaf.key'))
    listener = socket.socket()
    listener.bind(('127.0.0.1', 0))
    listener.listen(4)
    listener.settimeout(5)
    address = listener.getsockname()
    server_errors = []

    def server():
        try:
            client, _ = listener.accept()
            with context.wrap_socket(client, server_side=True) as tls:
                tls.recv(1)
        except Exception as e:
            server_errors.append(repr(e))
        finally:
            listener.close()

    thread = threading.Thread(target=server, daemon=True)
    thread.start()
    ctx = ssl.create_default_context(cafile=str(root / 'ca.crt'))
    if FORCE_STRICT:
        ctx.verify_flags |= ssl.VERIFY_X509_STRICT
    try:
        with socket.create_connection(address, timeout=5) as s:
            with ctx.wrap_socket(s, server_hostname=name) as t:
                t.getpeercert(binary_form=True)
        result = 'OK'
    except Exception as e:
        result = 'FAIL %s: %s (verify_code=%s, verify_message=%r)' % (
            type(e).__name__, e, getattr(e, 'verify_code', None), getattr(e, 'verify_message', None))
    thread.join(6)
    return result, server_errors


CASES = [
    ('main fixture (as in setUpClass at 369a517)', [],
     'subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\n'),
    ('CA with keyUsage only; leaf unchanged', ['-addext', 'keyUsage=critical,keyCertSign,cRLSign'],
     'subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\n'),
    ('CA unchanged; leaf with explicit AKID', [],
     'subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\nauthorityKeyIdentifier=keyid:always\n'),
    ('CA with keyUsage; leaf with AKID, keyUsage, basicConstraints', ['-addext', 'keyUsage=critical,keyCertSign,cRLSign'],
     'subjectAltName=DNS:seed.example.org,IP:8.8.8.8\nextendedKeyUsage=serverAuth\nauthorityKeyIdentifier=keyid:always\nkeyUsage=critical,digitalSignature,keyEncipherment\nbasicConstraints=critical,CA:FALSE\n'),
]


def main():
    print('python', sys.version.replace('\n', ' '))
    print('openssl (ssl module)', ssl.OPENSSL_VERSION)
    print('openssl (cli)', subprocess.run(['openssl', 'version'], capture_output=True, text=True).stdout.strip())
    default = ssl.create_default_context()
    print('default verify_flags', int(default.verify_flags), repr(default.verify_flags))
    print('VERIFY_X509_STRICT in default:', bool(default.verify_flags & ssl.VERIFY_X509_STRICT))
    print('VERIFY_X509_PARTIAL_CHAIN in default:', bool(default.verify_flags & getattr(ssl, 'VERIFY_X509_PARTIAL_CHAIN', 0)))
    print('force strict:', FORCE_STRICT)
    for title, ca_args, leaf_ext in CASES:
        print('\n=== ' + title + ' ===')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            fixture(root, ca_args, leaf_ext)
            print('-- ca.crt', extensions(root, 'ca.crt'), sep='\n')
            print('-- leaf.crt', extensions(root, 'leaf.crt'), sep='\n')
            for name in ['seed.example.org', '8.8.8.8']:
                result, server_errors = handshake(root, name)
                print('handshake as %-17s -> %s' % (name, result))
                if server_errors:
                    print('   server side:', server_errors)
    if '--public' in sys.argv:
        print('\n=== renew.peer context against public Let\'s Encrypt endpoints (system trust store) ===')
        for host in sys.argv[sys.argv.index('--public') + 1:]:
            ctx = ssl.create_default_context()
            if FORCE_STRICT:
                ctx.verify_flags |= ssl.VERIFY_X509_STRICT
            try:
                with socket.create_connection((host, 443), timeout=8) as s:
                    with ctx.wrap_socket(s, server_hostname=host) as t:
                        cert = t.getpeercert()
                        issuer = dict(x[0] for x in cert['issuer'])
                        print('%-40s OK issuer=%s / %s notAfter=%s' % (host, issuer.get('organizationName'), issuer.get('commonName'), cert['notAfter']))
            except Exception as e:
                print('%-40s FAIL %s: %s' % (host, type(e).__name__, e))


if __name__ == '__main__':
    main()
