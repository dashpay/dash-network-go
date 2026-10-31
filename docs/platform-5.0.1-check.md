# Platform 5.0.1 compatibility check

## Exact revisions and result

- Release `425534983`, tag `v5.0.1`.
- Platform commit `093268c09bac75045756bc925deb530537b65580`.
- Upstream dash-network-go baseline `369a5173385f8a7bb1b03f57ecb989af431e18e9`.
- Tested source parent `8b09896c07b7c2bc12f0fb1e2cb3a75628034955` retains prior regression coverage and seed-TLS rollback retry-limit fix.
- Checked 2026-10-31. Newer `v5.0.2` exists; this check remains v5.0.1-bound.

**Source/configuration checks pass. Full runtime compatibility is unverified.**
No live upgrade is authorized and no production generator change is indicated by these checks.

Complete immutable source trees differ in 137 blob paths from v5.0.0.
Dashmate changes only its package version. Schema, defaults, migrations,
Compose and Core templates are unchanged; stock image derivation remains `:5`.
The worker replaces validator image selections with explicit release digest pins.
The stored fixture equals the retained RC.2 fixture after normalizing source-root
paths and the harness-assigned config-format metadata. No duplicate fixture added.
Configuration equivalence does not certify product behavior or dependencies.
Independent review found SDK PV14 feature preflight/refusal, document-shape
validation and stale-version retry changes; Drive maps inactive-feature errors
to INVALID_ARGUMENT. Dashnet consumes raw getStatus/Tenderdash/Core rather than
these SDK query APIs. Protobuf and rs-dapi/src trees are unchanged. Cargo.lock
changes local versions without third-party source/version/checksum/dependency
edge changes. No new matching dashnet failure was demonstrated.

## Actual validation

- 61 node-worker tests and 11 seed-TLS tests passed.
- Six actual worker configurations (validator/wallet/seed, initial/final) passed
  exact-release Config/Ajv/schema/Core-template checks.
- Fifty actual migrated configurations passed through the worker with explicit
  immutable Drive/rs-dapi selections.
- Fifty stock-image policy cases, two historical corrections, three missing
  shapes, ten deployment-info migration cases, TTL/RPC/Tor checks, idempotence
  and preservation of operator values passed.
- Twenty exact source-closure Git blobs verified before/after tests; harness
  dependency tree validated with npm ls.

Test inputs are synthetic; non-release component pins are not a resolved runtime
matrix. No packaged helper, CLI, container, service or database was executed.
The full Go suite passed for unchanged production/test source in the RC.7 review;
it was not rerun for this documentation-only change. The earlier retry-limit fix
is retained, not a new repair. Legacy seed-TLS rs_dapi refusal and Core guards
remain intact.

## Published artifacts and remaining gate

Index, platform manifest and configuration SHA256 hashes and release labels
verified for amd64 and arm64:

- `dashpay/drive:5.0.1@sha256:9e9d488a2c4e75585b86cc5d0627a48f4b664fd8398b010e31ddb0e7a18d0a68`
- `dashpay/rs-dapi:5.0.1@sha256:3de7da9cf3659686071f7e77bca1951bd2fe14fe88d36853fa15c80faf0ce59f`
- `dashpay/dashmate-helper:5.0.1@sha256:f48e1d463312356195e0125b95b26ffa6c77d66d425345be846d9c0f305854a8`

The known OCI execution-resource blocker persists: Docker, Podman, nerdctl,
runc and crun are absent locally; namespace creation returns Operation not
permitted. No shared runtime installation, runner mutation or live-service test
was attempted. Coordinator admission of an OCI-capable resource with bounded,
task-owned containers/storage and no conflicting use is required. Then resolve
the full dependency digest matrix and validate exact helper/CLI/RPC/Compose and
isolated service/database contracts. Missing execution is not incompatibility.

No live pin/configuration/binary change, Core/Platform product-code patch, PR
merge, publication or external message occurred. A normal revert of this document
is sufficient rollback. Earlier reports/receipts are unchanged.

Detailed evidence and independent release review:
`reports/incident-de41f2286c06bc54ec7a4ff1/`.
