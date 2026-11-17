# Platform 5.0.0-rc.4 source compatibility check

## Bound revisions

- Release `415559736`, tag `v5.0.0-rc.4`, published 2026-10-17.
- Platform commit `5b41ca376b310a1792ddd9f3106e1224f9c275b6`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `0972b4ec4c095f38a078a96c81a6485f4d4b004e`.

**Exact-source configuration checks pass; artifact/runtime compatibility remains
incomplete.** This review changes documentation only. No production generator,
upgrade guards, dependency installation or deployed pins are changed.

## Release contract and validation

RC.4 changes Dashmate Platform status handling and raises its dashd-rpc package
range from ^19.0.0 to ^19.1.0. Defaults, schema, migrations, Compose and Core
template remain unchanged. Exact RC.4 stored defaults equal the existing RC.2
fixture after normalizing the task source root to `/platform`; no duplicate
fixture or speculative generator patch was added.

Passing checks on the isolated worktree:

- All 61 worker tests, including immutable image-pinning and migration/render
  ordering regressions plus existing Core-preserving upgrade refusal.
- Six actual Worker.configure outputs (validator/wallet/seed × initial/final)
  pass exact RC.4 Config/Ajv/schema and Core-template validation.
- Fifty exact upstream stock-image migration cases, two historical stored-default
  corrections and three missing-option shapes pass against RC.4 source.
- All fifty upstream-migrated configurations pass through the actual validator
  worker with explicitly requested immutable Drive/DAPI pins.
- Ten deployment-info migration cases and retained TTL/RPC/Tor checks pass.
- Twenty configuration-closure source blobs verified against the immutable tree;
  real task-local npm dependency-tree validation passes.

The npm check covers the actual configuration harness, not Dashmate's complete
updated RPC dependency installation or the unpublished helper environment.
Neither those runtime dependencies nor real helper/Compose/service/database
execution are claimed tested. No Docker executable is installed locally.

## Full dependency matrix remains a rollout gate

RC.4 source defaults include Core `dashpay/dashd:23`, Tenderdash
`dashpay/tenderdash:1.8`, gateway `dashpay/envoy:1.39.0-impr.1`, Tor
`osminogin/tor-simple:0.4.9.11` (source-pinned digest), rate limiter
`envoyproxy/ratelimit:3fcc3609`, Compose Redis `redis:alpine` and conditional
metrics `prom/statsd-exporter:v0.26.1`. Insight and quorum-list services are
conditional, not assumed enabled or runtime-validated by this check.
Drive and rs-dapi derive mutable `5-rc` defaults, overridden by dashnet's requested
immutable validator pins. Source defaults are not validated runtime digests.
Do not assume a previously installed Tenderdash or other component is compatible
merely because Core preservation was requested. Core24 legacy migration and its
separate owner-specific branch are not implicitly imported or certified here.

Protocol/runtime changes require exact-release validation beyond equal config
or protocol numbers. No product implementation was patched or custom-built.
No mining/signalling/protocol activation or live upgrade is authorized here.

## Remaining gate

At 2026-10-17 03:32:14–15 UTC, exact `5.0.0-rc.4` Drive, rs-dapi and helper tags
returned HTTP404/MANIFEST_UNKNOWN. Release Platform workflow `38998993424` was
queued. Final observations, independent review and tested revision are retained
in workspace report `reports/incident-66e02e68b88e4802db36a8a3/report.md`.

After publication, resolve immutable release and full dependency digests,
validate the actual helper/RPC installation, migration/environment/Compose and
isolated service contracts in admitted task-owned resources. Missing artifacts
are a publication blocker, not incompatibility evidence; older release success
does not prove RC.4. Preserve Core guards and all existing data.

No live deployment/upgrade/reset/restart, installed binary/config/pin change,
release or PR publication/merge, Core/Platform patch or Slack message occurred.
