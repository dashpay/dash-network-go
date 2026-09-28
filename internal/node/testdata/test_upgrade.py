import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
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

    def wait_abci(self):
        self.commands.append(('wait-abci',))
        self.require(self.containers['drive']['State']['Running'], 'upgrade-drive-abci-timeout')

    def docker(self,*args,timeout=120):
        self.commands.append(args)
        if args[0] in ['stop','start']:
            service = next(k for k in self.containers if self.container_name(k)==args[-1])
            self.containers[service]['State']['Running'] = args[0]=='start'
            if args[0]=='start':self.containers[service]['RestartCount']=0
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
    def test_drive_update_withdraws_tenderdash_before_abci_disconnect(self):
        with tempfile.TemporaryDirectory() as tmp:
            w=Fixture(Path(tmp),'drive');old=copy.deepcopy(w.containers)
            w.lost=True
            with self.assertRaisesRegex(worker.Failure,'lost-apply-response'):w.execute()
            self.assertFalse(w.containers['tenderdash']['State']['Running'])
            self.assertEqual(w.read('upgrade.json')['dependency'],'stopped')
            w.q['action']='upgrade-stage';w.execute()
            self.assertFalse(w.containers['tenderdash']['State']['Running'])
            w.q['action']='upgrade-apply';w.execute()
            self.assertEqual(w.containers['tenderdash'],old['tenderdash'])
            self.assertEqual(w.read('upgrade.json')['dependency'],'started')
            stop=next(i for i,c in enumerate(w.commands) if c[0]=='stop')
            apply=next(i for i,c in enumerate(w.commands) if 'up' in c)
            ready=next(i for i,c in enumerate(w.commands) if c[0]=='wait-abci')
            start=next(i for i,c in enumerate(w.commands) if c[0]=='start')
            self.assertLess(stop,apply);self.assertLess(apply,ready);self.assertLess(ready,start)
            starts=sum(c[0]=='start' for c in w.commands)
            w.execute()
            self.assertEqual(sum(c[0]=='start' for c in w.commands),starts)

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


class CoreFixture(Upgrade):
    """A node whose Core image is replaced; Platform/miner are fakes."""
    def __init__(self, root, role='validator'):
        q = request()
        q['target'].update(role=role, name=role + '-1')
        q['context']['miningNodeName'] = 'wallet-1'
        components = ['core', 'drive', 'dapi', 'gateway', 'tenderdash', 'helper'] if role == 'validator' else ['core']
        before = {k: 'docker.io/example/' + k + '@sha256:' + 'd' * 64 for k in components}
        after = dict(before, core=before['core'].replace('d' * 64, 'e' * 64))
        q['target']['images'] = [dict(component=k, pinned=v) for k, v in after.items()]
        names = ['drive', 'tenderdash', 'dapi', 'gateway'] if role == 'validator' else []
        if role == 'wallet':
            names = ['miner']
        self.containers = {k: dict(Id=hashlib.sha256(k.encode()).hexdigest(), Image='image-' + (before['core'] if k == 'miner' else before[k]),
                                   State=dict(Running=True), RestartCount=3) for k in names}
        self.containers['core'] = dict(Id='a' * 64, Image='image-' + before['core'], State=dict(Running=True), RestartCount=0)
        preserve = dict(coreId='a' * 64, coreStarted='2026-09-24T00:00:00Z', coreConfig='b' * 64, coreGenesis='c' * 64)
        if role == 'validator':
            preserve.update(containers={k: self.containers[k]['Id'] for k in names}, restarts={k: 3 for k in names})
        q.update(action='upgrade-apply', upgrade=dict(id='f' * 64, previousId='', scope='core', **{'from': before}, to=after, preserve=preserve))
        super().__init__(q, root, root / 'lock')
        self.commands, self.lose_after_replace, self.heights = [], False, [13]
        # Quorum members still unverified after each dkgstatus call (the last repeats).
        self.links, self.valid = [set()], {'m1', 'm2', 'm3'}
        self.atomic('deployment.json', dict(planId=self.c['planId']))
        self.atomic('core/compose.json', dict(name=self.project, services={'core': self.service('core', before['core'])}))
        if role == 'wallet':
            self.atomic('miner/compose.json', dict(name=self.project, services={'miner': self.service('miner', before['core'])}))

    def verify_instance(self):
        pass

    def owned(self):
        pass

    def inspect_container(self, service):
        return self.containers.get(service)

    def verify_image(self, value, pinned):
        self.require(value['Image'] == 'image-' + pinned, 'running-image-drift')

    def wait_drive(self):
        self.commands.append(('wait-drive',))
        self.require(self.containers['drive']['State']['Running'], 'upgrade-drive-abci-timeout')

    def core_status(self):
        value = self.containers['core']
        self.verify_image(value, self.images['core'])
        height = self.heights.pop(0) if len(self.heights) > 1 else self.heights[0]
        self.commands.append(('height', height))
        return dict(containerId=value['Id'], startedAt='t', configSha256='b' * 64, genesis='c' * 64,
                    synced=True, ibd=False, height=height, headers=height)

    def rpc(self, method, params=None, wallet=False):
        self.commands.append(('rpc', method, *(params or [])))
        if method == 'getpeerinfo':
            return [dict(id=7), dict(id=9)]
        if method == 'disconnectnode':
            if params[1] == 9:
                raise worker.RPCFailure(method, -29)
            return None
        if method == 'getconnectioncount':
            return 12
        if method == 'protx':
            return sorted(self.valid)
        if method == 'quorum':
            missing = self.links.pop(0) if len(self.links) > 1 else self.links[0]
            members = [dict(proTxHash=m, connected=m not in missing) for m in ['m1', 'm2', 'm3', 'banned']]
            return dict(quorumConnections=[dict(llmqType='llmq_devnet', quorumConnections=members)])
        raise AssertionError('unexpected rpc ' + method)

    def docker(self, *args, timeout=120):
        self.commands.append(args)
        if args[0] in ['stop', 'start']:
            service = next(k for k in self.containers if self.container_name(k) == args[-1])
            self.containers[service]['State']['Running'] = args[0] == 'start'
            if args[0] == 'start':
                self.containers[service]['RestartCount'] = 0
        if args[:2] == ('image', 'inspect'):
            return json.dumps([dict(Id='image-' + args[2], Architecture='amd64', Os='linux', RepoDigests=[args[2]])]).encode()
        if args[0] == 'compose' and 'up' in args:
            service = args[-1]
            image = self.read(service + '/compose.json')['services'][service]['image']
            if self.containers[service]['Image'] != 'image-' + image:
                self.containers[service] = dict(Id=hashlib.sha256(image.encode()).hexdigest(), Image='image-' + image, State=dict(Running=True), RestartCount=0)
            self.containers[service]['State']['Running'] = True  # up -d also starts a stopped container
            if service == 'core' and self.lose_after_replace:
                self.lose_after_replace = False
                raise worker.Failure('lost-apply-response')
        return b''


class CoreUpgradeTests(unittest.TestCase):
    def test_validator_withdraws_platform_replaces_core_and_restores(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            platform = {k: w.containers[k]['Id'] for k in ['drive', 'tenderdash', 'dapi', 'gateway']}
            result = w.execute()
            index = lambda cmd: next(i for i, c in enumerate(w.commands) if c[:len(cmd)] == cmd)
            name = w.container_name
            self.assertLess(index(('stop', '--time', '120', name('tenderdash'))), index(('stop', '--time', '120', name('drive'))))
            replace = next(i for i, c in enumerate(w.commands) if c[0] == 'compose' and 'up' in c and c[-1] == 'core')
            self.assertLess(index(('stop', '--time', '120', name('drive'))), replace)
            self.assertLess(replace, index(('start', name('drive'))))
            self.assertLess(index(('start', name('drive'))), index(('start', name('tenderdash'))))
            self.assertEqual({k: w.containers[k]['Id'] for k in platform}, platform, 'Platform containers kept')
            self.assertTrue(all(w.containers[k]['State']['Running'] for k in platform))
            self.assertNotEqual(result['core']['containerId'], 'a' * 64)
            self.assertEqual(w.read('core/compose.json')['services']['core']['image'], w.q['upgrade']['to']['core'])
            self.assertEqual(w.read('upgrade.json')['phase'], 'applied')
            self.assertFalse(any(c[0] == 'stop' and name('gateway') in c for c in w.commands), 'gateway keeps serving')

    def test_lost_response_after_replacement_resumes_without_a_second_replacement(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            w.lose_after_replace = True
            with self.assertRaisesRegex(worker.Failure, 'lost-apply-response'):
                w.execute()
            self.assertEqual(w.read('upgrade.json')['step'], 'withdrawn')
            core = w.containers['core']['Id']
            w.execute()
            replaced = [c for c in w.commands if c[0] == 'compose' and 'up' in c and c[-1] == 'core']
            self.assertEqual(len(replaced), 1, 'Core already on the new image is not replaced again')
            self.assertEqual(w.containers['core']['Id'], core)
            self.assertEqual(w.read('upgrade.json')['step'], 'done')
            self.assertTrue(w.containers['drive']['State']['Running'])
            before = len(w.commands)
            w.execute()
            self.assertFalse(any(c[0] in ['stop', 'start'] or 'up' in c for c in w.commands[before:]), 'a finished node is only verified')

    def test_mining_node_pauses_and_recreates_the_miner_on_the_new_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp), 'wallet')
            w.execute()
            stop = next(i for i, c in enumerate(w.commands) if c[0] == 'stop' and w.container_name('miner') in c)
            replace = next(i for i, c in enumerate(w.commands) if c[0] == 'compose' and 'up' in c and c[-1] == 'core')
            miner = next(i for i, c in enumerate(w.commands) if c[0] == 'compose' and 'up' in c and c[-1] == 'miner')
            self.assertLess(stop, replace)
            self.assertLess(replace, miner)
            self.assertEqual(w.containers['miner']['Image'], 'image-' + w.q['upgrade']['to']['core'])

    def test_core_upgrade_refuses_other_component_changes(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            w.q['upgrade']['to'] = dict(w.q['upgrade']['to'], drive=w.q['upgrade']['to']['drive'].replace('d' * 64, 'e' * 64))
            with self.assertRaisesRegex(worker.Failure, 'core-upgrade-scope'):
                w.execute()


class CoreUpgradeReviewTests(unittest.TestCase):
    def test_validator_core_stops_only_in_the_quiet_dkg_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            w.heights = [13, 20, 23, 0, 5, 12, 14]
            with mock.patch.object(scope['time'], 'sleep', lambda s: None):
                w.execute()
            stop = next(i for i, c in enumerate(w.commands) if c[0] == 'stop' and w.container_name('core') in c)
            last_height = max(i for i, c in enumerate(w.commands[:stop]) if c[0] == 'height')
            self.assertIn(w.commands[last_height][1] % 24, range(13, 15), 'Core stopped outside the quiet window')
            self.assertLess(next(i for i, c in enumerate(w.commands) if c[0] == 'stop' and w.container_name('tenderdash') in c), stop, 'Platform withdrawn first')

    def test_validator_reconnects_once_synced_and_waits_for_every_quorum_link(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            # One-sided links survive until the reconnect; a banned member never connects.
            w.links = [{'m2', 'banned'}, {'m2', 'banned'}, {'banned'}]
            with mock.patch.object(scope['time'], 'sleep', lambda s: None):
                w.execute()
            index = lambda cmd: next(i for i, c in enumerate(w.commands) if c[:len(cmd)] == cmd)
            self.assertLess(max(i for i, c in enumerate(w.commands) if c[0] == 'height'
                                and i < index(('rpc', 'getpeerinfo'))), index(('rpc', 'getpeerinfo')))
            self.assertLess(index(('rpc', 'disconnectnode', '', 9)), index(('start', w.container_name('drive'))))
            self.assertEqual(sum(1 for c in w.commands if c[:2] == ('rpc', 'quorum')), 3, 'waited for m2')
            self.assertEqual(w.read('upgrade.json')['step'], 'done')
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            w.links = [{'m1'}]
            clock = iter(range(0, 10000, 10))
            with mock.patch.object(scope['time'], 'sleep', lambda s: None), \
                    mock.patch.object(scope['time'], 'monotonic', lambda: next(clock)):
                with self.assertRaisesRegex(worker.Failure, 'core-upgrade-quorum-links-missing'):
                    w.execute()
            self.assertEqual(w.read('upgrade.json')['step'], 'replaced', 'a resume reconnects again')
            self.assertFalse(w.containers['drive']['State']['Running'])
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp), 'wallet')
            w.execute()
            self.assertFalse(any(c[0] == 'rpc' for c in w.commands), 'only masternodes verify quorum links')

    def test_nodes_an_earlier_upgrade_did_not_touch_accept_an_older_or_missing_marker(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp), 'wallet')
            w.q['upgrade']['previousId'] = '9' * 64  # a Platform upgrade that skipped the wallet
            w.execute()
            self.assertEqual(w.read('upgrade.json')['phase'], 'applied')
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp), 'wallet')
            w.q['upgrade']['previousId'] = '9' * 64
            w.atomic('upgrade.json', dict(id='8' * 64, phase='applied'))
            w.execute()
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp), 'wallet')
            w.atomic('upgrade.json', dict(id='8' * 64, phase='applying'))
            with self.assertRaisesRegex(worker.Failure, 'upgrade-previous-operation'):
                w.execute()

    def test_stage_on_an_in_progress_node_needs_no_running_core(self):
        with tempfile.TemporaryDirectory() as tmp:
            w = CoreFixture(Path(tmp))
            w.lose_after_replace = True
            with self.assertRaises(worker.Failure):
                w.execute()
            w.containers['core']['State']['Running'] = False
            w.q['action'] = 'upgrade-stage'
            result = w.execute()
            self.assertNotIn('core', result)
            w.q['action'] = 'upgrade-apply'
            w.execute()
            self.assertTrue(w.containers['core']['State']['Running'], 'a stopped new-image Core is started on resume')
            self.assertEqual(w.read('upgrade.json')['step'], 'done')
