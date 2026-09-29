# Devnet lifecycle and human recovery

`deployment-plan`, `deploy`, `doctor`, and `stop` extend provisioning/bootstrap.
They use the **same network journal and exclusive runner claim**, direct AWS SDK
inspection, and authenticated SSH. No Terraform or Ansible. Every node's services
are **dashmate's own**, rendered by the release being deployed and run from that
release's dashmate compose files; dashmate itself never starts, stops or
reconfigures a service (see [dashmate services](#dashmate-services)).

## Supported profile and evidence

Profile: `devnet-dashmate-compose`, Ubuntu 24.04, IPv4, an
existing VPC/subnet/security groups, an IPAM Elastic IP on every host,
**13–25 validators, exactly one wallet,
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
[registration RPC](https://github.com/dashpay/dash/blob/v23.0.0/src/rpc/evo.cpp).
Service configuration is the deployed release's dashmate: dashmate has no devnet
preset, so the profile sets Drive's validator-set, ChainLock and InstantSend quorum
options to Core's `llmq_devnet*` types above.

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

`deployment-plan --block-time N` sets the Core block interval (8..600 seconds,
default 10): Core's `powtargetspacing` and the miner's cadence. DKG, ChainLock
and upgrade timing follow it; doctor stretches its observation window to at
least two and a half blocks on chains slower than the default. Core must
advance between the samples. An idle Platform chain makes an empty block only
every three minutes (dashmate's `createEmptyBlocksInterval`), so Platform is live
when it advanced or its latest block, by Tenderdash's block time on the node, is
at most four minutes old; nodes must still agree on the block hash.

`deployment-plan --epoch-time N` sets the Platform epoch length (180 seconds, dashmate's minimum, to
30 days, default 3600): Drive's `EPOCH_TIME_LENGTH_S` on every validator, kept
through Platform upgrades. Plans made before the option existed run one hour.

`deploy --core-only` ends once Core is mining with DKG enabled (stage
`quorums`). Quorums then form on their own; running `deploy` again with the
same plan waits for them, starts Platform and completes the health gate.

The controller runs named stages:

1. Check exact AWS ownership/placement/addresses and **all hosts** before mutation.
2. Render every node with its release's dashmate (nothing starts). Every node must
   render with the same dashmate release; the images it selects for Tor and the
   gateway rate limiter are pinned once, by digest, in the shared journal.
3. Start Core, verify the same devnet genesis on every target.
4. Persist wallet/spork/BLS/Ed25519/TLS identities; configure Core for masternodes.
5. Advance the chain at minimum difficulty to the plan's `premineHeight` (4032, as
   the legacy devnet tooling does with `minimumdifficultyblocks=4032`) before any
   EvoNode exists, then mine local collateral and register every validator
   serially on the wallet. Quorums and the Platform genesis ChainLock therefore
   form on a mature chain instead of during the first few hundred blocks, where
   DKG sessions were observed to PoSe-ban late-registered validators.
6. Mine the quorum rotation cycles at minimum difficulty, then activate devnet
   sporks (InstantSend 2/3, superblocks 9, DKG 17, ChainLocks 19, all-connected
   quorums 21), start persistent mining, wait for READY masternodes, all three
   quorum types and ChainLocks. A rotated `llmq_devnet_dip0024` quorum (48-block
   cycle) is assembled from quarters picked at the three previous cycle bases,
   each from the masternode list 8 blocks earlier (a masternode counts once
   confirmed), so its first full instance
   forms three cycles after the last registration: about 190 blocks, over 30
   minutes at ten seconds a block. Block processing records those picks whether
   or not DKG runs, and before activation (SPORK_17 off) no DKG session exists,
   so no member can be PoSe-punished: the wallet mines to 6 blocks before that
   cycle at minimum difficulty, and every quorum type then forms in the same
   normally paced DKG. Nothing is mined this way once DKG is enabled (resume).
7. Persist the initial chainlocked Core height once, render immutable Platform
   genesis/node identity, and start Drive, Tenderdash, rs-dapi, the gateway and
   its rate limiter on validators.
8. Observe all nodes twice: advancing Core, live (advancing or recently
   blocked) Platform, common-height Platform
   block-hash agreement, exact identities/images, no container replacement during
   observation, and successful TLS→HTTP/2→DAPI gRPC with responsive Drive/TD.

Host work is bounded: renders and service starts run on up to sixteen hosts at
once, other host work on four; wallet transactions are serial. dashmate runs only
when a node's config or release changed: a resume, or a start straight after the
render stage, reuses the last render, and only a new release migrates the config.
The wallet's signed registration bytes and collateral output are fsynced **before**
broadcast. Retries reconcile/resend those same bytes, never fund a replacement
registration. Collateral is persistently locked and restored before further
funding. Only unlocked spendable outputs count towards available funds.

`network-ready` means a point-in-time observation, not permanent health. The legacy
compute `applicationHealth: unknown` stays deliberately unchanged; fresh chain
observations are in `deployment` and `doctor`. No target disappears on failure.

## dashmate services

Each node's services are the deployed release's dashmate services, not a
dash-network-go rendition of them. Every render runs that release's pinned
`dashmate-helper` image (already cached by bootstrap on every node) as a
one-shot container with no network, no Docker socket and not its entrypoint, as
uid 1000, to:

1. create the node's dashmate config from dashmate's own `base` config, or
   migrate the existing one exactly as `dashmate update` does;
2. set this devnet's options on it (below); every other value stays the
   release's default or what its migrations produced;
3. run `dashmate config render` and `dashmate config envs`.

Compose then runs that release's own compose files with that environment, under
the project `dashnet-<compute>-<node>`. The worker adds one Compose override
only: its ownership labels, a configuration fingerprint label per service, its
container names (`dashnet-<compute>-<node>-<service>`, as before), the pinned
sidecar images, and a client-only `/etc/dash/dash.conf` inside Core so
`dash-cli -conf=/etc/dash/dash.conf` keeps working (dashd reads its own). dashmate's
helper service is never started, and no dashmate command starts, stops or
reconfigures a service.

Options set by this tool: network `devnet` with the plan's devnet name, block
time and minimum-difficulty blocks; the node's public address; Core, Tenderdash
and gateway ports; `addnode` peers and Tenderdash persistent peers; a password
for every dashmate RPC user plus a `dashnet` user for this tool and the wallet
host's services; the pinned images; masternode, spork and node identities; the
Platform genesis (chain ID, genesis time, ChainLocked height, quorum type,
protocol version) and epoch length; Drive's quorum types (dashmate has no devnet
preset); the Let's Encrypt provider when trusted certificates are planned;
the fleet's addresses on the rate limiter's allow list; and Insight's indexes on
the wallet. Everything else is dashmate's, so devnets now match testnet and
mainnet nodes in, for example:

- per-service Core RPC users with method whitelists, and low RPC priority for
  DAPI (`rpcwhitelistdefault=0`, `rpcexternaluser`);
- BIP157/158 compact block filters served to SPV clients;
- dashmate's Tor sidecar (an onion service for Core);
- the gateway's routes and timeouts (including long-lived subscriptions), HTTP/2
  limits, overload manager, and a rate limiter (150 requests a minute per client
  address; the fleet's own addresses are exempt);
- Tenderdash's mempool, P2P and consensus settings, including an empty block
  every three minutes on an idle chain.

Core's dashmate-rendered `dash.conf` carries freshly salted `rpcauth` lines, and
the Tor configuration a freshly salted password hash, on every render. The worker
verifies each salted line against the configured credential, then fingerprints
without the salt, so a render changes a service only when its configuration does.
Compose recreates exactly the services whose fingerprint (definition and rendered
files) changed. Rendered files are rewritten in place, as dashmate does, so a
running container's bind mounts keep seeing them. Platform genesis and node
identity, once Platform started, never change: a render that would change them
fails.

Sidecar images (Tor, the rate limiter and its Redis) are the ones the release's
dashmate selects. The first `deploy` stage has every node render and report them;
the controller resolves each once to per-architecture digests (anonymous registry
access) and keeps the pins in the shared journal, so every node runs the same
images and a resume never resolves a tag again.

## Host services, networking and private material

Host prerequisite: the bootstrap runtime plus Ubuntu Python 3 and curl with HTTP/2.
The Go executable embeds a fixed, digest-bound Python worker; input is typed JSON
on SSH stdin, never arbitrary shell/RPC. Python is not required on the operator's
machine. Host flock covers each complete operation, including stop.

All data/config is under root-owned `/var/lib/dashnet` (0700). dashmate's home,
with its `config.json` and rendered service configuration, is
`/var/lib/dashnet/dashmate`, owned by uid 1000 as a dashmate install's home would
be; the root-only parent keeps it private. Private files are 0600:
RPC/BLS/spork/Ed25519/TLS keys, wallet data and signed transactions stay on
their owning hosts. Never upload this tree, Docker inspect/config output, or raw
wallet/RPC output into public Actions artifacts or chat. Take encrypted backups
through a separately authorized backup procedure. The DynamoDB journal contains
only public identities and operational facts, not recovery key material.
Service data lives in dashmate's named volumes (`<project>_core_data`, …).

Services run on dashmate's Compose network (`172.24.24.0/24`), with the ports
dashmate publishes:

| Service | Bind/port |
|---|---|
| Core P2P | all interfaces, 20001 |
| Core RPC / ZMQ | loopback, 20002 / 29998 |
| Tenderdash P2P | all interfaces, 26656 |
| Tenderdash RPC | loopback, 26657 |
| Gateway (TLS; dashmate's self-signed listener also serves plaintext) | all interfaces, 1443 |
| Gateway admin / metrics, Drive and Tenderdash diagnostics (dashmate defaults, disabled) | loopback |
| ACME HTTP-01 (trusted certificates only, during issuance) | all interfaces, 80 |

Drive's ABCI/gRPC and DAPI listen only on the Compose network.

Every preflight rejects containers dashnet did not create, including stopped
ones, and never adopts a container by name. The one exception is an operator
service that deliberately shares a host (a devnet's quorum list server, explorer
or faucet): a container labelled `dashnet.auxiliary=<network>/<node>` for that
exact network and node is ignored, provided its name is outside dashnet's
`dashnet-<compute>-<node>-` namespace. Such services must not bind the ports above.

Existing SGs must permit fleet Core/Tenderdash P2P and operator SSH.

**Service addresses.** Every host needs an Elastic IP allocated from the
network's IPAM pool: dashmate's Core never connects to private addresses
(`allowprivatenet=0`), so `deployment-plan` binds the public addresses, as
long-running devnets do: EvoNodes register `<public-ip>:20001`, Core advertises
it (`externalip`), Tenderdash advertises `<public-ip>:26656`, and peers connect
over public addresses. Clients outside the VPC (SDKs, the quorum list server's
masternode list) can then reach every node. Each target keeps its VPC
`privateAddress`; drift checks require both addresses to stay associated with the
same instance. Security groups must allow Core 20001 and Tenderdash 26656 from the
fleet's public addresses (rules that reference a security group match only
private traffic). `--advertise private` is refused. The tool does not open SGs or
create DNS/load balancers. By default the gateway uses dashmate's self-signed
provider with persisted per-node certificates (one-year lifetime); probes pin
that certificate, never `--insecure`.

**Trusted gateway certificates.** With an `acme` image (for example
`docker.io/goacme/lego:v5.5.2`) in the network images and `--acme-email`,
`deployment-plan --gateway-tls auto` selects Let's Encrypt, as long-running
devnets' dashmate gateways do: the node's dashmate config uses dashmate's
`letsencrypt` provider (a TLS-only listener). dashmate's helper, which would renew
it, is not run: each validator runs a pinned ACME client (`dashnet-…-acme`) that
obtains a short-lived certificate for its public IP over HTTP-01 on port 80 (the
security group must allow it), renews it when three days remain, and writes the
pair in place into dashmate's gateway certificate files. A host path unit then
signals Envoy's hot restarter (`SIGHUP`), exactly as dashmate's helper reloads a
renewed certificate; until the first issuance the gateway serves the self-signed
pair. Probes then trust the pinned self-signed certificate or the system CAs for
the node's public IP. `--gateway-tls letsencrypt-staging` uses the staging CA;
`self-signed` opts out. The ACME client is not part of upgrade image sets.

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
readback, stops the mining host before withdrawing validators, stops each node's
own services (miner, ACME client) and then dashmate's with dashmate's own Compose
stop (dependants first, each with its grace period), preserves all
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
