import copy,importlib.util,tempfile,unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('worker',Path(__file__).parents[1]/'worker.py')
w=importlib.util.module_from_spec(spec);spec.loader.exec_module(w)

def container(name):
 return dict(id=name+'-1',imageId='sha256:'+name,image=name,digests=[],configHash=name+'-config',running=True,startedAt='2026-11-18T10:00:00Z',restarts=0)

class PreserveTest(unittest.TestCase):
 def fixture(self,root):
  worker=w.Worker(dict(fleet={'chainType':'testnet','metadata':{'name':'testnet'}},target={'name':'hp-masternode-18','role':'validator'}),root=Path(root))
  baseline=dict(filesHash='files',companions={'logs':container('logs')},nativeProcesses={'tor:1':'tor'},
                components={'core':container('core'),'drive':container('drive')})
  return worker,baseline
 def test_restart_in_place_is_not_drift(self):
  with tempfile.TemporaryDirectory() as root:
   worker,baseline=self.fixture(root)
   actual=copy.deepcopy(baseline)
   # The host restarted an unselected service and a companion since the snapshot.
   actual['components']['drive'].update(startedAt='2026-11-18T19:10:00Z',restarts=1)
   actual['companions']['logs'].update(startedAt='2026-11-18T19:10:00Z',restarts=4)
   worker.assert_baseline(actual,baseline)
   worker.assert_baseline(actual,baseline,('core',))
   # Core outside the scope is preserved exactly, process included.
   actual['components']['core'].update(startedAt='2026-11-18T19:10:00Z',restarts=1)
   with self.assertRaisesRegex(w.Failure,'preserved-service-changed'):worker.assert_baseline(actual,baseline)
   worker.assert_baseline(actual,baseline,('core',))
 def test_replacement_and_stop_are_still_refused(self):
  with tempfile.TemporaryDirectory() as root:
   worker,baseline=self.fixture(root)
   for kind,key,change,code in [
     ('components','drive',dict(id='drive-2'),'preserved-service-changed'),
     ('components','drive',dict(imageId='sha256:other'),'preserved-service-changed'),
     ('components','drive',dict(running=False),'preserved-service-changed'),
     ('components','drive',dict(configHash='other'),'workload-configuration-changed'),
     ('companions','logs',dict(id='logs-2'),'companion-changed'),
     ('companions','logs',dict(running=False),'companion-changed')]:
    actual=copy.deepcopy(baseline);actual[kind][key].update(change)
    with self.assertRaisesRegex(w.Failure,code):worker.assert_baseline(actual,baseline)
   actual=copy.deepcopy(baseline);actual['companions']['new']=container('new')
   with self.assertRaisesRegex(w.Failure,'companion-changed'):worker.assert_baseline(actual,baseline)

if __name__=='__main__':unittest.main()
