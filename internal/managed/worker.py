"""Existing-workload adapter. No shell, Dashmate, reset, volume removal or key export.

Docker's effective container configuration stays on its owning host. Enrollment
adds private recovery records only. Image replacements preserve exact mounts,
network attachments, command/env and published ports; there is no inferred rollback.
"""
import copy
import datetime
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request


class Failure(Exception):
    pass


def require(value, code):
    if not value:
        raise Failure(code)


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect('/var/run/docker.sock')


class Engine:
    def __init__(self):
        self.version = ''
        version = self.call('GET', '/version')['ApiVersion']
        require(re.fullmatch(r'1\.[0-9]{2,3}', version), 'docker-api-version')
        self.version = '/v' + version

    def call(self, method, path, body=None, timeout=120, missing=False):
        conn = UnixHTTP('localhost', timeout=timeout)
        try:
            conn.request(method, self.version + path, body=None if body is None else json.dumps(body),
                         headers={'Content-Type': 'application/json'})
            response = conn.getresponse()
            raw = response.read(16 * 1024 * 1024 + 1)
            require(len(raw) <= 16 * 1024 * 1024, 'docker-response-size')
            if missing and response.status == 404:
                return None
            require(response.status in [200, 201, 204, 304], 'docker-api-' + str(response.status))
            return json.loads(raw) if raw else None
        finally:
            conn.close()

    def inspect(self, name):
        return self.call('GET', '/containers/' + urllib.parse.quote(name, safe='') + '/json', missing=True)

    def image(self, name):
        return self.call('GET', '/images/' + urllib.parse.quote(name, safe='') + '/json', missing=True)


def run(args, stdin=None, timeout=60):
    p = subprocess.run(args, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=False)
    require(p.returncode == 0, 'command-failed')
    require(len(p.stdout) <= 2 * 1024 * 1024, 'command-output-size')
    return p.stdout


def protobuf(raw):
    fields, pos = {}, 0
    def varint():
        nonlocal pos
        value = 0
        for shift in range(0, 70, 7):
            require(pos < len(raw), 'protobuf-truncated')
            b = raw[pos]; pos += 1; value |= (b & 127) << shift
            if b < 128: return value
        raise Failure('protobuf-varint')
    while pos < len(raw):
        tag = varint(); number, wire = tag >> 3, tag & 7
        require(number and number not in fields, 'protobuf-field')
        if wire == 0: value = varint()
        elif wire in [1, 2, 5]:
            size = varint() if wire == 2 else (8 if wire == 1 else 4)
            require(size <= len(raw) - pos, 'protobuf-size')
            value = raw[pos:pos+size]; pos += size
        else: raise Failure('protobuf-wire')
        fields[number] = value
    return fields


class Worker:
    def __init__(self, q, engine=None, root=None):
        self.q, self.f, self.t = q, q['fleet'], q['target']
        self.engine = engine
        self.root = root or Path('/var/lib/dashnet-managed') / self.f['metadata']['name']
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def identity(self):
        request = urllib.request.Request('http://169.254.169.254/latest/api/token', method='PUT',
                   headers={'X-aws-ec2-metadata-token-ttl-seconds': '60'})
        with self.opener.open(request, timeout=5) as response:
            token = response.read(4096).decode()
        request = urllib.request.Request('http://169.254.169.254/latest/dynamic/instance-identity/document',
                   headers={'X-aws-ec2-metadata-token': token})
        with self.opener.open(request, timeout=5) as response:
            value = json.loads(response.read(65536))
        require(value['instanceId'] == self.t['instanceId'] and value['accountId'] == self.f['accountId']
                and value['region'] == self.f['region'], 'instance-identity')

    def atomic(self, name, value):
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        require(not self.root.is_symlink() and not (self.root/name).is_symlink(), 'state-symlink')
        fd, temp = tempfile.mkstemp(prefix='.pending-', dir=self.root)
        try:
            with os.fdopen(fd, 'w') as stream:
                json.dump(value, stream, sort_keys=True); stream.flush(); os.fsync(stream.fileno())
            os.replace(temp, self.root/name)
            d = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
            try: os.fsync(d)
            finally: os.close(d)
        finally:
            if os.path.exists(temp): os.unlink(temp)

    def read(self, name):
        p = self.root/name
        require(not p.is_symlink(), 'state-symlink')
        return json.loads(p.read_text()) if p.exists() else None

    def recipe(self, c):
        config, host = copy.deepcopy(c['Config']), copy.deepcopy(c['HostConfig'])
        # All realized volumes—including anonymous image volumes—become exact
        # existing sources. Docker must not silently allocate fresh state.
        require(not host.get('AutoRemove') and not host.get('VolumesFrom') and not host.get('Links'), 'unsupported-container-lifecycle')
        require(not host.get('NetworkMode', '').startswith(('container:', 'service:')), 'unsupported-network-mode')
        mounts = []
        for m in c['Mounts']:
            if m['Type'] == 'tmpfs': continue
            require(m['Type'] in ['bind', 'volume'], 'unsupported-mount-type')
            value = dict(Type=m['Type'], Source=m.get('Name') if m['Type']=='volume' else m['Source'],
                         Target=m['Destination'], ReadOnly=not m['RW'])
            if m['Type']=='bind': value['BindOptions']=dict(Propagation=m.get('Propagation') or 'rprivate')
            else: value['VolumeOptions']=dict(NoCopy=True)
            mounts.append(value)
        host['Binds'] = None; host['Mounts'] = sorted(mounts, key=lambda m: m['Target'])
        # Endpoint runtime IDs/IPs are not configuration. Preserve configured
        # IPAM, stable aliases and driver options, not the old container ID alias.
        endpoints = {}
        for name, n in c['NetworkSettings'].get('Networks', {}).items():
            aliases = [a for a in (n.get('Aliases') or []) if a not in [c['Id'], c['Id'][:12]]]
            endpoints[name] = dict(IPAMConfig=n.get('IPAMConfig'), Aliases=aliases,
                                   DriverOpts=n.get('DriverOpts'))
        return dict(Config=config, HostConfig=host, NetworkingConfig=dict(EndpointsConfig=endpoints))

    def config_hash(self, recipe):
        value = copy.deepcopy(recipe); value['Config'].pop('Image', None)
        # Docker expands a few null default fields when creating from an API
        # spec. Normalize only representational defaults, never env/args/mounts.
        def normalize(v):
            if isinstance(v,dict):return {k:normalize(x) for k,x in v.items() if x is not None and x != [] and x != {}}
            if isinstance(v,list):return [normalize(x) for x in v]
            return v
        return fingerprint(normalize(value))

    def containers(self):
        allc = self.engine.call('GET','/containers/json?all=1')
        selected, companions = {}, {}
        expected = self.t['containers']; reverse = {v:k for k,v in expected.items()}
        for item in allc:
            c = self.engine.inspect(item['Id']); name = c['Name'].lstrip('/')
            if name in reverse: selected[reverse[name]] = c
            else: companions[name] = c
        require(set(selected) == set(expected), 'missing-selected-container')
        return selected, companions

    def files_hash(self, selected):
        files = {}
        for c in selected.values():
            for m in c['Mounts']:
                if m['Type']!='bind':continue
                target=m['Destination']; source=Path(m['Source'])
                # Persistent databases/logs and validator signing *state* are
                # mutable. Hash only identity/configuration mounts.
                if not (target.endswith(('.conf','.toml','.yaml','.yml','.crt','.key')) or target.endswith('/config')):continue
                paths=sorted(source.rglob('*')) if source.is_dir() else [source]
                for p in paths:
                    if p.is_dir():continue
                    require(not p.is_symlink() and p.is_file() and p.stat().st_size<=2*1024*1024,'unsupported-config-file')
                    if p.name.endswith('.lock') or p.name=='priv_validator_state.json':continue
                    files[str(p)]=hashlib.sha256(p.read_bytes()).hexdigest()
        require(len(files)<=200,'configuration-file-count')
        return fingerprint(files)

    def summary(self, c):
        image=self.engine.image(c['Image']);require(image is not None,'missing-image')
        require(image['Architecture']==self.t['architecture'] and image['Os']=='linux','image-architecture')
        return dict(id=c['Id'],imageId=c['Image'],image=c['Config']['Image'],digests=image.get('RepoDigests',[]),
                    configHash=self.config_hash(self.recipe(c)),running=c['State']['Running'] and not c['State'].get('Restarting'),
                    startedAt=c['State']['StartedAt'],restarts=c['RestartCount'])

    def rpc(self, selected, method, *args):
        core=selected['core']
        paths=[m['Destination'] for m in core['Mounts'] if m['Destination'].endswith('/dash.conf')]
        require(len(paths)==1,'core-config-mount')
        raw=run(['docker','exec',core['Id'],'dash-cli','-conf='+paths[0],method,*[str(x) for x in args]])
        try:return json.loads(raw)
        except json.JSONDecodeError:return raw.decode().strip()

    def http(self, port, method):
        with self.opener.open('http://127.0.0.1:'+str(port)+'/'+method,timeout=15) as response:
            raw=response.read(1024*1024+1)
        require(len(raw)<=1024*1024,'tenderdash-response-size')
        value=json.loads(raw);require(isinstance(value,dict) and value.get('error') is None,'tenderdash-rpc')
        return value.get('result',value)

    def port(self,c,container_port):
        bindings=c['HostConfig'].get('PortBindings') or {}
        values=bindings.get(str(container_port)+'/tcp') or []
        require(len(values)==1 and values[0].get('HostIp','') in ['', '0.0.0.0','127.0.0.1'], 'unsupported-port-binding')
        return int(values[0]['HostPort'])

    def dapi(self, selected):
        gateway=selected['gateway'];port=self.port(gateway,10000)
        cert=[m['Source'] for m in gateway['Mounts'] if m['Destination'].endswith('/bundle.crt')]
        require(len(cert)==1,'gateway-trust-mount')
        with tempfile.TemporaryDirectory(prefix='dashnet-probe-') as d:
            headers=Path(d)/'headers'
            raw=run(['curl','--silent','--show-error','--fail','--noproxy','*','--max-time','20',
                     '--http2','--cacert',cert[0],'--connect-to',self.t['address']+':'+str(port)+':127.0.0.1:'+str(port),
                     '-D',str(headers),'-H','content-type: application/grpc','-H','te: trailers',
                     '--data-binary','@-','https://'+self.t['address']+':'+str(port)+'/org.dash.platform.dapi.v0.Platform/getStatus'],
                     stdin=b'\x00\x00\x00\x00\x02\x0a\x00',timeout=25)
            require('grpc-status: 0' in headers.read_text().lower().splitlines(),'dapi-grpc')
        require(len(raw)>=5 and raw[0]==0 and int.from_bytes(raw[1:5],'big')==len(raw)-5,'dapi-frame')
        value=protobuf(protobuf(raw[5:])[1]);software=protobuf(protobuf(value[1])[1]);chain=protobuf(value[3]);network=protobuf(value[4]);identity=protobuf(value[2])
        require(software.get(2) and software.get(3) and not chain.get(1,0),'dapi-upstream')
        return dict(height=chain.get(4,0),chain=network[1].decode(),node=identity[1].hex(),protx=identity[2].hex())

    def health(self, selected):
        chain={};problems=[]
        if 'core' in selected:
            try:
                info=self.rpc(selected,'getblockchaininfo');sync=self.rpc(selected,'mnsync','status')
                chain.update(coreNetwork=info['chain'],coreGenesis=self.rpc(selected,'getblockhash',1 if self.f['chainType']=='devnet' else 0),
                             coreHeight=info['blocks'],coreSynced=not info['initialblockdownload'] and sync['IsSynced'] and sync['IsBlockchainSynced'],
                             chainLockHeight=self.rpc(selected,'getbestchainlock')['height'])
                require(info['chain']==self.f['coreNetwork'],'wrong-core-chain')
                if self.t['role']=='validator':
                    mn=self.rpc(selected,'masternode','status');chain.update(masternodeState=mn.get('state',''),proTxHash=mn.get('proTxHash',''))
            except Exception:problems.append('core-health-unavailable')
        if self.t['role']=='validator':
            try:
                port=self.port(selected['tenderdash'],36657);status=self.http(port,'status');members=self.http(port,'validators')
                require(len(members['validators'])==int(members['total']),'incomplete-validator-membership')
                powers={v['pro_tx_hash'].lower():int(v['voting_power']) for v in members['validators']}
                require(len(powers)==len(members['validators']) and all(v>0 for v in powers.values()),'validator-membership')
                reference=self.q.get('referenceHeight',0)
                block=self.http(port,'block?height='+str(reference))['block_id']['hash'].lower() if reference else ''
                dapi=self.dapi(selected)
                require(dapi['chain']==status['node_info']['network'] and dapi['protx']==chain.get('proTxHash',''),'dapi-identity')
                chain.update(platformChainId=dapi['chain'],platformHeight=int(status['sync_info']['latest_block_height']),
                    platformHash=block,platformProtocol=int(status['node_info']['protocol_version']['app']),platformNodeId=dapi['node'],
                    catchingUp=status['sync_info']['catching_up'],votingPower=powers,dapiHeight=dapi['height'],dapiHealthy=True)
            except Exception:problems.append('platform-health-unavailable')
        return chain,problems

    def observe(self):
        selected,companions=self.containers()
        components={k:self.summary(v) for k,v in selected.items()}
        # Unknown companions are preserved, even if they are not Dash services.
        extra={k:dict(id=v['Id'],imageId=v['Image'],image=v['Config']['Image'],digests=[],configHash=self.config_hash(self.recipe(v)),
                     running=v['State']['Running'],startedAt=v['State']['StartedAt'],restarts=v['RestartCount']) for k,v in companions.items()}
        chain,problems=self.health(selected)
        for k,c in selected.items():require(self.engine.inspect(c['Id']) is not None,'concurrent-container-change')
        return dict(instanceId=self.t['instanceId'],at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    components=components,companions=extra,filesHash=self.files_hash(selected),chain=chain,problems=problems)

    def assert_baseline(self, actual, baseline, mutable=()):
        require(actual['filesHash']==baseline['filesHash'],'configuration-files-changed')
        require(actual['companions']==baseline['companions'],'companion-changed')
        for k,b in baseline['components'].items():
            a=actual['components'][k]
            require(a['configHash']==b['configHash'],'workload-configuration-changed')
            if k not in mutable:require(a==b,'preserved-service-changed')

    def owner(self):
        owner=self.read('owner.json')
        require(owner==dict(fleetId=self.q['fleetId'],instanceId=self.t['instanceId']),'not-enrolled')

    def enroll(self):
        actual=self.observe();self.assert_baseline(actual,self.q['expected'])
        selected,_=self.containers()
        value=self.read('owner.json')
        require(value is None or value==dict(fleetId=self.q['fleetId'],instanceId=self.t['instanceId']),'different-owner')
        if value is None:
            self.atomic('workloads.json',{k:self.recipe(c) for k,c in selected.items()})
            self.atomic('owner.json',dict(fleetId=self.q['fleetId'],instanceId=self.t['instanceId']))
        return actual

    def stage(self):
        self.owner()
        for pin in self.q['pins'].values():
            require(re.fullmatch(r'[a-zA-Z0-9./:_-]+@sha256:[0-9a-f]{64}',pin),'image-pin')
            image=self.engine.image(pin)
            if image is None:
                run(['docker','pull','--platform','linux/'+self.t['architecture'],pin],timeout=600)
                image=self.engine.image(pin)
            require(image and image['Architecture']==self.t['architecture'] and image['Os']=='linux'
                    and any(d.split('@')[-1]==pin.split('@')[-1] for d in image.get('RepoDigests',[])),'image-proof')
        return dict(instanceId=self.t['instanceId'])

    def operation_guard(self, marker):
        # Recheck unaffected workloads and files even when the selected container
        # is absent between remove/create. Missing selected mounts are reconstructed
        # from the saved private recipe, never from user-provided paths.
        baseline=marker['baseline'];pins=marker['pins'];selected={};companions={}
        reverse={v:k for k,v in self.t['containers'].items()}
        for item in self.engine.call('GET','/containers/json?all=1'):
            c=self.engine.inspect(item['Id']);name=c['Name'].lstrip('/')
            if name in reverse:selected[reverse[name]]=c
            else:
                companions[name]=dict(id=c['Id'],imageId=c['Image'],image=c['Config']['Image'],digests=[],
                    configHash=self.config_hash(self.recipe(c)),running=c['State']['Running'],
                    startedAt=c['State']['StartedAt'],restarts=c['RestartCount'])
        require(companions==baseline['companions'],'companion-changed')
        file_sources={}
        for k,b in baseline['components'].items():
            c=selected.get(k)
            if c is None:
                require(k in pins and marker['phase']=='applying','missing-preserved-container')
                mounts=[]
                for m in marker['recipes'][k]['HostConfig']['Mounts']:
                    if m['Type']=='bind':mounts.append(dict(Type='bind',Source=m['Source'],Destination=m['Target']))
                file_sources[k]=dict(Mounts=mounts)
                continue
            a=self.summary(c)
            require(a['configHash']==b['configHash'],'workload-configuration-changed')
            if k not in pins:require(a==b,'preserved-service-changed')
            elif k in marker['completed']:require(c['Id']==marker['completed'][k],'applied-container-replaced')
            else:
                desired=self.engine.image(pins[k]);require(desired is not None,'image-not-staged')
                require((c['Id']==b['id'] and c['Image']==b['imageId'])
                        or c['Image']==desired['Id'],'unexpected-selected-container')
            file_sources[k]=c
        require(self.files_hash(file_sources)==baseline['filesHash'],'configuration-files-changed')

    def apply(self):
        self.owner();pins=self.q['pins'];baseline=self.q['expected']
        marker=self.read('operation.json');same=marker and marker['id']==self.q['operationId']
        if same:
            require(marker['pins']==pins and marker['baseline']==baseline and marker['operation']==self.q['operation'],'operation-marker-drift')
        else:
            require(marker is None or marker['phase']=='complete','unfinished-host-operation')
            actual=self.observe();self.assert_baseline(actual,baseline, pins if self.q['operation']=='deploy' else ())
            selected,_=self.containers();recipes={k:self.recipe(c) for k,c in selected.items() if k in pins}
            marker=dict(id=self.q['operationId'],pins=pins,baseline=baseline,recipes=recipes,
                        operation=self.q['operation'],completed={},phase='applying')
            self.atomic('operation.json',marker)
        self.operation_guard(marker)
        # Recover in service dependency order. Companion containers are untouched.
        for k in ['core','drive','tenderdash','dapi','gateway','helper']:
            if k not in pins:continue
            self.operation_guard(marker)
            name=self.t['containers'][k];c=self.engine.inspect(name);desired=self.engine.image(pins[k])
            require(desired is not None,'image-not-staged')
            recipe=copy.deepcopy(marker['recipes'][k]);recipe['Config']['Image']=pins[k]
            if c is not None:
                require(self.config_hash(self.recipe(c))==baseline['components'][k]['configHash'],'service-config-drift')
                require(c['Image'] in [baseline['components'][k]['imageId'],desired['Id']],'unexpected-running-image')
            if self.q['operation']=='deploy':
                require(desired['Id']==baseline['components'][k]['imageId'],'deploy-cannot-change-image')
            if c is not None and c['Image']!=desired['Id']:
                require(c['Id']==baseline['components'][k]['id'],'unexpected-container-identity')
                self.engine.call('POST','/containers/'+c['Id']+'/stop?t=120',timeout=150)
                # No v=true: existing named AND anonymous volumes are retained.
                self.engine.call('DELETE','/containers/'+c['Id']+'?v=false')
                c=None
            if c is None:
                body=dict(recipe['Config'],HostConfig=recipe['HostConfig'],NetworkingConfig=recipe['NetworkingConfig'])
                self.engine.call('POST','/containers/create?name='+urllib.parse.quote(name,safe=''),body)
                c=self.engine.inspect(name)
            require(c and c['Image']==desired['Id'],'created-image-mismatch')
            if not c['State']['Running']:self.engine.call('POST','/containers/'+c['Id']+'/start')
            c=self.engine.inspect(name)
            require(c['State']['Running'] and self.config_hash(self.recipe(c))==baseline['components'][k]['configHash'],'replacement-not-preserved')
            marker['completed'][k]=c['Id'];self.atomic('operation.json',marker)
        actual=self.observe();self.assert_baseline(actual,baseline,pins)
        marker['phase']='complete';self.atomic('operation.json',marker)
        return actual

    def execute(self):
        require(self.q['action'] in ['observe','enroll','stage','apply'],'action-refused')
        self.identity()
        if self.engine is None:self.engine=Engine()
        if self.q['action']=='observe':return self.observe()
        self.root.mkdir(mode=0o700,parents=True,exist_ok=True)
        with open('/run/dashnet-managed.lock','a') as lock:
            try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise Failure('host-busy') from None
            return getattr(self,self.q['action'])()


def main():
    try:
        raw=sys.stdin.buffer.read(2*1024*1024+1);require(len(raw)<=2*1024*1024,'input-size')
        print(json.dumps(Worker(json.loads(raw)).execute(),separators=(',',':')))
    except Exception as e:
        code=str(e) if isinstance(e,Failure) else type(e).__name__.lower()
        code=re.sub('[^a-z0-9:_-]','',code)[:100]
        print(json.dumps(dict(error=code)));sys.exit(1)


if __name__=='__main__':main()
