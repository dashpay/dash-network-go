"""Real Docker/Compose image-only rollout contract, not Dash consensus proof.

Two tiny immutable images stand in for services. We exercise actual container
replacement, unchanged Core process/config, persistent mounts and a lost response
using the production upgrade adapter. No cloud credentials or live hosts.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
from test_worker import request, worker
from test_upgrade import Upgrade


class Disposable(Upgrade):
    lose_after_apply = False
    remove_before_apply = False

    def verify_instance(self):
        self.require(os.environ.get('DASHNET_DISPOSABLE_CI')=='1','disposable-ci-only')

    def core_status(self):
        value=self.inspect_container('core')
        self.require(value and value['State']['Running'],'fixture-core-not-running')
        return dict(containerId=value['Id'],startedAt=value['State']['StartedAt'],
                    configSha256=hashlib.sha256((self.root/'core/dash.conf').read_bytes()).hexdigest(),
                    genesis='c'*64)

    def docker(self,*args,timeout=120):
        if self.remove_before_apply and args[0]=='compose' and 'up' in args:
            self.remove_before_apply=False
            super().docker('rm','-f',self.container_name('drive'))
            raise worker.Failure('simulated-replacement-create-failure')
        result=super().docker(*args,timeout=timeout)
        if self.lose_after_apply and args[0]=='compose' and 'up' in args:
            self.lose_after_apply=False
            raise worker.Failure('simulated-lost-apply-response')
        return result


def pins(q):
    return {v['component']:v['pinned'] for v in q['target']['images']}


def baseline(w):
    c=w.core_status()
    values={k:w.inspect_container(k) for k in ['drive','tenderdash','dapi','gateway']}
    return dict(coreId=c['containerId'],coreStarted=c['startedAt'],coreConfig=c['configSha256'],
                coreGenesis=c['genesis'],containers={k:v['Id'] for k,v in values.items()},
                restarts={k:v['RestartCount'] for k,v in values.items()})


def main():
    first,second=os.environ['UPGRADE_FROM'],os.environ['UPGRADE_TO']
    assert first!=second and '@sha256:' in first and '@sha256:' in second
    q=request();q['target'].update(name='validator-upgrade-ci',role='validator')
    q['target']['images']=[dict(component=k,pinned=first) for k in ['core','drive','tenderdash','dapi','gateway','helper']]
    with tempfile.TemporaryDirectory(prefix='dashnet-upgrade-ci-') as tmp:
        root=Path(tmp);root.chmod(0o700)
        (root/'owner').write_text(q['context']['computePlanId']+':'+q['target']['instanceId'])
        (root/'ready').write_text(q['context']['bootstrapId'])
        w=Disposable(q,root,root/'lock')
        w.atomic('deployment.json',dict(planId=q['context']['planId']))
        w.atomic('core/dash.conf','private-fixture-config-preserve')
        w.atomic('platform/persistent/sentinel','original-state')
        def service(name):
            value=w.service(name,first)
            value.update(entrypoint=['/bin/sh','-c'],command=["trap 'exit 0' TERM; while :; do sleep 1; done"],
                         stop_grace_period='2s',volumes=[str(root/'platform/persistent')+':/data'])
            return value
        try:
            w.compose('core',dict(core=service('core')))
            w.compose('platform',{k:service(k) for k in ['drive','tenderdash','dapi','gateway']})
            initial=baseline(w)
            before=pins(q);after=before.copy();after['tenderdash']=second
            q.update(action='upgrade-stage',upgrade=dict(id='d'*64,previousId='',
                      **{'from':before},to=after,preserve=initial))
            w.execute()
            assert baseline(w)==initial,'staging changed a running service'
            q['action']='upgrade-apply';w.execute()
            current=baseline(w)
            assert current['coreId']==initial['coreId'] and current['coreStarted']==initial['coreStarted']
            assert current['containers']['tenderdash']!=initial['containers']['tenderdash']
            for k in ['drive','dapi','gateway']:
                assert current['containers'][k]==initial['containers'][k],k+' unexpectedly replaced'
            print('Real Tenderdash-only image replacement preserves Core and the other service processes.',flush=True)
            before=after;after=before.copy()
            for k in ['drive','dapi','gateway','helper']:after[k]=second
            q['target']['images']=[dict(component=k,pinned=v) for k,v in before.items()]
            q['upgrade']=dict(id='e'*64,previousId='d'*64,**{'from':before},to=after,preserve=current)
            w.remove_before_apply=True
            try:w.execute()
            except worker.Failure as e:assert str(e)=='simulated-replacement-create-failure'
            else:raise AssertionError('expected failure after removing old selected container')
            assert w.inspect_container('drive') is None
            assert w.read('upgrade.json')['phase']=='applying'
            w=Disposable(q,root,root/'lock')
            q['action']='upgrade-stage';w.execute()
            assert w.inspect_container('drive') is None,'staging started a service'
            q['action']='upgrade-apply'
            w.lose_after_apply=True
            try:w.execute()
            except worker.Failure as e:assert str(e)=='simulated-lost-apply-response'
            else:raise AssertionError('expected accepted operation with lost acknowledgement')
            interrupted=baseline(w)
            assert w.read('upgrade.json')['phase']=='applying'
            resumed=Disposable(q,root,root/'lock')
            resumed.execute()
            assert baseline(resumed)==interrupted,'resume replaced already-correct containers'
            assert resumed.read('upgrade.json')['phase']=='applied'
            assert resumed.core_status()['configSha256']==initial['coreConfig']
            assert (root/'platform/persistent/sentinel').read_text()=='original-state'
            for k in ['drive','tenderdash','dapi','gateway']:
                assert resumed.docker('exec',resumed.container_name(k),'cat','/data/sentinel').decode()=='original-state'
            print('Real Platform image replacement survives a lost response; Core, configuration and persistent mounts are preserved.',flush=True)
        finally:
            for k in ['drive','tenderdash','dapi','gateway','core']:
                subprocess.run(['docker','rm','-f',w.container_name(k)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=False)


if __name__=='__main__':main()
