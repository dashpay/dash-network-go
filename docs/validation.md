# Multi-validator AWS acceptance run

The next release gate is a **real disposable fleet**, not another mocked
consensus test. This document is a run plan, not a claim that the run passed.
Keep validation fixes together in one continuing PR; do not require a merge
between individual deployment stages.

## Concrete scope before allocation

Record the exact account, region, network/generation, approved time/budget and
cleanup authority privately. The initial profile needs **13 validators and one
wallet/miner**. Use dedicated hosts and a dedicated security group, never adopt
testnet, Moutai, or another running network for this experiment.

The proposed baseline is ARM64 Ubuntu 24.04, 13 `m7g.large` validators plus one
`m7g.medium` wallet/miner, and 100 GiB encrypted gp3 per host. Non-burstable
instances avoid CPU-credit surprises. Pin an owner-verified Canonical AMI and
release digests before allocation. Use an existing subnet with verified Internet
access, a temporary SSH key pair and a private regional DynamoDB journal. No new
NAT gateway, load balancer, DNS record, public dashboard or existing-network
change is part of this run.

Allow SSH only from the operator's current egress address; Core and Tenderdash
P2P only between validation security-group members. Gateway access, if needed,
is operator-only. RPC, Drive and Tenderdash administrative listeners remain
loopback. Outbound package/image access is needed for the fresh-image path.

Keep the exact binary/checksum, private network definition, release lock,
plans, trust file and operation output together. Full console/host logs and
private identities are never public CI artifacts. Log phase timings so time in
AWS launch, bootstrap, image pulls, collateral, quorum formation and Platform
startup can be distinguished.

## Acceptance sequence

1. Read-only prerequisite check: AMI owner/architecture/root mappings, instance
   offerings/quota, VPC/subnet/route, SSH identity, security rules and journal.
   Additional **EBS** mappings are rejected; Canonical's `ephemeral0/1`
   declarations do not imply billable extra disks.
2. Use the ordinary CLI for `provision-plan` → `provision`. Record **every**
   instance and root volume. Nothing is healthy merely because EC2 is running.
3. `resolve` → `bootstrap-plan` → `host-trust` → `bootstrap`. Verify all host
   identities and each role's pinned images. No unauthenticated key enrollment.
4. Generate one `deployment-plan` with the explicit protocol version from the
   selected Platform release source. Retain it; never generate a second genesis
   to get around an interrupted operation.
5. Run `deploy`. Require all 13 registrations to be confirmed and READY, all
   configured quorum types, fresh ChainLocks, consistent Core genesis, and
   advancing Platform consensus across the intended fleet.
6. Run independent `doctor` checks after deployment, including common-height
   Platform block agreement and functioning TLS/HTTP2/DAPI with live backends.
   Retain a row for each of the 14 targets; unreachable is unknown, not omitted.
   The default observation gap is 15 seconds. For this profile's 50-second
   proposal timeouts, also record a longer check with
   `--observation-window 90s --timeout 5m`. The report records the requested
   interval. This does not waive stalled-chain, identity, restart, quorum or
   block-agreement failures; retain short-window failures and investigate them.
7. Rehearse controller interruption/resume using the same binary and plans.
   Compare instance IDs, validator identities, genesis, signed registration
   transaction IDs and collateral before/after; no duplicate launch or payment.
   Inspect/settle remote work before releasing any abandoned controller claim.
8. Exercise explicit data-preserving `stop` and `deploy` resume on this disposable
   fleet; prove consensus and DAPI again. A process exit or container start is not
   the acceptance gate.
9. Save sanitized results and timings, then perform the explicitly approved
   teardown. Report success only after residual-resource checks complete.

Keep one failure visible at a time. Patch compatibility defects in the continuing
PR, retain the failed-stage evidence privately, rerun the relevant checks and
resume when the immutable-plan/recipe contract allows it. A changed recipe may
require a new explicitly scoped disposable run; do not edit plan IDs or delete
journal/host markers to force compatibility.

## Bound the run and clean up

Start the time limit when the first billable resource is created. Arrange a
durable expiry check before launch; expiry is a cleanup/attention path, not a
promise of healthy consensus. Stop before the authorized duration or budget
would be exceeded. Do not expand the fleet or extend it silently.

The current CLI's `stop` **does not terminate EC2 or stop billing**, and a
`destroy` executor is not implemented. Teardown for this first proof is a
separately scoped direct AWS operation:

- Export the final journal/evidence privately; no private keys in public reports.
- Re-read and compare account, network/generation, compute-plan tags, client
  tokens and the exact journaled instance IDs. Unknown or foreign resources stay
  explicitly unresolved; never widen a tag-based deletion to make it succeed.
- Terminate only those instances; wait for termination.
- Re-read the recorded root-volume IDs and ownership. Roots are retained by the
  provisioning contract: delete only the now-detached, explicitly approved test
  volumes and verify their absence. Termination alone is not cleanup.
- Remove only the dedicated validation security group/key pair when detached.
  Retain or remove the dedicated journal table according to the approved scope;
  never delete a shared table or state to unblock a running operation.
- Report exact residual instances, volumes, interfaces and supporting resources,
  including errors or unknowns. An empty resource list must follow a successful
  complete read, not a failed query.

## Follow-on work

Once creation/recovery is evidenced, prove existing-state, health-gated upgrades
(especially Platform-only with unchanged Core). Then build the public/operator
dashboard. Import **testnet and Moutai** there as existing observed networks;
display/import is not authority to reprovision or upgrade them.
