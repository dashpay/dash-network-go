# Legacy testnet: in-place Platform 4.1 → 5

The ordinary managed image replacement is **not sufficient** for this transition.
`managed-plan --migration policy.json` opts into a fixed `legacy-testnet-v5`
adapter. No wipe, reset, bootstrap, vote, key export, database replacement,
volume deletion or automatic downgrade is part of this operation.

## Supported boundary

This is a **runtime migration for the managed Docker adapter**, not a Dashmate
installation upgrade. It retains Docker's effective environment, commands,
ports, networks, realized named/anonymous volumes, keys, identity and certificates.
The old Dashmate CLI, helper, source `config.json`, Compose files, gateway and
companions are not replaced. As with other managed upgrades, do not subsequently
run legacy Dashmate/Compose against these workloads: that independent controller
can overwrite the migration or revert images and does not honor the journal.

Source comparison: Platform [`v4.1.0` → `v5.0.0-rc.3`](https://github.com/dashpay/platform/compare/v4.1.0...v5.0.0-rc.3),
specifically Dashmate's `docker-compose.yml`, Core/Tenderdash templates and
`configs/getConfigFileMigrationsFactory.js`. The selected services' runtime
environment is unchanged by that release diff. The quorum-list cache TTL addition
belongs to an unselected companion; the manager does not upgrade that service.
Dashmate's removed JSON `unsafeOverride.commit` schema field is not loaded by the
direct Engine adapter; its corresponding obsolete Tenderdash TOML keys are removed.

The fixed migration:

1. Requires testnet validators already running **Core 24**, verified by RPC.
   Preserves each host's captured Core repository digest **and actual image ID**;
   there is no default to Core 23 and no Core image override in this mode.
2. Requires explicit new Drive and DAPI artifacts. Resolve and review the actual
   rc.3 build per architecture; mutable `5-rc` is not proof of rc.3 provenance.
   Custom source pins do not silently carry forward as the new Platform release.
3. Adds `getspecialtxes` and `getdeploymentinfo` to every existing
   `drive_consensus` whitelist entry, preserving the other permissions/users and
   credentials. Adds `deprecatedrpc=service` for Core 24's compatibility output.
   Included configs, ambiguous layouts and unexpected consensus users fail closed.
4. Removes only the obsolete `unsafe-commit-timeout-override` and
   `unsafe-bypass-commit-timeout-override` keys from `[consensus]` in Tenderdash's
   mounted `config.toml`. Other TOML, Core settings, Tor and identity files remain.
5. Stops DAPI → Tenderdash → Drive → Core before installing the configuration.
   Recreates the four containers with their **existing mount sources**, even when
   the image is unchanged, so file bind mounts see the atomically installed files.
   Starts Core first and verifies both RPCs with **Drive's consensus credentials**
   (password passed via stdin, never in output), then Drive → ABCI-ready →
   Tenderdash → DAPI. Whole-fleet health and the existing observed >2/3 quorum gate
   still apply between hosts.
6. Permits only protocol 13/14, rejecting unexpected versions and regression
   within a health sample or below the snapshot. Chain/genesis/proTx/node identity,
   consensus block agreement, advancement and preservation checks remain active.
   A plan-bound host receipt proves the whole configuration file set is unchanged
   after reversing only the fixed migration delta; unrelated drift still blocks.

Unknown layouts fail before the first service stop. In particular, companions
sharing a selected container's network namespace are unsupported. A backup and
write-ahead journal are root-only on the node. Original file bytes/permissions
remain in `migration-<PLAN_ID>.json` even after later operations. Do not publish
that file: it contains private configuration. An interrupted stop, file install,
container create or accepted start resumes the **same plan**, not a rollback.

## Plan (does not execute the upgrade)

Take a fresh authenticated import, explicitly enroll the intended managed nodes,
and prepare the reviewed release image choices as described in
[managed networks](managed-networks.md). Include all intended nodes, not merely
the current Tenderdash committee. Do not act on external operators' validators.

`policy.json`:

```json
{
  "kind": "legacy-testnet-v5",
  "epochStart": "2026-10-15T12:40:09Z"
}
```

**The timestamp above is an example, not an operational value.** Obtain the
current epoch start from chain evidence and review it before planning. It must
precede plan creation by no more than 24 hours. The policy and exact images are
hashed into the plan confirmation. The admission gate assumes one-hour epochs:
it refuses a new withdrawal in the first five or last ten minutes of the epoch,
and outside Core heights 13–20 modulo the 24-block DKG interval. This is a start
budget, **not a guarantee of restart completion**. Revalidate the epoch schedule
for the actual rollout. An already withdrawn node is recovered without waiting
for another window; a refusal before withdrawal rechecks fleet/quorum on resume.

```sh
dashnet managed-plan --snapshot snapshot.json --operation upgrade \
  --scope core,drive,tenderdash,dapi --images candidates.json \
  --migration policy.json --profile OPS --out change.json
```

`candidates.json` must supply Drive and DAPI; supply a reviewed Tenderdash target
if changing it. **Omit Core**: its exact existing pin is retained per node.
`--nodes` can select explicitly authorized validators while retaining other fleet
members as observers. The executor rejects broader scopes such as `all` and
Platform-only scope for this migration. Existing `managed-upgrade --plan ...
--confirm PLAN_ID` executes the reviewed artifact only under separate operational
authorization. Nothing here authorizes live staging, pulling or restarting.

After completion, take a fresh snapshot for ordinary `managed-doctor` and future
plans. The ordinary doctor intentionally remains strict when given an old,
pre-migration snapshot. Resume an interrupted operation with its original
snapshot, exact plan and compatible binary.

## Protocol coordination is separate from installing software

Do not equate installed v5 images, current committee voting power or READY status
with upgrade lock-in. For rc.3, lock-in requires `1 + floor(N * 67 / 100)` distinct
enabled EvoMN proposers with app version 14 in the **same epoch**, where `N` comes
from the on-chain enabled EvoMN list. Observe deduplicated actual block proposers,
lock-in and subsequent activation; recheck changing membership. The adapter does
not manufacture proposals, vote, fund traffic or promise activation timing.
Coordinate any in-flight DPNS/withdrawal test and external operators separately.

## Validation boundary

Go tests cover plan binding, preserved Core pins, scope refusal, narrow
configuration receipts and a simulated network-wide 13→14 activation. Python
tests exercise exact deltas, repeated RPC whitelist intersection, unrelated-file
drift, stopped-before-write ordering, interrupted installation, and a fake Engine
replaying lost create/start responses with unchanged mount sources and Core image.
These are **not a real Dash/Core/Tenderdash rehearsal**. Before live use, exercise
the actual candidate images and captured legacy layout on a separately authorized
disposable clone, including stop/restart, consensus RPC authorization, existing
database open/migration, protocol activation and interrupted-run recovery.

## Readiness-driven fleet-wide mining pause

The reviewed policy below binds **one fleet operation** to both owned miners,
all selected validators, and explicit architecture canaries. Stage every image
before pausing. Do not split the canaries and batches into separate plans.

```json
{
  "kind": "legacy-testnet-v5",
  "epochStart": "2026-10-15T14:40:09Z",
  "readinessDriven": true,
  "miners": ["miner-1", "miner-2"],
  "fleetMiningPause": true,
  "canaries": ["hp-masternode-2", "hp-masternode-4"]
}
```

The names and timestamp are examples, not operational authority. Verify one
amd64 and one arm64 canary, a chain-derived epoch anchor, and explicitly select
**all intended validators** in `--nodes`. Add each owned miner as an unselected,
enrolled Core-only fleet target with `miningService:
"dashd-generate-miner.service"`, verified EC2 identity/address/architecture and
actual Core container name. Import its stable service/script fingerprint and
original active state. Miners' Core containers, wallets and data are never
stopped or replaced by mining control.

Successful execution:

1. Verify initial Core/ChainLock/Platform progress and stage all images while
   mining runs. Wait for fresh healthy quorum, epoch and quiet-DKG admission.
2. Journal pause intent; arm independent recovery on each miner **before**
   stopping generation. Drain in-flight generation RPCs inside Core and confirm
   both miners agree on a valid Core checkpoint.
3. Upgrade the two explicit architecture canaries sequentially; verify and report
   both. Mining remains paused. Continue the rest sequentially with progress
   reports after each three-node review batch. Each node gets the same immediate
   health, available >2/3 voting-power and runtime-preservation gates.
4. Keep mining paused across epoch/DKG admission holds. Renew each host-local
   fifteen-minute recovery lease every minute, including during validator
   recovery. Arm the replacement timer before committing its new token; failed
   renewal cancels the rollout. Expired/foreign leases cannot authorize another
   withdrawal. An old timer cannot resume a newer lease.
5. While paused, check Core sync, unchanged identity/data/image/configuration,
   checkpoint retention, PoSe, DAPI and actual **Platform advancement**. Do not
   require new Core or ChainLock blocks between validators. External miners can
   still produce blocks: recheck live DKG and the checkpoint before withdrawal.
6. Verify the full selected fleet, actual artifacts and a common retained Core
   checkpoint. Journal final resumption, start both miners once, then require
   new Core, ChainLock and Platform progress before marking complete.

No mandatory four-minute sleep is used: evidence is checked immediately, with
15-second polls while waiting and ten-minute progress deadlines. Validator
apply/readiness is separately bounded to ten minutes. The operation's overall
deadline also bounds epoch holds and lease renewal; allow enough time for the
whole fleet and admission holds. This does not introduce simultaneous validator
withdrawal, automatic protocol activation, or a Dashmate CLI/helper upgrade.

### Failure and resumption

An error stops further withdrawals, stops renewal, and attempts to restore
owned mining using an independent cleanup deadline. The host-local watchdogs
also restore originally active generation after controller loss. Thus **one
pause/resume is the successful path, not a promise to leave the network paused
indefinitely after failure**. A watchdog can restore mining while a failed
validator still needs recovery; no zero-PoSe guarantee is made.

The journal records `pausing`, `paused`, `resuming`, `resumed` separately from
per-node completion. Resume the exact original plan/snapshot. A still-valid
owned pause continues without a start/stop cycle and without restaging. Lost or
expired pauses fail closed and reconcile mining before an explicit retry. If
mining was already restored on failure, recover an already withdrawn validator
before admitting a new one. A replay after final resumption never starts another
fleet pause. Existing network runner-claim rules still apply after a hard kill;
inspect before releasing a stale claim.

Older plans without `fleetMiningPause` retain their previously hashed per-node
mining behavior. **Do not execute those for this fleet-wide maintenance.** The
console's existing generic planner does not select this policy automatically;
use the explicit reviewed migration plan and matching executor.

Actual rc.3-image/legacy-data rehearsal, Dashmate CLI/helper reconciliation,
verified release pins and separate network GO remain required. Installing this
controller does not clear those gates or authorize stopping live miners.
