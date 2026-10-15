# Legacy testnet: in-place Platform 4.1 → 5

The ordinary managed image replacement is **not sufficient** for this transition.
`managed-plan --migration policy.json` opts into a fixed `legacy-testnet-v5`
adapter. No wipe, reset, bootstrap, mining, vote, key export, database replacement,
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

## Readiness-driven, owned-miner-coordinated execution

Opt in through the reviewed migration policy (the fields are hashed into the
plan, not an ambient environment variable):

```json
{
  "kind": "legacy-testnet-v5",
  "epochStart": "2026-10-15T14:40:09Z",
  "readinessDriven": true,
  "miners": ["miner-1", "miner-2"]
}
```

Again, the epoch timestamp is illustrative, not operational authority. Explicitly
list the validators in `--nodes`. Add each owned miner as an **unselected**
Core-only fleet target, with `miningService: "dashd-generate-miner.service"`, its
verified EC2 identity/address/architecture and actual Core container name. Import
and explicitly enroll those miners too. Their service and script fingerprint and
original active state must be captured in the snapshot. The plan never replaces
a miner's Core container, image, configuration, wallets or chain data.

Readiness mode replaces the fixed four-minute health gap with immediate checks,
then 15-second polling when evidence is missing, bounded to ten minutes. Each
poll compares against the original progression baseline, so a slow block does
not cause false failure after four short samples. It rejects identity/protocol
regression, conflicting common Platform history, increased PoSe penalty, and
banned/removed nodes. Core, ChainLock and Platform advancement are still required
before staging and after each restarted validator. Fresh pre-withdrawal samples
check health and available quorum without waiting for another block merely to
repeat a just-completed progression check. The ordinary non-opted-in workflow
retains its former behavior.

For each new withdrawal:

1. Verify epoch/DKG admission, then pause only the explicit mining services.
   Each miner receives an operation-bound, tokenized fifteen-minute systemd
   recovery timer **before** generation stops. A private write-ahead receipt
   survives a lost SSH response. No timer extends implicitly on retries.
2. Wait for Core's `getrpcinfo` to show no active generation RPC; stopping a
   client alone is not sufficient. Confirm miners are inactive and agree on a
   valid Core tip. Recheck fleet health and available quorum after this drain.
3. Recheck epoch/DKG and the paused Core checkpoint on the target, perform the
   same backed-up no-wipe migration, then poll target readiness. Controller work
   for apply/readiness is bounded to ten minutes, below the independent lease.
4. Require the recovered validator to retain the paused checkpoint, and verify
   the mining leases did not expire. Resume mining and verify active services,
   then require Core/ChainLock and Platform progression before the next node.

Any error attempts mining restoration using an independent cleanup deadline.
The watchdog also restores an originally active service after controller death,
provided its configuration fingerprint still matches. Foreign pauses and
originally inactive services are never claimed. Old timer tokens cannot resume a
later pause. An expired lease or ambiguous recovery stops further withdrawals.
Already withdrawn validators are recovered without waiting for a new epoch/DKG
admission window. Their prior owned mining pause is reconciled before recovery.

Public miners are outside this controller's authority, so owned-miner pauses
**cannot guarantee a frozen chain or zero PoSe risk**. The height admission gate
remains in force, even with mining paused. A watchdog can restore generation
while a failed validator is still recovering; that is an explicitly bounded
network-pause fallback, not a promise that its DKG risk has disappeared.

Use separate reviewed canary and small-batch plans/resumptions; this change does
not introduce parallel validator withdrawal, auto-select a canary, upgrade the
Dashmate CLI/helper, activate a protocol, or authorize a live network operation.
The actual-image/legacy-data rehearsal and exact release pins remain required.
