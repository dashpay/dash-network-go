# New Core fullnodes on an existing chain

`join` is a **new-node deployment**, not `managed-deploy`'s restoration of captured
containers. It provisions/prepares a separate owned allocation and joins the same
Core chain as testnet or a devnet. It never creates a wallet, mines, registers a
masternode, copies a validator identity or rewrites an existing network.

Current new-node roles: **Core fullnode only**. Adding existing-network EvoNodes,
Platform seeds or migrating protocols is not implemented. Existing validators can
be explicitly enrolled and upgraded through the managed commands.

## 1. Capture the public chain contract

Use an independently verified existing manifest and trusted SSH keys. This is
read-only and does not enroll the source. The result contains a confirmed
checkpoint, network/genesis, a peer endpoint and allowlisted public chain options.
It does not include RPC passwords, wallets, BLS keys or private spork keys.

```sh
dashnet managed-join-profile --manifest testnet.json --source hp-masternode-1 \
  --profile operator --ssh-key ~/.ssh/deploy --known-hosts ./trusted_hosts \
  --out chain.json
```

Review the chain/checkpoint against another trusted node. Review reachability of
the peer address from the new allocation. Add additional independent IPv4:port
peers to `chain.json` if needed before making a plan. Testnet uses compiled
consensus defaults; devnet custom consensus parameters are explicitly retained.
Unsupported include files and consensus overrides are rejected, not silently
copied. An incompatible release or wrong genesis/checkpoint fails the join.

## 2. Allocate fresh hosts

Copy `examples/devnet-compute.yaml`. Use a **unique allocation name**, for example
`testnet-rpc-20261001` with `chain.type: testnet`, or `devnet-moutai-readers-1`
with `chain.type: devnet`. The allocation name is its AWS tag/journal ownership
namespace, not the Core chain name. Never reuse `testnet`/`devnet-moutai` or grow
an existing immutable compute manifest in place.

Set every node group to `role: fullnode`. Specify the reviewed AMIs, subnet,
security groups, emergency key, instance count/type, root disk size and existing
journal table. Size disk/sync time for the actual chain. No firewall, DNS or
load-balancer changes happen implicitly.

```sh
dashnet validate --network fullnodes.yaml
dashnet resolve --network fullnodes.yaml --out release.lock.json
dashnet provision-plan --network fullnodes.yaml --profile operator --out compute.json
# Review footprint and cost before authorizing the exact printed ID.
dashnet provision --plan compute.json --confirm COMPUTE_ID --profile operator
dashnet bootstrap-plan --compute-plan compute.json --lock release.lock.json \
  --address private --out bootstrap.json
dashnet host-trust --bootstrap-plan bootstrap.json --profile operator --out node_hosts
dashnet bootstrap --plan bootstrap.json --confirm BOOTSTRAP_ID --profile operator \
  --ssh-key ~/.ssh/deploy --known-hosts ./node_hosts
```

The Core image is cached by digest; bootstrap does not start services. Fresh-node
checks refuse existing containers or foreign ownership. Testnet allocation refuses
wallet/miner/validator roles rather than accidentally deploying a private genesis.

## 3. Join and verify

```sh
dashnet join-plan --bootstrap-plan bootstrap.json --chain chain.json \
  --profile operator --out join.json
dashnet join --plan join.json --confirm JOIN_ID --profile operator \
  --ssh-key ~/.ssh/deploy --known-hosts ./node_hosts --timeout 4h --out joined.json
```

The join uses the shared allocation lock/revision journal, exact EC2 placement,
pinned SSH identity and local host lock. Each host receives fresh local-only RPC
credentials, a disabled wallet and persistent owned storage. A joined result
requires every target's expected genesis/checkpoint, synchronization, peers,
zero restarts and recent ChainLocks relative to its tip. It is not a permanent
network-health claim. Source nodes are never restarted or reconfigured.

A timeout or lost response preserves the container, data and incomplete targets.
Inspect `dashnet operation --plan compute.json` and rerun **the same** join plan
and binary. Do not rerun fresh bootstrap after starting containers. Do not create
another allocation to work around uncertain launch outcomes. No auto-downgrade,
reset, old-node replacement or disposal occurs.

The resulting allocation has independent ownership. It is not silently appended
to an existing legacy managed manifest: fleet enrollment/expansion and dashboard
control of these new allocations are separate explicit steps. Keep all unresolved
legacy targets in their original reports.

## GitHub Actions

`operate.yml` supports `provision`, `bootstrap`, `join-plan` and `join` for the
reviewed allocation bundle. Testnet-named allocations use a separate protected
`testnet-fullnode-operations` environment; they do not inherit the existing testnet
validator-management role. Bundle inputs are fixed paths under
`networks/ALLOCATION/` in the private bucket: `ec2-plan.json`,
`bootstrap-plan.json`, `chain.json`, `join.json`, and verified `known_hosts`.
Planning outputs are not silently promoted. Retain the exact compatible CLI
revision; recipe drift rejects execution. The workflow's 75-minute sync deadline
may be insufficient for a large chain: resume the same join from another run or
the retained local binary. The shared journal, not the runner, owns progress.
