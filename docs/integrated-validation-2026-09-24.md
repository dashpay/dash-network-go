# Integrated management and console validation — 2026-09-24

This report separates executable features, fixture tests, and live evidence.
It does not authorize an existing-network rollout or a public website cutover.

## Implemented interfaces

| Path | Behavior | Evidence |
| --- | --- | --- |
| Native devnet lifecycle | Direct AWS provision/bootstrap, Core/EvoNode registration, Platform startup and health | Earlier [14-host lifecycle and stop/resume proof](validation-2026-09-24.md) |
| Native image upgrade | Exact pinned images, serial withdrawal, health gates, durable interruption/recovery | Real 13-validator Actions upgrade, deliberate interruption and same-plan recovery |
| Existing-state management | Read-only import; explicit enrollment; scoped image replacement and restoration of captured workloads | Real Moutai/testnet observations; direct Docker API recovery contracts |
| New Core fullnode | Separate owned allocation joining a captured public chain profile; no wallet/mining/registration | Real Core 23 container join/replay; profiles captured from both existing networks |
| Public/operator console | Public allowlist, independent health, explicit GitHub-ID grants, reviewed Actions dispatch | [Companion status PR #6](https://github.com/dashpay/status/pull/6); unit and Chromium fixture checks |

New existing-network EvoNodes/Platform seeds and configuration/protocol migration
are not implemented by the Core fullnode join path. A restored workload is not a
new node. Existing-network enrollment/rollout is not proved by importing it.

## Existing-network observations

- Moutai: all 14 intended targets imported, and an independent four-minute health
  observation passed with consensus, common-height agreement and DAPI.
- Testnet: 29 of 30 intended targets imported. `seed-1` remains present and
  unknown, not silently dropped. AWS guest-reachability failure and console OOM
  evidence support a host fault; a seed-only reboot is awaiting authorization.
- The apparent testnet RPC configuration drift was fresh `rpcauth` salt. The
  adapter normalizes it only after verifying the on-host password/HMAC relation;
  it does not ignore arbitrary authentication or configuration changes.
- No Moutai/testnet enrollment, image change, restart, or new-node allocation was
  performed for this validation. Private configuration and credentials stay on
  their owning hosts or in private controller state.

## Disposable real-version upgrade proof

The authorized fixture uses 13 ARM validators and one wallet/miner, on the same
14 instances throughout. Target: Platform `4.2.0-beta.3` to `4.2.0-beta.4`, keeping
protocol 13, Core and the existing identities/data.

### First attempt and correction

[Actions run 36041833935](https://github.com/dashpay/dash-network-go/actions/runs/36041833935)
first exposed the repository's immutable-ID OIDC subject. Exact trust was
corrected; no broad wildcard was introduced. See [OIDC setup](lifecycle.md#oidc-subject-format).

The subsequent attempt changed Drive/DAPI on the first validator, then stopped
at the preservation gate: Tenderdash 1.8 panicked on an ABCI EOF and Docker
restarted it. Core was unchanged; no second validator was changed. The failed
operation and its original executable, plans, journal and private node data were
retained as failure evidence, not edited into a successful result.

Both upgrade adapters now deliberately stop Tenderdash before replacing Drive,
wait for the actual ABCI listener, and restart the retained Tenderdash container
when its image is unchanged. Planned dependency restarts are explicit; unexpected
automatic restarts, changed Core processes, identity drift and missing unrelated
containers still fail. Recovery markers cover accepted stop/start requests and
lost replies.

The disposable baseline was recreated with operator assistance on the **same**
instances, after archiving the failed fixture. This is not a demonstrated
production rollback/reset procedure. Fresh recipe-bound bootstrap/deployment
plans were required; old plan hashes were not rewritten. The new proof uses a
frozen executable built from `d335497`.

### Corrected run

[Interruption run 36048495361](https://github.com/dashpay/dash-network-go/actions/runs/36048495361)
upgraded and verified two validators, then received SIGINT. The claim was
released, the runner's temporary SSH firewall rule was revoked, and the journal
retained validator 3 as the unfinished target.

The first attempt of [resume run 36050237058](https://github.com/dashpay/dash-network-go/actions/runs/36050237058)
refused a missing Platform observation on validator 5 before changing another
node. Independent inspection found it running without restarts; the failed
observation did not reproduce. Only pending validator 3 still had its old images,
as expected from the interrupted operation. The exact failed run was retried,
without changing the network, plan or executable. Its original reports are
retained. The final controller source now preserves sanitized observation failure
reasons instead of hiding the SSH/RPC detail behind a generic missing observation.

The second resume attempt verified validators 3–11, then stopped while checking
validator 12 because a later observation of validator 7 was missing. The claim
was released with validator 12 still pending, so validator 13 was not withdrawn.
A separate diagnostic also captured an `unsupported-validator-quorum` response
on validator 1. Subsequent exact-endpoint reads showed quorum type 107 and all
12 expected members; 90 further reads did not reproduce the error. Services
continued advancing with zero automatic restarts. This is a non-reproducing
observation failure, not a proven diagnosis of its upstream cause. The third
attempt retains the same plan, executable and health/membership checks.

The third attempt completed successfully: all 13 validators were verified on
beta.4, and the shared claim was released. No membership or health gate was
relaxed, and the pending target was reconciled before the final withdrawal.
A separate SSH inspection confirmed all 14 Core container IDs **and process
start timestamps** match the pre-upgrade baseline. Independent `doctor` passed
all 14 targets with consensus progress, common-height agreement, Core/ChainLock
readiness and DAPI. Before/after snapshots confirmed:

- all 13 validators run changed Drive and DAPI images, with zero automatic
  restarts;
- Core containers/images are unchanged;
- identity, genesis, application configuration fingerprints and saved
  registration transactions are unchanged;
- exactly 13 registrations remain, with the same 13 collateral outputs locked.

This proves the tested ARM64 beta.3-to-beta.4 combination, not arbitrary versions
or protocol/database migration. Exact public image digests and executable
provenance are in [the release evidence](upgrade-release-evidence-2026-09-24.json).
The frozen proof executable retains its historical `lastError` after successful
recovery; final phase, health and preservation evidence establish completion.
The current controller clears that obsolete current-error field only after a
successful health gate, with a lost-response recovery regression test.

The independent [Actions doctor run 36056882414](https://github.com/dashpay/dash-network-go/actions/runs/36056882414)
also passed against all 14 hosts. Live Actions evidence therefore covers OIDC,
private pinned artifacts, exact host trust, upgrade, deliberate interruption,
same-plan resume, independent observation and temporary runner-firewall cleanup.
These were dedicated disposable credentials, not Moutai/testnet production roles.

### Cleanup

Cleanup completed before the 21:05 UTC deadline. At 20:50 UTC, a separate
read-only verifier confirmed all 14 instances terminated; all 14 retained root
volumes absent; no residual network-tagged volumes or security-group interfaces;
and the dedicated journal, security group, EC2 key pair, temporary IAM role,
private S3 artifact bucket and Actions SSH secret absent. The temporary GitHub
environment was then deleted and its absence verified. Only after these checks
was the expiry automation removed.

Private operation evidence was exported before deletion. The shared VPC/subnet,
OIDC provider, Moutai and testnet were outside cleanup scope. No extra instance
allocation or budget/deadline extension was used.

## Console evidence and remaining activation

The private Control UI Portal runs the real network observations. Browser tests
exercise desktop/mobile public views, login, numeric-ID grants, CSRF, exact plan
review, dispatch and logout against fixture identity/workflow providers. These
are not a live OAuth or production workflow execution claim.

[Status CI 36045672132](https://github.com/dashpay/status/actions/runs/36045672132)
passed on `1eaa0bc`, including Chromium. Operator execution remains disabled in
the preview. The real hostname, OAuth application, first operator grant and
production dispatch identity remain to configure and verify. No public site
has been replaced. Missing/stale observations remain visibly unknown.

## Reproducibility

All ten [Go CI jobs](https://github.com/dashpay/dash-network-go/actions/runs/36051831289)
passed on `385ec92`, including race, real DynamoDB claims, Docker recovery and
Core join contracts. All four downloaded binary checksums passed. The Linux
binary's embedded merge revision `9f6a735` was verified to contain `385ec92`, and
its validation smoke check passed. These artifacts are distinct from the frozen
`d335497` live-proof executable. The subsequent controller-only `3a20001` fix
clears a superseded failure after verified recovery; its focused lifecycle tests
pass. Final PR CI also covers that regression. Documentation-only evidence
commits do not change the retained live operation binary or its node recipes.

Keep the exact executable, plans and private journal with each operation. A new
recipe cannot silently resume an old mutating plan. Live-proof artifacts stay in
private operational storage; the repository contains sanitized conclusions and
testable contracts, not SSH keys, RPC credentials or private inventories.
