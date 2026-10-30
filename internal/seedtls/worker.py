"""Existing DAPI seed adoption. Private configuration never leaves this host.

Only the named helper, TLS files, renewal units and Dashmate package may change.
Core, DAPI, Drive, Tenderdash, rate limiters, networks and all volumes are kept.
"""
import datetime
import fcntl
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import urllib.request


class Failure(Exception):
    pass


def require(ok, code):
    if not ok:
        raise Failure(code)


def run(args, timeout=60, data=None):
    p = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    require(p.returncode == 0, 'command-failed')
    return p.stdout


def sha(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def atomic(path, value, raw=False):
    path = Path(path)
    tmp = path.with_name(path.name + '.tmp')
    with open(tmp, 'wb') as f:
        os.fchmod(f.fileno(), 0o600)
        f.write(value if raw else (json.dumps(value, indent=2) + '\n').encode())
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def identity(t):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    token = opener.open(urllib.request.Request('http://169.254.169.254/latest/api/token', method='PUT', headers={'X-aws-ec2-metadata-token-ttl-seconds': '60'}), timeout=4).read().decode()
    doc = json.load(opener.open(urllib.request.Request('http://169.254.169.254/latest/dynamic/instance-identity/document', headers={'X-aws-ec2-metadata-token': token}), timeout=4))
    require(doc['instanceId'] == t['instance'] and doc['accountId'] == t['account'] and doc['region'] == t['region'], 'host-identity-mismatch')


def containers():
    ids = run(['docker', 'ps', '-aq']).decode().split()
    return json.loads(run(['docker', 'inspect'] + ids)) if ids else []


def package():
    p = subprocess.run(['dpkg-query', '-W', '-f=${db:Status-Status} ${Version}', 'dashmate'], capture_output=True, text=True)
    return p.stdout.strip() if p.returncode == 0 else 'absent'


def container_fingerprint(c):
    # Docker emits Mounts from a map; ordering is not a workload change.
    return sha([c['Config'], c['HostConfig'], sorted(c['Mounts'], key=lambda m: m['Destination'])])


def configuration_digest(path):
    data = path.read_bytes()
    # The legacy helper can regenerate rpcauth salts while running. As in the
    # existing managed adapter, normalize ONLY HMAC-verified credentials. Do not
    # ignore authentication lines, unknown users, or any other configuration.
    if path.name == 'dash.conf' and path.parent.name == 'core' and path.parents[2].name == '.dashmate':
        source = path.parents[2] / 'config.json'
        require(source.is_file() and not source.is_symlink() and source.stat().st_size <= 2*1024*1024, 'rpc-auth-source')
        users = json.loads(source.read_text())['configs'][path.parents[1].name]['core']['rpc']['users']
        lines = []
        for line in data.decode().splitlines(keepends=True):
            if line.startswith('rpcauth='):
                user, auth = line[len('rpcauth='):].strip().split(':', 1)
                salt, digest = auth.split('$', 1)
                password = users.get(user, {}).get('password')
                require(isinstance(password, str) and bool(password), 'rpc-auth-source')
                verified = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
                require(hmac.compare_digest(verified, digest), 'rpc-auth-source-mismatch')
                credential = sha(['dashnet-rpcauth-v1', user, password])
                ending = '\r\n' if line.endswith('\r\n') else ('\n' if line.endswith('\n') else '')
                line = 'rpcauth=' + user + ':verified-credential-' + credential + ending
            lines.append(line)
        data = ''.join(lines).encode()
    return hashlib.sha256(data).hexdigest()


def file_hashes(c, excluded):
    result = {}
    for mount in c['Mounts']:
        if mount['Type'] != 'bind' or mount['Source'] in excluded:
            continue
        p = Path(mount['Source'])
        require(not p.is_symlink(), 'symlink-bind-source')
        files = [p] if p.is_file() else sorted(p.rglob('*'))
        for f in files:
            require(not f.is_symlink(), 'symlink-bound-config')
            if f.is_file():
                require(f.stat().st_size < 32 * 1024 * 1024, 'unexpected-large-bound-file')
                result[str(f)] = configuration_digest(f)
    return result


def observe(t, allow_retired=False):
    cs = containers()
    by_name = {c['Name'].lstrip('/'): c for c in cs}
    require(t['gateway'] in by_name, 'gateway-missing')
    gateway = by_name[t['gateway']]
    project = gateway['Config'].get('Labels', {}).get('com.docker.compose.project')
    require(project and gateway['Config']['Labels'].get('com.docker.compose.service') == 'gateway', 'gateway-layout-unsupported')
    family = [c for c in cs if c['Config'].get('Labels', {}).get('com.docker.compose.project') == project]
    supported = {'core', 'gateway', 'dapi_api', 'dapi_core_streams', 'drive_abci', 'drive_tenderdash', 'gateway_rate_limiter', 'gateway_rate_limiter_redis', 'dashmate_helper'}
    names = {c['Config']['Labels'].get('com.docker.compose.service') for c in family}
    require(names <= supported and {'core', 'gateway', 'dapi_api', 'dapi_core_streams'} <= names, 'seed-service-layout-unsupported')
    helpers = [c for c in family if c['Config']['Labels'].get('com.docker.compose.service') == 'dashmate_helper']
    require((len(helpers) == 1 and helpers[0]['Name'].lstrip('/') == t['helper']) or (allow_retired and not helpers), 'helper-binding-mismatch')
    require(gateway['State']['Running'], 'gateway-not-running')
    require(gateway['Config'].get('Entrypoint') == ['/etc/envoy/hot-restarter.py', '/etc/envoy/start_envoy.sh'], 'unsupported-gateway-reload')
    require(gateway['HostConfig']['PortBindings'].get('10000/tcp') == [{'HostIp': '0.0.0.0', 'HostPort': '443'}], 'gateway-port-mismatch')
    mounts = {m['Destination']: m for m in gateway['Mounts']}
    paths = {}
    for key, dest in [('certificate', '/etc/ssl/bundle.crt'), ('key', '/etc/ssl/private.key')]:
        m = mounts.get(dest, {})
        require(m.get('Type') == 'bind' and not m.get('RW'), 'certificate-mount-unsupported')
        p = Path(m['Source'])
        require(p.is_file() and not p.is_symlink(), 'certificate-path-invalid')
        paths[key] = str(p)
    baseline = {'project': project, 'paths': paths, 'containers': {}, 'files': {}, 'package': package()}
    require(allow_retired or baseline['package'].startswith('installed '), 'installed-dashmate-package-required')
    for c in family:
        n = c['Name'].lstrip('/')
        is_helper = n == t['helper']
        require(is_helper or c['State']['Running'], 'seed-service-not-running')
        baseline['containers'][n] = {'id': c['Id'], 'image': c['Image'], 'config': container_fingerprint(c), 'startedAt': '' if is_helper else c['State']['StartedAt']}
        if not is_helper:
            baseline['files'].update(file_hashes(c, set(paths.values())))
    return baseline, family


def core_ready(family):
    c = next(c for c in family if c['Config']['Labels']['com.docker.compose.service'] == 'core')
    d = json.loads(run(['docker', 'exec', c['Id'], 'dash-cli', 'getblockchaininfo'], timeout=25))
    require(d['chain'] == 'main' and not d['initialblockdownload'] and d['headers'] == d['blocks'], 'mainnet-core-not-synced')
    return d['blocks']


def assert_preserved(expected, actual, helper):
    for name, value in expected['containers'].items():
        if name != helper:
            require(actual['containers'].get(name) == value, 'preserved-container-drift')
    require(actual['files'] == expected['files'], 'preserved-configuration-drift')


def check_retirement():
    # Maintainer scripts could stop unrelated services. This adapter never runs
    # unknown uninstall hooks or generic apt autoremove/purge.
    for suffix in ['preinst', 'postinst', 'prerm', 'postrm', 'triggers']:
        require(not Path('/var/lib/dpkg/info/dashmate.' + suffix).exists(), 'dashmate-package-hooks-unsupported')
    for directory in ['/etc/cron.d', '/var/spool/cron/crontabs']:
        for p in Path(directory).glob('*'):
            if p.is_file():
                require('dashmate' not in p.read_text(errors='replace').lower(), 'dashmate-cron-requires-review')
    units = run(['systemctl', 'list-unit-files', '--no-pager', '--no-legend']).decode().lower()
    require('dashmate' not in units, 'dashmate-unit-requires-review')


def backup_package(root):
    archive = root / 'dashmate-rollback.deb'
    if archive.exists():
        return
    stage = root / 'package-stage'
    stage.mkdir(mode=0o700, exist_ok=True)
    (stage / 'DEBIAN').mkdir(exist_ok=True)
    control = run(['dpkg-query', '-s', 'dashmate']).decode()
    control = '\n'.join(line for line in control.splitlines() if not line.startswith(('Status:', 'Config-Version:'))) + '\n'
    (stage / 'DEBIAN/control').write_text(control)
    for line in run(['dpkg-query', '-L', 'dashmate']).decode().splitlines():
        source = Path(line)
        require(source.is_absolute() and '..' not in source.parts, 'invalid-package-path')
        if str(source) in ['/', '/.'] or not source.exists():
            continue
        dest = stage / str(source).lstrip('/')
        if source.is_symlink():
            dest.parent.mkdir(parents=True, exist_ok=True)
            if not dest.is_symlink():
                dest.symlink_to(os.readlink(source))
        elif source.is_dir():
            dest.mkdir(parents=True, exist_ok=True)
            shutil.copystat(source, dest)
        else:
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, dest)
    run(['dpkg-deb', '--build', '--root-owner-group', '-Zgzip', '-z1', str(stage), str(archive)], timeout=300)
    run(['dpkg-deb', '--info', str(archive)])


def restore_helper(saved):
    """Recreate only the retired helper, via the Docker API (no secret argv)."""
    name = saved['Name'].lstrip('/')
    existing = {c['Name'].lstrip('/'): c for c in containers()}
    if name not in existing:
        class Docker(http.client.HTTPConnection):
            def connect(self):
                self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                self.sock.settimeout(30)
                self.sock.connect('/var/run/docker.sock')
        body = dict(saved['Config'])
        body['Image'] = saved['Image']  # Exact local image ID, never a moved tag.
        body['HostConfig'] = saved['HostConfig']
        body['NetworkingConfig'] = {'EndpointsConfig': {
            name: {k: v for k, v in config.items() if k in ['Aliases', 'Links', 'IPAMConfig'] and v is not None}
            for name, config in saved['NetworkSettings']['Networks'].items()
        }}
        client = Docker('localhost')
        try:
            client.request('POST', '/v1.45/containers/create?name=' + name, json.dumps(body), {'Content-Type': 'application/json'})
            response = client.getresponse()
            response.read()
            require(response.status == 201, 'helper-restore-failed')
        finally:
            client.close()
    else:
        require(existing[name]['Id'] == saved['Id'], 'helper-replaced-externally')
    policy = saved['HostConfig']['RestartPolicy']
    restart = policy['Name']
    if restart == 'on-failure' and policy.get('MaximumRetryCount'):
        restart += ':' + str(policy['MaximumRetryCount'])
    run(['docker', 'update', '--restart=' + restart, name])
    if saved['State']['Running'] or saved['State'].get('Restarting'):
        run(['docker', 'start', name])


def rollback(q, root):
    t, plan = q['target'], q['plan']
    require(root.is_dir() and not root.is_symlink(), 'seed-operation-missing')
    with open(root / 'operation.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        state = json.loads((root / 'operation.json').read_text())
        require(state['planId'] == plan['id'], 'rollback-plan-mismatch')
        b, _ = observe(t, allow_retired=True)
        assert_preserved(state['baseline'], b, t['helper'])
        unit = 'dashnet-' + t['name'] + '-tls'
        run(['systemctl', 'disable', '--now', unit + '.timer'])
        run(['systemctl', 'stop', unit + '.service'])
        for suffix in ['-acme', '-acme-staging']:
            name = 'dashnet-' + t['name'] + suffix
            matches = [c for c in containers() if c['Name'].lstrip('/') == name]
            if matches:
                require(matches[0]['Config']['Image'] == plan['acmeImage'], 'acme-container-ownership-mismatch')
                run(['docker', 'stop', '--time', '20', name])
        config = json.loads((root / 'tls.json').read_text())
        ns = {'__name__': 'seed_renew'}; exec((root / 'renew.py').read_text(), ns)
        with open(root / 'renew.lock', 'a') as renewal_lock:
            fcntl.flock(renewal_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            ns['recover'](root, config)
            ns['write_existing'](config['key'], (root / 'original-key').read_bytes())
            original_cert = (root / 'original-certificate').read_bytes()
            ns['write_existing'](config['certificate'], original_cert)
            ns['reload'](config)
            ns['wait_peer'](config, ns['leaf_der'](original_cert), verify=False)
        if package() != state['baseline']['package']:
            require(not package().startswith('installed '), 'different-dashmate-package-installed')
            run(['dpkg', '--install', str(root / 'dashmate-rollback.deb')], timeout=120)
        saved = json.loads((root / 'containers.private.json').read_text())
        restore_helper(next(c for c in saved if c['Name'].lstrip('/') == t['helper']))
        b, cs = observe(t)
        assert_preserved(state['baseline'], b, t['helper'])
        require(b['package'] == state['baseline']['package'], 'package-restore-mismatch')
        state['phase'] = 'rolled-back'; atomic(root / 'operation.json', state)
        return {'rolledBack': True, 'healthy': False, 'coreHeight': core_ready(cs), 'note': 'Original certificate may be expired; rollback is not recovery.'}


def doctor(root, t):
    require((root / 'operation.json').exists(), 'seed-not-adopted')
    state = json.loads((root / 'operation.json').read_text())
    require(state['phase'] == 'complete', 'seed-adoption-incomplete')
    b, cs = observe(t, allow_retired=True)
    assert_preserved(state['baseline'], b, t['helper'])
    require(t['helper'] not in b['containers'] and not b['package'].startswith('installed '), 'dashmate-not-retired')
    require(shutil.which('dashmate') is None, 'dashmate-binary-remains')
    ns = {'__name__': 'seed_renew'}
    exec((root / 'renew.py').read_text(), ns)
    config = json.loads((root / 'tls.json').read_text())
    require(not (root / 'pending-install.json').exists(), 'certificate-install-incomplete')
    ns['wait_peer'](config, ns['leaf_der'](Path(config['certificate']).read_bytes()))
    run(['systemctl', 'is-active', 'dashnet-' + t['name'] + '-tls.timer'])
    status = json.loads((root / 'tls-status.json').read_text())
    require(status.get('healthy'), 'renewal-last-check-failed')
    checked = datetime.datetime.fromisoformat(status['checkedAt'])
    require((datetime.datetime.now(datetime.timezone.utc) - checked).total_seconds() < 7200, 'renewal-status-stale')
    return {'healthy': True, 'coreHeight': core_ready(cs), 'certificateExpiresAt': status['expiresAt'], 'dashmateRetired': True, 'renewalTimer': True}


def apply(q, root):
    t, plan = q['target'], q['plan']
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    require(not root.is_symlink() and root.stat().st_uid == 0 and root.stat().st_mode & 0o077 == 0, 'unsafe-operation-directory')
    with open(root / 'operation.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        record = root / 'operation.json'
        if record.exists():
            old = json.loads(record.read_text())
            require(old['planId'] == plan['id'], 'another-seed-operation-exists')
            require(old['phase'] == 'complete', 'interrupted-adoption-requires-reconciliation')
            return doctor(root, t)
        require(plan['snapshot']['target'] == t, 'plan-target-mismatch')
        baseline, family = observe(t)
        expected = plan['snapshot']
        require(sha(baseline) == expected['fingerprint'] == sha(expected['baseline']), 'seed-baseline-drift')
        core_ready(family)
        check_retirement()
        resolved = {x[4][0] for x in socket.getaddrinfo(t['hostname'], 443, type=socket.SOCK_STREAM)}
        require(resolved == {t['address']}, 'seed-dns-mismatch')
        with socket.socket() as sock:
            sock.bind(('0.0.0.0', 80))  # Refuse to displace another HTTP service.
        # Everything until the operation record is staging, not service mutation.
        backup_package(root)
        atomic(root / 'containers.private.json', family)
        atomic(root / 'baseline.json', baseline)
        for kind, path in baseline['paths'].items():
            atomic(root / ('original-' + kind), Path(path).read_bytes(), raw=True)
        run(['docker', 'pull', plan['acmeImage']], timeout=300)
        (root / 'acme').mkdir(mode=0o700, exist_ok=True)
        config = dict(t, **baseline['paths'], image=plan['acmeImage'], email=plan['email'])
        atomic(root / 'tls.json', config)
        atomic(root / 'renew.py', q['renewSource'].encode(), raw=True)
        unit = 'dashnet-' + t['name'] + '-tls'
        service = '[Unit]\nDescription=Renew DAPI seed hostname and IP TLS\nAfter=docker.service network-online.target\nRequires=docker.service\n[Service]\nType=oneshot\nUMask=0077\nExecStart=/usr/bin/python3 ' + str(root / 'renew.py') + ' ' + str(root) + '\nTimeoutStartSec=12min\n'
        timer = '[Unit]\nDescription=Verify and renew DAPI seed TLS\n[Timer]\nOnBootSec=5min\nOnUnitActiveSec=1h\nRandomizedDelaySec=5min\n[Install]\nWantedBy=timers.target\n'
        for ext, text in [('service', service), ('timer', timer)]:
            p = Path('/etc/systemd/system') / (unit + '.' + ext)
            require(not p.exists(), 'renewal-unit-already-exists')
            atomic(p, text.encode(), raw=True)
            p.chmod(0o644)
        state = {'planId': plan['id'], 'phase': 'prepared', 'baseline': baseline}
        atomic(record, state)
        helper = next(c for c in family if c['Name'].lstrip('/') == t['helper'])
        try:
            run(['docker', 'update', '--restart=no', t['helper']])
            run(['docker', 'stop', '--time', '20', t['helper']])
            state['phase'] = 'helper-stopped'; atomic(record, state)
            ns = {'__name__': 'seed_renew'}; exec(q['renewSource'], ns)
            ns['issue'](root, config, staging=True)
            state['phase'] = 'staging-issued'; atomic(record, state)
            run(['systemctl', 'daemon-reload'])
            run(['systemctl', 'start', unit + '.service'], timeout=730)
            b, cs = observe(t)
            assert_preserved(baseline, b, t['helper'])
            core_ready(cs)
            state['phase'] = 'tls-verified'; atomic(record, state)
        except Exception:
            # Before retirement, restore original TLS and the helper restart
            # policy. Keep the journal/artifacts; never silently re-apply.
            ns = {'__name__': 'seed_renew'}; exec(q['renewSource'], ns)
            with open(root / 'renew.lock', 'a') as renewal_lock:
                fcntl.flock(renewal_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                ns['recover'](root, config)
                ns['write_existing'](config['key'], (root / 'original-key').read_bytes())
                ns['write_existing'](config['certificate'], (root / 'original-certificate').read_bytes())
                ns['reload'](config)
            policy = helper['HostConfig']['RestartPolicy']['Name']
            run(['docker', 'update', '--restart=' + policy, t['helper']])
            if helper['State']['Running'] or helper['State'].get('Restarting'):
                run(['docker', 'start', t['helper']])
            state['phase'] = 'rolled-back'; atomic(record, state)
            raise
        # No package maintainer scripts, no purge, no volumes removed. Historical
        # rendered files remain mounted; they are data, not a Dashmate dependency.
        check_retirement()
        require(package() == baseline['package'], 'dashmate-package-drift')
        run(['dpkg', '--remove', 'dashmate'], timeout=120)
        run(['docker', 'rm', t['helper']])
        run(['systemctl', 'enable', '--now', unit + '.timer'])
        state['phase'] = 'complete'; atomic(record, state)
        return doctor(root, t)


def main(q):
    os.umask(0o077)
    require(os.geteuid() == 0, 'root-required')
    t = q['target']
    identity(t)
    root = Path('/var/lib/dashnet/dapi-seeds') / t['name']
    if q['action'] == 'inspect':
        baseline, family = observe(t)
        core_ready(family)
        check_retirement()
        return {'baseline': baseline, 'fingerprint': sha(baseline)}
    if q['action'] == 'doctor':
        return {'status': doctor(root, t)}
    if q['action'] == 'rollback':
        return {'status': rollback(q, root)}
    require(q['action'] == 'apply', 'unsupported-operation')
    return {'status': apply(q, root)}


if __name__ == '__main__':
    try:
        print(json.dumps(main(json.load(sys.stdin))))
    except Exception as e:
        print(json.dumps({'error': str(e) if isinstance(e, Failure) else 'seed-operation-failed'}))
        sys.exit(1)
