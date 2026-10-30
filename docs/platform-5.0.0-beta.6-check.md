# Platform 5.0.0-beta.6 source compatibility check

## Bound revisions

- Release `411462600`, tag `v5.0.0-beta.6`, published 2026-10-13.
- Platform commit `28fcb529bbfb4670aa33cc838e7dabcc7589f7f3`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `05b3b799f0c6fb891d8218ac06d857bcb3ffe722`.

**Source/configuration checks pass; artifact/runtime compatibility is incomplete.**
This local documentation-only review changes no production code, dependencies,
deployed configuration, image pins or binaries.

## Exact-release evidence

Complete non-truncated beta.5/beta.6 Git trees differ in 94 blobs. Within Dashmate
only package.json changes its version; defaults, Config/schema, migrations,
Core template and Compose retain the same bytes. All 20 configuration-closure
source blobs were verified against the immutable beta.6 tree. Real beta.6 stored
defaults equal the existing beta.4 fixture after normalizing the checkout path
to /platform; no duplicate fixture or generator patch is indicated.

Passing validation on the isolated worktree:

- All 51 existing node-worker tests pass.
- Six actual Worker.configure outputs (validator/wallet/seed × initial/final)
  pass the exact beta.6 Config/Ajv/schema and unmodified Core-template renderer.
- Exact migration checks pass: old missing-TTL configuration is rejected before
  migration and accepted afterward; local/nonlocal defaults, operator TTL5,
  invalid values, RPC whitelist and Tor-bind contracts remain intact.
- Real npm dependency-tree validation passes; no dependency/source stubs used.

The configuration contract is not a proof of product runtime, database or wire
compatibility. The older beta.3 Core-template restriction is not a new beta.6
delta. A separate owner-authorized status-reset migration was subsequently
implemented outside this repository; this check neither changes that workflow
nor weakens dash-network-go's Core-preservation guard.

## Remaining gate

At 2026-10-13 08:42:46–49 UTC the exact `5.0.0-beta.6` tags for dashpay/drive,
dashpay/rs-dapi and dashpay/dashmate-helper returned HTTP404/MANIFEST_UNKNOWN.
Release Platform workflow `38370753739` was queued. This is missing publication,
not proof of incompatibility; earlier beta.5 publication delays are historical.

After publication, resolve immutable artifact digests and validate actual
helper migration/environment/Compose and isolated service contracts. Use an
admitted task-owned runtime (none is installed locally), not a live network.
Any auxiliary decoder chosen for this release needs its own version-bound
replay gate; a prior beta's successful replay is not sufficient evidence.

Full evidence and the tested revision are recorded in workspace report
`reports/incident-068abd9616795c2be316b2bc/report.md`. No live upgrade/reset,
service restart, deployment, release/PR publication or Slack message occurred.
