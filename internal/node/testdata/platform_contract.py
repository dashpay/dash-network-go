"""Real Tenderdash/Envoy configuration and TLS/gRPC contract, no chain claim.

DAPI intentionally has no Drive/Core backend: its successful getStatus response
must NOT be accepted as healthy by our worker. Disposable Docker CI only.
"""
import base64
import json
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import time
from test_worker import worker, request

class Disposable(worker.Worker):
    def verify_instance(self):
        self.require(os.environ.get('DASHNET_DISPOSABLE_CI')=='1','disposable-ci-only')

def main():
    q=request();q['target']['role']='validator'
    q['target']['images']=json.loads(os.environ['PLATFORM_IMAGES'])
    node_id=hashlib.sha256(bytes(32)).hexdigest()[:40]
    q['peers']=[dict(name=q['target']['name'],address=q['target']['peerAddress'],nodeId=node_id,operatorPublicKey='b'*96,proTxHash='e'*64)]
    q['genesisCoreHeight']=160
    with tempfile.TemporaryDirectory(prefix='dashnet-platform-ci-') as tmp:
        root=Path(tmp);root.chmod(0o700)
        (root/'owner').write_text(q['context']['computePlanId']+':'+q['target']['instanceId'])
        (root/'ready').write_text(q['context']['bootstrapId'])
        w=Disposable(q,root,root/'lock')
        # Match the executor's Ed25519 certificate profile and loopback SAN.
        subprocess.run(['openssl','req','-new','-x509','-newkey','ed25519','-nodes','-keyout',str(root/'key.pem'),'-out',str(root/'cert.pem'),'-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        key_der=subprocess.check_output(['openssl','pkey','-in',str(root/'key.pem'),'-outform','DER'])
        public_der=subprocess.check_output(['openssl','pkey','-in',str(root/'key.pem'),'-pubout','-outform','DER'])
        private=key_der[-32:]+public_der[-32:]
        node_id=hashlib.sha256(public_der[-32:]).hexdigest()[:40]
        q['peers'][0]['nodeId']=node_id
        w.atomic('secrets.json',dict(rpcPassword='private-ci-only',platformNodeID=node_id,operatorPublicKey='b'*96,nodePrivateKey=base64.b64encode(private).decode(),tlsCertificate=(root/'cert.pem').read_text(),tlsPrivateKey=(root/'key.pem').read_text()))
        w.platform_files()
        try:
            td=w.run(['docker','run','--rm','--user','0:0','--network','none','--entrypoint','tenderdash','-v',str(root/'platform/tenderdash')+':/tenderdash',w.images['tenderdash'],'show-node-id','--home','/tenderdash']).decode().strip()
            assert td==node_id, 'Tenderdash node-key readback'
            print('Tenderdash accepts native configuration and node key.',flush=True)
            w.run(['docker','run','--rm','--user','0:0','--network','none','--entrypoint','envoy','-v',str(root/'platform/envoy.json')+':/etc/envoy/config.json:ro','-v',str(root/'platform/tls')+':/tls:ro',w.images['gateway'],'-c','/etc/envoy/config.json','--mode','validate'])
            print('Envoy accepts native config and generated TLS identity.',flush=True)
            services=w.platform_services()
            w.compose('platform',{k:services[k] for k in ['dapi','gateway']})
            deadline=time.monotonic()+45
            while True:
                try: result=w.dapi_status();break
                except Exception:
                    if time.monotonic()>deadline: raise
                    time.sleep(1)
            software=worker.protobuf(worker.protobuf(result[1])[1])
            assert software.get(1), 'DAPI software version missing'
            assert not software.get(2), 'Fixture unexpectedly has Drive'
            try: w.platform_status()
            except worker.Failure: pass
            else: raise AssertionError('Missing Drive/consensus was reported healthy')
            print('Real TLS/HTTP2/gRPC getStatus succeeds; absent consensus is correctly unhealthy.',flush=True)
        finally:
            for name in ['drive','tenderdash','dapi','gateway']:
                subprocess.run(['docker','rm','-f',w.container_name(name)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=False)

if __name__=='__main__': main()
