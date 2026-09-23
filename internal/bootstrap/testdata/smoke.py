"""Exercise the actual recipe in a disposable Ubuntu filesystem.

EC2 metadata, packages, systemd and Docker are fixtures, NOT a real deployment.
Never run on an operator host: this intentionally replaces container executables.
"""
import fcntl
import json
import os
from pathlib import Path
import subprocess

assert Path('/run/dashnet-recipe-test-container').is_file(), 'container only'
assert Path('/test/recipe.sh').is_file(), 'container only'
state_path = Path('/run/dashnet-recipe-fixture.json')
state = dict(installed=False, daemon=False, images=[], pulls=0, installs=0,
             instance='i-00000001', cloud='done', containers=False, bad_arch=False)
state_path.write_text(json.dumps(state))
mock = '''#!/usr/bin/python3
import json,sys,os
from pathlib import Path
p=Path('/run/dashnet-recipe-fixture.json')
s=json.loads(p.read_text()); args=sys.argv[1:]; tool=Path(sys.argv[0]).name
def save(): p.write_text(json.dumps(s))
def fail(): sys.exit(1)
if tool=='curl':
    print('fixture-token' if args[-1].endswith('/api/token') else s['instance'])
elif tool=='cloud-init': print(json.dumps(dict(status=s['cloud'])))
elif tool=='apt-get':
    if 'install' in args: s['installed']=True; s['installs']+=1; save()
elif tool=='systemctl': s['daemon']=True; save()
elif tool=='docker':
    args=args[4:] # --host SOCKET --config DIR
    if not s['installed']: fail()
    if args[:2]==['compose','version']: print('2.37.1'); sys.exit(0)
    if not s['daemon']: fail()
    if args[0]=='info': print('{}')
    elif args[0]=='ps': print('old-container' if s['containers'] else '')
    elif args[0]=='version': print('28.2.2')
    elif args[0]=='pull':
        s['images'].append(args[-1]); s['pulls']+=1; save()
    elif args[:2]==['image','inspect']:
        if args[-1] not in s['images']: fail()
        digest=args[-1].removeprefix('index.docker.io/')
        print(json.dumps([dict(Os='linux',Architecture='arm64' if s['bad_arch'] else 'amd64',RepoDigests=[digest])]))
    else: fail()
else: fail()
'''
for tool in ['curl', 'cloud-init', 'apt-get', 'systemctl', 'docker']:
    path = Path('/usr/bin') / tool
    path.write_text(mock)
    path.chmod(0o755)

plan = 'a' * 64
owner = 'b' * 64 + ':i-00000001'
image = 'index.docker.io/dashpay/dashd@sha256:' + 'c' * 64

def update(**values):
    state = json.loads(state_path.read_text())
    state.update(values)
    state_path.write_text(json.dumps(state))

def run(mode, expected_error=None, plan_id=plan):
    result = subprocess.run(['/bin/bash', '/test/recipe.sh', mode, owner,
                             plan_id, 'i-00000001', 'amd64', image],
                            capture_output=True, text=True, timeout=10)
    data = json.loads(result.stdout)
    if expected_error:
        assert result.returncode != 0, result
        assert data == dict(error=expected_error), (data, result.stderr)
    else:
        assert result.returncode == 0, (data, result.stderr)
        assert data['planId'] == plan_id and data['instanceId'] == 'i-00000001'
    return data

# Identity and readiness checks must fail before creating any host state.
update(instance='i-ffffffff')
run('apply', 'instance-identity')
assert not Path('/var/lib/dashnet').exists()
update(instance='i-00000001', cloud='running')
run('apply', 'cloud-init')
assert not Path('/var/lib/dashnet').exists()
update(cloud='done')
assert not run('probe')['ready']
assert not Path('/var/lib/dashnet').exists()
assert run('apply')['ready']
assert Path('/var/lib/dashnet/data').stat().st_mode & 0o777 == 0o700
assert Path('/var/lib/dashnet/owner').read_text().strip() == owner
before = json.loads(state_path.read_text())
assert before['pulls'] == 1 and before['installs'] == 1
assert run('probe')['ready']
assert json.loads(state_path.read_text()) == before, 'probe mutated runtime'
run('apply', 'host-ownership', plan_id='d' * 64)
assert json.loads(state_path.read_text()) == before
# A disconnected process retains its flock until it exits. Probe/apply both stop.
with open('/run/dashnet-bootstrap.lock', 'r') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    run('probe', 'host-lock')
    run('apply', 'host-lock')
update(containers=True)
run('apply', 'existing-containers')
assert json.loads(state_path.read_text())['pulls'] == 1
update(containers=False, bad_arch=True)
assert not run('probe')['ready'], 'ready marker hid wrong image architecture'
run('apply', 'runtime-verify')
update(bad_arch=False, images=[])
assert not run('probe')['ready'], 'ready marker hid missing image'
# Resume works with a prebaked runtime; it does not reinstall Docker.
assert run('apply')['ready']
assert json.loads(state_path.read_text())['installs'] == 1
print('recipe smoke: identity, preflight, installation, pins, locks and resume passed')
