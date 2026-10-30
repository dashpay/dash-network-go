#!/usr/bin/python3
"""Host-local, locked ACME renewal. No Dashmate or Docker socket in ACME.

The gateway bind-mounts individual files: retain their inodes, journal the old
pair before either write, and signal reload only after both writes complete.
An interrupted install restores the old pair before attempting another renewal.
"""
import datetime
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import time


class Failure(Exception):
    pass


def require(ok, code):
    if not ok:
        raise Failure(code)


def run(args, timeout=60, data=None):
    p = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    require(p.returncode == 0, 'command-failed')
    return p.stdout


def atomic(path, data, mode=0o600):
    path = Path(path)
    tmp = path.with_name(path.name + '.tmp')
    with open(tmp, 'wb') as f:
        os.fchmod(f.fileno(), mode)
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_existing(path, data):
    # Do not follow a replacement symlink and do not replace a mounted inode.
    fd = os.open(path, os.O_WRONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'wb') as f:
        f.seek(0)
        f.write(data)
        f.truncate()
        f.flush()
        os.fsync(f.fileno())


def leaf_der(cert):
    return run(['openssl', 'x509', '-outform', 'DER'], data=cert)


def peer(config, name, verify=True):
    ctx = ssl.create_default_context() if verify else ssl._create_unverified_context()
    with socket.create_connection((config['address'], 443), timeout=8) as s:
        with ctx.wrap_socket(s, server_hostname=name) as t:
            return t.getpeercert(binary_form=True)


def verify_pair(cert, key, config, ca_path='/etc/ssl/certs'):
    require(Path(cert).is_file() and Path(key).is_file(), 'certificate-files-missing')
    run(['openssl', 'x509', '-in', str(cert), '-noout', '-checkhost', config['hostname']])
    decoded = ssl._ssl._test_decode_cert(str(cert))
    verify_names(decoded, config)
    run(['openssl', 'x509', '-in', str(cert), '-noout', '-checkend', '86400'])
    cert_public = run(['openssl', 'x509', '-in', str(cert), '-pubkey', '-noout'])
    key_public = run(['openssl', 'pkey', '-in', str(key), '-pubout'])
    require(cert_public == key_public, 'certificate-key-mismatch')
    run(['openssl', 'verify', '-CApath', ca_path, '-untrusted', str(cert), str(cert)])


def verify_names(decoded, config):
    # Require exact SANs, not CN fallback or a wildcard that broadens authority.
    sans = decoded.get('subjectAltName', [])
    require(('DNS', config['hostname']) in sans, 'hostname-san-missing')
    ips = {ipaddress.ip_address(v) for k, v in sans if k == 'IP Address'}
    require(ipaddress.ip_address(config['address']) in ips, 'ip-san-missing')


def reload(config):
    # This adapter only accepts the inspected Envoy hot-restarter layout.
    run(['docker', 'kill', '--signal', 'HUP', config['gateway']])


def wait_peer(config, der, verify=True):
    for _ in range(15):
        try:
            if peer(config, config['hostname'], verify) == der:
                if verify:
                    require(peer(config, config['address'], True) == der, 'ip-tls-mismatch')
                return
        except (OSError, ssl.SSLError, Failure):
            pass
        time.sleep(2)
    raise Failure('gateway-certificate-not-served')


def recover(root, config):
    if not (root / 'pending-install.json').exists():
        return
    old = json.loads((root / 'pending-install.json').read_text())
    require(old['certificate'] == config['certificate'] and old['key'] == config['key'], 'recovery-path-mismatch')
    cert = (root / 'previous.crt').read_bytes()
    key = (root / 'previous.key').read_bytes()
    require(hashlib.sha256(cert).hexdigest() == old['certificateHash'] and hashlib.sha256(key).hexdigest() == old['keyHash'], 'recovery-backup-mismatch')
    write_existing(config['key'], key)
    write_existing(config['certificate'], cert)
    reload(config)
    # Old cert may be expired. Compare exact leaf bytes, never call this healthy.
    wait_peer(config, leaf_der(cert), verify=False)
    (root / 'pending-install.json').unlink()


def install(root, config, cert, key):
    verify_pair(cert, key, config)
    new_cert, new_key = Path(cert).read_bytes(), Path(key).read_bytes()
    old_cert, old_key = Path(config['certificate']).read_bytes(), Path(config['key']).read_bytes()
    if new_cert == old_cert and new_key == old_key:
        wait_peer(config, leaf_der(new_cert))
        return
    atomic(root / 'previous.crt', old_cert)
    atomic(root / 'previous.key', old_key)
    atomic(root / 'pending-install.json', json.dumps({
        'certificate': config['certificate'], 'key': config['key'],
        'certificateHash': hashlib.sha256(old_cert).hexdigest(),
        'keyHash': hashlib.sha256(old_key).hexdigest(),
    }).encode())
    try:
        write_existing(config['key'], new_key)
        write_existing(config['certificate'], new_cert)
        reload(config)
        wait_peer(config, leaf_der(new_cert))
    except Exception:
        recover(root, config)
        raise
    (root / 'pending-install.json').unlink()


def expires(path):
    return ssl.cert_time_to_seconds(ssl._ssl._test_decode_cert(str(path))['notAfter'])


def renew(root):
    c = json.loads((root / 'tls.json').read_text())
    recover(root, c)
    try:
        decoded = ssl._ssl._test_decode_cert(c['certificate'])
        verify_names(decoded, c)
        if expires(c['certificate']) - time.time() > 3 * 86400:
            wait_peer(c, leaf_der(Path(c['certificate']).read_bytes()))
            return
    except (OSError, ssl.SSLError, KeyError, ValueError, Failure):
        pass
    issue(root, c)
    cert = root / 'acme/certificates' / (c['hostname'] + '.crt')
    key = root / 'acme/certificates' / (c['hostname'] + '.key')
    install(root, c, cert, key)


def issue(root, c, staging=False):
    # Staging proves HTTP-01 and mixed SAN support without ever installing a
    # staging certificate. Its account and files are isolated from production.
    name = 'dashnet-' + c['name'] + ('-acme-staging' if staging else '-acme')
    p = subprocess.run(['docker', 'inspect', name], capture_output=True)
    require(p.returncode != 0, 'acme-container-still-present')
    directory = root / ('acme-staging' if staging else 'acme')
    directory.mkdir(mode=0o700, exist_ok=True)
    server = 'https://acme-staging-v02.api.letsencrypt.org/directory' if staging else 'https://acme-v02.api.letsencrypt.org/directory'
    args = ['docker', 'run', '--rm', '--name', name, '--network', 'host',
            '--cap-drop', 'ALL', '--cap-add', 'NET_BIND_SERVICE',
            '--security-opt', 'no-new-privileges', '--read-only', '--tmpfs', '/tmp',
            '--volume', str(directory) + ':/acme', c['image'],
            'run', '--server', server,
            '--accept-tos', '--email', c['email'], '--path', '/acme',
            '--domains', c['hostname'], '--domains', c['address'],
            '--http', '--http.address', ':80', '--profile', 'shortlived',
            '--key-type', 'RSA2048', '--renew-days', '3']
    run(args, timeout=600)
    if staging:
        cert = directory / 'certificates' / (c['hostname'] + '.crt')
        verify_names(ssl._ssl._test_decode_cert(str(cert)), c)


def main():
    os.umask(0o077)
    root = Path(sys.argv[1]).resolve()
    with open(root / 'renew.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        result = {'checkedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'healthy': False}
        try:
            renew(root)
            config = json.loads((root / 'tls.json').read_text())
            result.update(healthy=True, expiresAt=expires(config['certificate']))
        except Exception as e:
            result['error'] = str(e) if isinstance(e, Failure) else type(e).__name__
            atomic(root / 'tls-status.json', json.dumps(result).encode())
            print('seed-tls-renewal-failed', file=sys.stderr)
            return 1
        atomic(root / 'tls-status.json', json.dumps(result).encode())
        print('seed-tls-verified')
        return 0


if __name__ == '__main__':
    sys.exit(main())
