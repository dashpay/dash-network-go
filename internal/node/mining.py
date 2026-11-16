# Optional, plan-bound native miner leases; no mutation of the base recipe.
class MiningWorker(Worker):
    def receipt(self, marker, miner):
        return dict(operationId=marker['operationId'],containerId=miner['Id'],
                    paused=bool(marker.get('paused')) and not miner['State']['Running'],
                    expiresAt=marker.get('expiresAt',0))

    def arm(self, marker):
        recovery = '''import fcntl,json,os,subprocess,sys,tempfile
from pathlib import Path
root=Path(sys.argv[1]);token=sys.argv[2]
with open('/run/dashnet-bootstrap.lock','a') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 path=root/'rollout-mining.json'
 if not path.exists():sys.exit(0)
 m=json.loads(path.read_text())
 if not m.get('paused') or m.get('token')!=token:sys.exit(0)
 v=json.loads(subprocess.check_output(['docker','inspect',m['containerId']],timeout=30))[0]
 if v['Id']!=m['containerId'] or v['Image']!=m['imageId']:sys.exit(1)
 subprocess.run(['docker','start',m['containerId']],check=True,timeout=60,stdout=subprocess.DEVNULL)
 m['paused']=False
 fd,tmp=tempfile.mkstemp(dir=root,prefix='.rollout-mining-')
 with os.fdopen(fd,'w') as f:json.dump(m,f);f.flush();os.fsync(f.fileno())
 os.replace(tmp,path)
'''
        self.run(['systemd-run','--quiet','--unit=dashnet-native-mining-'+marker['token'],
                  '--on-active=900s','--collect','--timer-property=AccuracySec=1s',
                  '/usr/bin/python3','-c',recovery,str(self.root),marker['token']],timeout=30)

    def execute(self):
        self.verify_instance()
        self.require(self.t['name']==self.c['miningNodeName'] and self.t['role'] in ['wallet','miner'],'mining-target')
        control=self.q.get('miningControl') or {}
        self.require(re.fullmatch('[0-9a-f]{64}',control.get('operationId','')) and
                     re.fullmatch('[0-9a-f]{64}',control.get('containerId','')),'mining-policy')
        with self.lock.open('a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.owned()
            miner=self.inspect_container('miner')
            self.require(miner and miner['Id']==control['containerId'],'mining-container-drift')
            self.verify_image(miner,self.images['core'])
            marker=self.read('rollout-mining.json')
            action=self.q['action']
            if action=='rollout-mining-pause':
                if marker and marker.get('paused'):
                    self.require(marker['operationId']==control['operationId'] and
                                 marker['containerId']==miner['Id'] and marker['expiresAt']>time.time() and
                                 not miner['State']['Running'],'mining-existing-pause')
                else:
                    self.require(miner['State']['Running'],'mining-not-originally-active')
                    marker=dict(operationId=control['operationId'],containerId=miner['Id'],imageId=miner['Image'],
                                token=secrets.token_hex(16),paused=True,expiresAt=int(time.time())+900)
                    self.atomic('rollout-mining.json',marker)
                    self.arm(marker) # independent recovery precedes any stop
                    self.mine_pause()
                deadline=time.monotonic()+90
                while True:
                    commands=self.rpc('getrpcinfo').get('active_commands')
                    self.require(isinstance(commands,list),'mining-rpc-observation')
                    if not any(c.get('method') in ['generate','generatetoaddress','generateblock'] for c in commands):break
                    self.require(time.monotonic()<deadline,'mining-rpc-did-not-drain')
                    time.sleep(2)
            else:
                self.require(marker and marker['operationId']==control['operationId'] and
                             marker['containerId']==miner['Id'] and marker['imageId']==miner['Image'],'mining-owner-or-drift')
                if action=='rollout-mining-renew':
                    self.require(marker.get('paused') and not miner['State']['Running'] and marker['expiresAt']>time.time(),'mining-lease-lost')
                    marker=dict(marker,token=secrets.token_hex(16),expiresAt=int(time.time())+900)
                    self.arm(marker);self.atomic('rollout-mining.json',marker)
                elif action=='rollout-mining-resume':
                    self.docker('start',miner['Id'],timeout=60)
                    marker['paused']=False;self.atomic('rollout-mining.json',marker)
                else:self.require(action=='rollout-mining-status','mining-action')
            miner=self.inspect_container('miner')
            self.require(miner and miner['Id']==control['containerId'],'mining-container-drift')
            receipt=self.receipt(marker,miner)
            if action in ['rollout-mining-pause','rollout-mining-renew']:
                self.require(receipt['paused'],'mining-pause-unverified')
            if action=='rollout-mining-resume':self.require(miner['State']['Running'],'mining-resume-unverified')
            return dict(instanceId=self.t['instanceId'],planId=self.c['planId'],action=action,miningPause=receipt)

Worker=MiningWorker
