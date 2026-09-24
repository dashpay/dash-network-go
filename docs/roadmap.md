# Implementation roadmap

## 1. Planning and discovery — implemented

- Strict network definitions and explicit cloud/network identity.
- Anonymous OCI resolution to index and architecture-specific manifest digests.
- Deterministic intent previews with explicit component/preservation boundaries.
- Direct AWS SDK discovery with account verification, exact tag filtering,
  pagination, and unknown application health.
- Public status projection that rejects private networks and excludes private
  inventory fields; operator output remains distinct.
- CI tests, four standalone binaries, and a read-only Actions planning workflow.

This milestone cannot deploy or upgrade a network. Tests prove the behavior above,
not consensus readiness or a successful infrastructure lifecycle.

## 2. One real devnet lifecycle — operator-assisted AWS proof complete

Implemented and exercised on the disposable fleet:

- Read-only EC2 footprint planning with explicit subnet/security groups/key pair,
  owner-pinned architecture-checked AMIs, and per-target launch identity.
- Direct EC2 creation, durable DynamoDB journal, non-expiring shared runner claims,
  conditional recovery, and reconciliation after lost responses/checkpoints.
- EC2-running verification for every target; application health stays unknown.
- Failure-path tests and a human provisioning/recovery runbook.
- Authenticated SSH transport, instance-scoped host trust, IMDSv2 identity checks,
  Ubuntu 24.04 Docker/Compose preparation and role-specific immutable image pulls.
- Additive bootstrap journal checkpoints, host-side locks, all-host preflight and
  verified interrupted-run resume; no service starts. Loopback SSH and disposable
  recipe integration tests plus a human bootstrap runbook. All 14 hosts verified.

Implemented lifecycle increment:

- Exact-host immutable devnet plans; Core, wallet/validator identities, signed
  pre-broadcast registrations, persistent collateral locks and native Platform.
- Quorum/ChainLock gates, independent two-sample health with DAPI and common-height
  block comparison; interrupted resume and explicit stop preserving data.
- Protected Actions entry point, private evidence output, human recovery runbook,
  fake-cloud failure tests and real-container contracts. See the
  [live creation/interruption/stop-resume/cleanup results](validation-2026-09-24.md).

Remaining:

- Complete a concrete topology/genesis contract, AMI/runtime preparation, and
  the remaining network infrastructure beyond existing-VPC EC2 placement.
- Extend the single-stage journal into reviewed lifecycle transitions/history;
  implement an evidence-backed repair path for unobserved/rejected launch intent.
- Authenticated EC2-console host-key enrollment is implemented with scope,
  freshness and duplicate-key checks; its real-cloud proof is part of the
  [multi-validator acceptance run](validation.md).
- Prove unattended creation with the final release binary; the first real
  acceptance run required documented operator-assisted compatibility fixes.
- Implement account/network-scoped cleanup and interrupted-operation recovery.
- Prove generalized CLI destroy. The first run's explicitly scoped teardown
  verified every instance, retained root volume and dedicated support resource.

## 3. Upgrades and execution workflows

- Implemented Platform/Tenderdash image-only plans and execution, live membership
  and protocol gates, shared/host locks, preserved Core start/configuration,
  bounded staging and one-validator-at-a-time health gates. Runtime images survive
  restart/recovery without rewriting creation intent. Go/Python failure tests and
  a real Docker replacement/replay contract cover the implementation.
- Prove actual Dash version-to-version upgrades on existing state. Core-only,
  protocol/config migration remain unimplemented in the native devnet path.
- Existing Moutai/testnet adapters now support explicit authenticated import and
  enrollment, captured-workload deployment/recovery and scoped image upgrades.
  See [managed networks](managed-networks.md); live management remains unproved.
  Additional testnet-node provisioning/registration remains unimplemented.
- Configure and live-prove the implemented Actions entry point; add explicit
  live workflow proof for the implemented upgrade entry point through the same
  CLI and shared state/locks.
- Configure devnet and managed-testnet roles/policies separately; no implicit
  reset or destruction inside an upgrade operation.

## 4. Status integration

- Multi-network public read-only and authenticated operational views fed by independent health collection and durable
  operation events; extend beyond validators to the managed service topology.
- Include existing testnet and Moutai with deploy/upgrade/recovery controls for
  authorized operators, backed by managed CLI/Actions. Initial discovery is
  read-only; explicit enrollment preserves existing state and enables management.
  Merely showing a network does not trigger adoption or deployment.
- Public showcase pages and authenticated, authorized operator views.
- GitHub run links first; custom deployment/upgrade/resume forms after operation
  contracts are proven. The UI dispatches the same workflows, not a second engine.

## 5. Release tracking

- Discover stable/beta/nightly candidates and dependency hints automatically.
- Exercise intended combinations and persist evidence-qualified known-working sets.
- Add opt-in tracking policies for disposable devnets, with explicit rollout
  policies for managed testnet. Human emergency operation uses recorded artifacts.
