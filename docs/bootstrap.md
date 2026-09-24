# Authenticated node bootstrap

This stage prepares **new devnet instances created by this tool**. It does not
adopt existing networks, start Core/Platform, create keys/wallets, register
masternodes, open ports, or reset any data. `hosts-ready` is not network health.

## Prerequisites and trust

- Finish `provision` and retain the exact EC2 plan. The regional DynamoDB record
  must report `compute-ready`, and every instance must still match its recorded
  ownership, launch identity, placement, security groups, and IMDSv2 setting.
- Use dedicated Ubuntu **24.04** cloud images, amd64 or arm64, with curl, Python 3,
  util-linux/flock, cloud-init completed, and passwordless sudo for the chosen
  SSH login. Root login is also supported if explicitly selected.
- The controller must reach the configured SSH port. Private IPv4 is the default;
  public IPv4 must also have been enabled in the compute plan. There is no
  jump-host/SSM transport yet. No firewall changes are made by bootstrap.
- Keep the selected private key locally with mode 0600 or stricter. Currently an
  unencrypted deployment key is required; agent/encrypted-key support is not yet
  implemented. The CLI never copies that key into plans, journals, or nodes.
- Supply an independently verified `known_hosts` file. Obtain each host's public
  key through a trusted channel such as the authenticated AWS serial console
  (where available) or an established host-key provisioning process. A raw
  `ssh-keyscan` result alone is **not** authentication. Host trust enrollment is
  currently an operator step, not an automatically implemented workflow.

Host keys are addressed by an immutable alias, not the instance's changing IP:

```text
i-0123456789abcdef0.us-east-1.123456789012.dashnet ssh-ed25519 <verified-public-key>
```

The alias is `INSTANCE_ID.REGION.ACCOUNT_ID.dashnet`. For a nonstandard port use
`[ALIAS]:PORT`, following OpenSSH known_hosts syntax. Missing, changed, or IP-only
entries are rejected. Never disable host-key checking to bypass a failed check.

## Plan, execute, inspect, resume

```sh
# Resolve against the SAME network definition used by the compute plan.
dashnet resolve --network networks/devnet-lab.yaml --out out/lab.lock.json

# Offline: binds compute plan, release lock, transport choices and recipe hash.
dashnet bootstrap-plan --compute-plan out/lab-ec2-plan.json \
  --lock out/lab.lock.json --ssh-user ubuntu --address private \
  --out out/lab-bootstrap-plan.json

# Review the bootstrap plan ID and per-role image digests, then execute.
dashnet bootstrap --plan out/lab-bootstrap-plan.json --confirm BOOTSTRAP_PLAN_ID \
  --ssh-key /secure/deployment-key --known-hosts /secure/lab-known-hosts \
  --profile YOUR_AWS_PROFILE --timeout 30m --out out/lab-hosts-ready.json

# The same network operation record contains both compute and bootstrap progress.
dashnet operation --plan out/lab-ec2-plan.json --profile YOUR_AWS_PROFILE
```

Repeat `bootstrap` with the **same plan and original CLI binary** to resume. Use a
new output filename, or omit `--out`. Recipe changes invalidate old bootstrap
plans; no implicit migration to a new recipe/release is allowed in this stage.
A different bootstrap plan cannot replace an existing one. Keep the verified
binary alongside the plan. These operator artifacts are not public status data.

## What happens

1. Verify AWS account, read the completed compute record, acquire its existing
   non-expiring network claim, and recheck the **whole** EC2 target set.
2. Probe every host over authenticated SSH before any node mutation: Ubuntu/CPU,
   IMDSv2 instance identity, cloud-init, host ownership/lock, existing containers,
   Docker/Compose, and any previously prepared image digests.
3. If all probes pass, prepare unready hosts in deterministic order. Each
   mutation is preceded by a revision-conditional journal checkpoint.
4. On each host, acquire a nonblocking `flock`, claim `/var/lib/dashnet`, install
   missing `docker.io`/`docker-compose-v2` from configured signed Ubuntu package
   repositories, enable/start Docker, and pull role-specific **child-manifest
   digests**. Prebaked Docker/Compose skips package installation. No full package
   upgrade, Docker restart, prune, or container start is requested.
5. Verify image OS, architecture and repository digests. Publish the host-ready
   marker last. Probe again independently and perform a final fleet readback.

Validators cache all six components. Seeds cache Core and Tenderdash. Wallet,
miner, and fullnode roles cache Core only. No Compose service definitions or
application configuration are generated yet. Dedicated data/release directories
are root-owned mode 0700; application-specific ownership comes with the service
stage. Existing containers cause refusal even if they are stopped.

Runtime packages are **not version-locked** in this slice: the recipe requests
Ubuntu's distro packages only when runtime components are missing, and records
the observed Docker/Compose versions. Image artifacts and the recipe are pinned.
A tested prebaked AMI is the repeatable fast path for stable OS dependencies.
Initial execution is serial; bounded fleet parallelism remains future work.

## Failures and recovery

- Failed preflight: no nodes are mutated. The journal retains every target;
  unreachable/mismatched hosts are `unknown`, not successful or silently omitted.
- SSH timeout/lost response: remote work may continue. The record is interrupted,
  and host state is re-probed on resume. The host-side lock refuses overlapping
  work while the previous process (including its package/image children) holds it.
- A completed host whose response/checkpoint was lost is verified and skipped,
  rather than blindly rerunning its preparation.
- A previous `ready` checkpoint is never trusted without a new probe. Missing
  images or runtime drift cannot produce a successful fresh observation.
- A crashed controller leaves its shared claim. Use the existing
  [stopped-runner recovery procedure](provisioning.md#crashed-runner--persistent-claim).
  Stop the old controller **and settle/check any remote bootstrap processes**
  before forcibly releasing a claim. Never remove a lock file to bypass flock;
  that creates another inode/lock and can allow overlapping processes.
- Do not delete DynamoDB state or `/var/lib/dashnet` markers to change plans.
  Partial/corrupt ownership markers fail closed; there is no automated marker
  repair or bootstrap-plan replacement in this release.

Host phases and bounded diagnostics are in `dashnet operation`. Raw SSH stdout,
stderr, package output and potential secrets are not copied into the journal.
Package/image diagnostics remain at `/var/log/dashnet-bootstrap.log` (mode 0600);
use normal authenticated host access to inspect locally. Failures report target,
fixed recipe stage when available, and exit status. Review logs before sharing.

No rollback or cleanup is implicit. Failed runs may leave installed packages,
image layers and directories. Application health remains `unknown` throughout.

## Actions and evidence boundary

The command can run on an appropriately connected Actions runner using OIDC AWS
credentials plus explicitly provisioned SSH identity/trust. No deployment workflow
or fleet credentials are enabled by this PR. Do not publish keys, trust files,
private plans, operation records or detailed logs as public workflow artifacts.

Tests cover fake-AWS/journal recovery, actual loopback SSH authentication and
cancellation, output bounds, and the actual recipe in a disposable Ubuntu
filesystem with mocked metadata/packages/Docker. **This is not a live EC2 launch,
real Docker installation, or Dash-network deployment proof.** Those acceptance
checks still require a scoped disposable network.
