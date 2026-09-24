"""Real Core RPC contract proof, in an ephemeral CI runner only. No AWS calls.

Test subclass substitutes instance identity and filesystem location; all Core
configuration, Compose, wallet, registration and reconciliation code is real.
No wallet/config/key/transaction artifacts or raw container logs are published.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
from test_worker import worker, request

class Disposable(worker.Worker):
    def verify_instance(self):
        self.require(os.environ.get('DASHNET_DISPOSABLE_CI')=='1','disposable-ci-only')

def main():
    image=os.environ['CORE_IMAGE']
    q=request();q['target']['images']=[dict(component='core',pinned=image)]
    with tempfile.TemporaryDirectory(prefix='dashnet-core-ci-') as tmp:
        root=Path(tmp);root.chmod(0o700)
        (root/'owner').write_text(q['context']['computePlanId']+':'+q['target']['instanceId'])
        (root/'ready').write_text(q['context']['bootstrapId'])
        w=Disposable(q,root,root/'lock')
        def call(action):
            q['action']=action
            result=w.execute()
            print(action+' passed',flush=True)
            return result
        try:
            call('inspect');first=call('core-start')['core'];wallet=call('wallet')
            q['sporkAddress']=wallet['sporkAddress'];call('core-finalize')
            # Reload after Core restart before funding (wallet load is explicit).
            call('wallet');q['requiredBalance']=4001;call('fund')
            pair=w.rpc('bls',['generate'])
            q['registration']=dict(name='validator-1',address='10.0.0.2',operatorPublicKey=pair['public'],nodeId='a'*40)
            q['requiredConfirmations']=1
            registration=call('register')
            saved=(root/'transactions/validator-1.json').read_bytes()
            balance=w.rpc('getbalance',[],True)
            again=call('register')
            assert registration['proTxHash']==again['proTxHash']
            assert saved==(root/'transactions/validator-1.json').read_bytes()
            assert balance==w.rpc('getbalance',[],True)
            assert w.rpc('listlockunspent',[],True), 'Collateral was not locked'
            resumed=call('core-start')['core']
            assert first['genesis']==resumed['genesis']
            call('stop');call('core-start');call('wallet')
            assert w.rpc('listlockunspent',[],True), 'Collateral lock lost on restart'
            assert call('register')['proTxHash']==registration['proTxHash']
            q['registration']=dict(name='validator-2',address='10.0.0.3',operatorPublicKey=w.rpc('bls',['generate'])['public'],nodeId='b'*40)
            call('fund');call('register')
            assert w.rpc('protx',['info',registration['proTxHash']])['proTxHash']==registration['proTxHash']
            assert len(w.rpc('listlockunspent',[],True))==2, 'Second registration spent first collateral'
            print('Real Core: config, wallet, signed EvoNode registration, idempotent replay, stop/resume passed.',flush=True)
        finally:
            for name in ['core','miner']:
                subprocess.run(['docker','rm','-f',w.container_name(name)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=False)

if __name__=='__main__': main()
