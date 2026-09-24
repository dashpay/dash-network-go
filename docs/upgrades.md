# Health-gated image upgrades

`upgrade-plan` and `upgrade` operate on **an existing, owned devnet deployment**.
They do not create/adopt networks, operate testnet, reset state, migrate protocols,
or implement Core upgrades. The original deployment/genesis plan stays immutable.

The current profiles are:

- `platform`: update selected release images other than Core on validator hosts.
  The helper image is cached only; no helper container is started.
- `tenderdash`: replace Tenderdash only; Drive, DAPI, gateway and Core stay intact.

Image availability is not a compatibility guarantee. The executor requires the
live protocol to remain the deployment's initial protocol, the supported 12-member
Platform quorum to consist entirely of known owned validators, and **every target**
to pass health checks. An image that requires another configuration/schema or
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
3. Stage/verify all needed image digests, at most four hosts concurrently. Cached
   exact artifacts are reused. A staging failure withdraws no services.
4. Record intent for **one validator** before its remote operation.
5. Under the host lock, verify ownership, exact Core process ID/start time/config,
   current images and unchanged service identities. Edit only image fields in the
   existing Platform Compose document; do not regenerate configuration or keys.
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

If Compose removed a selected old container but failed before creating its
replacement, the exact unfinished host marker permits recreating that selected
service on resume. Missing unselected services, a missing container after a
completed host rollout, or absent/mismatched markers are treated as drift—not as
permission to recreate arbitrary containers.

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
resume, membership, Core preservation and runtime-image retention. Python tests
exercise document-only edits, host markers, drift rejection and input boundaries.
The disposable Docker contract exercises actual image replacement and lost-response
replay with small stand-in services. **It does not prove a Dash version-to-version
upgrade.** A live existing-state version change remains a separate acceptance run.
