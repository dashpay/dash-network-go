import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from test_worker import worker, request, observer_scope

scope = observer_scope.copy()
exec((Path(__file__).parents[1] / 'upgrade.py').read_text(), scope)
Upgrade = scope['UpgradeWorker']


class Fixture(Upgrade):
    def __init__(self, root, component='tenderdash'):
        q = request()
        q['target'].update(role='validator', name='validator-1')
        before = {k: 'docker.io/example/' + k + '@sha256:' + 'd' * 64
                  for k in ['core', 'drive', 'dapi', 'gateway', 'tenderdash', 'helper']}
        after = before.copy()
        after[component] = before[component].replace('d' * 64, 'e' * 64)
        q['target']['images'] = [dict(component=k, pinned=v) for k,v in before.items()]
        self.core = dict(containerId='a'*64, startedAt='2026-09-24T00:00:00Z',
                         configSha256='b'*64, genesis='c'*64)
        self.containers = {k: dict(Id=hashlib.sha256(k.encode()).hexdigest(),
                                  Image='image-'+before[k], State=dict(Running=True),
                                  RestartCount=0) for k in ['drive','tenderdash','dapi','gateway']}
        preserve = dict(coreId=self.core['containerId'],coreStarted=self.core['startedAt'],
                        coreConfig=self.core['configSha256'],coreGenesis=self.core['genesis'],
                        containers={k:v['Id'] for k,v in self.containers.items()},
                        restarts={k:0 for k in self.containers})
        q.update(action='upgrade-apply',upgrade=dict(id='f'*64,previousId='',
                 **{'from':before},to=after,preserve=preserve))
        super().__init__(q,root,root/'lock')
        self.commands=[]
        self.lost=False
        self.partial=False
        self.accessed=False
        self.atomic('deployment.json',dict(planId=self.c['planId']))
        services={k:self.service(k,before[k]) for k in self.containers}
        for v in services.values():
            v['environment']={'RPC_PASSWORD':'private-fixture-value'}
            v['volumes']=['/persistent/never-delete:/db']
        self.original=dict(name=self.project,services=services)
        self.atomic('platform/compose.json',self.original)
        self.atomic('platform/data-sentinel','keep-me')

    def verify_instance(self):
        self.accessed=True

    def owned(self):
        pass

    def core_status(self):
        return self.core.copy()

    def inspect_container(self, service):
        return self.containers.get(service)

    def docker(self,*args,timeout=120):
        self.commands.append(args)
        if args[:2]==('image','inspect'):
            pin=args[2]
            return json.dumps([dict(Id='image-'+pin,Architecture='amd64',Os='linux',RepoDigests=[pin])]).encode()
        if args[0]=='compose' and 'up' in args:
            assert self.read('upgrade.json')['phase'] in ['applying','applied']
            changed=args[args.index('never')+1:]
            desired=self.read('platform/compose.json')['services']
            if self.partial:
                self.partial=False
                del self.containers[changed[0]]
                raise worker.Failure('replacement-create-failed')
            for k in changed:
                image='image-'+desired[k]['image']
                if k not in self.containers:
                    self.containers[k]=dict(Image='',State=dict(Running=True),RestartCount=0)
                if self.containers[k]['Image']!=image:
                    self.containers[k]['Image']=image
                    self.containers[k]['Id']=hashlib.sha256(image.encode()).hexdigest()
            if self.lost:
                self.lost=False
                raise worker.Failure('lost-apply-response')
        return b''


class UpgradeTests(unittest.TestCase):
    def test_real_document_changes_images_only_and_preserves_unselected_services(self):
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp))
            old=copy.deepcopy(w.containers)
            w.q['action']='upgrade-stage'
            w.execute()
            self.assertEqual(w.read('platform/compose.json'),w.original)
            self.assertIsNone(w.read('upgrade.json'))
            self.assertFalse(any('up' in c for c in w.commands))
            w.q['action']='upgrade-apply'
            result=w.execute()
            current=w.read('platform/compose.json')
            current['services']['tenderdash']['image']=w.original['services']['tenderdash']['image']
            self.assertEqual(current,w.original)
            self.assertEqual((w.root/'platform/data-sentinel').read_text(),'keep-me')
            for k in ['drive','dapi','gateway']:
                self.assertEqual(w.containers[k],old[k])
            self.assertNotEqual(w.containers['tenderdash']['Id'],old['tenderdash']['Id'])
            self.assertEqual(w.read('upgrade.json')['phase'],'applied')
            self.assertNotIn('private-fixture-value',json.dumps(result))
            self.assertNotIn('private-fixture-value',(w.root/'upgrade.json').read_text())
            self.assertFalse(any(c[0] in ['stop','rm','restart'] for c in w.commands))

    def test_lost_response_and_configuration_drift(self):
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp));w.lost=True
            with self.assertRaisesRegex(worker.Failure,'lost-apply-response'):w.execute()
            self.assertEqual(w.read('upgrade.json')['phase'],'applying')
            ids={k:v['Id'] for k,v in w.containers.items()}
            w.execute()
            self.assertEqual(ids,{k:v['Id'] for k,v in w.containers.items()})
            current=w.read('platform/compose.json')
            current['services']['drive']['environment']['RPC_PASSWORD']='different'
            w.atomic('platform/compose.json',current)
            w.commands=[]
            with self.assertRaisesRegex(worker.Failure,'upgrade-config-changed'):w.execute()
            self.assertFalse(any('up' in c for c in w.commands))

    def test_core_restart_foreign_marker_and_missing_deployment_fail_before_apply(self):
        for case in ['restart','foreign','missing','core']:
            with self.subTest(case=case),tempfile.TemporaryDirectory() as tmp:
                w=Fixture(Path(tmp))
                if case=='restart':w.core['startedAt']='2026-09-24T01:00:00Z'
                elif case=='foreign':w.atomic('upgrade.json',dict(id='a'*64,phase='applied'))
                elif case=='missing':(w.root/'deployment.json').unlink()
                else:w.q['upgrade']['to']['core']=w.q['upgrade']['to']['tenderdash']
                with self.assertRaises(worker.Failure):w.execute()
                self.assertFalse(any('up' in c for c in w.commands))
                self.assertEqual(w.read('platform/compose.json'),w.original)

    def test_missing_container_recovery_requires_exact_inflight_selected_change(self):
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp));w.partial=True
            with self.assertRaisesRegex(worker.Failure,'replacement-create-failed'):w.execute()
            self.assertNotIn('tenderdash',w.containers)
            self.assertEqual(w.read('upgrade.json')['phase'],'applying')
            w.q['action']='upgrade-stage';w.execute()
            self.assertNotIn('tenderdash',w.containers)
            w.q['action']='upgrade-apply';w.execute()
            self.assertEqual(w.read('upgrade.json')['phase'],'applied')
            self.assertIn('tenderdash',w.containers)
            del w.containers['tenderdash']
            with self.assertRaisesRegex(worker.Failure,'upgrade-missing-service'):w.execute()
        for service in ['drive','tenderdash']:
            with self.subTest(service=service),tempfile.TemporaryDirectory() as tmp:
                w=Fixture(Path(tmp))
                del w.containers[service]
                with self.assertRaisesRegex(worker.Failure,'upgrade-missing-service'):w.execute()
                self.assertIsNone(w.read('upgrade.json'))
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp));w.partial=True
            with self.assertRaises(worker.Failure):w.execute()
            del w.containers['drive']
            with self.assertRaisesRegex(worker.Failure,'upgrade-missing-service'):w.execute()

    def test_upgrade_entrypoint_cannot_run_arbitrary_lifecycle_actions(self):
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp));w.q['action']='stop'
            with self.assertRaisesRegex(worker.Failure,'upgrade-action-refused'):w.execute()
            self.assertFalse(w.accessed)
