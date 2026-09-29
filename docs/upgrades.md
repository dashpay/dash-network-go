# Health-gated image upgrades

`upgrade-plan` and `upgrade` operate on **an existing, owned devnet deployment**.
They do not create/adopt networks, operate testnet, reset state or migrate
protocols. The original deployment/genesis plan stays immutable.

The current profiles are:

- `platform`: update selected release images other than Core on validator hosts,
  including the dashmate helper: the **target release's dashmate renders each
  validator again**, migrating its config as `dashmate update` does, and Compose
  recreates exactly the Platform services whose image or rendered configuration
  changed. A newer dashmate's configuration therefore arrives with its release.
  Core's rendered files are not installed by this profile: when a release changes
  them too, the rollout reports it and they take effect at the next Core rollout.
- `tenderdash`: replace Tenderdash only; Drive, DAPI, gateway and Core stay intact.
- `core`: replace the Core image on **every node**, one at a time: validators,
  then fullnodes and other nodes, then the mining node. On a validator, Tenderdash,
  DAPI and Drive are stopped gracefully (Tenderdash first), the node's dashmate
  renders Core's new image and Compose replaces Core (and its Tor sidecar, which
  shares Core's network namespace), Core must come back synced (and the masternode READY in the next
  health gate), then Drive, Tenderdash and DAPI restart in place; the gateway keeps
  serving. Core ignores MNAUTH until its own sync completes, so peers that
  reconnect during startup verify the validator while it never verifies them, and
  masternode de-duplication keeps those one-sided links: the validator then misses
  quorum connections, loses DKG contributions and is PoSe-punished (observed up to
  a ban). Once synced it therefore disconnects its peers once and waits (up to 5
  minutes) for an MNAUTH-verified link to every valid member of its quorums. Because DKG sessions advance only with blocks and the devnet's miner is
  dash-network-go's own, the runner waits for the quiet part of the 24-block DKG
  cycle (height mod 24 in 13..23: every devnet session has finalized by block 13
  and the next starts at 24), **pauses mining** while that validator's Core
  is replaced, waits until it is READY and connected (at least 30 seconds), then resumes mining: no DKG
  session can start while it is away, so it is not PoSe-punished. (A restart
  timed only by height was observed to miss a session.) A Platform quorum that
  formed with fewer members (at least the minimum of 9) does not stop a rollout. Allow about 8 minutes per node:
  a 13-validator devnet needs `--timeout 240m` or more (it resumes if exceeded). On the mining node the miner pauses and is recreated on the new image. (The miner exits on SIGTERM, so a pause lands inside the two-block window.)
  `dash.conf`, genesis, keys, wallets and chain data are kept; every step is
  journaled in the host marker, so a lost response resumes at the same step and
  never replaces Core twice. Other images are unchanged. A Core release that needs
  new configuration or a hard-fork activation is not handled by this profile.

Image availability is not a compatibility guarantee. The executor requires the
live protocol to remain the deployment's initial protocol, the live Platform
quorum (configured size 12) to have at least its minimum of 9 members, all known
owned validators, and **every target** to pass health checks. An image that requires another configuration/schema or
protocol needs a separately implemented adapter; this command never invents one.

## Human command sequence

Retain the creation binary/plan and use a build supporting that exact recipe.
Copy your private network file to `candidate.yaml` and change only image choices.
Do not edit or regenerate the original deployment, genesis or bootstrap plan.

```sh
# Registry discovery only; moving tags resolve once to immutable digests.
dashnet resolve --network candidate.yaml --out candidate.lock.json

# Read-only AWS/journal planning. Review exact old/new architecture-specific pins.
dashnet upgrade-plan --deployment-plan deployment.json \
  --network candidate.yaml --lock candidate.lock.json --scope platform \
  --profile YOUR_AWS_PROFILE --out upgrade.json

# --scope tenderdash, or --scope core to replace only Core on every node.

# SSH/journal mutation using the exact reviewed upgrade plan ID.
dashnet upgrade --plan upgrade.json --confirm UPGRADE_PLAN_ID \
  --ssh-key /secure/deployment-key --known-hosts /secure/known_hosts \
  --profile YOUR_AWS_PROFILE --observation-window 90s --timeout 75m \
  --out upgrade-result.json

# Original network plan; expected images come from the shared runtime record.
dashnet doctor --plan deployment.json \
  --ssh-key /secure/deployment-key --known-hosts /secure/known_hosts \
  --profile YOUR_AWS_PROFILE --observation-window 90s --timeout 5m \
  --out post-upgrade-health.json
```

Planning verifies cloud target identity and the last recorded image set. Execution
rechecks actual running digests, health and membership under the shared claim;
a source version changed after planning invalidates the plan. Tags are never
resolved again during a resumed operation.

## Execution and preservation

1. Claim the same network journal used by terminal/Actions deployment operations.
2. Require fresh whole-fleet health and capture public preservation fingerprints.
3. Stage/verify all needed image digests, at most sixteen hosts concurrently. Cached
   exact artifacts are reused. A staging failure withdraws no services. For a
   Platform rollout every validator also renders the target release (nothing is
   installed): every validator must render with the same dashmate release; sidecar
   images it requests beyond the pinned ones are pinned, and staging repeats. The
   services each validator will recreate are journaled, and its apply must change
   exactly those.
4. Record intent for **one validator** before its remote operation.
5. Under the host lock, verify ownership, exact Core process ID/start time/config,
   current images and unchanged service identities. Install the target render's
   Platform files and Compose environment (keys and genesis are never
   regenerated) and let Compose recreate the changed services.
   A Drive change gracefully stops Tenderdash before the ABCI disconnect, waits
   for Drive's ABCI listener, then starts Tenderdash. Its unchanged-image container
   and state are preserved; its process is intentionally restarted. Stop/start
   intent is recorded before each action for lost-response recovery. Unplanned
   automatic restarts still fail the post-upgrade gate.
6. Reconcile that same node after a lost response. Never withdraw a second node
   until the whole fleet passes advancing consensus, common-height block, DAPI,
   membership/protocol and Core/unselected-service preservation checks.
7. Persist current images separately from immutable creation intent. Subsequent
   `doctor`, `stop` and completed-upgrade `deploy` recovery use these images.

Old executables that do not understand the runtime journal refuse its unknown
fields; they cannot silently restore creation-time images. Keep the upgraded
binary and operation artifacts available for emergency use without GitHub/AI.

## Failure and recovery

There is **no automatic rollback**: an image may have migrated its database.
A timeout, failed canary, image drift, unknown member or unexpected Core restart
leaves the exact current node and desired images recorded. The node's write-ahead
marker retains image/config fingerprints, not secret-bearing Compose content.

Use `operation --plan ec2-plan.json` to inspect the shared record. Allow in-flight
remote work to settle, then rerun **the same `upgrade` plan/binary** with a fresh
output filename. A hard-killed controller leaves a non-expiring claim; the existing
[stopped-runner unlock procedure](provisioning.md) applies. Never force-unlock an
active runner or delete journal/host markers to make a retry pass.

An unfinished rollout blocks ordinary `deploy`/`stop` so that they cannot bypass
pending image intent. Resume that rollout first. Unexpected external process or
configuration changes require diagnosis and an explicitly reviewed repair; there
is not yet an automated abandon/rebase/rollback or emergency full-fleet stop
for a partially applied upgrade. A failed live version change is not "supported"
merely because its images were pulled.

The host marker records the target render's fingerprints. A resumed apply renders
again and must reproduce them exactly; Compose then converges only services not
yet on them, so a lost response never recreates a service twice. Missing
unselected services, changed unselected services, or absent/mismatched markers are
treated as drift—not as permission to recreate arbitrary containers.

## Actions

The protected `Operate managed devnet` workflow supports `upgrade-plan` and
`upgrade`, using the same CLI, OIDC and shared claim. Planning reads private
`deployment.json`, `candidate.yaml` and `candidate.lock.json`. Review its private
result and explicitly publish it as `networks/<network>/upgrade.json` in the
operations bucket before running `upgrade` with that plan ID. No automatic
promotion of a plan is implied by a green planning job.

`upgrade_scope` only affects planning; execution is bound to the content-addressed
reviewed plan. `observation_window` defaults to 90 seconds. Detailed logs/results
remain private. The workflow remains inert until its environment, OIDC role,
runner/access and private artifact store are configured; no live Actions execution
has been proved by the disposable terminal run.

## Evidence boundary

Go failure-path tests exercise sequencing, stale plans, lost responses, healthy
resume, membership, Core preservation, runtime-image and sidecar retention, and
staged-change enforcement. Python tests exercise re-rendering, deferred Core
changes, host markers, drift rejection and input boundaries. The disposable Docker
contract runs a validator rendered by the real dashmate helper and shows Compose
recreating exactly the service a new image changes. Re-rendering a 4.2.0-beta.3
node with the 4.2.0-beta.6 helper migrated its config and changed no service.
**This does not prove a Dash version-to-version upgrade.** A live existing-state
version change remains a separate acceptance run.
