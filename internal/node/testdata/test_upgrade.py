import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import fakehost
from test_worker import worker, observer_scope, node_request, host, platform_request, pin, SPORK

scope = observer_scope.copy()
exec((Path(__file__).parents[1] / 'upgrade.py').read_text(), scope)
Upgrade = scope['UpgradeWorker']
COMPONENTS = ['core', 'drive', 'dapi', 'gateway', 'tenderdash', 'helper']
PLATFORM = ['drive_abci', 'drive_tenderdash', 'rs_dapi', 'gateway_rate_limiter_redis', 'gateway_rate_limiter', 'gateway']


class Host(fakehost.FakeHost, Upgrade):
    """A deployed node on the fake host, upgraded by the real upgrade worker."""
    failure = worker.Failure

    def __init__(self, q, root):
        Upgrade.__init__(self, q, root, root / 'lock')
        self.setup_host()
        self.heights, self.lose = [13], None
        # Quorum members still unverified after each dkgstatus call (the last repeats).
        self.links, self.valid = [set()], {'m1', 'm2', 'm3'}
        self.core_template = ''

    def fake_render(self, stage, d):
        super().fake_render(stage, d)
        with open(stage / self.config_name / 'core/dash.conf', 'a') as f:
            f.write(self.core_template)

    def verify_instance(self):
        pass

    def owned(self):
        pass

    def wait_abci(self):
        self.commands.append(('wait-abci',))
        self.require(self.service_container('drive_abci')['State']['Running'], 'upgrade-drive-abci-timeout')

    def core_status(self):
        value = self.service_container('core')
        self.require(value and value['State']['Running'], 'core-not-running')
        self.verify_image(value, self.images['core'])
        height = self.heights.pop(0) if len(self.heights) > 1 else self.heights[0]
        self.commands.append(('height', height))
        return dict(containerId=value['Id'], startedAt='started-' + value['Id'][:8], configSha256=self.core_config(),
                    genesis='c' * 64, synced=True, ibd=False, height=height, headers=height)

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

    def docker(self, *args, timeout=120, env=None):
        if self.lose and args[0] == 'compose' and 'up' in args and self.lose in args:
            self.lose = None
            super().docker(*args, timeout=timeout, env=env)
            raise worker.Failure('lost-apply-response')
        return super().docker(*args, timeout=timeout, env=env)

    def ids(self):
        return {k: v['Id'] for k, v in self.containers.items()}


def deployed(tmp, role='validator'):
    """A node deployed by the real worker, then handed to the upgrade worker."""
    w = host(tmp, role)
    q = copy.deepcopy(w.q)
    u = Host(q, Path(tmp))
    u.containers, u.commands = w.containers, w.commands
    if role == 'validator':
        platform_request(u)
    else:
        u.q['sporkAddress'] = SPORK
    worker.Worker.ensure_core(u, True)
    if role == 'validator':
        worker.Worker.platform_start(u)
    if role == 'wallet':
        u.q['payoutAddress'] = 'y' + '1' * 33
        u.mine_start()
    u.atomic('deployment.json', dict(planId=u.c['planId']))
    u.commands = []
    return u


def change(u, scope_name=None, **new):
    before = dict(u.images)
    after = dict(before, **new)
    core = u.core_status()
    preserve = dict(coreId=core['containerId'], coreStarted=core['startedAt'], coreConfig=core['configSha256'], coreGenesis=core['genesis'])
    if u.t['role'] == 'validator':
        preserve.update(containers={worker.COMPONENTS.get(n, n): u.service_container(n)['Id'] for n in PLATFORM},
                        restarts={worker.COMPONENTS.get(n, n): 0 for n in PLATFORM})
    u.q.update(action='upgrade-apply', upgrade=dict(id='f' * 64, previousId='', **{'from': before}, to=after, preserve=preserve))
    if scope_name:
        u.q['upgrade']['scope'] = scope_name
    u.commands = []


def index(commands, predicate):
    return next(i for i, c in enumerate(commands) if predicate(c))


def up(c, service):
    return c[0] == 'compose' and 'up' in c and service in c


class PlatformUpgradeTests(unittest.TestCase):
    def test_stage_renders_the_target_release_and_changes_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, tenderdash=pin('tenderdash-2'))
            live = (u.home / '.dashnet-compose.json').read_text()
            ids = u.ids()
            u.q['action'] = 'upgrade-stage'
            result = u.execute()
            self.assertEqual(result['render']['changes'], ['drive_tenderdash'])
            self.assertEqual(result['render']['sidecars']['gateway_rate_limiter_redis'], 'redis:alpine')
            self.assertEqual((u.home / '.dashnet-compose.json').read_text(), live, 'staging installs nothing')
            self.assertEqual(u.ids(), ids)
            self.assertIsNone(u.read('upgrade.json'))
            self.assertFalse(any(c[0] == 'compose' and 'up' in c for c in u.commands))

    def test_image_change_recreates_only_that_service(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, tenderdash=pin('tenderdash-2'))
            ids = u.ids()
            result = u.execute()
            self.assertEqual(result['render']['changes'], ['drive_tenderdash'])
            changed = {k for k in ids if u.ids()[k] != ids[k]}
            self.assertEqual(changed, {u.container_name('drive_tenderdash')})
            self.assertEqual(u.service_container('drive_tenderdash')['Image'], 'image-' + pin('tenderdash-2'))
            self.assertEqual(u.read('upgrade.json')['phase'], 'applied')
            self.assertFalse(any(c[0] == 'stop' for c in u.commands), 'no drain without a Drive change')
            before = len(u.commands)
            u.execute()
            self.assertFalse(any(c[0] == 'compose' and 'up' in c and 'drive_tenderdash' in c for c in u.commands[before:]),
                             'an applied node is only verified')

    def test_drive_change_withdraws_tenderdash_around_abci(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, drive=pin('drive-2'))
            td = u.service_container('drive_tenderdash')['Id']
            u.lose = 'drive_abci'
            with self.assertRaisesRegex(worker.Failure, 'lost-apply-response'):
                u.execute()
            self.assertEqual(u.read('upgrade.json')['dependency'], 'stopped')
            self.assertFalse(u.service_container('drive_tenderdash')['State']['Running'])
            drive = u.service_container('drive_abci')['Id']
            u.execute()
            self.assertEqual(u.service_container('drive_abci')['Id'], drive, 'a replayed apply replaces nothing twice')
            self.assertEqual(u.service_container('drive_tenderdash')['Id'], td, 'Tenderdash keeps its container')
            self.assertTrue(u.service_container('drive_tenderdash')['State']['Running'])
            c = u.commands
            stop = index(c, lambda x: x[0] == 'stop' and u.container_name('drive_tenderdash') in x)
            replace = index(c, lambda x: up(x, 'drive_abci'))
            ready = index(c, lambda x: x[0] == 'wait-abci')
            start = index(c, lambda x: x[0] == 'start' and u.container_name('drive_tenderdash') in x)
            self.assertLess(stop, replace)
            self.assertLess(replace, ready)
            self.assertLess(ready, start)
            self.assertEqual(u.read('upgrade.json')['dependency'], 'started')

    def test_a_newer_dashmate_reconfigures_platform_and_defers_core(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, helper=pin('helper-2'))
            u.gateway_template = 'overload_manager: newer\n'
            u.core_template = 'newoption=1\n'
            conf = (u.home / u.config_name / 'core/dash.conf').read_text()
            ids = u.ids()
            result = u.execute()
            self.assertEqual(result['render']['changes'], ['gateway'])
            self.assertEqual(result['render']['deferred'], ['core', 'core_tor'])
            self.assertNotEqual(u.service_container('gateway')['Id'], ids[u.container_name('gateway')])
            self.assertEqual(u.service_container('gateway')['Image'], 'image-' + u.images['gateway'], 'same image, new configuration')
            self.assertIn('overload_manager: newer', (u.home / u.config_name / 'platform/gateway/envoy.yaml').read_text())
            self.assertEqual((u.home / u.config_name / 'core/dash.conf').read_text(), conf, "Core's rendered files wait")
            for name in ['core', 'core_tor', 'drive_abci', 'rs_dapi']:
                self.assertEqual(u.service_container(name)['Id'], ids[u.container_name(name)], name)

    def test_replay_reuses_the_render_and_refuses_changed_inputs(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, helper=pin('helper-2'))
            u.gateway_template = 'one: 1\n'
            u.q['action'] = 'upgrade-stage'
            u.execute()
            u.q['action'] = 'upgrade-apply'
            u.commands = []
            u.lose = 'gateway'
            with self.assertRaisesRegex(worker.Failure, 'lost-apply-response'):
                u.execute()
            self.assertFalse(any(c[0] == 'helper' for c in u.commands), 'the apply reuses the staged render')
            u.c['platformEpochSeconds'] = 600
            with self.assertRaisesRegex(worker.Failure, 'upgrade-config-changed'):
                u.execute()

    def test_unselected_changes_and_core_restarts_fail_before_apply(self):
        for case in ['restart', 'foreign', 'missing', 'core', 'unselected']:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                u = deployed(tmp)
                change(u, tenderdash=pin('tenderdash-2'))
                if case == 'restart':
                    u.q['upgrade']['preserve']['coreStarted'] = 'earlier'
                elif case == 'foreign':
                    u.atomic('upgrade.json', dict(id='a' * 64, phase='applied'))
                elif case == 'missing':
                    (u.root / 'deployment.json').unlink()
                elif case == 'core':
                    u.q['upgrade']['to']['core'] = pin('core-2')
                else:
                    u.service_container('rs_dapi')['RestartCount'] = 2
                ids = u.ids()
                with self.assertRaises(worker.Failure):
                    u.execute()
                self.assertEqual(u.ids(), ids)
                self.assertFalse(any(c[0] == 'compose' and 'up' in c for c in u.commands))

    def test_upgrade_entrypoint_cannot_run_lifecycle_actions(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            u.q['action'] = 'stop'
            with self.assertRaisesRegex(worker.Failure, 'upgrade-action-refused'):
                u.execute()


class CoreUpgradeTests(unittest.TestCase):
    def test_validator_withdraws_platform_replaces_core_and_restores(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            platform = {n: u.service_container(n)['Id'] for n in PLATFORM}
            config = u.core_config()
            result = u.execute()
            c, name = u.commands, u.container_name
            self.assertLess(index(c, lambda x: x[0] == 'stop' and name('drive_tenderdash') in x),
                            index(c, lambda x: x[0] == 'stop' and name('drive_abci') in x))
            replace = index(c, lambda x: up(x, 'core'))
            self.assertLess(index(c, lambda x: x[0] == 'stop' and name('drive_abci') in x), replace)
            self.assertLess(replace, index(c, lambda x: x[0] == 'start' and name('drive_abci') in x))
            self.assertLess(index(c, lambda x: x[0] == 'start' and name('drive_abci') in x),
                            index(c, lambda x: x[0] == 'start' and name('drive_tenderdash') in x))
            self.assertEqual({n: u.service_container(n)['Id'] for n in PLATFORM}, platform, 'Platform containers kept')
            self.assertTrue(all(u.service_container(n)['State']['Running'] for n in PLATFORM))
            self.assertEqual(u.service_container('core')['Image'], 'image-' + pin('core-2'))
            self.assertEqual(result['core']['configSha256'], config, 'dash.conf preserved')
            self.assertEqual(u.read('upgrade.json')['phase'], 'applied')
            self.assertFalse(any(x[0] == 'stop' and name('gateway') in x for x in c), 'gateway keeps serving')

    def test_lost_response_after_replacement_resumes_without_a_second_replacement(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            u.lose = 'core'
            with self.assertRaisesRegex(worker.Failure, 'lost-apply-response'):
                u.execute()
            self.assertEqual(u.read('upgrade.json')['step'], 'withdrawn')
            core = u.service_container('core')['Id']
            u.execute()
            self.assertEqual(sum(1 for x in u.commands if up(x, 'core')), 1, 'Core on the new image is not replaced again')
            self.assertEqual(u.service_container('core')['Id'], core)
            self.assertEqual(u.read('upgrade.json')['step'], 'done')
            before = len(u.commands)
            u.execute()
            self.assertFalse(any(x[0] in ['stop', 'start'] or 'up' in x for x in u.commands[before:]), 'a finished node is only verified')

    def test_mining_node_pauses_and_recreates_the_miner_on_the_new_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp, 'wallet')
            change(u, 'core', core=pin('core-2'))
            u.execute()
            c = u.commands
            stop = index(c, lambda x: x[0] == 'stop' and u.container_name('miner') in x)
            replace = index(c, lambda x: up(x, 'core'))
            miner = index(c, lambda x: x[0] == 'compose' and 'up' in x and 'miner' in x)
            self.assertLess(stop, replace)
            self.assertLess(replace, miner)
            self.assertEqual(u.service_container('miner')['Image'], 'image-' + pin('core-2'))
            self.assertFalse(any(x[0] == 'rpc' for x in c), 'only masternodes verify quorum links')

    def test_core_upgrade_refuses_other_component_changes(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'), drive=pin('drive-2'))
            with self.assertRaisesRegex(worker.Failure, 'core-upgrade-scope'):
                u.execute()

    def test_validator_core_stops_only_in_the_quiet_dkg_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            u.heights = [24, 29, 36, 44]  # 0, 5, 12 are session blocks; 20 is quiet
            with mock.patch.object(scope['time'], 'sleep', lambda s: None):
                u.execute()
            c = u.commands
            stop = index(c, lambda x: x[0] == 'stop' and u.container_name('core') in x)
            last = max(i for i, x in enumerate(c[:stop]) if x[0] == 'height')
            self.assertEqual(c[last][1], 44, 'Core stopped outside the quiet window')
            self.assertLess(index(c, lambda x: x[0] == 'stop' and u.container_name('drive_tenderdash') in x), stop, 'Platform withdrawn first')

    def test_validator_reconnects_once_synced_and_waits_for_every_quorum_link(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            # One-sided links survive until the reconnect; a banned member never connects.
            u.links = [{'m2', 'banned'}, {'m2', 'banned'}, {'banned'}]
            with mock.patch.object(scope['time'], 'sleep', lambda s: None):
                u.execute()
            c = u.commands
            peers = index(c, lambda x: x[:2] == ('rpc', 'getpeerinfo'))
            self.assertLess(index(c, lambda x: x[:4] == ('rpc', 'disconnectnode', '', 9)),
                            index(c, lambda x: x[0] == 'start' and u.container_name('drive_abci') in x))
            self.assertLess(max(i for i, x in enumerate(c) if x[0] == 'height' and i < peers), peers)
            self.assertEqual(sum(1 for x in c if x[:2] == ('rpc', 'quorum')), 3, 'waited for m2')
            self.assertEqual(u.read('upgrade.json')['step'], 'done')
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            u.links = [{'m1'}]
            clock = iter(range(0, 10000, 10))
            with mock.patch.object(scope['time'], 'sleep', lambda s: None), \
                    mock.patch.object(scope['time'], 'monotonic', lambda: next(clock)):
                with self.assertRaisesRegex(worker.Failure, 'core-upgrade-quorum-links-missing'):
                    u.execute()
            self.assertEqual(u.read('upgrade.json')['step'], 'replaced', 'a resume reconnects again')
            self.assertFalse(u.service_container('drive_abci')['State']['Running'])

    def test_nodes_an_earlier_upgrade_did_not_touch_accept_an_older_or_missing_marker(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp, 'wallet')
            change(u, 'core', core=pin('core-2'))
            u.q['upgrade']['previousId'] = '9' * 64  # a Platform upgrade that skipped the wallet
            u.execute()
            self.assertEqual(u.read('upgrade.json')['phase'], 'applied')
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp, 'wallet')
            change(u, 'core', core=pin('core-2'))
            u.q['upgrade']['previousId'] = '9' * 64
            u.atomic('upgrade.json', dict(id='8' * 64, phase='applied'))
            u.execute()
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp, 'wallet')
            change(u, 'core', core=pin('core-2'))
            u.atomic('upgrade.json', dict(id='8' * 64, phase='applying'))
            with self.assertRaisesRegex(worker.Failure, 'upgrade-previous-operation'):
                u.execute()

    def test_stage_on_an_in_progress_node_needs_no_running_core(self):
        with tempfile.TemporaryDirectory() as tmp:
            u = deployed(tmp)
            change(u, 'core', core=pin('core-2'))
            u.lose = 'core'
            with self.assertRaises(worker.Failure):
                u.execute()
            u.service_container('core')['State']['Running'] = False
            u.q['action'] = 'upgrade-stage'
            result = u.execute()
            self.assertNotIn('core', result)
            u.q['action'] = 'upgrade-apply'
            u.execute()
            self.assertTrue(u.service_container('core')['State']['Running'], 'a stopped new-image Core is started on resume')
            self.assertEqual(u.read('upgrade.json')['step'], 'done')


if __name__ == '__main__':
    unittest.main()
