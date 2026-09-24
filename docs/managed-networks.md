# Existing Moutai and managed testnet workloads

Both networks are intended to support deployment, upgrades and operator recovery.
Only the public dashboard and discovery are read-only. The `managed-*` path
operates existing, explicitly selected EC2/Docker workloads without requiring
that the native devnet creator originally made them.

## Implemented boundary

- `managed-import`: authenticated AWS/SSH/IMDS inspection; exports public workload
  fingerprints and chain/consensus/DAPI facts to a **private** snapshot. No host
  enrollment, state table write, image pull, configuration write or restart.
- `managed-enroll`: confirms the exact snapshot, takes the network journal claim
  and creates private host recovery definitions. Existing containers, keys,
  databases and original Dashmate/Compose files remain untouched.
- `managed-plan`: resolves candidates once to architecture-specific digests and
  binds original state, network, targets, component scope and previous operation.
- `managed-upgrade`: stages artifacts; withdraws one selected host at a time;
  preserves unselected workloads, configuration and existing mounts. Whole-fleet
  health, protocol, identities and quorum availability gate further withdrawals.
- `managed-deploy`: starts captured, stopped workloads with the **same images**.
  This is existing-state deployment/recovery, **not new-node provisioning**.
  Restoring stopped hosts does not wait for fleet quorum between each start; the
  final full-fleet health gate still must pass. A version change uses `upgrade`.
- `managed-doctor`, `managed-operation`, `managed-unlock`: independent current
  health, shared recovery state and exact-owner stopped-runner claim release.

Scopes are `core`, `platform`, `tenderdash`, and `all`. Platform scope preserves
Core and the Dashmate helper; it selects Drive, Tenderdash, DAPI and gateway where
present. Ancillary rate-limit/metrics/Tor containers are preserved, not recreated.
Seed-only Tenderdash containers are explicit targets, not silently omitted.

**Not implemented:** provisioning/registration of additional testnet nodes,
configuration/schema/protocol migration, resets, automatic downgrade or abandoning
an unfinished operation. These remain requirements/adapter work, not claims hidden
behind the word "deploy". Existing executors and the dashboard must expose these
capability limits honestly.

## Explicit manifest and trust

See [existing-network example](../examples/existing-network.json). Declare the
exact AWS account, region, network tag, existing journal table, SSH access and
instance/container bindings. Select only DCG-managed resources: public testnet as
a whole is not ours to deploy/reset. An unknown/replaced instance, wrong network
label, stale address, missing host key or unreachable target blocks management.

The journal uses the same `account/region/network` key as native deployments and
conditional, non-expiring owner/revision claims. Use **one shared table per
managed authority domain**—separate tables would not exclude competing runners.
A native-create record cannot be silently converted into an existing-network
record, nor can a changed manifest overwrite enrollment. Keep the manifest stable;
address/target/ownership changes need a reviewed rebind implementation, not manual
marker deletion.

Host keys must be independently verified and stored under the existing CLI alias
`<instance-id>.<region>.<account-id>.dashnet`. Reusing a verified key from an existing
operation is supported; blindly accepting `ssh-keyscan` output is not enrollment.
An expired/reused public IP must not inherit another instance's trust.

## Human command sequence

```sh
# Read-only: preserve every intended target, including unknown failures.
dashnet managed-import --manifest existing.json \
  --ssh-key /secure/key --known-hosts /secure/known_hosts --profile OPS \
  --out snapshot.json

# Explicit cutover into management; no service restart.
dashnet managed-enroll --snapshot snapshot.json --confirm SNAPSHOT_ID \
  --ssh-key /secure/key --known-hosts /secure/known_hosts --profile OPS \
  --out enrolled.json

# candidates.json: e.g. {"tenderdash":"dashpay/tenderdash:VERSION"}.
# Omitted selected components keep their captured immutable image.
dashnet managed-plan --snapshot snapshot.json --operation upgrade \
  --scope tenderdash --images candidates.json --profile OPS --out change.json

dashnet managed-upgrade --plan change.json --confirm PLAN_ID \
  --ssh-key /secure/key --known-hosts /secure/known_hosts --profile OPS \
  --observation-window 4m --timeout 110m --out upgraded.json

dashnet managed-doctor --snapshot snapshot.json \
  --ssh-key /secure/key --known-hosts /secure/known_hosts --profile OPS \
  --observation-window 4m --timeout 10m --out health.json
```

Take a fresh `managed-import` snapshot before planning a later operation so its
source container IDs/pins reflect the current state. Preserve the original
snapshot and exact plan/binary for an interrupted operation. A separate
`managed-plan --operation deploy` takes no image overrides and is executed with
`managed-deploy`; it restores previously captured containers after an intended
stop. It cannot bootstrap an uncaptured empty machine.

## What is preserved, and where secrets stay

The adapter reads Docker's effective configuration, not a guessed common Compose
file. Testnet's sidecars and historical per-service Compose overlays differ from
Moutai's layout. Commands/environment, labels, restart policy, port bindings,
network attachment/aliases and realized mounts are captured on the owning host.
All existing named **and anonymous** data volumes retain their exact sources.
No delete-volume flag is used. Configuration/identity/certificate contents never
leave the host; exported observations contain only hashes, identities and health.
Private host recovery files are root-only and may contain secrets: do not attach
or print them in chat, public artifacts or issue reports.

A version rollout modifies the selected container image, not the application's
configuration schema. Core process and native Core/Tor fingerprints are preserved
when Core is outside scope. Core upgrades preserve configuration/genesis and
require readiness after the expected process replacement. Live protocol changes
stop verification; migrated data is never blindly downgraded.

After enrollment, operate selected workloads through this manager. The original
Dashmate files are preserved, **not rewritten to the latest manager image pins**;
running legacy Dashmate/Compose deployment commands concurrently can revert
images or change configuration. Its independent process does not honor the new
journal claim. No assertion of exclusive control over other tools is made.

## Failure and quorum policy

Choose an observation window longer than the network's idle-block interval.
The managed default is four minutes: current Moutai emits empty blocks every
three minutes, so a 90-second sample can report no progress on a healthy idle
network. The gate still requires actual advancement; it does not reinterpret an
unchanged height as healthy. Actions exposes the same observation-window input.

The network journal records the pending target before withdrawal. On-host
write-ahead state records each selected container's original configuration and
replacement progress. A lost response or interruption between stop, remove,
create and start resumes the same plan/target. Existing companions and native
Core/Tor state are rechecked even if a selected container is temporarily absent.
New operations cannot replace an unfinished operation in either journal.

Only observed healthy, owned validators count toward the remaining **greater than
two-thirds** Platform voting-power gate. Unknown external testnet participants
are not presumed healthy to justify withdrawing our nodes. If the managed subset
cannot establish this margin, stop and expand independently verified observation
coverage or implement a reviewed network-specific policy—not an override button.

A hard-killed runner retains its claim. Inspect `managed-operation`, establish
that the controller and its remote operations have stopped, then use
`managed-unlock --manifest existing.json --expected-owner ID
--confirm-runner-stopped`. Resume the exact existing plan. Do not remove ownership
or operation files to make a mismatch disappear.

## Actions and UI

`Operate existing Moutai or managed testnet` runs the same commands from trusted
main, with separate `testnet-operations`/`devnet-operations` environments, scoped
OIDC roles, private artifact storage and verified SSH trust. It is inert until
those dependencies are configured; the workflow is not live-proved merely by CI.

Artifacts are private under `managed/<network>/`: `existing.json`, reviewed
`snapshot.json`, optional `candidates.json`, reviewed `plan.json`, and
`known_hosts`. Results/logs go under `operations/<run>-<attempt>`. Import and plan
outputs are **not automatically promoted** into privileged input artifacts.
Public Actions logs only show coarse stage names and success/failure.

The authenticated dashboard will dispatch these same workflows and show their
capability/health limits. Public visitors get an allowlisted read-only projection,
not raw imported snapshots. UI/authentication are not part of this implementation.

## Evidence boundary

Real read-only canaries on both networks verified existing workload layouts,
Core/Platform identity and TLS/HTTP2 DAPI. Go tests exercise all-target scope,
claims, staging failure, quorum gates, sequential withdrawal, pending-target
reconciliation and stopped-workload restoration. The disposable Docker contract
exercises actual container replacement, interruption and mount/config preservation
with stand-in images. No live enrollment or deployment/upgrade of Moutai or testnet
has been performed as part of this implementation.
