"""Real Core new-node join and replay. Disposable runner only; no AWS calls."""
import copy,os,subprocess,tempfile,time
from pathlib import Path
from core_integration import Disposable
from test_worker import request
from test_join import JoinWorker

class NewNode(JoinWorker):
 def verify_instance(self):self.require(os.environ.get('DASHNET_DISPOSABLE_CI')=='1','disposable-ci-only')

def root_at(base,name,q):
 p=base/name;p.mkdir(mode=0o700)
 (p/'owner').write_text(q['context']['computePlanId']+':'+q['target']['instanceId'])
 (p/'ready').write_text(q['context']['bootstrapId']);return p

def main():
 q=request();q['target']['images']=[dict(component='core',pinned=os.environ['CORE_IMAGE'])]
 with tempfile.TemporaryDirectory(prefix='dashnet-join-ci-') as d:
  base=Path(d);source=Disposable(q,root_at(base,'source',q),base/'source-lock');joining=None
  try:
   q['action']='core-start';source.execute();q['action']='wallet';source.execute()
   address=source.rpc('getnewaddress',[],True);source.rpc('generatetoaddress',[30,address])
   info=source.rpc('getblockchaininfo');height=info['blocks']-5
   n=copy.deepcopy(q);n['action']='join-start';n['context']['planId']='f'*64;n['context']['computePlanId']='e'*64
   n['context']['coreNetwork']=info['chain'];n['target'].update(name='fullnode-001',role='fullnode',instanceId='i-00000002')
   n['context']['ports']['coreP2P']=20011;n['context']['ports']['coreRPC']=20012
   public={'llmqchainlocks','llmqinstantsenddip0024','llmqplatform','llmqmnhf','minimumdifficultyblocks','highsubsidyblocks','highsubsidyfactor','powtargetspacing'}
   options=[x for x in source.core_config().splitlines() if x.partition('=')[0] in public]
   n['join']=dict(chainType='devnet',coreNetwork=info['chain'],genesis=source.rpc('getblockhash',[1]),checkpointHeight=height,checkpointHash=source.rpc('getblockhash',[height]),peers=['127.0.0.1:20001'],options=options)
   joining=NewNode(n,root_at(base,'new',n),base/'join-lock')
   joining.execute();deadline=time.monotonic()+100
   while True:
    n['action']='join-status';o=joining.execute()['core']
    if o['checkpointHash']==n['join']['checkpointHash'] and o['height']>=info['blocks']:break
    assert time.monotonic()<deadline,'new Core did not sync from existing peer';time.sleep(1)
   cid=o['containerId'];genesis=o['genesis'];source_id=source.inspect_container('core')['Id']
   try:
    joining.rpc('getwalletinfo')
   except Exception as e:
    assert type(e).__name__=='RPCFailure' and e.code==-32601, 'wallet RPC did not reject predictably'
   else:
    raise AssertionError('wallet RPC unexpectedly available')
   n['action']='join-start';o=joining.execute()['core'];assert o['containerId']==cid and o['genesis']==genesis
   assert source.inspect_container('core')['Id']==source_id and source.inspect_container('core')['State']['Running']
   joining.docker('stop',joining.container_name('core'));o=joining.execute()['core'];assert o['containerId']==cid and o['checkpointHash']==n['join']['checkpointHash']
   assert not (joining.root/'transactions').exists() and not joining.inspect_container('miner')
   print('New fullnode joined real existing Core chain; checkpoint, source preservation, wallet-disabled and same-container restart/replay passed.',flush=True)
  finally:
   for w in [joining,source]:
    if w:subprocess.run(['docker','rm','-f',w.container_name('core')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=False)
if __name__=='__main__':main()
