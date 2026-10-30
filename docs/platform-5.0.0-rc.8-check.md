# Platform 5.0.0-rc.8 compatibility check

## Bound revisions and scope

- Release `421551047`, tag `v5.0.0-rc.8`.
- Platform commit `78d494f52572851d6502c99b151a7ce963dd0ac0`.
- Upstream dash-network-go baseline `369a5173385f8a7bb1b03f57ecb989af431e18e9`.
- Tested source lineage `4b781475d4f7f385fac3b8360220db966922ad64`, retaining the
  prior review's tested helper rollback retry-limit fix and regression coverage.
- Checked 2026-10-30; newer releases `v5.0.0` and `v5.0.1` exist. This check
  remains RC.8-bound.

**Source/configuration and artifact identity checks pass. Full packaged-runtime
compatibility remains unverified.** No live changes are authorized or performed.

## Release comparison

Complete RC.7/RC.8 trees differ in 128 blob paths. Dashmate changes only its
package version; configuration, migrations, schema, Compose and status source
remain unchanged. Twenty exact configuration-source blobs were verified against
the RC.8 tree. Stored defaults equal the retained RC.2 fixture after normalizing
source-root paths; no duplicate fixture or generator patch is indicated.

The wider product delta is not wire-identical: non-unique public-key-hash
identity proof responses and protocol-14 handling change. No matching identity
proof consumer was found in dash-network-go; its existing status use does not
prove compatibility for those unrelated consumers or the complete product stack.
Product dependency and proof behavior require their own runtime validation.
The legacy seed-TLS adapter still intentionally rejects the `rs_dapi` layout;
this check does not broaden seed adoption support or bypass Core guards.

## Actual validation

- 61 node-worker tests and 11 seed-TLS tests pass, including the inherited
  failure-recovery retry-limit regression.
- Six worker outputs (validator/wallet/seed, initial/final) pass the exact RC.8
  Config/Ajv schema and Core template checks.
- Fifty actual stock-image migration outputs pass through the validator worker
  with explicit RC.8 Drive and rs-dapi digest pins.
- Exact-source migration tests pass: 50 image-policy cases, two historical
  stored-default corrections, three missing shapes, ten deployment-info cases,
  idempotence, and retained TTL/RPC/Tor checks.
- All 20 source-closure blobs still match after testing. Generated defaults
  equal the existing fixture after source-root normalization only.

The full Go suite already passed for the identical source revision in the RC.7
review; it was not rerun for this documentation-only change. Tests use synthetic
node identities, no live wallets or data, and no running helper/containers.
Only the three release image identities below were resolved; other component
pins in worker fixtures are synthetic and not a full effective dependency matrix.

## Published artifact identity

Index/platform/config content hashes and version/revision labels were checked
for both amd64 and arm64:

- Drive `sha256:1369f8d98e12fece2db42fee5b90f9ba64ae85075ff0c1b2ef5337989f02f85c`
- rs-dapi `sha256:3ac163fe9ebcc7b63b60c0ae8ad8d8956a1cbd91059c7ebf8893c3f1cde1bba6`
- helper `sha256:d409ba4a828fad604c218e186e04ea9dbdfce5e1ae89d945886373c3b4c0ed76`

Images are published; missing artifacts are not the blocker. No RC.8 image
layers were executed and no packaged helper/CLI/RPC, Compose or service/database
stack acceptance is claimed.

## Remaining gate

The previously recorded execution-resource blocker remains: no local OCI runtime,
and user namespace creation is denied. No shared runtime installation, runner
change or fleet operation was attempted. Next, the coordinator must admit an
OCI-capable isolated resource with bounded task-owned containers/storage and no
conflicting work. Then resolve the full dependency digest matrix and validate
the exact RC.8 helper/service/database contracts. Live networks are not a test
substitute and no upgrade is authorized.

Detailed evidence, final tested commit and review:
`reports/incident-d2f537365af63000cce31ce4/`. Prior reports/receipts remain unchanged.
