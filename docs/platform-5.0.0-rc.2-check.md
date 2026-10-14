# Platform 5.0.0-rc.2 source compatibility check

## Bound revisions

- Release `412575564`, tag `v5.0.0-rc.2`, published 2026-10-14.
- Platform commit `2fda500d7a6cf01fdb76a2d32126a2ea0b484019`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `fe4c13a618985a9fbe9fed8e8c32aad82234065f`.

**Exact-source checks pass; artifact/runtime compatibility remains incomplete.**
This change adds the release's normalized stored-default fixture and regression
coverage. No production generator code, dependencies, deployed pins or binaries
are changed.

## Configuration and upgrade contract

RC.2 adds `getdeploymentinfo` to the default `drive_consensus` RPC whitelist.
Its upstream migration appends that permission only when an existing array
lacks it, preserving custom entries and unrestricted/null or missing policies.
The generator already preserves these upstream permissions while setting
per-node credentials; no production patch is needed for this contract.

The exact fixture differs from the previous beta.4 fixture solely by that
additional permission, after normalizing the source root to `/platform`.
Complete RC.1/RC.2 trees differ in 58 blobs. Dashmate changes four paths:
defaults, migration, migration tests and package version. Schemas, Compose and
the Core template remain unchanged. Template equality does not mean rendered
configuration equality: the new whitelist changes the rendered Core policy.

Existing nodes missing this permission may require a separately authorized
Core configuration activation/restart. Migrating a file alone does not change
Core's effective whitelist. The Platform-only upgrade guard correctly reports
the Core delta at staging and refuses application; it must not be bypassed.
Already-permitted/unrestricted policies and older Core fallback behavior must
be assessed against actual node/version evidence, not assumed fleet-wide.

## Passing gates

- All 59 worker tests, including eight RC.2 cases reusing role, immutable-pin,
  RPC-policy, TTL and fail-closed checks plus new permission/policy assertions.
- Six actual Worker.configure outputs (validator/wallet/seed × initial/final)
  pass exact RC.2 Config/Ajv/schema and Core-template checks, including the
  rendered `getdeploymentinfo` permission.
- Ten exact upstream RC.2 migration cases: legacy/custom/already-present/null/
  empty lists and five missing-option shapes; idempotence and preservation.
  Complete configurations also pass real schema validation. Rendered before/
  after comparison isolates the single deterministic whitelist addition.
- Existing TTL migration, operator TTL, RPC whitelist and Tor-bind checks pass.
- Twenty source-closure blobs match the immutable release tree; real npm
  dependency-tree validation passes. No schema/source stubs were used.
- The existing upgrade refusal regression passes with unchanged Core state.

The task-local source harness is not the actual helper image. Configuration
success does not prove service, database or wire compatibility. Latest protocol
remains 14, while upstream Core RPC fallback, rust-dashcore dependency and
accepted-nullifier behavior change; product code was inspected, not modified.

## Remaining gate

Initial fresh registry reads at 2026-10-14 10:12:16 UTC returned
HTTP404/MANIFEST_UNKNOWN for exact `5.0.0-rc.2` Drive, rs-dapi and helper tags;
Release Platform workflow `38547584695` was queued. Later observations, final
tested revision and independent review are recorded in workspace report
`reports/incident-551d39ac685b5ecbbca5280b/report.md`.

After publication, resolve immutable artifact digests and validate actual
helper migration/environment/Compose and isolated runtime contracts in admitted
task-owned resources. No Docker executable is installed locally. Missing
artifacts are a publication blocker, not proof of incompatibility. Prior RC.1
publication and separate owner-accepted replay results do not validate RC.2.

No live upgrade/deployment/reset/restart, Core/Platform product patch, installed
dashnet change, release publication, PR merge or Slack announcement occurred.
