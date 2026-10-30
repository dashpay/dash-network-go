# Platform 5.0.0-rc.6 compatibility check

## Bound revisions

- Release `419276838`, tag `v5.0.0-rc.6`, published 2026-10-21.
- Platform commit `e99300ee69b30b66b7b359c8567826396e5311aa`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `3bb4cbd5fedc1c65ba1fce3db808b97ffe12a3cd`.
- Checked 2026-10-30; newer releases exist, most recently `v5.0.1`.

**Exact-source configuration checks pass; complete runtime compatibility remains
unverified.** This documentation-only review does not change generator behavior,
upgrade guards or deployed configuration. No generator defect was demonstrated.

## Source and validation

Complete RC.5/RC.6 source trees differ in 265 blob paths. In Dashmate only the
package version changes: dependencies, configuration/defaults/schema, migrations,
Compose, status implementation and Core template are unchanged. Wider product
changes are not certified by equal configuration or worker tests.

Passing validation against the exact immutable RC.6 configuration source:

- 61 worker regression tests, including immutable pins and upgrade safeguards.
- Six actual Worker.configure outputs (validator/wallet/seed, initial/final)
  validated by the real Config/Ajv/schema and Core template.
- 50 stock-image migration cases, idempotence/schema checks, two historical
  stored-default corrections and three missing-option shapes.
- All 50 migrated configurations processed by the actual validator worker and
  assigned its explicitly requested immutable Drive/DAPI pins.
- Ten deployment-info migration cases plus retained TTL/RPC/Tor checks.
- Twenty source blobs verified against the immutable tree; stored defaults equal
  the existing RC.2 fixture after source-root normalization. Actual configuration
  harness dependency-tree checks pass. No duplicate fixture was needed.

These tests do not execute the complete helper CLI/RPC dependencies, Compose or
service/database runtime. No Go run is claimed; no Go or product code changed.

## Published artifacts and remaining gate

Exact release indexes, platform manifests and config digests were verified for
both amd64 and arm64. Their OCI version and revision labels match RC.6:

- Drive: `sha256:fb89fcd79ca8a25d7b77d9324c744aaa17a1973c838249f37a51b6070dc0377d`
- rs-dapi: `sha256:a85d1f1c6152acb807b81a00dc17533de9b23aa790afcff18a40f334044306d0`
- helper: `sha256:e2055f2362bb1a817c476c620a6d41f0a6374798f7e36c979f400aa6b24751e1`

Artifacts are published; publication is not the blocker. Minimal fresh checks
confirm the existing execution gap: neither native nor Gateway execution has
Docker/Podman/nerdctl, and local user-namespace creation is denied. No host-wide
runtime was installed/reconfigured. A separate uncertain repair owns the Pasta
runner scope; no resource from that scope was used or changed.

The earlier Docker-free v4.1 fullnode prototype omitted the helper and retained
Docker-only lifecycle/status. It is not evidence that this RC.6 helper/Compose
contract has been validated. No native-service rewrite or live host substitution
was attempted under this version-bound source/tests task.

Next: coordinator-admitted OCI-capable task-owned resources for exact helper,
CLI/RPC, migrations/environment/Compose and isolated service/database contracts.
Resolve the full effective component digest matrix there; source defaults or
retained older components are not runtime certification. Preserve existing Core
guards, data and unrelated activity. No live upgrade is authorized by this task.

Detailed source, artifact, test, independent-review and runtime-capability
evidence is under `reports/incident-88dd4fc608261948e619a14b/`; its final report
records the tested revision and outstanding blocker. No live configuration,
pin/binary, service, network, Core/Platform implementation, release, PR or external
message was changed or published.
