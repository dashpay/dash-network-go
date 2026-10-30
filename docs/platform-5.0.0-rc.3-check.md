# Platform 5.0.0-rc.3 source compatibility check

## Bound revisions

- Release `413661645`, tag `v5.0.0-rc.3`, published 2026-10-15.
- Platform commit `0f38a693a4d37c3ca68dd9605b615ea0fbda9338`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `c4e1953e9a37707d121adca2fa971c8277ab2a91`.

**Exact-source checks pass; full artifact/runtime compatibility is incomplete.**
This change adds two worker regression tests and documentation. Production
generator/upgrade code, dependencies and deployed pins remain unchanged.

## Release contract

RC.3 fixes historical stock-image migrations to use stored defaults rather than
resolved family tags, and adds a migration clearing recognized major-4 stock
and major-5 prerelease image tags. Exact versions, minor tags, stable major-5,
custom images and digest pins are preserved by that migration.

The worker already runs helper migration before applying the explicitly
requested immutable validator image pins and rendering. No generator patch is
needed for this contract. Wallet/seed roles keep Platform disabled and do not
carry validator image pins. The new regression protects both role behavior
and migrate → pin → render ordering without starting services.

Complete RC.2/RC.3 trees differ in 49 blob paths. Dashmate changes migrations,
stock-image matching, migration tests, config documentation and package version.
RC.3 stored defaults equal the committed RC.2 fixture after source-root
normalization to `/platform`; no duplicate fixture was added. Schemas, Core
template, Compose and dependency pins are unchanged. There is no new RC.2→RC.3
Core-policy delta; earlier-version RPC migration prerequisites remain conditional
on actual node/version/effective policy. The Core-preserving guard is unchanged.

## Passing gates

- All 61 worker tests, including the two new role/pinning and render-order cases.
- Six actual Worker.configure outputs (validator/wallet/seed × initial/final)
  pass exact RC.3 Config/Ajv/schema and Core-template validation.
- Fifty exact upstream RC.3 image-policy migration cases, idempotence and schema
  validation; two historical getStored corrections; three missing-option shapes.
- All fifty upstream-migrated configurations pass through the actual validator
  worker and receive its explicitly requested immutable Drive/DAPI pins.
- Ten retained deployment-info migration cases and existing TTL/RPC/Tor checks
  pass against RC.3 source. No actual helper CLI/container runtime is claimed.
- Twenty configuration-closure source blobs verified against the immutable
  release tree; real task-local npm dependency-tree validation passes.

Latest protocol remains 14, but identity-create validation behavior changes.
Configuration compatibility and equal protocol numbers do not establish
database, consensus or serialized-data compatibility. Product source was
inspected, never modified, vendored or custom-built.

## Remaining gate

Fresh initial registry reads at 2026-10-15 10:09:16 UTC returned
HTTP404/MANIFEST_UNKNOWN for exact `5.0.0-rc.3` Drive, rs-dapi and helper tags.
Release Platform workflow `38717981624` was queued. Final observations, tested
revision and independent review are recorded in workspace report
`reports/incident-17f72c02f014ec7b63193467/report.md`.

After publication, resolve immutable artifact digests and validate actual
helper migration/environment/Compose and isolated service contracts in admitted
task-owned runtime resources. No Docker executable is installed locally.
Missing artifacts are a publication blocker, not proof of incompatibility.
Later RC.2 publication and separately authorized validation do not prove RC.3.

No live deployment/upgrade/reset/restart, deployed pin/binary change,
Core/Platform product patch, release publication, PR merge or Slack message
occurred. Retired Jameson resources were not investigated or recreated.
