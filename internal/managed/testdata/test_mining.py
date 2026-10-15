import importlib.util,json,tempfile,time,unittest
from pathlib import Path
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('worker',Path(__file__).parents[1]/'worker.py')
w=importlib.util.module_from_spec(spec);spec.loader.exec_module(w)

class MiningTest(unittest.TestCase):
 def fixture(self,root):
  worker=w.Worker(dict(fleet={'chainType':'testnet','metadata':{'name':'testnet'}},target={'name':'miner','role':'core','miningService':'dashd-generate-miner.service'},operationId='a'*64,migration={'kind':'legacy-testnet-v5','readinessDriven':True,'miners':['miner']},expected={'mining':{'serviceHash':'hash','active':True}}),root=Path(root))
  state={'active':True,'calls':[],'rpc':0,'drain':True}
  worker.owner=lambda:None;worker.mining_service=lambda:('hash',state['active'])
  worker.observe=lambda:{'mining':worker.mining_receipt()};worker.assert_baseline=lambda *a:None;worker.containers=lambda:({}, {})
  def rpc(*args):
   state['rpc']+=1
   return {'active_commands':([{'method':'generatetoaddress'}] if state['rpc']==1 else [])}
  worker.rpc=rpc
  def run(args,**kwargs):
   state['calls'].append(args)
   if args[0]=='systemd-run':
    self.assertTrue(worker.read('mining-pause.json')['paused']);compile(args[args.index('-c')+1],'<recovery>','exec')
   if args[:2]==['systemctl','stop']:
    self.assertEqual('systemd-run',state['calls'][-2][0]);state['active']=False
   if args[:2]==['systemctl','start']:state['active']=True
   return b''
  return worker,state,run
 def test_recovery_armed_before_stop_and_server_rpc_drained(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root)
   with patch.object(w,'run',run),patch.object(w.time,'sleep'):
    self.assertTrue(worker.mining_pause()['mining']['paused'])
    self.assertEqual(2,state['rpc'])
    worker.mining_resume();self.assertTrue(state['active']);self.assertFalse(worker.read('mining-pause.json')['paused'])
 def test_foreign_lease_not_resumed(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root);state['active']=False
   worker.atomic('mining-pause.json',{'paused':True,'operationId':'b'*64,'serviceHash':'hash'})
   with patch.object(w,'run',run),self.assertRaisesRegex(w.Failure,'mining-owner-or-drift'):worker.mining_resume()
   self.assertFalse(state['calls'])
 def test_unowned_inactive_service_not_started(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root);state['active']=False
   with patch.object(w,'run',run),self.assertRaisesRegex(w.Failure,'mining-unowned-inactive'):worker.mining_resume()
   self.assertFalse(state['calls'])
 def test_lost_stop_response_keeps_recovery_receipt(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root)
   def lost(args,**kwargs):
    result=run(args,**kwargs)
    if args[:2]==['systemctl','stop']:raise w.Failure('lost-stop')
    return result
   with patch.object(w,'run',lost),self.assertRaisesRegex(w.Failure,'lost-stop'):worker.mining_pause()
   self.assertTrue(worker.read('mining-pause.json')['paused'])
   with patch.object(w,'run',run):worker.mining_resume()
   self.assertTrue(state['active'])
 def test_failed_watchdog_arm_never_stops_generation(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root)
   def broken(args,**kwargs):
    if args[0]=='systemd-run':raise w.Failure('arm-failed')
    return run(args,**kwargs)
   with patch.object(w,'run',broken),self.assertRaisesRegex(w.Failure,'arm-failed'):worker.mining_pause()
   self.assertTrue(state['active'])
   with patch.object(w,'run',run):worker.mining_resume()
 def test_live_dkg_phase_overrides_quiet_height(self):
  import datetime
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root)
   # Choose an anchor so the live wall clock is always inside the epoch window.
   worker.q['migration']['epochStart']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=15)).isoformat()
   def rpc(selected,method,*args):
    if method=='getblockcount':return 24*100+13
    return {'session':[{'llmqType':'test','status':{'phase':4}}]}
   worker.rpc=rpc
   with self.assertRaisesRegex(w.Failure,'migration-dkg-active'):worker.migration_window({})
   worker.rpc=lambda selected,method,*args:24*100+13 if method=='getblockcount' else {'session':[{'status':{'phase':6}}]}
   worker.migration_window({})
