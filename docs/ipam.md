# Public IPv4 from the AWS BYOIP/IPAM pool

New publicly addressed devnets and servers must use an explicitly selected AWS
IPAM pool. **There is no fallback to Amazon-assigned public IPv4.** This is address
allocation from AWS IPAM, not registration with a separate inventory application.

```yaml
aws:
  # Other required account/network fields omitted here.
  provision:
    publicIpv4: true
    ipamPoolId: ipam-pool-00000000000000001 # replace with the regional owned pool
```

`provision-plan` verifies the pool's account, regional locale, public IPv4/BYOIP
source, EC2 use and ready state. The exact pool is part of the immutable plan ID.
Private-only allocations keep `publicIpv4: false` and omit the pool. Their servers
still use the selected VPC subnet's private IP space; the public pool does not
grant or configure network access. This initial adapter supports regional pools,
not Local Zones or cross-account shared pools.

## Execution and recovery

- Instance launch disables automatic public IPv4, even in auto-assign subnets.
- After every intended instance is running, allocate one Elastic IP per target
  using **`AllocateAddress.IpamPoolId`**, not `PublicIpv4Pool`.
- Ownership tags are applied in the allocation request. The shared journal
  records allocation intent before AWS, then retains address/allocation and
  association identities. Existing or foreign EIPs are never adopted.
- Association uses the exact owned instance with `AllowReassociation=false`.
  The readback must name its primary ENI and the intended pool/region.
- `compute-ready` for a new public fleet now requires all IPAM addresses to be
  associated. It still says nothing about SSH or application health.

`AllocateAddress` has no idempotency token. Its SDK retries are explicitly disabled.
If a response is lost, resume finds the tag-on-create address and reconciles it;
if it is not visible, the operation remains **unknown**, with no duplicate
allocation. Association and release also retain their uncertain outcomes.
Never delete the journal or clear an intent just to force another allocation.

A foreign attachment, detached previously-associated EIP, duplicate, changed pool,
missing recorded EIP or altered ownership stops the entire operation. Automatic
replacement/reassociation would break identities and hide leaks, so it is refused.

Historical immutable plans without `ipamPoolId` remain readable, and their existing
bootstrap/lifecycle plans are not rewritten. This version refuses **provisioning**
with such a public plan. Finish historical provisioning with its retained binary,
or review a deliberate migration; never edit a saved plan ID or silently move an
existing server's public address. Existing Moutai/testnet/status addresses are not
changed by this implementation.

## Address cleanup

Terminating EC2 does **not** release its Elastic IP. After separately authorized
teardown has terminated every original instance, promptly run:

```sh
dashnet release-addresses --plan ec2-plan.json --confirm EXACT_PLAN_ID \
  --profile YOUR_PROFILE --out released-addresses.json
```

The command acquires the same network claim, verifies **all** original instance
IDs, ownership tags/client tokens and terminated state before releasing any EIP.
All addresses must be detached and match the journal/pool/tags. It does not stop
or terminate instances, detach addresses, delete disks, or delete the journal.
If AWS has already removed the terminated-instance records, the tool fails closed;
retain evidence and review the resource scope manually. Stopped instances do not
qualify. It will never release an address from a still-running server.

Release intent is checkpointed before AWS. A lost reply is reconciled by absence,
not repeated against a potentially reused address. Successful cleanup marks the
allocation retired; provisioning cannot resurrect it. Partial cleanup remains
resumable. Root disks and other resources need their own reviewed cleanup.

## IAM and proof boundary

Add `DescribeIpamPools` and `DescribeAddresses` to scoped read access. Provisioning
also needs `AllocateAddress`, `AssociateAddress` and tag-on-create `CreateTags`;
restrict allocation to the approved pool and use plan/network ownership tags for
resource access. Cleanup separately needs `ReleaseAddress`. It does not need
`DisassociateAddress` or `TerminateInstances`. Keep this separate from read-only
observers and the status web process.

Failure/replay and cleanup are tested using fake EC2 plus real journal CI. Pool
and existing status-host address discovery were verified read-only against AWS.
No live EIP was allocated, moved or released to test this change; that proof must
use an explicitly authorized disposable footprint.
