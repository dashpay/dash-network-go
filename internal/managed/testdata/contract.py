"""Exercise enrollment and direct Engine replacement on a disposable Docker host.

Stand-in services prove runtime preservation/recovery, not Dash protocol support.
"""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile

spec=importlib.util.spec_from_file_location('managed_worker',Path(__file__).parents[1]/'worker.py')
worker=importlib.util.module_from_spec(spec);spec.loader.exec_module(worker)

class Engine(worker.Engine):
    fail_create=False
    lose_start=False
    def call(self,method,path,body=None,timeout=120,missing=False):
        if self.fail_create and method=='POST' and path.startswith('/containers/create'):
            self.fail_create=False
            raise worker.Failure('simulated-create-failure')
        result=super().call(method,path,body,timeout,missing)
        if self.lose_start and method=='POST' and path.endswith('/start'):
            self.lose_start=False
            raise worker.Failure('simulated-start-response-loss')
        return result

class Fixture(worker.Worker):
    def identity(self):
        worker.require(os.environ.get('DASHNET_DISPOSABLE_CI')=='1','disposable-only')
    def health(self,selected):
        return {},[]


def main():
    before,after=os.environ['MANAGED_FROM'],os.environ['MANAGED_TO']
    engine=Engine()
    with tempfile.TemporaryDirectory(prefix='dashnet-managed-contract-') as tmp:
        root=Path(tmp);prefix=root.name
        network=prefix+'-net';volume=prefix+'-state'
        run=lambda args:subprocess.check_output(args,stderr=subprocess.STDOUT,timeout=60)
        run(['docker','network','create',network]);run(['docker','volume','create',volume])
        names={c:prefix+'-'+c for c in ['core','tenderdash','drive']}
        companion=prefix+'-companion'
        (root/'dash.conf').write_text('private-fixture-password=never-export\n')
        try:
            for c,name in list(names.items())+[('companion',companion)]:
                run(['docker','run','-d','--name',name,'--network',network,'--network-alias',c,
                     '--restart','unless-stopped','--label','com.docker.compose.project=legacy',
                     '--label','com.docker.compose.service='+c,'--mount','type=volume,src='+volume+',dst=/state',
                     '--mount','type=bind,src='+str(root/'dash.conf')+',dst=/config/dash.conf,readonly',
                     '-e','RPC_PASSWORD=private-fixture-password',before,'sh','-c','trap "exit 0" TERM; while :; do sleep 1; done'])
            run(['docker','exec',names['core'],'sh','-c','printf durable-state > /state/sentinel'])
            q=dict(action='observe',fleetId='a'*64,fleet=dict(metadata=dict(name='devnet-fixture'),coreNetwork='devnet-fixture',chainType='devnet'),
                   target=dict(instanceId='i-'+'1'*17,architecture='amd64',role='validator',containers=names))
            w=Fixture(q,engine,root/'managed')
            initial=w.execute();assert not (root/'managed').exists(),'read-only import wrote state'
            assert 'private-fixture-password' not in json.dumps(initial)
            q.update(action='enroll',expected=initial);enrolled=w.execute()
            assert enrolled['components']==initial['components'],'enrollment touched containers'
            assert (root/'managed/workloads.json').stat().st_mode&0o777==0o600
            q.update(action='stage',operation='upgrade',operationId='b'*64,pins=dict(tenderdash=after))
            w.execute();assert w.observe()['components']==initial['components'],'staging touched containers'
            q['action']='apply';engine.fail_create=True
            try:w.execute()
            except worker.Failure as e:assert str(e)=='simulated-create-failure'
            else:raise AssertionError('expected failure after old container removal')
            assert engine.inspect(names['tenderdash']) is None
            assert w.read('operation.json')['phase']=='applying'
            w=Fixture(q,engine,root/'managed');engine.lose_start=True
            try:w.execute()
            except worker.Failure as e:assert str(e)=='simulated-start-response-loss'
            else:raise AssertionError('expected accepted start with lost response')
            replacement=engine.inspect(names['tenderdash'])['Id']
            w=Fixture(q,engine,root/'managed');result=w.execute()
            assert engine.inspect(names['tenderdash'])['Id']==replacement,'resume replaced accepted container'
            assert result['components']['core']==initial['components']['core'],'Core changed'
            assert result['components']['drive']==initial['components']['drive'],'Drive changed'
            assert result['companions']==initial['companions'],'companion changed'
            assert result['filesHash']==initial['filesHash'],'configuration changed'
            assert run(['docker','exec',names['tenderdash'],'cat','/state/sentinel'])==b'durable-state'
            assert engine.inspect(names['tenderdash'])['Config']['Env']==engine.inspect(names['core'])['Config']['Env']
            assert 'private-fixture-password' not in json.dumps(result)
            # Deploy restores an explicitly stopped captured workload, not a new
            # empty node or a silent image upgrade.
            baseline=w.observe();engine.call('POST','/containers/'+replacement+'/stop?t=5')
            q.update(operation='deploy',operationId='c'*64,expected=baseline)
            result=w.execute();assert result['components']['tenderdash']['id']==replacement
            assert result['components']['tenderdash']['running']
            print('Existing-state enrollment, exact Engine image replacement, stop/remove/create interruption, lost start response, same-image deployment and persistent state preservation passed.')
        finally:
            for name in [*names.values(),companion]:
                subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            subprocess.run(['docker','volume','rm',volume],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            subprocess.run(['docker','network','rm',network],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)

if __name__=='__main__':main()
