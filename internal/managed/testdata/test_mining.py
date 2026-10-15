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
 def test_fleet_renew_arms_before_commit_and_old_timer_is_cancelled(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root);worker.q['migration']['fleetMiningPause']=True
   with patch.object(w,'run',run),patch.object(w.time,'sleep'):worker.mining_pause()
   old=worker.read('mining-pause.json');state['calls']=[]
   def renewed(args,**kwargs):
    if args[0]=='systemd-run':
     self.assertEqual(old,worker.read('mining-pause.json'))
     compile(args[args.index('-c')+1],'<recovery>','exec')
    state['calls'].append(args);return b''
   with patch.object(w,'run',renewed):worker.mining_renew()
   new=worker.read('mining-pause.json')
   self.assertNotEqual(old['token'],new['token']);self.assertFalse(state['active'])
   self.assertEqual(state['calls'][1],['systemctl','stop','dashnet-mining-'+old['token']+'.timer'])
 def test_failed_renew_keeps_old_recovery_authoritative(self):
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root);worker.q['migration']['fleetMiningPause']=True
   with patch.object(w,'run',run),patch.object(w.time,'sleep'):worker.mining_pause()
   old=worker.read('mining-pause.json')
   with patch.object(w,'run',side_effect=w.Failure('arm-failed')),self.assertRaisesRegex(w.Failure,'arm-failed'):worker.mining_renew()
   self.assertEqual(old,worker.read('mining-pause.json'))
 def test_expired_or_foreign_renew_cannot_reacquire(self):
  for change in [{'expiresAt':0},{'operationId':'b'*64},{'serviceHash':'foreign'}]:
   with tempfile.TemporaryDirectory() as root:
    worker,state,run=self.fixture(root);worker.q['migration']['fleetMiningPause']=True
    with patch.object(w,'run',run),patch.object(w.time,'sleep'):worker.mining_pause()
    marker=worker.read('mining-pause.json');marker.update(change);worker.atomic('mining-pause.json',marker)
    with patch.object(w,'run') as commands,self.assertRaisesRegex(w.Failure,'mining-lease-lost'):worker.mining_renew()
    commands.assert_not_called()
 def test_worker_dispatches_real_mining_lifecycle(self):
  # Exercise execute(), not just direct method calls: the old dispatcher
  # rejected every mining action although its methods passed unit tests.
  with tempfile.TemporaryDirectory() as root:
   worker,state,run=self.fixture(root);worker.q['migration']['fleetMiningPause']=True
   worker.identity=lambda:None;worker.engine=object()
   def local_open(path,*args,**kwargs):return open(Path(root)/'lock' if path=='/run/dashnet-managed.lock' else path,*args,**kwargs)
   def commands(args,**kwargs):
    if args[:2]==['systemctl','stop'] and args[-1].endswith('.timer'):return b''
    return run(args,**kwargs)
   with patch.object(w,'open',local_open,create=True),patch.object(w,'run',commands),patch.object(w.time,'sleep'):
    for action in ['mining_pause','mining_status','mining_renew','mining_resume']:
     worker.q['action']=action;worker.execute()
   self.assertTrue(state['active']);self.assertFalse(worker.read('mining-pause.json')['paused'])
   worker.q['action']='admit';worker.migration_window=lambda selected:None
   with patch.object(w,'open',local_open,create=True):worker.execute()
