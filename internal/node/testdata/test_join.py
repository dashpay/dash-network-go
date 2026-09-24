import copy,json,tempfile,unittest
from pathlib import Path
from test_worker import worker,request

scope=vars(worker).copy()
exec((Path(__file__).parents[1]/'join.py').read_text(),scope)
JoinWorker=scope['JoinWorker']
def join_request():
 q=request();q['target']['name']='new-core';q['target']['role']='fullnode';q['target']['images']=q['target']['images'][:1]
 q['context']['coreNetwork']='devnet-existing';q['action']='join-start'
 q['join']=dict(chainType='devnet',coreNetwork='devnet-existing',genesis='a'*64,checkpointHeight=100,checkpointHash='b'*64,peers=['10.0.0.5:20001'],options=['powtargetspacing=10'])
 return q
class Fake(JoinWorker):
 def inspect_container(self,service):return dict(Id='c'*64,State=dict(Running=True),RestartCount=0)
 def verify_image(self,*args):pass
 def rpc(self,method,params=None,wallet=False):
  if method=='getblockchaininfo':return dict(chain=self.q['join']['coreNetwork'],blocks=120,headers=120,initialblockdownload=False)
  if method=='getblockhash':return self.q['join']['genesis'] if params[0]==1 else getattr(self,'checkpoint',self.q['join']['checkpointHash'])
  if method=='getnetworkinfo':return dict(connections=2)
  if method=='getbestchainlock':return dict(height=119)
  raise AssertionError('Unexpected RPC: '+method)
class JoinTest(unittest.TestCase):
 def test_private_identity_is_fresh_and_wallet_disabled(self):
  with tempfile.TemporaryDirectory() as d:
   q=join_request();w=JoinWorker(q,Path(d),Path(d)/'lock');config=w.join_config()
   self.assertIn('disablewallet=1',config);self.assertNotIn('masternodeblsprivkey',config);self.assertNotIn('sporkkey',config)
   self.assertIn('addnode=10.0.0.5:20001',config);self.assertIn('devnet=existing',config)
   q['join'].update(chainType='testnet',coreNetwork='test',options=[])
   config=w.join_config();self.assertIn('testnet=1',config);self.assertNotIn('devnet=',config)
 def test_wrong_checkpoint_and_config_drift_refused(self):
  with tempfile.TemporaryDirectory() as d:
   q=join_request();w=Fake(q,Path(d),Path(d)/'lock');w.atomic('core/dash.conf',w.join_config());w.atomic('join.json',q['join']);w.atomic('join-container.json',dict(id='c'*64))
   self.assertEqual(w.join_status()['checkpointHash'],'b'*64)
   w.checkpoint='e'*64
   with self.assertRaisesRegex(worker.Failure,'join-wrong-checkpoint'):w.join_status()
   del w.checkpoint;w.atomic('core/dash.conf',w.join_config()+'wallet=foreign\n')
   with self.assertRaisesRegex(worker.Failure,'join-config-drift'):w.join_status()
 def test_status_does_not_regenerate_missing_secret(self):
  with tempfile.TemporaryDirectory() as d:
   q=join_request();w=Fake(q,Path(d),Path(d)/'lock');w.atomic('join.json',q['join']);w.atomic('join-container.json',dict(id='c'*64))
   with self.assertRaisesRegex(worker.Failure,'missing-rpc-identity'):w.join_status()
   self.assertFalse((Path(d)/'secrets.json').exists())
if __name__=='__main__':unittest.main()
