# EC2 provisioning and recovery

This is the first **mutating** slice of the new engine. It creates a bounded
fleet directly with the AWS Go SDK and keeps its progress in DynamoDB. It has
been failure-tested with fake AWS clients; **no live AWS launch has been proved**.

It is deliberately named EC2 provisioning, not network deployment. There is no
node preparation, authenticated node transport, Core bootstrap, funding,
registration, Platform start, upgrade, reset, adoption, or destroy implementation.
`compute-ready` means every intended instance was observed **EC2 running**, not
that SSH, the OS, Core, Platform, or DAPI passed a health check.

## Concrete input

Start from [devnet-compute.yaml](../examples/devnet-compute.yaml). All IDs in the
example are placeholders. Keep the real definition and plan private.

- An explicit account, region, network name, generation, and canonical state table.
- An existing VPC, subnet, 1–5 security groups, and emergency SSH key pair. The CLI
  verifies ownership and placement; it neither edits firewall rules nor proves
  routes, security-group policy, outbound access, or SSH host identity. Review
  these first. `publicIpv4: false` suppresses public IPv4 allocation, **not IPv6**;
  an IPv6-enabled subnet can still assign IPv6. Security groups remain authoritative.
- One exact AMI **and owner account** per requested architecture. The CLI checks
  architecture, HVM/Linux/EBS, availability, root size, and absence of marketplace
  product codes/extra disks. These checks do not audit the AMI's content. Select a
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
A different generation or plan cannot bypass a previous claim/operation. This
milestone stores one immutable provision operation per network, not a general
workflow history service. It refuses changed plans even after completion. Reset,
replacement, and state migration need their own reviewed lifecycle implementation;
never delete the record just to make a retry pass.

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
before sending it. Instances and volumes get `dashnet:managed-by`, network,
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
it fail. All journal writes and releases are owner-conditional. This prevents a
stale runner from changing state, **not** an already-issued EC2 API request from
completing. There is no claim that DynamoDB fencing fences EC2 itself. The stopped
runner prerequisite matters. Never unlock just because a job appears slow.

## Permissions and Actions boundary

The CLI uses the normal AWS SDK credential chain. `--profile` is for terminals;
short-lived OIDC credentials can use the same executable without a profile.
Minimum API surfaces to scope in IAM are:

| Purpose | API actions |
| --- | --- |
| Identity | `sts:GetCallerIdentity` |
| Footprint/readback | `ec2:DescribeSubnets`, `DescribeSecurityGroups`, `DescribeKeyPairs`, `DescribeImages`, `DescribeInstanceTypes`, `DescribeInstances` |
| Instance creation | `ec2:RunInstances`, `ec2:CreateTags` for tag-on-create; scoped AMI, subnet, groups, key pair, and instance/volume resources |
| Journal | `dynamodb:DescribeTable`, `GetItem`, `PutItem`, `UpdateItem` on the one regional table |

Encrypted volumes using account-specific KMS policies may require additional
permissions. IAM restrictions are defense in depth; these action names alone are
**not** a ready-to-deploy least-privilege policy. There is no `iam:PassRole`, node
IAM profile, security-group mutation, termination, or journal deletion call here.

There is not yet a deployment workflow in this **public** repository. The existing
planning workflow remains read-only. Before wiring production credentials, add
trusted-ref/environment-scoped OIDC policies, protected environments, and
per-network `concurrency` with `cancel-in-progress: false`. DynamoDB provides the
cross-terminal/Actions exclusion that GitHub concurrency alone cannot provide.
Private plans, inventories, and journals must not be placed in public workflow
logs, summaries, or downloadable artifacts. An authenticated status view will
need a separate authorized operator-data path; it must never publish this record
through the public projection.
