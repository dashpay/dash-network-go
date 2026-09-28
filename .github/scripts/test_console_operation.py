import copy,importlib.util,unittest
from pathlib import Path
p=Path(__file__).with_name('console-operation.py');spec=importlib.util.spec_from_file_location('operation',p);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
class ScopeTest(unittest.TestCase):
 def setUp(self):
  self.env=dict(NETWORK='testnet',OPERATION='upgrade',CONFIRM='a'*64,REQUEST_ID='12345678-1234-4234-8234-123456789abc',DASHNET_AWS_ACCOUNT='123456789012',AWS_REGION='us-west-2',DASHNET_STATE_TABLE='managed-state')
  self.bundle=dict(network='testnet',action='upgrade',planId='a'*64,binarySHA256='b'*64,targets=['one'],artifact=dict(id='a'*64,operation='upgrade',targets=['one'],snapshot=dict(fleet=dict(metadata=dict(name='testnet'),accountId='123456789012',region='us-west-2',stateTable='managed-state',targets=[dict(name='one'),dict(name='two')]))))
 def test_valid_exact_request(self):
  module.validate(self.bundle,self.env)
 def test_tampered_authority(self):
  for k,v in dict(NETWORK='devnet-moutai',OPERATION='deploy',CONFIRM='c'*64,REQUEST_ID='../bad',DASHNET_AWS_ACCOUNT='000000000000',AWS_REGION='us-east-1',DASHNET_STATE_TABLE='other').items():
   with self.subTest(k=k),self.assertRaises(ValueError):module.validate(self.bundle,{**self.env,k:v})
 def test_target_plan_mismatch(self):
  for targets in [['two'],['one','one'],['foreign'],[]]:
   value=copy.deepcopy(self.bundle);value['targets']=targets
   with self.subTest(targets=targets),self.assertRaises(ValueError):module.validate(value,self.env)
if __name__=='__main__':unittest.main()
