"""Join an existing chain on fresh owned fullnodes. No wallet, mining or ProTx."""

class JoinWorker(Worker):
    def join_config(self):
        j = self.q['join']
        lines = ["testnet=1" if j['chainType']=='testnet' else "devnet="+j['coreNetwork'][7:],
                 "daemon=0", "server=1", "disablewallet=1", "txindex=1", "dnsseed=0",
                 "discover=0", "allowprivatenet=1", "listen=1", "maxconnections=128",
                 "rpcuser=dashnet", "rpcpassword="+self.secret()['rpcPassword'],
                 "rpcallowip=127.0.0.1"]
        lines += j.get('options') or []
        lines += ["[test]" if j['chainType']=='testnet' else "[devnet]",
                  "port="+str(self.ports['coreP2P']), "rpcport="+str(self.ports['coreRPC']),
                  "rpcbind=127.0.0.1", "bind=0.0.0.0"]
        lines += ["addnode="+p for p in j['peers']]
        return '\n'.join(lines)+'\n'

    def join_start(self):
        self.require(self.t['role']=='fullnode', 'join-fullnodes-only')
        self.immutable('join.json',self.q['join'])
        config=self.join_config();path=self.root/'core/dash.conf'
        self.require(not path.exists() or path.read_text()==config,'join-config-drift')
        self.atomic('core/dash.conf',config)
        (self.root/'core/data').mkdir(mode=0o700,parents=True,exist_ok=True)
        service=self.service('core',self.images['core'])
        service.update(entrypoint=['dashd'],command=['-conf=/etc/dash/dash.conf','-datadir=/var/lib/dash'],
                       volumes=[str(path)+':/etc/dash/dash.conf:ro',str(self.root/'core/data')+':/var/lib/dash'])
        old=self.inspect_container('core')
        if old:
            self.verify_image(old,self.images['core'])
            self.require(old['Config']['Cmd']==service['command'] and old['Config']['Entrypoint']==service['entrypoint'] and old['HostConfig']['NetworkMode']=='host','join-container-config-drift')
            marker=self.read('join-container.json')
            if marker:self.require(marker['id']==old['Id'],'join-container-replaced')
        self.compose('core',{'core':service})
        value=self.inspect_container('core');self.atomic('join-container.json',dict(id=value['Id']))
        deadline=time.monotonic()+120
        while True:
            try:return self.join_status()
            except (RPCFailure,urllib.error.URLError):
                self.require(time.monotonic()<deadline,'join-rpc-readiness');time.sleep(2)

    def join_status(self):
        j=self.q['join'];c=self.inspect_container('core')
        self.require(c is not None and c['State']['Running'],'join-core-not-running')
        self.verify_image(c,self.images['core'])
        marker=self.read('join-container.json')
        self.require(marker is not None and marker['id']==c['Id'],'join-container-replaced')
        self.require(self.read('join.json')==j,'join-contract-drift')
        path=self.root/'core/dash.conf'
        self.require(self.read('secrets.json') is not None,'missing-rpc-identity')
        self.require(path.read_text()==self.join_config(),'join-config-drift')
        info=self.rpc('getblockchaininfo')
        self.require(info['chain']==j['coreNetwork'],'join-wrong-network')
        genesis=self.rpc('getblockhash',[1 if j['chainType']=='devnet' else 0])
        self.require(genesis==j['genesis'],'join-wrong-genesis')
        checkpoint=self.rpc('getblockhash',[j['checkpointHeight']]) if info['blocks']>=j['checkpointHeight'] else ''
        self.require(not checkpoint or checkpoint==j['checkpointHash'],'join-wrong-checkpoint')
        try:chainlock=self.rpc('getbestchainlock')['height']
        except RPCFailure:chainlock=0
        return dict(genesis=genesis,checkpointHash=checkpoint,height=info['blocks'],headers=info['headers'],
                    synced=not info['initialblockdownload'] and bool(checkpoint),ibd=info['initialblockdownload'],
                    peers=self.rpc('getnetworkinfo')['connections'],containerId=c['Id'],restarts=c['RestartCount'],
                    configSha256=hashlib.sha256(path.read_bytes()).hexdigest(),chainLockHeight=chainlock)

    def execute(self):
        self.verify_instance();action=self.q['action']
        self.require(action in ['join-start','join-status'],'action-refused')
        self.require(not self.lock.is_symlink(),'symlink-refused')
        with self.lock.open('a') as lock:
            try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise Failure('host-busy') from None
            self.owned()
            self.require(self.t['role']=='fullnode' and set(self.images)=={'core'},'join-fullnodes-only')
            if action=='join-start':
                prior=self.read('deployment.json')
                if not prior:self.atomic('deployment.json',dict(planId=self.c['planId'],kind='CoreJoin'))
                for component in ['miner','drive','tenderdash','gateway','dapi']:
                    self.require(self.inspect_container(component) is None,'join-unexpected-service')
            self.stage=action
            core=self.join_start() if action=='join-start' else self.join_status()
            return dict(instanceId=self.t['instanceId'],planId=self.c['planId'],action=action,core=core)

Worker=JoinWorker
