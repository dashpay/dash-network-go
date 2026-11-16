"""Owned native mining control, independent recovery and replay boundaries."""
import copy
from pathlib import Path
import tempfile
import unittest
from test_worker import worker, request

scope=vars(worker).copy()
exec((Path(__file__).parents[1]/'mining.py').read_text(),scope)
Mining=scope['MiningWorker']

class Host(Mining):
    def __init__(self,root):
        q=request();q['miningControl']=dict(operationId='e'*64,containerId='f'*64)
        super().__init__(q,root,root/'lock')
        self.miner=dict(Id='f'*64,Image=self.images['core'],State=dict(Running=True))
        self.commands=[];self.fail_arm=False
    def verify_instance(self):pass
    def owned(self):pass
    def inspect_container(self,service):return copy.deepcopy(self.miner)
    def verify_image(self,c,pin):self.require(c['Image']==pin,'image-drift')
    def run(self,args,**kwargs):
        self.commands.append(args)
        self.require(not self.fail_arm,'arm-failed')
        compile(args[-3],'<recovery>','exec')
        return b''
    def docker(self,*args,**kwargs):
        self.commands.append(args)
        if args[0]=='start':self.miner['State']['Running']=True
        elif args[0]=='stop':self.miner['State']['Running']=False
        else:raise AssertionError(args)
        return b''
    def rpc(self,method,params=None,wallet=False):
        assert method=='getrpcinfo'
        return dict(active_commands=[])
    def call(self,action):
        self.q['action']='rollout-mining-'+action
        return self.execute()['miningPause']

class MiningTests(unittest.TestCase):
    def test_pause_arms_recovery_before_stop_and_resumes_same_container(self):
        with tempfile.TemporaryDirectory() as d:
            h=Host(Path(d));self.assertTrue(h.call('pause')['paused'])
            self.assertEqual(h.commands[0][0],'systemd-run')
            self.assertEqual(h.commands[1][0],'stop')
            first=h.read('rollout-mining.json')
            self.assertTrue(h.call('pause')['paused']) # idempotent, no new pause
            self.assertTrue(h.call('renew')['paused'])
            self.assertNotEqual(first['token'],h.read('rollout-mining.json')['token'])
            self.assertFalse(h.call('resume')['paused'])
            self.assertFalse(h.call('resume')['paused'])
            self.assertEqual(h.commands[-1],('start','f'*64))
    def test_failed_timer_never_stops_mining(self):
        with tempfile.TemporaryDirectory() as d:
            h=Host(Path(d));h.fail_arm=True
            with self.assertRaises(worker.Failure):h.call('pause')
            self.assertTrue(h.miner['State']['Running'])
    def test_foreign_operation_and_replaced_container_are_not_resumed(self):
        for drift in ['owner','container']:
            with tempfile.TemporaryDirectory() as d:
                h=Host(Path(d));h.call('pause')
                if drift=='owner':h.q['miningControl']['operationId']='1'*64
                else:h.miner['Id']='2'*64
                with self.assertRaises(worker.Failure):h.call('resume')
                self.assertFalse(h.miner['State']['Running'])
    def test_stopped_miner_is_not_adopted_and_expired_lease_is_not_renewed(self):
        with tempfile.TemporaryDirectory() as d:
            h=Host(Path(d));h.miner['State']['Running']=False
            with self.assertRaises(worker.Failure):h.call('pause')
            h.miner['State']['Running']=True;h.call('pause')
            marker=h.read('rollout-mining.json');marker['expiresAt']=1;h.atomic('rollout-mining.json',marker)
            with self.assertRaises(worker.Failure):h.call('renew')
            self.assertFalse(h.call('resume')['paused'])
