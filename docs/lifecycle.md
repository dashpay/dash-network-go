# Devnet lifecycle and human recovery

`deployment-plan`, `deploy`, `doctor`, and `stop` extend provisioning/bootstrap.
They use the **same network journal and exclusive runner claim**, direct AWS SDK
inspection, and authenticated SSH. No Terraform, Ansible or Dashmate runtime.

## Supported profile and evidence

Initial profile: `devnet-core23-platform4-tenderdash1`, Ubuntu 24.04, IPv4, an
existing VPC/subnet/security groups, **13–25 validators, exactly one wallet,
optional one miner and fullnodes**. Without a miner, the wallet mines. Seed groups
are rejected by this profile, not silently ignored. Testnet mutations and adoption
of existing networks are not supported. The small compute example is deliberately
not a valid full-network topology; use `examples/devnet-lifecycle.yaml` as a starting
point, replacing all example AWS identifiers before planning.

The profile owns explicit compatibility assumptions; it is not an upstream
release guarantee. Image tags are resolved once before bootstrap. The deployment
plan binds the existing bootstrap lock, exact instances/IPs, chain identity,
genesis time, initial protocol version and executable's node recipe digest.
`--protocol` is the **Platform protocol number**, not its software major version.
Check the matching release source; do not infer it from `4.x`.

Canonical Core v23 quorums used by this profile:

| Purpose | Type | Size/minimum/threshold | DKG interval | Active quorums |
|---|---|---|---|---|
| ChainLocks | 101 | 12/7/6 | 24 | 4 |
| Platform | 107 | 12/9/8 | 24 | 4 |
| Rotated InstantSend | 105 | 8/6/4 | 48 | 2 |

Config references: [Core v23 parameters](https://github.com/dashpay/dash/blob/v23.0.0/src/llmq/params.h),
[registration RPC](https://github.com/dashpay/dash/blob/v23.0.0/src/rpc/evo.cpp),
[Tenderdash 1.8 config](https://github.com/dashpay/tenderdash/blob/v1.8.0/config/config.go),
and Platform/Dashmate native container/env/protobuf definitions (review reference
`dashpay/platform@3fac2dd204`, not an execution dependency). The profile overrides
quorum sizes/counts explicitly rather than inheriting drifting defaults.

Tests distinguish three levels:

1. Fake-cloud/SSH orchestration and failure-path tests: scope, claims, lost
   responses, resume, unchanged genesis, stale/unknown/unreachable targets.
2. Disposable **real-container** contracts: Core config, wallet, signed EvoNode
   registration, transaction replay, persistent collateral locks and mining;
   Drive native configuration and its missing-ChainLock startup gate; Tenderdash
   config/node identity; Envoy TLS and DAPI gRPC. No AWS credentials/resources.
3. **Not yet proved:** a real AWS fleet forming quorums and advancing Platform
   consensus end-to-end. No full-network success claim follows from levels 1–2.

## Human command sequence

First follow [provisioning](provisioning.md) and [bootstrap](bootstrap.md). Retain
all plans, the binary/checksum and verified instance-scoped `known_hosts` in private
backup storage. Plans contain private topology but no wallet/validator keys.

```sh
# Read-only: bind the completed bootstrap to exact running instances and genesis.
dashnet deployment-plan --bootstrap-plan out/bootstrap-plan.json \
  --protocol ACTUAL_PROTOCOL_NUMBER --profile YOUR_AWS_PROFILE \
  --out out/deployment.json

# Review the generated file and copy its exact .id. Starts this new devnet only.
dashnet deploy --plan out/deployment.json --confirm EXACT_DEPLOYMENT_ID \
  --ssh-key /secure/key --known-hosts /secure/known_hosts \
  --profile YOUR_AWS_PROFILE --timeout 60m --out out/deployed.json

# Independent read-only health: nonzero exit for unhealthy/unknown targets.
dashnet doctor --plan out/deployment.json \
  --ssh-key /secure/key --known-hosts /secure/known_hosts \
  --profile YOUR_AWS_PROFILE --timeout 3m --out out/health-now.json

# Inspect progress/errors from another authorized machine, even after runner loss.
dashnet operation --plan out/ec2-plan.json --profile YOUR_AWS_PROFILE
```

The controller runs named stages:

1. Check exact AWS ownership/placement/addresses and **all hosts** before mutation.
2. Start Core, verify the same devnet genesis on every target.
3. Persist wallet/spork/BLS/Ed25519/TLS identities; configure Core for masternodes.
4. Mine local devnet collateral; register every validator serially on the wallet.
5. Activate available devnet sporks, start persistent mining, wait for READY
   masternodes, all three quorum types and ChainLocks.
6. Persist the initial chainlocked Core height once, render immutable Platform
   genesis/node identity, and start Drive/Tenderdash/rs-dapi/Envoy on validators.
7. Observe all nodes twice: advancing Core and Platform, common-height Platform
   block-hash agreement, exact identities/images, no container replacement during
   observation, and successful TLS→HTTP/2→DAPI gRPC with responsive Drive/TD.

Host work is bounded (four hosts concurrently); wallet transactions are serial.
The wallet's signed registration bytes and collateral output are fsynced **before**
broadcast. Retries reconcile/resend those same bytes, never fund a replacement
registration. Collateral is persistently locked and restored before further
funding. Only unlocked spendable outputs count towards available funds.

`network-ready` means a point-in-time observation, not permanent health. The legacy
compute `applicationHealth: unknown` stays deliberately unchanged; fresh chain
observations are in `deployment` and `doctor`. No target disappears on failure.

## Host services, networking and private material

Host prerequisite: the bootstrap runtime plus Ubuntu Python 3 and curl with HTTP/2.
The Go executable embeds a fixed, digest-bound Python worker; input is typed JSON
on SSH stdin, never arbitrary shell/RPC. Python is not required on the operator's
machine. Host flock covers each complete operation, including stop.

All data/config is under root-owned `/var/lib/dashnet` (0700). Private files are
0600: RPC/BLS/spork/Ed25519/TLS keys, wallet data and signed transactions stay on
their owning hosts. Never upload this tree, Docker inspect/config output, or raw
wallet/RPC output into public Actions artifacts or chat. Take encrypted backups
through a separately authorized backup procedure. The DynamoDB journal contains
only public identities and operational facts, not recovery key material.

Services use host networking with explicit port bindings:

| Service | Bind/port |
|---|---|
| Core P2P | all interfaces, 20001 |
| Core RPC / ZMQ | loopback, 20002 / 29998 |
| Tenderdash P2P | all interfaces, 26656 |
| Tenderdash RPC | loopback, 26657 |
| Drive ABCI / gRPC | loopback, 26658 / 26670 |
| DAPI gRPC / JSON | loopback, 3010 / 3009 |
| Envoy TLS gateway | all interfaces, 1443 |

Existing SGs must permit fleet Core/Tenderdash P2P and operator SSH. Peer discovery
uses private VPC IPs with Core's devnet private-address setting. The tool does not
open SGs, create DNS/load balancers, or provision public CA certificates. TLS uses
persisted per-node self-signed certificates (one-year lifetime); probes pin that
certificate, never `--insecure`. Public browser endpoints/certificate rotation
remain separate work. The helper image is cached for future adapters, **not run**.

## Interruption, stop and resume

On a normal failure/cancellation, the shared journal retains the stage/error and
releases the controller claim. SSH disconnection may leave host work running;
the host lock then refuses overlapping requests. An abruptly killed controller
can leave its non-expiring shared claim. Inspect the printed runner ID and follow
[the stopped-runner unlock procedure](provisioning.md) only after proving the old
controller and remote operations are inactive. Do not steal a claim by timeout.

Resume with the **same deployment plan and binary**, using `deploy` again. It
rechecks the fleet, restores the same identities/registrations and retains genesis.
Do not regenerate a deployment plan or change generation to bypass an interruption.
Address/instance, image, identity or genesis drift fails closed.

Read-only requests use a separately guarded observation adapter. Compatible RPC
response decoding can be repaired while retaining the exact original mutation
recipe in a reviewed recovery build. The adapter refuses every mutation action;
it cannot configure, register, start or stop services. Retain that build's source
revisions, recipe hash and executable checksum alongside the original plan.
This is not permission to replace a bound mutation recipe or edit plan IDs.

To halt this tool's owned containers while preserving recovery data:

```sh
dashnet stop --plan out/deployment.json --confirm EXACT_DEPLOYMENT_ID \
  --ssh-key /secure/key --known-hosts /secure/known_hosts --profile YOUR_AWS_PROFILE
```

Stop is disruptive and explicit. It verifies every target and stopped-container
readback, stops the mining host before withdrawing validators, preserves all
disks/identities, and **does not terminate EC2 or stop
billing**. Resume using `deploy`; there is no implicit reset, prune or rollback.
If the mining host cannot be verified stopped, other hosts are not stopped by
that invocation; inspect the unresolved target before retrying.
There is no generalized destroy/cleanup executor. Existing-state image changes use
the separate [upgrade executor](upgrades.md). Do not use this create profile to upgrade managed testnet or live
legacy networks.

## GitHub Actions execution

[`Operate managed devnet`](../.github/workflows/operate.yml) invokes the same
`provision`, `bootstrap`, `deploy`, `doctor` and `stop` CLI commands. It only runs
from `main`; PR workflows never receive fleet credentials. It is intentionally
inert until an administrator configures:

- Protected **`devnet-operations`** environment, allowed main-branch deployments,
  required reviewer policy, and narrowly scoped OIDC role trust for this repo and
  environment. IAM must constrain exact account/network and state/artifact paths.
- Environment variable `DASHNET_AWS_REGION` and optionally `DASHNET_RUNNER`
  (a runner label with private VPC reachability). Role maximum session duration
  must permit two hours.
- Environment secrets `DASHNET_AWS_ROLE`, `DASHNET_ARTIFACT_BUCKET` (private
  infrastructure identifiers, masked in this public repository), `DASHNET_SSH_KEY`,
  and independently verified host trust in
  the private bundle; use separate credentials/policy for managed testnet later.
- Versioned private S3 paths `networks/DEVNET_NAME/ec2-plan.json`,
  `bootstrap-plan.json`, `deployment.json`, `known_hosts`. Restrict writes as
  strongly as SSH trust itself. Upload reviewed stage artifacts; deployment plans
  are generated only after bootstrap. Do not let untrusted PRs replace bundles.

Dispatch the exact network/action and reviewed plan ID. GitHub concurrency queues
same-network runs; the DynamoDB claim also coordinates local humans/agents. Reports
are uploaded under private `operations/NETWORK/RUN_ID-ATTEMPT.json`. No private
plan, trust file, key or report is published as a GitHub artifact. SSH files are
removed on step exit. The public job summary only identifies network/action/result.
Public live logs
forward only exact stage names; full operator diagnostics stay in the private S3
operation log. Workflow-dispatch network names themselves are public metadata;
use a private operations repository if those names are confidential.

Retain the exact executable for resume. A later main revision with a changed
recipe refuses the old plan; recover with the cached matching binary locally
(or a separately reviewed workflow pinned to that binary), never by rewriting IDs.
Dashboard or GitHub availability is not required for the terminal path. No OIDC
role or environment for existing Moutai/testnet is implicitly provisioned. The
disposable acceptance proof uses separate, temporary authority; it is not a
production workflow credential.

### OIDC subject format

Inspect `gh api repos/dashpay/dash-network-go/actions/oidc/customization/sub`
before writing IAM trust. This repository currently uses GitHub's immutable
owner/repository subject: `sub_claim_prefix` is
`repo:dashpay@11511719/dash-network-go@1384209245`. An environment job appends
`:environment:ENVIRONMENT_NAME`. The older name-only `repo:dashpay/dash-network-go`
subject does not match this repository. Bind the exact observed prefix and
protected environment, with audience `sts.amazonaws.com`; never use a broad
repository/organization wildcard to get past a failed authentication. Never print
the raw OIDC token. Check these settings again if authority is deliberately moved.
