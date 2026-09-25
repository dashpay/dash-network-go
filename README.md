# Dash Network Go

A ground-up Dash network manager for **humans, GitHub Actions, and agents**.
No Terraform, Ansible, Dashmate installation, or OpenClaw is required by this CLI.

New public-facing allocations require an explicit [AWS BYOIP/IPAM pool](docs/ipam.md).
Their Elastic IPs are ownership-tagged and journaled through allocation, resume
and explicit post-termination cleanup; automatic Amazon public IPv4 is disabled.

**Current milestone: live-proved devnet lifecycle and health-gated image upgrades.**
The CLI plans/provisions EC2, prepares owned Ubuntu hosts, starts Core, funds and
registers EvoNodes, starts Platform, and runs independent health gates. It has
inspect/resume and explicit stop-preserving-data commands, backed by a shared
DynamoDB journal/runner claim. No AI session is required.

Use the [complete lifecycle/runbook](docs/lifecycle.md) and
[13-validator + wallet example](examples/devnet-lifecycle.yaml).
The [operator-assisted AWS proof](docs/validation-2026-09-24.md) covered 13 validators,
interruption, stop/resume, DAPI and complete scoped cleanup. [Image upgrades](docs/upgrades.md)
support owned devnets with Core preserved. Existing Moutai/testnet workloads have
an explicit [import/enrollment/management path](docs/managed-networks.md), and new
[Core fullnodes can join an existing chain](docs/join-existing-chain.md). A fresh
AWS version-upgrade proof is in progress; do not infer compatibility from CI.
Protocol migrations, adding existing-network EvoNodes, resets and generalized
destroy remain unimplemented.

## Quick start

Build with the Go version in `go.mod` (Go 1.27.1):

```sh
go build -trimpath -o bin/dashnet ./cmd/dashnet
bin/dashnet help
bin/dashnet validate --network examples/devnet.yaml

# Contacts registries, but downloads no image layers and changes no nodes.
mkdir -p out
bin/dashnet resolve --network examples/devnet.yaml --out out/example.lock.json
bin/dashnet plan --network examples/devnet.yaml --lock out/example.lock.json \
  --operation upgrade --scope platform --out out/example-plan.json
```

CI builds standalone Linux/macOS binaries for amd64 and arm64. Download the
matching workflow artifact, verify its `SHA256SUMS`, and `chmod +x` the binary.
Those binaries need neither Go nor Docker installed on the operator machine.

The sample component versions are **candidates, not a compatibility guarantee**.
`resolve` checks every requested Linux architecture, the multiarch index, and the
actual child image configuration. It records one immutable combination for the
whole operation. A missing image or architecture fails without a partial lock.

## Network definitions

Start from [the devnet example](examples/devnet.yaml) or
[the managed-testnet example](examples/testnet.yaml). Replace the example AWS
account, region, exact network tag, node groups, and images with intended values.
Keep operational copies outside the public repository (the local `networks/`
directory is ignored).

- Strict YAML: unknown/duplicate fields and additional documents are errors.
- Network name, generation, cloud account, region, visibility, topology, and image
  references are explicit. Increment generation when resetting a devnet.
- Visibility must be `private` or `public`; it never defaults to publication.
- Mutable tags, including nightlies, are allowed as **input**. Resolved output is
  pinned by digest. A changed network definition invalidates the previous lock.
- `create` plans are devnet-only. Testnet means our managed nodes joining the
  existing public network, not ownership of the whole network.
- Group counts describe intent. The first planner does not yet validate genesis
  quorum parameters, AMI compatibility, live membership, or migration safety.

The initial six components are Core, Drive, rs-dapi, Tenderdash, gateway, and the
helper image. Images come directly from registries; the resolver does not install
Dashmate or read Docker credential helpers. Automatic channel selection and
dependency hints from matching upstream source are the next resolver increment.

## Read-only AWS discovery

```sh
bin/dashnet inventory --network networks/testnet.yaml \
  --profile YOUR_AWS_PROFILE --out out/testnet.inventory.json
```

In CI, omit `--profile` to use short-lived OIDC/environment credentials. Required
AWS calls are `sts:GetCallerIdentity` and `ec2:DescribeInstances` only. The client
verifies the configured account before discovery and restricts the query to the
configured region and **exact network tag value**. Pagination failures and scope
mismatches are errors, never a partial successful fleet count.

The legacy deployment's `DashNetwork` tag is supported without adopting or
changing its resources. New `dashnet:role` tags are read when present; absent
roles are reported as unknown, not inferred from an instance name.

EC2 `running` is **not** application health. This snapshot explicitly reports
`applicationHealth: unknown`; `doctor` provides independent node/protocol probes.

## Resumable EC2 provisioning

See the [human command and recovery guide](docs/provisioning.md) and
[small compute example](examples/devnet-compute.yaml). Provisioning uses explicit
existing networking/AMIs and requires the exact reviewed plan ID. Devnets support
the native lifecycle; testnet allocations are restricted to fresh Core fullnodes
using the existing-chain join path:

```sh
# Real AWS IDs and an existing state table are required; read-only preflight.
bin/dashnet provision-plan --network networks/devnet-lab.yaml \
  --profile YOUR_AWS_PROFILE --out out/lab-ec2-plan.json

# Creates billable instances and writes shared operation state.
bin/dashnet provision --plan out/lab-ec2-plan.json --confirm PLAN_ID \
  --profile YOUR_AWS_PROFILE

# Inspect or resume from any authorized machine using the same plan.
bin/dashnet operation --plan out/lab-ec2-plan.json --profile YOUR_AWS_PROFILE
```

Normal retries reconcile existing instances. Lost-response ambiguity never causes
a blind relaunch. Claims do not expire automatically; a crashed runner requires
explicit stopped-runner recovery. A changed generation/plan cannot bypass the
existing operation. Root volumes are retained on termination; there is no
automatic cleanup/rollback yet. Direct EC2 launch and recovery are covered by the
[disposable AWS acceptance run](docs/validation-2026-09-24.md); its teardown used a
separately scoped helper, not a general-purpose CLI destroy command.

## Authenticated node bootstrap

The [bootstrap and recovery guide](docs/bootstrap.md) describes the next stage:

```sh
dashnet bootstrap-plan --compute-plan out/lab-ec2-plan.json \
  --lock out/lab.lock.json --out out/lab-bootstrap-plan.json
dashnet bootstrap --plan out/lab-bootstrap-plan.json --confirm BOOTSTRAP_PLAN_ID \
  --ssh-key /secure/deployment-key --known-hosts /secure/lab-known-hosts \
  --profile YOUR_AWS_PROFILE
```

It verifies instance-scoped SSH host keys and IMDSv2 identity, checks every host
before mutation, prepares Docker/Compose, and caches role-specific pinned images.
Runtime installation uses signed Ubuntu distro repositories; prebaked runtimes
skip it. Shared and host-side locks protect interrupted-run recovery. No Dash
containers start, and `hosts-ready` is **not** application health.

The same `operation` command shows per-host bootstrap progress. Trust enrollment
supports fresh authenticated EC2 console keys through `host-trust`; no insecure
host-key learning fallback exists. SSH identities stay local. This stage was
verified on all 14 hosts in the disposable AWS run.

## Complete devnet execution

After bootstrap, `deployment-plan` binds exact instances and immutable genesis.
`deploy` starts/resumes the chain lifecycle; `doctor` independently checks every
node and exits nonzero for unknown/degraded health. `stop` verifies owned services
are stopped and preserves all state (EC2 billing continues). See
[commands, compatibility assumptions and recovery](docs/lifecycle.md).

## Existing-state upgrades

`upgrade-plan` records exact old/new images for a completed owned deployment;
`upgrade` stages artifacts and rolls one validator at a time, with whole-fleet
health and Core-process/configuration preservation gates between withdrawals.
Runtime images live separately from the immutable creation plan, so later
inspection/recovery cannot silently revert the release. See the
[upgrade command and recovery guide](docs/upgrades.md) for supported profiles,
interrupted-run behavior and the remaining live-version validation boundary.

## Public and operator data

```sh
# Full operator inventory (contains IP addresses and cloud identifiers).
bin/dashnet status --inventory out/testnet.inventory.json

# Public allowlisted projection; fails for a private network.
bin/dashnet status --inventory out/testnet.inventory.json --public \
  --max-age 5m --out out/testnet.public.json
```

Public output includes name/purpose, chain identity/generation, aggregate compute
counts, observation source/time, and staleness. It never includes instance IDs,
addresses, account IDs, raw tags, or internal errors. Display name and description
are operator-authored public copy; do not put private information in them.

The public showcase and authenticated operator interface are implemented
separately in [`dashpay/status` PR #6](https://github.com/dashpay/status/pull/6).
**This CLI repository does not serve a web UI or implement browser authentication.**
Public projection is not a substitute for backend authorization.

All `--out` files are mode 0600, atomically published, and never overwritten.
Choose a new name for each resolution, plan, or observation. These local artifacts
are not the shared journal; EC2 provisioning stores that separately in DynamoDB.

## GitHub Actions

- **CI:** formatting, module integrity, vet, race-tested unit/integration tests,
  example validation, four platform binaries, and an isolated Ubuntu recipe smoke
  test. Tests use an in-process OCI registry, fake AWS clients, and loopback SSH;
  the recipe fixture mocks metadata/packages/Docker. No live cloud access.
- **Plan network (read-only):** manually resolve the checked-in example images
  and preview a scoped operation. Publishes the lock and plan plus a job summary.
  Registry access is anonymous; there is no cloud role or mutation step.

- **Operate managed devnet:** main-branch-only, protected-environment execution
  using OIDC, reviewed private plans, explicit plan confirmation and the same CLI.
  Private reports stay in S3/DynamoDB, never public Actions artifacts. Inert until
  separately configured; see [setup](docs/lifecycle.md#github-actions-execution).
- **Existing networks:** separate Moutai/testnet environment policies, reviewed
  enrollment and management inputs; see [managed operations](docs/managed-networks.md).
- **New Core fullnodes:** the allocation workflow includes existing-chain join
  planning/execution, with a separate protected testnet allocation environment.
  This does not register a new validator or expand an enrolled fleet implicitly.
- **Container contracts:** real Core wallet/registration/recovery and real
  Tenderdash/Envoy/DAPI configuration/TLS/gRPC; no AWS access or fleet-health claim.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
python3 -m unittest discover -s internal/node/testdata -p 'test_*.py'
gofmt -w cmd internal
```

If your system mounts `/tmp` with `noexec`, set `GOTMPDIR` to an executable local
build directory before running Go tests.

The tests exercise cross-account/scope refusal, failed pagination, immutable
multiarch resolution, mismatched architecture advertisements, lock drift,
Platform/Core boundaries, private/public separation, observation freshness,
artifact preservation, exclusive runner claims, uncertain-launch reconciliation,
interrupted checkpoints, footprint drift, strict SSH host trust, bounded output,
host preparation/readback, and cancellation without target loss.

See [architecture](docs/architecture.md) and [the implementation roadmap](docs/roadmap.md).

## Existing Moutai and managed testnet

The `managed-*` commands support explicit existing-state import/enrollment,
scoped image upgrades and captured-container deployment/recovery. They preserve
identities/data and use live per-network health/quorum gates. See the
[managed-network commands, cutover and recovery guide](docs/managed-networks.md).
This is not a read-only product boundary: authorized operators can manage both
networks. New testnet-node provisioning, protocol migrations, live rollout proof,
and the authenticated dashboard remain separate work.
