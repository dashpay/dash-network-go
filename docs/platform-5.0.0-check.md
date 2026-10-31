# Platform 5.0.0 compatibility check

## Exact scope and revisions

- Release `424720158`, tag `v5.0.0`, published 2026-10-26.
- Platform commit `389dd6a16ee3f57e97de6494277b33720b9d7fef`.
- Upstream dash-network-go baseline `369a5173385f8a7bb1b03f57ecb989af431e18e9`.
- Tested source parent `07d5499f9a66ba4b4abc01178d536be2f3bde6ce` retains prior
  regression coverage and the reviewed helper rollback retry-limit fix.
- Checked 2026-10-31. Newer `v5.0.1` exists; this task remains v5.0.0-bound.

**Source/configuration checks and published artifact identities pass; complete
runtime compatibility remains unverified.** No live upgrade is authorized.

## Release contract

Complete RC.8/stable trees differ in 34 blob paths. Dashmate changes only its
package version: schema/default/migration/Compose/status source is unchanged.
The version-dependent effective Drive and rs-dapi stock tags become `:5`, not
`:5-rc`. Exact stable configuration checks verify those defaults and that the
worker replaces validator selections with explicit immutable release pins.
Stored defaults still equal the retained RC.2 fixture after source-root
normalization, so no duplicate fixture or production generator patch is needed.

The old release-check harness initially expected the prerelease derived tag and
failed with actual `dashpay/drive:5`. Its expectation was corrected for the stable
release; this was a test-harness assumption, not a product/generator defect.
The initial failure and successful rerun are preserved in the detailed report.

The wider release is not runtime-equivalent to RC.8: Rust Tenderdash bindings
advance from v1.8.0 to v1.8.1, certificate/dependency revisions change, and wallet
ancestor-permission handling changes for Android. No dashnet generator/status
failure was demonstrated. Configuration equality does not certify the packaged
helper, full dependency matrix, service stack, database or product behavior.
The prior identity-proof API change is not newly introduced here. Legacy seed
TLS still rejects the stock rs_dapi layout; no support or Core guard is bypassed.

## Actual passing checks

- 61 node-worker tests and 11 seed-TLS tests.
- Six actual worker configurations: validator/wallet/seed, initial/final, through
  exact stable Config/Ajv/schema/Core-template validation.
- Fifty actual stock-image migration outputs through the worker, preserving
  explicit stable Drive/rs-dapi digest pins.
- Fifty image-policy cases, two historical stored-default corrections, three
  missing shapes, ten deployment-info migration cases and retained TTL/RPC/Tor
  checks, including idempotence and preservation of operator policies.
- Twenty source-closure blobs Git-hash-verified before/after validation; local
  harness dependency tree validates. Source-root-only fixture normalization.

Worker data and other component pins are synthetic, not live credentials or a
complete runtime selection. No helper/container/service was executed. Full Go
tests passed for identical source in the prior RC.7 review; they were not rerun
for this documentation-only change and are not presented as fresh stable-runtime
validation. The prior rollback fix is retained, not claimed as a new repair.

## Published images and remaining gate

Index/platform/config content hashes and version/revision labels verified for
amd64 and arm64:

- Drive `sha256:79f4f115ed157e5d5d15ac97784ea957cb4925ed2533ba4e65cef8916aa29e3e`
- rs-dapi `sha256:254a5e8e29876837521eb7fc5a6dd77b3d2587cac8870072e672492fff96e18d`
- helper `sha256:24cdd866c14c3a1710949f412886153d8d1c6c8bda1f3e1259fab6ba74327143`

Images are published; no image layers were executed. The existing isolated
runtime-resource blocker persists: local OCI executables are absent and user
namespace creation is denied. No host-wide setup, runner or live service change
was attempted. Coordinator admission of an OCI-capable resource with bounded,
task-owned containers/storage is needed before testing exact helper/CLI/RPC,
Compose, full dependency digests and isolated service/database contracts.

No deployment, live pins/configuration/binary change, Core/Platform product-code
patch, PR merge, release publication or external message occurred. A normal
revert of this document is sufficient rollback; no live rollback is needed.

Detailed evidence and final review commit:
`reports/incident-79b6e18e1bddaac3ed05d901/`.
