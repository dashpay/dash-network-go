"""Regression gates for the no-wipe legacy runtime migration (no live hosts)."""
import base64
import copy
import importlib.util
import json
import hashlib
import datetime
import urllib.parse
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('worker', Path(__file__).parents[1] / 'worker.py')
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)


class MigrationTest(unittest.TestCase):
    def test_whitelist_intersections_credentials_and_other_settings(self):
        source = b'# preserve\nrpcauth=secret\nrpcwhitelist=drive_consensus:a,b\nrpcwhitelist=other:keep\n[test]\nrpcwhitelist=drive_consensus:b\nbind=custom\n'
        result = w.Worker.migrate_core(source, 'drive_consensus')
        self.assertEqual(result, b'deprecatedrpc=service\n# preserve\nrpcauth=secret\nrpcwhitelist=drive_consensus:a,b,getspecialtxes,getdeploymentinfo\nrpcwhitelist=other:keep\n[test]\nrpcwhitelist=drive_consensus:b,getspecialtxes,getdeploymentinfo\nbind=custom\n')
        self.assertEqual(result, w.Worker.migrate_core(result, 'drive_consensus'))

    def test_unknown_effective_permissions_fail_closed(self):
        for source in [b'includeconf=other\nrpcwhitelist=drive_consensus:a\n',
                       b'rpcwhitelist=other:a\n',
                       b'[regtest]\nrpcwhitelist=drive_consensus:a\n',
                       b'rpcwhitelist=drive_consensus:a # ambiguous\n']:
            with self.assertRaises(w.Failure):
                w.Worker.migrate_core(source, 'drive_consensus')

    def test_tenderdash_delta_only_removes_obsolete_keys(self):
        source = b'# private keys in other files\n[consensus]\nunsafe-commit-timeout-override = "1s"\nunsafe-bypass-commit-timeout-override = true\nunsafe-vote-timeout-override = "4s"\n[p2p]\nseeds = "keep"\n'
        expected = b'# private keys in other files\n[consensus]\nunsafe-vote-timeout-override = "4s"\n[p2p]\nseeds = "keep"\n'
        self.assertEqual(expected, w.Worker.migrate_tenderdash(source))
        self.assertEqual(expected, w.Worker.migrate_tenderdash(expected))
        with self.assertRaises(w.Failure):
            w.Worker.migrate_tenderdash(b'[p2p]\nunsafe-commit-timeout-override="1s"\n')

    def fixture(self, root):
        core = root / 'dash.conf'
        core.write_bytes(b'rpcwhitelist=drive_consensus:keep\n[test]\nport=19999\n')
        td = root / 'config.toml'
        td.write_bytes(b'[consensus]\nunsafe-commit-timeout-override="1s"\n')
        identity = root / 'node.key'
        identity.write_bytes(b'private-test-only')
        selected = {
            'core': dict(Mounts=[dict(Type='bind', Source=str(core), Destination='/dash/dash.conf')]),
            'tenderdash': dict(Mounts=[dict(Type='bind', Source=str(td), Destination='/td/config.toml'), dict(Type='bind',Source=str(identity),Destination='/td/node.key')]),
            'drive': dict(Config=dict(Env=['CORE_CONSENSUS_JSON_RPC_USERNAME=drive_consensus']),Mounts=[]),
            'dapi': dict(Mounts=[]),
        }
        q = dict(fleet=dict(metadata=dict(name='testnet'),chainType='testnet'),target=dict(containers={k:k for k in selected}),operationId='a'*64,
                 migration=dict(kind='legacy-testnet-v5',epochStart='2026-10-15T00:00:00Z'))
        worker = w.Worker(q, root=root/'journal')
        worker.containers = lambda: (selected,{})
        return worker, selected, core, td, identity

    def test_full_fileset_proof_never_masks_unrelated_file_change(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, core, td, identity = self.fixture(Path(tmp))
            before = worker.files_hash(selected)
            files = worker.migration_files(selected)
            for path, record in files.items():
                Path(path).write_bytes(base64.b64decode(record['after']))
            self.assertNotEqual(before, worker.files_hash(selected))
            self.assertEqual(before, worker.files_hash(selected, files))
            identity.write_bytes(b'changed-identity')
            self.assertNotEqual(before, worker.files_hash(selected, files))
            core.write_bytes(core.read_bytes()+b'rpcpassword=changed\n')
            with self.assertRaisesRegex(w.Failure, 'migration-file-drift'):
                worker.files_hash(selected, files)

    def test_interrupted_file_install_replays_without_losing_backups(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, core, td, identity = self.fixture(Path(tmp))
            files = worker.migration_files(selected)
            before = worker.files_hash(selected)
            calls=[]
            class Engine:
                def inspect(self,name): return dict(Id=name)
                def call(self,*args,**kwargs): calls.append(args)
            worker.engine=Engine()
            worker.migration_window=lambda selected:None
            marker=dict(id='a'*64,migrationFiles=files)
            # Simulate a crash after Core was installed but before TD was written.
            marker['migrationPhase']='writing'
            core.write_bytes(base64.b64decode(files[str(core)]['after']))
            worker.install_migration(marker)
            self.assertEqual('installed',marker['migrationPhase'])
            self.assertEqual(['dapi','tenderdash','drive','core'],[x[1].split('/')[2] for x in calls])
            self.assertEqual(before,worker.files_hash(selected,files))
            self.assertEqual(b'private-test-only',identity.read_bytes())
            self.assertEqual(b'rpcwhitelist=drive_consensus:keep\n[test]\nport=19999\n',base64.b64decode(files[str(core)]['before']))
            count=len(calls)
            worker.install_migration(marker)
            self.assertEqual(count,len(calls))
            self.assertEqual(0o600,(worker.root/'operation.json').stat().st_mode&0o777)

    def test_failed_stop_never_writes_configuration(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, core, td, _ = self.fixture(Path(tmp))
            before=(core.read_bytes(),td.read_bytes())
            class Engine:
                def inspect(self,name): return dict(Id=name)
                def call(self,*args,**kwargs): raise w.Failure('lost-stop-response')
            worker.engine=Engine()
            worker.migration_window=lambda selected:None
            marker=dict(migrationFiles=worker.migration_files(selected))
            with self.assertRaisesRegex(w.Failure,'lost-stop-response'):worker.install_migration(marker)
            self.assertEqual(before,(core.read_bytes(),td.read_bytes()))

    def test_recovery_cleans_only_exact_owned_partial_temporary_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, core, _, _ = self.fixture(Path(tmp))
            files=worker.migration_files(selected)
            marker=dict(id='a'*64,migrationPhase='writing',migrationFiles=files)
            temp=core.parent/('.dashnet-migration-'+marker['id']+'-'+core.name)
            temp.write_bytes(base64.b64decode(files[str(core)]['after'])[:7])
            worker.clean_migration_temp(marker)
            self.assertFalse(temp.exists())
            temp.write_bytes(b'foreign data')
            with self.assertRaisesRegex(w.Failure,'migration-temporary-drift'):worker.clean_migration_temp(marker)
            self.assertEqual(b'foreign data',temp.read_bytes())

    def test_stop_intent_without_actual_withdrawal_rechecks_epoch_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, _, _, _=self.fixture(Path(tmp))
            worker.q['pins']={k:'fixture' for k in selected}
            for c in selected.values():c['State']=dict(Running=True)
            marker=dict(migrationPhase='stopping')
            def refused(selected):raise w.Failure('migration-epoch-window')
            worker.migration_window=refused
            with self.assertRaisesRegex(w.Failure,'migration-epoch-window'):worker.install_migration(marker)

    def test_epoch_and_dkg_admission_boundaries(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, _, _, _=self.fixture(Path(tmp))
            def epoch_ago(seconds):
                worker.q['migration']['epochStart']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(seconds=seconds)).isoformat()
            epoch_ago(600);worker.rpc=lambda *args:13
            worker.migration_window(selected)
            for seconds in [60,3100,-60]:
                epoch_ago(seconds)
                with self.assertRaises(w.Failure):worker.migration_window(selected)
            epoch_ago(600)
            for height in [12,21,23,24]:
                worker.rpc=lambda *args:height
                with self.assertRaisesRegex(w.Failure,'migration-dkg-window'):worker.migration_window(selected)

    def test_core_runtime_identity_and_version_are_not_inferred_from_tag(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, _, _, _=self.fixture(Path(tmp))
            worker.t['role']='validator'
            worker.q.update(operation='upgrade',pins={k:'new-'+k for k in selected},
                            expected=dict(components={k:dict(imageId='old-'+k) for k in selected}))
            for k,c in selected.items():c.update(Id=k,HostConfig={})
            images={v:dict(Id=v) for v in worker.q['pins'].values()}
            class Engine:
                def image(self,pin):return images[pin]
            worker.engine=Engine()
            worker.rpc=lambda *args:dict(subversion='/Dash Core:24.0.0/')
            with self.assertRaisesRegex(w.Failure,'migration-core-image-changed'):worker.validate_migration()
            images['new-core']['Id']='old-core'
            worker.validate_migration()
            worker.rpc=lambda *args:dict(subversion='/Dash Core:23.1.8/')
            with self.assertRaisesRegex(w.Failure,'migration-requires-core24'):worker.validate_migration()

    def test_apply_replays_lost_create_and_start_without_wiping_or_downgrading(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker, selected, core, td, identity = self.fixture(Path(tmp))
            class Engine:
                def __init__(self):
                    self.containers={};self.images={};self.serial=0;self.calls=[];self.fail=None
                def image(self,name):return self.images.get(name)
                def inspect(self,name):
                    return next((copy.deepcopy(c) for c in self.containers.values() if name in [c['Id'],c['Name'].lstrip('/')]),None)
                def call(self,method,path,body=None,**kw):
                    self.calls.append((method,path))
                    if path=='/containers/json?all=1':return [dict(Id=c['Id']) for c in self.containers.values()]
                    if path.startswith('/containers/create?'):
                        name=urllib.parse.parse_qs(urllib.parse.urlparse(path).query)['name'][0]
                        old=next(c for c in originals.values() if c['Name']=='/'+name)
                        self.serial+=1
                        c=copy.deepcopy(old);c['Id']=hashlib.sha256(str(self.serial).encode()).hexdigest()
                        c['Config']={k:v for k,v in body.items() if k not in ['HostConfig','NetworkingConfig']}
                        c['HostConfig']=copy.deepcopy(body['HostConfig'])
                        c['Image']=self.image(c['Config']['Image'])['Id']
                        c['State']=dict(Running=False,StartedAt='new')
                        self.containers[name]=c
                        if self.fail=='create':self.fail=None;raise w.Failure('lost-create-response')
                        return dict(Id=c['Id'])
                    name=urllib.parse.urlparse(path).path.split('/')[2];c=self.inspect(name)
                    if method=='DELETE':
                        assert path.endswith('?v=false'), 'data deletion requested'
                        del self.containers[c['Name'].lstrip('/')]
                        return None
                    c=self.containers[c['Name'].lstrip('/')]
                    if '/stop?' in path:c['State']['Running']=False
                    elif path.endswith('/start'):
                        c['State']['Running']=True;c['State']['StartedAt']='new'
                        if self.fail=='start':self.fail=None;raise w.Failure('lost-start-response')
                    else:raise AssertionError(path)
            engine=Engine()
            for component,c in selected.items():
                for m in c['Mounts']:m.update(RW=False,Mode='ro')
                # Realized database volume and its name must survive every create.
                c['Mounts'].append(dict(Type='volume',Name=component+'-existing-data',Destination='/data',RW=True,Mode=''))
                c.update(Id=hashlib.sha256(component.encode()).hexdigest(),Name='/'+component,Image='old-'+component,
                         State=dict(Running=True,StartedAt='old'),RestartCount=0,HostConfig=dict(NetworkMode='host'),NetworkSettings=dict(Networks={}))
                c.setdefault('Config',{}).update(Image='old-'+component)
                for generation in ['old','new']:
                    pin=generation+'-'+component
                    engine.images[pin]=dict(Id=pin,Architecture='amd64',Os='linux',RepoDigests=[pin])
                engine.containers[component]=copy.deepcopy(c)
            originals=copy.deepcopy(engine.containers)
            worker.engine=engine
            del worker.containers  # exercise the actual container scanner
            worker.health=lambda selected:({},[])
            worker.native_processes=lambda:{}
            worker.validate_migration=lambda:None  # artifact/version guards tested separately
            worker.migration_window=lambda selected:None
            events=[]
            def core_ready():
                self.assertTrue(engine.inspect('core')['State']['Running'])
                self.assertEqual('old-core',engine.inspect('core')['Image'])
                self.assertFalse(engine.inspect('drive')['State']['Running'])
                events.append('core-ready')
            worker.wait_migrated_core=core_ready
            worker.wait_abci=lambda: self.assertTrue(engine.inspect('drive')['State']['Running'])
            worker.q.update(fleetId='fixture',operation='upgrade',pins=dict(core='old-core',drive='new-drive',tenderdash='new-tenderdash',dapi='new-dapi'))
            worker.t.update(instanceId='i-00000001',architecture='amd64')
            before=worker.observe();worker.q['expected']=before
            worker.enroll()
            engine.fail='create'
            with self.assertRaisesRegex(w.Failure,'lost-create-response'):worker.apply()
            core_id=engine.inspect('core')['Id']
            engine.fail='start'
            with self.assertRaisesRegex(w.Failure,'lost-start-response'):worker.apply()
            result=worker.apply()
            self.assertEqual(core_id,engine.inspect('core')['Id'])
            self.assertEqual('complete',worker.read('operation.json')['phase'])
            self.assertEqual(before['filesHash'],result['migration']['before'])
            self.assertNotEqual(before['filesHash'],result['filesHash'])
            self.assertEqual(['core-ready'],events)
            for component,c in engine.containers.items():
                self.assertEqual(originals[component]['Mounts'],c['Mounts'])
                self.assertEqual(originals[component]['Config'].get('Env'),c['Config'].get('Env'))
                self.assertTrue(c['State']['Running'])
            self.assertEqual(b'private-test-only',identity.read_bytes())
            count=len(engine.calls)
            worker.apply()
            self.assertFalse(any(method!='GET' for method,_ in engine.calls[count:]), 'completed replay mutated services')
            self.assertTrue((worker.root/('migration-'+worker.q['operationId']+'.json')).exists())


if __name__ == '__main__': unittest.main()
