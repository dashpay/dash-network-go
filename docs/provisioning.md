# EC2 provisioning and recovery

This stage creates a bounded fleet directly with the AWS Go SDK and keeps its
progress in DynamoDB. Other commands handle the subsequent
[network lifecycle](lifecycle.md). For new public-facing nodes, use the explicit
[IPAM/BYOIP address contract](ipam.md). `compute-ready` means all intended instances
are running and their requested IPAM addresses are associated—not that SSH,
the OS, Core, Platform, or DAPI passed a health check.

## Concrete input

Start from [devnet-compute.yaml](../examples/devnet-compute.yaml). All IDs in the
example are placeholders. Keep the real definition and plan private.

- An explicit account, region, network name, generation, and canonical state table.
- An existing VPC, subnet, 1–5 security groups, and emergency SSH key pair. The CLI
  verifies ownership and placement; it neither edits firewall rules nor proves
  routes, security-group policy, outbound access, or SSH host identity. Review
  these first. `publicIpv4: false` suppresses public IPv4 allocation, **not IPv6**;
  an IPv6-enabled subnet can still assign IPv6. Security groups remain authoritative.
  `publicIpv4: true` requires `ipamPoolId`; Amazon automatic public IPs are never
  requested. The plan accounts for one IPAM Elastic IP per public target.
- One exact AMI **and owner account** per requested architecture. The CLI checks
  architecture, HVM/Linux/EBS, availability, root size, and absence of marketplace
  product codes/extra EBS disks. Canonical's ephemeral instance-store mappings are
  allowed; they do not allocate extra EBS volumes. These checks do not audit the
  AMI's content. Select a
  trusted image; it will boot as supplied, without user-data or an IAM profile.
- Explicit instance types/counts, at most 100 instances. No capacity or exact cost
  guarantee. The footprint summary lists instance count, total root storage, and
  public IPv4 selection; the plan lists every intended target.
- Encrypted gp3 root volumes, IMDSv2 required, one on-demand instance per target.
  Root volumes are **retained on termination**. Failed operations do not auto-delete
  machines or disks. They remain billable until explicitly cleaned up outside this
  milestone; use operation tags/IDs to review the exact footprint first.

The existing component image declarations remain desired intent. EC2 provisioning
**does not consume or install those images**. Save a release lock separately for
the subsequent node/bootstrap stage; do not infer compatibility from this result.

## One-time shared state setup

Use one regional DynamoDB table for all CLI/Actions operators of these networks.
It must be ACTIVE, in the same account/region as the fleet, and have only the
string partition key `Network` (no sort key). The CLI checks this contract.

For an approved account, the administrator can create it with the ordinary AWS
CLI (this command creates a billable cloud resource; it is not run by dashnet):

```sh
aws --profile YOUR_PROFILE --region YOUR_REGION dynamodb create-table \
  --table-name dashnet-operations \
  --attribute-definitions AttributeName=Network,AttributeType=S \
  --key-schema AttributeName=Network,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --deletion-protection-enabled

aws --profile YOUR_PROFILE --region YOUR_REGION dynamodb wait table-exists \
  --table-name dashnet-operations

aws --profile YOUR_PROFILE --region YOUR_REGION dynamodb update-continuous-backups \
  --table-name dashnet-operations \
  --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true
```

DynamoDB's at-rest encryption applies; restrict access and retain backups. Journal
records are **operator data**, not public status. Do not TTL, delete, copy between
active tables, or restore a journal while a runner is active. Do not grant runner
roles table deletion or state item deletion permissions. Restrict roles to the
canonical table: choosing another table is not a recovery procedure.

The network key is `account/region/name`, intentionally **without generation**.
A different generation or plan cannot bypass a previous claim/operation. The
journal holds one immutable provision operation per network key, not a general
workflow history service, and refuses changed plans even after completion. A key
changes hands in exactly one reviewed way:
[the name of a destroyed network is reused](#reusing-the-name-of-a-destroyed-network).
Reset, in-place replacement and state migration are not implemented; never delete
or edit a record to make a retry pass.

## Human and agent command path

```sh
# Read-only AWS checks; writes a new private local plan.
dashnet provision-plan --network networks/devnet-lab.yaml \
  --profile YOUR_PROFILE --out out/lab-ec2-plan.json

# Read the plan and footprint. PLAN_ID is its exact id, not a network name.
# This command can create billable EC2 instances and writes DynamoDB state.
dashnet provision --plan out/lab-ec2-plan.json --confirm PLAN_ID \
  --profile YOUR_PROFILE --timeout 15m

# Read the shared journal and runner claim.
dashnet operation --plan out/lab-ec2-plan.json --profile YOUR_PROFILE
```

`provision` repeats account/AMI/placement checks, acquires the shared claim, checks
**the whole live scope** before the first launch, and journals each launch intent
before sending it. Interrupted records retain a bounded `lastError`, runner ID,
and every target with observation timestamps for inspection from another machine.
Instances and volumes get `dashnet:managed-by`, network,
generation, plan, node, and role tags, alongside the configured discovery tag.
A legacy/unmanaged instance under the same network tag blocks provisioning; the
command never adopts it. Identity, ownership, placement, type, key pair, security
groups, and IMDSv2 drift block continuation. Stops/terminations are not interpreted
as permission to restart or replace a node. No target disappears from the record.

All launches are submitted before waiting for EC2 running, so node boots overlap.
Every target uses a deterministic EC2 client token bound to the exact plan and
node. After an interrupted run, rerun **the same plan and command**. Already
created targets are reconciled instead of recreated. The journal is authoritative;
a missing local output or GitHub artifact does not mean AWS did nothing.

### Lost response or failed launch

A request can be accepted even if the runner sees a timeout. If a journaled target
has no visible instance, resume **does not blindly submit another launch**. Retry
reconciliation after EC2 propagation. The current runner also waits through
post-launch visibility lag up to its command deadline.

If a launch-intent checkpoint exists but the request never reached EC2 (or was
rejected), this conservative first executor remains blocked. It does not assume
indefinite AWS token retention or use “not found” as proof of non-creation. A
reviewed reconciliation/repair procedure is needed; automatic reset of an
unobserved launch is not implemented. Preserve the plan, operation, runner ID,
AWS request evidence, and account/region while investigating. There is no
automatic rollback, replacement, or silent target omission.

### Crashed runner / persistent claim

Normal completion, failure, or cancellation records progress and releases its
runner claim. A hard kill or ambiguous DynamoDB response can leave the claim.
Claims have **no TTL or automatic stealing**: timing out is not proof a process
or in-flight AWS request has stopped.

Checkpoint retries are idempotent only for the exact same plan, current owner,
revision and serialized payload. A delayed retry cannot overwrite a later
revision, and different data at an accepted revision is refused. This handles a
lost write acknowledgement without weakening the persistent runner claim.

1. Inspect `dashnet operation` and identify the exact `owner` (also printed to
   stderr when the invocation starts).
2. Stop the old terminal process / GitHub runner and verify it cannot resume.
   Allow its in-flight AWS requests to settle and inspect EC2 activity.
3. Release only that exact owner, then resume the original plan:

```sh
dashnet operation-unlock --plan out/lab-ec2-plan.json \
  --expected-owner EXACT_OLD_RUNNER_ID --confirm-runner-stopped \
  --profile YOUR_PROFILE
```

Unlock is a conditional update, not journal deletion. A changed owner/plan makes
it fail. All journal writes and releases are owner-conditional. Checkpoint writes also
compare a monotonic revision, preventing delayed same-runner retries from
overwriting newer progress. This prevents a
stale runner from changing state, **not** an already-issued EC2 API request from
completing. There is no claim that DynamoDB fencing fences EC2 itself. The stopped
runner prerequisite matters. Never unlock just because a job appears slow.

## Reusing the name of a destroyed network

A network name is released for reuse when, and only when, the operation the
journal holds under its key proves that network is gone:

- its phase is `addresses-released`, which only `dashnet release-addresses`
  writes, after it has seen **every** journaled instance terminated under this
  plan's ownership tags and client token (or reported absent by its exact ID once
  EC2 purged it) and **every** address absent by its exact allocation ID;
- every target has a journaled instance ID, and no address is in any state but
  `released`;
- the record is consistent with its own plan, schema and revision;
- no runner holds its claim.

Nothing weaker counts. A terminated fleet whose addresses were not released, an
interrupted or pending operation, and a claimed record all keep their name. A
plan without IPAM public addresses (`publicIpv4: false`) has no address
retirement and therefore **no proof**: its name is never released by this tool.

Reuse is part of the plan, so it is reviewed like any other plan:

```sh
# After release-addresses for the old plan. Reads the journal (read-only).
dashnet provision-plan --network networks/devnet-lab.yaml \
  --profile YOUR_PROFILE --out out/lab-ec2-plan-2.json
```

When the key holds a retired operation, `provision-plan` says so and writes a
plan with `"succeeds": "RETIRED_PLAN_ID"`. That field is covered by the plan ID,
so the new plan differs from the retired one even if the network definition is
unchanged: every EC2 client token and every `dashnet:plan` tag of the new fleet
is new. `provision --confirm NEW_PLAN_ID` then, and only then:

1. reads the live record and checks again that it is exactly the plan named by
   `succeeds`, retired and unclaimed;
2. copies it verbatim to the archive key
   `account/region/name#retired#OLD_PLAN_ID#REVISION` in the same table, where
   `REVISION` is the journal revision that was read (create-if-absent; a repeat
   finds the same item and goes on);
3. replaces the live item with the new plan's fresh operation, claimed by this
   runner, in one write conditional on the old plan ID, that same revision and
   the absence of a claim.

Nothing is deleted. Of two runners, one conditional write wins; the other finds
an operation that is no longer the one it read and is refused by the ordinary
plan and claim rules. If the response to step 3 is lost, the claim may exist:
inspect and unlock it exactly as after any [crashed runner](#crashed-runner--persistent-claim),
then rerun the same plan. An SDK retry of the accepted write is recognised as
this runner's own. `bootstrap`, `deploy`, `join`, `upgrade` and
`release-addresses` never hand a name over; only `provision` does.

Steps 2 and 3 are two writes, not one transaction. A handover that stops between
them leaves the retired operation in place with a copy beside it, and the next
`provision` of the same plan goes on from there. Because the archive key names
the revision, a copy can never block a later attempt: if the retired operation
has a different revision by then, that revision is archived under its own key and
the earlier copy stays where it is.

A retired operation does not change any more. Once an operation is in
`addresses-released`, `provision`, `bootstrap`, `deploy`, `join` and `upgrade`
refuse to claim it ("allocation retired"), so none of them can write a checkpoint
into it. `release-addresses` can still be repeated against it and changes nothing.

The successor starts empty. No instance, address, bootstrap, deployment, runtime
or upgrade state is carried over, and nothing of the old fleet is adopted. Its
record keeps only a `predecessor` entry: the retired plan ID, the archive key and
the retired instance IDs. `dashnet operation --plan OLD_PLAN` keeps working and
shows the archived record, the revision that was replaced, with `"archived": true`. Planning the same network
definition again returns the same successor plan, so a lost plan file does not
strand an interrupted operation.

The live scope is checked as strictly as for a first provision: any instance
carrying the network tag that is not this plan's own blocks it. The single
exception is the retired predecessor's own instances, which EC2 keeps listing as
`terminated` for about an hour: an instance is skipped only if it is
`terminated`, carries the predecessor's `dashnet:plan` tag **and** its ID is one
the predecessor journaled. Such an instance can never run again. A stopped,
shutting-down or running one, or a terminated one the predecessor did not
record, still blocks. Only the direct predecessor is known, so a third reuse
within that hour waits for EC2 to purge the first fleet's instances.

What reuse does **not** change or cover:

- **Chain identity.** The Core devnet name and the Platform chain ID follow the
  network name and generation. A name reused at the same generation is the same
  chain identity as the destroyed network, on a new genesis. Raise the
  generation in the network definition if the two must be distinguishable.
- **Root volumes.** They are retained on termination and keep the old plan's
  tags. They are never adopted and never checked; delete them separately.
- **Existing-network enrollments.** A name enrolled with the managed-network
  commands shares the table and key space and is never retired.
- **Old binaries.** A release without this feature rejects a plan with
  `succeeds` and a record with `predecessor` as unknown fields.

## Permissions and Actions boundary

The CLI uses the normal AWS SDK credential chain. `--profile` is for terminals;
short-lived OIDC credentials can use the same executable without a profile.
Minimum API surfaces to scope in IAM are:

| Purpose | API actions |
| --- | --- |
| Identity | `sts:GetCallerIdentity` |
| Footprint/readback | `ec2:DescribeSubnets`, `DescribeSecurityGroups`, `DescribeKeyPairs`, `DescribeImages`, `DescribeInstanceTypes`, `DescribeInstances` |
| Instance creation | `ec2:RunInstances`, `ec2:CreateTags` for tag-on-create; scoped AMI, subnet, groups, key pair, and instance/volume resources |
| IPAM public addresses | `ec2:DescribeIpamPools`, `DescribeAddresses`, `AllocateAddress`, `AssociateAddress`, scoped `CreateTags`; see [IPAM](ipam.md) |
| Explicit retired-address cleanup | `ec2:ReleaseAddress` after every original instance is terminated and every owned address is detached |
| Journal | `dynamodb:DescribeTable`, `GetItem`, `PutItem`, `UpdateItem` on the one regional table. Name reuse needs nothing more: the archive and the handover are conditional `PutItem`s. `provision-plan` now also reads the network's item (`GetItem`) |

Encrypted volumes using account-specific KMS policies may require additional
permissions. IAM restrictions are defense in depth; these action names alone are
**not** a ready-to-deploy least-privilege policy. There is no `iam:PassRole`, node
IAM profile, security-group mutation, termination, or journal deletion call here.

The planning workflow remains read-only. Before wiring production credentials, use
trusted-ref/environment-scoped OIDC policies, protected environments, and
per-network `concurrency` with `cancel-in-progress: false`. DynamoDB provides the
cross-terminal/Actions exclusion that GitHub concurrency alone cannot provide.
Private plans, inventories, and journals must not be placed in public workflow
logs, summaries, or downloadable artifacts. An authenticated status view will
need a separate authorized operator-data path; it must never publish this record
through the public projection.
