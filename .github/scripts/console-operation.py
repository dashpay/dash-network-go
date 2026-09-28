#!/usr/bin/env python3
"""Execute an immutable console request. Never regenerate a reviewed plan."""
import hashlib,json,os,re,subprocess,sys,tempfile
from pathlib import Path
UUID=re.compile(r'^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$')
DIGEST=re.compile(r'^[a-f0-9]{64}$')
def validate(bundle,env):
 network=env['NETWORK'];action=env['OPERATION'];confirm=env['CONFIRM'];request=env['REQUEST_ID']
 if network not in ['testnet','devnet-moutai'] or action not in ['upgrade','deploy','doctor','import','enroll'] or not UUID.fullmatch(request) or not DIGEST.fullmatch(confirm):raise ValueError('Invalid explicit request')
 if bundle['network']!=network or bundle['action']!=action or bundle['planId']!=confirm or not DIGEST.fullmatch(bundle['binarySHA256']):raise ValueError('Request/artifact identity mismatch')
 artifact=bundle['artifact'];snapshot=artifact['snapshot'] if action in ['upgrade','deploy'] else artifact;fleet=snapshot['fleet']
 if artifact['id']!=confirm or fleet['metadata']['name']!=network or fleet['accountId']!=env['DASHNET_AWS_ACCOUNT'] or fleet['region']!=env['AWS_REGION'] or fleet['stateTable']!=env['DASHNET_STATE_TABLE']:raise ValueError('Artifact is outside environment authority')
 targets=bundle['targets'];names={t['name'] for t in fleet['targets']}
 if not targets or len(set(targets))!=len(targets) or any(t not in names for t in targets):raise ValueError('Invalid selected targets')
 if action in ['upgrade','deploy'] and (artifact['operation']!=action or sorted(artifact['targets'])!=sorted(targets)):raise ValueError('Plan target mismatch')
 return artifact,snapshot,fleet

def main():
 env=os.environ;bucket=env['BUCKET'];network=env['NETWORK'];rid=env['REQUEST_ID']
 if not UUID.fullmatch(rid) or network not in ['testnet','devnet-moutai']:raise ValueError('Invalid scope')
 prefix=f's3://{bucket}/managed/{network}/requests/{rid}/';result_prefix=f's3://{bucket}/managed/{network}/results/{rid}/{env["GITHUB_RUN_ATTEMPT"]}/'
 def cp(source,destination):subprocess.run(['aws','s3','cp','--only-show-errors',source,destination],check=True,stdout=subprocess.DEVNULL)
 with tempfile.TemporaryDirectory(prefix='dash-console-',dir=env['RUNNER_TEMP']) as folder:
  work=Path(folder);work.chmod(0o700)
  for name in ['input.json','dashnet','known_hosts']:cp(prefix+name,str(work/name))
  bundle=json.loads((work/'input.json').read_text());artifact,snapshot,fleet=validate(bundle,env)
  binary=work/'dashnet'
  if hashlib.sha256(binary.read_bytes()).hexdigest()!=bundle['binarySHA256']:raise ValueError('Frozen binary checksum mismatch')
  binary.chmod(0o700)
  key=work/'key';key.write_text(env.pop('SSH_KEY'));key.chmod(0o600)
  for name,value in [('artifact.json',artifact),('snapshot.json',snapshot),('manifest.json',fleet)]:
   p=work/name;p.write_text(json.dumps(value));p.chmod(0o600)
  access=['--ssh-key',str(key),'--known-hosts',str(work/'known_hosts')]
  def run(command,args,name):
   with (work/(name+'.log')).open('w') as log:
    code=subprocess.run([str(binary),'managed-'+command,*args,'--timeout','100m','--out',str(work/(name+'.json'))],stdout=log,stderr=log).returncode
   print(name+': '+('passed' if not code else 'stopped'),flush=True)
   return code
  code=1
  try:
   action=bundle['action'];targets=','.join(bundle['targets'])
   if action in ['upgrade','deploy']:
    status=run('operation',['--manifest',str(work/'manifest.json')],'journal')
    state=json.loads((work/'journal.json').read_text()) if status==0 else None
    record=state.get('record',{}) if state else {}
    if state and state.get('owner'):raise ValueError('Another runner owns this network')
    # Do not re-enroll an unfinished matching operation: resume its exact recipe.
    if record.get('operationId')!=artifact['id']:
     if record.get('operationId') and record.get('phase') not in ['complete','enrolled']:raise ValueError('Recover the unfinished operation first')
     code=run('enroll',['--snapshot',str(work/'snapshot.json'),'--confirm',snapshot['id'],'--nodes',targets,*access],'enrollment')
     if code:raise ValueError('Selected workload enrollment did not complete')
    code=run(action,['--plan',str(work/'artifact.json'),'--confirm',artifact['id'],*access],'execution')
   elif action=='enroll':code=run(action,['--snapshot',str(work/'snapshot.json'),'--confirm',snapshot['id'],'--nodes',targets,*access],'enrollment')
   elif action=='doctor':code=run(action,['--snapshot',str(work/'snapshot.json'),*access],'health')
   else:code=run('import',['--manifest',str(work/'manifest.json'),*access],'inventory')
  finally:
   for p in work.iterdir():
    if p.name.endswith('.log') or p.name in ['journal.json','enrollment.json','execution.json','health.json','inventory.json']:cp(str(p),result_prefix+p.name)
   receipt={'network':network,'action':bundle['action'],'planId':bundle['planId'],'requestId':rid,'exitCode':code,'targets':bundle['targets']}
   p=work/'receipt.json';p.write_text(json.dumps(receipt));cp(str(p),result_prefix+p.name)
   with open(env['GITHUB_STEP_SUMMARY'],'a') as summary:summary.write(f'## {network} · {bundle["action"]}\n\nRequest `{rid}` · exit `{code}`. Detailed observations remain in the private operation log.\n')
  sys.exit(code)
if __name__=='__main__':main()
