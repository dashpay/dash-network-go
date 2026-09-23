# Dash Network Go

A ground-up Dash network manager for **humans, GitHub Actions, and agents**.
No Terraform, Ansible, Dashmate installation, or OpenClaw is required by this CLI.

**Current milestone: read-only planning and discovery.** You can validate network
intent, resolve image tags into architecture-checked digests, produce scoped
plans, discover tagged EC2 instances, and export explicitly public status data.
There is deliberately no `apply`, `create`, `reset`, or `destroy` command yet.
An intent plan is not an executable deployment or evidence of a healthy network.

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
`applicationHealth: unknown` until node and protocol probes are implemented.

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

This is the data boundary for the future public showcase and authenticated
operator interface in [`dashpay/status`](https://github.com/dashpay/status).
**This repository does not yet serve a web UI or implement authentication.**
Public projection is not a substitute for backend authorization.

All `--out` files are mode 0600, atomically published, and never overwritten.
Choose a new name for each resolution, plan, or observation. These local artifacts
are not the future distributed operation journal or network lock.

## GitHub Actions

- **CI:** formatting, module integrity, vet, race-tested unit/integration tests,
  example validation, and four platform binaries. Tests use an in-process OCI
  registry and fake AWS clients; they need no cloud credentials or running nodes.
- **Plan network (read-only):** manually resolve the checked-in example images
  and preview a scoped operation. Publishes the lock and plan plus a job summary.
  Registry access is anonymous; there is no cloud role or mutation step.

The manual workflow becomes available on the default branch after merge. It is
not labeled deployment: real execution workflows will arrive with the executor,
shared locking, recovery semantics, and health verification. Do not upload private
inventory or configuration as artifacts of this public repository.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
gofmt -w cmd internal
```

If your system mounts `/tmp` with `noexec`, set `GOTMPDIR` to an executable local
build directory before running Go tests.

The tests exercise cross-account/scope refusal, failed pagination, immutable
multiarch resolution, mismatched architecture advertisements, lock drift,
Platform/Core boundaries, private/public separation, observation freshness,
and artifact preservation.

See [architecture](docs/architecture.md) and [the implementation roadmap](docs/roadmap.md).
