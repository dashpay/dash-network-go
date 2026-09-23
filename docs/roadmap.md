# Implementation roadmap

## 1. Planning and discovery — this change

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

## 2. One real devnet lifecycle

- Complete a concrete topology/genesis contract, AMI/runtime preparation, and
  direct AWS resource creation with ownership tags and idempotency tokens.
- Implement the shared journal/lock and a small, named-stage runner.
- Implement node transport/configuration, Core start, funding/registration,
  Platform start, and health checks. Retain published container packaging initially.
- Implement account/network-scoped cleanup and interrupted-operation recovery.
- Prove create, resume, and destroy on an explicitly authorized disposable devnet.
  Preserve a record for every intended target, including unreachable ones.

## 3. Upgrades and execution workflows

- Use live membership, exact old/new versions, migration constraints, and quorum
  state to make plans executable.
- Prove Platform-only, Tenderdash-only, and Core-only changes preserve unselected
  components. Test existing-state upgrades separately from fresh-network startup.
- Add Actions execution using the same released CLI and shared state/locks.
- Configure devnet and managed-testnet roles/policies separately; no implicit
  reset or destruction inside an upgrade operation.

## 4. Status integration

- Multi-network read-only views fed by independent health collection and durable
  operation events; extend beyond validators to the managed service topology.
- Public showcase pages and authenticated, authorized operator views.
- GitHub run links first; custom deployment/upgrade/resume forms after operation
  contracts are proven. The UI dispatches the same workflows, not a second engine.

## 5. Release tracking

- Discover stable/beta/nightly candidates and dependency hints automatically.
- Exercise intended combinations and persist evidence-qualified known-working sets.
- Add opt-in tracking policies for disposable devnets, with explicit rollout
  policies for managed testnet. Human emergency operation uses recorded artifacts.
