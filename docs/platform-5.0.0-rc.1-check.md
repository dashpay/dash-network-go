# Platform 5.0.0-rc.1 source compatibility check

## Bound revisions

- Release `411823274`, tag `v5.0.0-rc.1`, published 2026-10-13.
- Platform commit `2f4d161c89bd1750a5cc2ceef391e1cee192a9a7`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `3b74e5614db4c355ce0a688740894ce18fd31224`.

**Source/configuration checks pass; artifact/runtime compatibility is incomplete.**
This documentation-only review changes no production code, dependencies,
deployed configuration, image pins or binaries.

## Exact-release evidence

Complete non-truncated beta.6/RC.1 Git trees differ in 245 blobs. Within Dashmate
only package.json changes, and its parsed contents differ only in version.
Defaults, Config/schema, migrations, Core template and Compose are unchanged.
All 20 configuration-closure source blobs were Git-blob-verified against the
immutable RC.1 tree and reverified after validation.

Real RC.1 stored defaults equal the existing beta.4 fixture after normalizing
the task checkout path to `/platform`. No duplicate fixture or generator patch
is indicated by these results.

Passing validation on the isolated worktree:

- All 51 existing node-worker tests.
- Six actual Worker.configure outputs (validator/wallet/seed × initial/final)
  validated with exact RC.1 Config/Ajv/schema and Core-template code.
- Exact upstream migration: missing-TTL beta.3 configuration rejected before
  migration and accepted afterward; local/nonlocal defaults and explicit
  operator TTL preserved; four invalid TTL values rejected; numeric-string
  coercion, RPC whitelist and Tor-bind assertions pass.
- Real npm dependency-tree validation and fixture equality.

Dependencies were copied into a task-local harness without source/schema stubs;
this is not a claim about the unpublished helper's installed environment.
Configuration checks do not prove database, wire-protocol or service-runtime
compatibility. The existing Core-preservation guard remains unchanged. The
older onion-bind migration in the status reset workflow is separate work and
is neither deployed nor transplanted here.

## Remaining gate

At 2026-10-13 15:25:47 UTC, exact `5.0.0-rc.1` tags for dashpay/drive,
dashpay/rs-dapi and dashpay/dashmate-helper returned HTTP404/MANIFEST_UNKNOWN.
Release Platform workflow `38421338025` was queued. This is missing publication,
not proof of incompatibility. No newer published release was found during the
check; RC.1 remains the bound target regardless of later publications.

After publication, resolve immutable image digests and validate actual helper
migrations, environment/Compose and isolated service contracts using task-owned
resources. No Docker executable is installed in the local environment. Do not
substitute a live network, older release or mutable family tag as evidence.
Selected auxiliary decoders require their own exact-release/data replay gate.

Full evidence, independent review and the tested revision are recorded in
workspace report `reports/incident-81898f055c86ecfd74565670/report.md`.
No live upgrade/reset/restart/deployment, installed binary change, product-code
modification, release/PR publication, merge or Slack message occurred.
