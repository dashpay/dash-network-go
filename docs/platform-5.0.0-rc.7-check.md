# Platform 5.0.0-rc.7 compatibility check

## Bound revisions

- Release `419848008`, tag `v5.0.0-rc.7`, published 2026-10-21.
- Platform commit `79693ec1f3b83b0cf12b27f3ca01651787f3c443`.
- Current upstream dash-network-go baseline `369a5173385f8a7bb1b03f57ecb989af431e18e9`.
- Prior review/test lineage `01c71c24ffdde82e408b281493b14eac3ae5d64e`.
- Checked 2026-10-30; newer releases exist, most recently `v5.0.1`.

**Source/configuration checks pass and a baseline rollback defect is fixed in
this review; complete packaged-runtime compatibility remains unverified.** No
live upgrade or deployment is authorized or performed.

## Release and baseline differences

Complete RC.6/RC.7 trees differ in 41 blob paths. Dashmate changes only package
version; configuration, migrations, Compose and status implementation are
unchanged. Rust-dashcore and wallet/storage changes are not certified by equal
configuration. No new RC.7-specific generator requirement was demonstrated.

The newer upstream baseline adds existing-stack DAPI seed TLS tooling. Prior
review fixtures/tests/docs are retained through a local review-branch merge.
The seed adapter intentionally requires legacy dapi_api/dapi_core_streams and
rejects RC.7's rs_dapi layout; its unit tests are not RC.7 seed-adoption or
Mainnet support certification. This review does not broaden that boundary.

Independent review found a baseline automatic-rollback defect: failure before
helper retirement restores only RestartPolicy.Name, dropping MaximumRetryCount
from on-failure:N. The source fix shares exact restart-policy argument formatting
between explicit restore and automatic failure recovery. A mocked apply-failure
regression fails on the baseline (on-failure instead of on-failure:5) and passes
after the fix, checking helper restart, journal settlement and original files.
This is a dash-network-go repair, not a Core/Platform product patch or a defect
introduced by RC.7. No Docker operation or live certificate change ran in tests.

## Passing validation

- Full `go test ./...` before and after the repair, using checksum-verified
  Go 1.27.1 in task-owned storage, two-way package/CPU parallelism and private
  module/build caches. Nothing installed into a host-wide tool or live binary path.
- 61 node-worker regressions and 11 seed-TLS tests after the fix.
- Six actual configured roles/stages validated by exact RC.7 Config/Ajv/schema
  and Core template, plus 50 migration-to-worker integrations.
- Fifty upstream stock-image migration cases, two stored-default corrections,
  three missing shapes, ten deployment-info migration cases and TTL/RPC/Tor checks.
- Twenty exact source-closure blobs verified; stored defaults equal the existing
  RC.2 fixture after source-root normalization; harness dependency tree validates.

## Artifacts and remaining runtime gate

Exact image index/platform/config bodies and OCI revision/version labels were
verified for both amd64 and arm64:

- Drive: `sha256:a12ba125c9caf200d990d5b3c8e64ba1bf13d512538e3e92eeebaa946168cf0d`
- rs-dapi: `sha256:9879a17485fd649bfe2b9c350bd281dc0a5aeb1d8f994532368d025257d4ed38`
- helper: `sha256:0a701cd0e066363f9ea054690891caa0ce62a46a62c9742c08c5a19dfd1661cf`

No packaged helper/CLI/RPC, Compose or service/database stack was executed.
Local and Gateway OCI runtimes remain unavailable; user namespaces are denied.
This is the existing execution-resource blocker, not missing publication. No
shared host runtime/runner change was attempted. A native v4.1 spike that omitted
the helper cannot substitute for this gate.

Next: coordinator-admitted OCI-capable task-owned execution resources; verify
the full effective dependency digest matrix and exact RC.7 helper/service/database
contracts. Existing Core-preservation and unsupported-layout guards remain.
No live configuration/pins/binary, service, network, product implementation,
release/PR publication or external message changed.

Detailed report, tested final commit, independent review, regression evidence
and runtime blocker: `reports/incident-69a5b34b7bc35b71aa5db7bf/`.
