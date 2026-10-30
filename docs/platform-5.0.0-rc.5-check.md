# Platform 5.0.0-rc.5 compatibility check

## Bound revisions

- Release `417406934`, tag `v5.0.0-rc.5`.
- Platform commit `8fccc88fc8387622fc3a57a6367171df0e741297`.
- Upstream dash-network-go baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- Starting review revision `14910c364e8390ecf88ef06591ae5b7643cc0942`.
- Checked 2026-10-30; newer releases exist, most recently `v5.0.1`.

**Source configuration checks pass; full runtime compatibility remains unverified.**
This documentation-only change neither patches the generator nor authorizes a
live upgrade. No configuration failure requiring a generator patch was found.

## Exact source validation

The complete RC.4/RC.5 trees differ in 95 blob paths. Dashmate changes only its
package version: dependencies, defaults, schema, migrations, Compose, status
implementation and Core template are unchanged. Protocol declarations are
unchanged. Rust dependency and client proof-verification changes are not proof
of a dashnet generator defect and are not certified by configuration tests.

Passing checks in the isolated review worktree:

- 61 worker regression tests, including immutable pins and upgrade safeguards.
- Six actual Worker.configure outputs (validator/wallet/seed, initial/final)
  pass RC.5 Config/Ajv/schema/Core-template validation.
- 50 upstream stock-image migration cases, idempotence/schema checks, two
  historical stored-default corrections and three missing-option shapes.
- All 50 upstream-migrated configurations pass through the real validator
  worker and receive its explicitly requested immutable Drive/DAPI pins.
- Ten deployment-info migration cases and retained TTL/RPC/Tor checks.
- Twenty configuration-closure source blobs Git-blob verified against RC.5;
  generated stored defaults equal the existing RC.2 fixture after source-root
  normalization. Actual configuration-harness dependency-tree checks pass.

These checks do not execute the complete packaged helper CLI/RPC installation,
Compose or service/database runtime. No Go test run is claimed.

## Published artifacts and remaining execution gate

Unlike the historical publication observations, exact RC.5 images are now
available. Both amd64 and arm64 image/config digests were verified, and OCI
version/revision labels match the bound release. Immutable image indexes:

- Drive: `sha256:f5af44b4ae95aa8fc62eb5af2430aeb1b1582e7858ef484f1fb61f7732ae85f4`
- rs-dapi: `sha256:116676601a364f8f98ccfd6fe548473764b7c20e828ef698c04d46eab638a09e`
- helper: `sha256:7fcbd3b92fa18b06435ee7f80fc2c48095c4e7fed6ad85aa77950cd95ad9dff3`

All 26 amd64 helper layers were downloaded and SHA256-verified without extracting
or executing image contents. The 20 configuration-closure files in that actual
published image match the tested immutable Git source exactly. This establishes
packaged-source correspondence, not complete helper runtime compatibility.

Neither the local execution environment nor the Gateway execution environment
has Docker, Podman or nerdctl. A local user-namespace probe is denied. No shared
host runtime was installed or reconfigured. A concurrent Brian runner repair
owns that host; this task does not use or change its resources.

Next gate: coordinator-admitted, task-owned OCI execution resources for these
exact artifacts, complete helper dependency/CLI/status checks, effective full
component digest matrix and isolated service/database contracts. Do not use a
live network or retained mutable component tags as substitute evidence. Existing
Core-preservation guards remain unchanged; historical separately authorized
RC.5 live rollout excluded helper/CLI certification and is not this test result.

Detailed artifact, packaged-source, test, independent-review and execution
capability evidence is retained under
`reports/incident-c33756acaa532f1d70ff6b6f/`. Its report records the final tested
revision and outstanding blocker. No live configuration/pin/binary, service,
network, Core/Platform implementation, release, PR or Slack changes occurred.
