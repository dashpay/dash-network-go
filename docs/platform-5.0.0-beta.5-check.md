# Platform 5.0.0-beta.5 source compatibility check

## Bound revisions and result

- Release `410812254`, tag `v5.0.0-beta.5`, published 2026-10-12.
- Immutable Platform commit `371a3b3961c24cdb4bcd5aed1d0b6f32f5fcc025`.
- dash-network-go upstream baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `ac067c5dcc242f6d5683dd377fe809665d47194a`.
- **Source/configuration checks pass; full release compatibility is incomplete.**

This documentation-only review extends the existing beta.3/beta.4 test branch.
No production source, dependency, deployed configuration, binary or image pin
changes are indicated by the source checks or made by this review.

## Exact-source checks

The complete, non-truncated beta.4 and beta.5 Git trees differ in 499 blobs.
Within Dashmate only `package.json` changes, from version `5.0.0-beta.4` to
`5.0.0-beta.5`; dependencies are unchanged. Config/defaults/schema/migrations,
Core template and Compose are byte-identical. Nineteen of the 20 fetched
configuration-closure blobs match beta.4; all 20 were verified against the
immutable beta.5 tree using Git blob hashes.

Real beta.5 `Config.getStoredOptions()` matches the existing beta.4 test fixture
after normalizing the task checkout path to `/platform`. The existing fixture
and regression tests are reused rather than adding a duplicate fixture.

Passing checks on the isolated source tree:

- All 51 node-worker tests pass with `python3 -m unittest discover -s
  internal/node/testdata -p 'test_*.py'`.
- Six actual `Worker.configure` outputs (validator/wallet/seed, initial/final)
  validate with the exact beta.5 Config/Ajv and Core template.
- The exact upstream migration still rejects unmigrated beta.3 options, then
  accepts them after migration; local/nonlocal TTL defaults, an explicit TTL5,
  invalid TTL values, RPC permissions and Tor binds retain their contracts.
- Twelve actual `Worker.normalized` comparisons verify RPC salts and retain
  the known beta.3-to-beta.5 Core-content difference. Tor-on changes active
  bind directives; Tor-off changes comments/blank lines.

There is **no new beta.4-to-beta.5 Core-template change**. The prior Core-change
constraint applies to older configurations such as the tested beta.3 baseline;
it is not evidence that every beta.4-to-beta.5 upgrade is refused. Preserve the
existing guard. Runtime fingerprints still require exact-artifact validation.

Latest/desired Platform protocol dispatch remains 14. Product implementation
changes do not become verified runtime/database compatibility merely because
the configuration contract and protocol number are unchanged.

## Remaining release gate

At 2026-10-12 16:37:51–53 UTC, Docker Hub returned HTTP404/MANIFEST_UNKNOWN for
`dashpay/drive:5.0.0-beta.5`, `dashpay/rs-dapi:5.0.0-beta.5` and
`dashpay/dashmate-helper:5.0.0-beta.5`. Release Platform workflow `38278544703`
was queued. Missing publication is not proof of incompatibility. No newer
published release was found during the check.

After publication, resolve immutable image/index/config digests for supported
architectures and verify actual helper migration, environment/Compose and
isolated service contracts. The local check environment has no Docker runtime;
use an admitted isolated runtime, never a live network. Older Core configuration
transitions retain their separately recorded migration follow-up. Do not bypass
the preservation guard or substitute mutable tags to complete the check.

Detailed source manifests, harness, test logs and the tested commit are recorded
in workspace report `reports/incident-eae531d7493241be9b4e4ce1/report.md`.
No live upgrade, deployment, restart, release/PR publication or Slack message
was authorized or performed by this release check.
