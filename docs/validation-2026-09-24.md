# Disposable AWS lifecycle proof — 2026-09-24

**Result: live creation, registration interruption/resume, consensus/DAPI,
full stop/resume and complete scoped cleanup verified.** This was an
**operator-assisted** acceptance run, not an unattended latest-binary benchmark.

## Scope and observed stack

- One isolated devnet, 13 ARM64 `m7g.large` validators and one `m7g.medium` wallet/miner.
- Ubuntu 24.04, 100 GiB encrypted gp3 roots, one AWS region/AZ and existing networking.
- Dedicated SSH key registration, operator-only ingress, fleet-only P2P and DynamoDB journal.
- Core image channel `23` reported 23.1.8; Drive/rs-dapi 4.2.0-beta.3,
  Tenderdash 1.8.0, gateway 1.39.0-impr.1; exact architecture digests retained privately.
- Platform protocol 14, canonical devnet quorum profiles. Helper cached, not run.
- Maximum authorized footprint: four hours/$10 including teardown.
- Existing testnet and Moutai were excluded and not changed.

## Evidence

| Gate | Result |
|---|---|
| Direct EC2 launch | 14/14 running in 43.8 seconds; every retained root volume captured |
| Authentication/bootstrap | 14/14 EC2-console enrolled SSH identities and prepared hosts; 803.8 seconds |
| Registration interruption | Controller stopped after five registrations; resume preserved those exact transactions and completed exactly 13 |
| Core readiness | 13 READY validators, synchronized Core, all three quorum types and fresh ChainLocks |
| Independent health | 14/14 healthy at 13:42 UTC; all 13 validators advancing, matching common-height block hashes and live TLS/HTTP2/gRPC DAPI |
| Data-preserving stop | Miner first, then validators; every service stopped in 178.3 seconds |
| Resume | Original plan/genesis and all 13 registrations reconciled; network-ready in 417.0 seconds |
| Independent post-resume health | 14/14 healthy at 14:02 UTC; Platform heights 361–365 across sequential observations |
| Preservation | Same instances/container IDs, configuration/genesis fingerprints and public identities; same 13 registration transactions and locked collateral outputs |
| Cleanup | Verified 14:06 UTC: 14 instances terminated, 14 retained roots deleted, dedicated journal/key/group removed, no residual volumes/interfaces |

The approximately two-hour footprint was below the authorized time/budget;
compute/storage/public-IPv4 baseline was roughly $2.70, **an estimate, not a bill**.
Expiry protection was installed before launch and removed after verified teardown.
Private operation records and final journal were retained outside the deleted fleet.

## Defects discovered and fixed in the continuing PR

- Canonical AMIs declare ephemeral mappings in addition to the EBS root; these
  declarations are not unexpected billable disks.
- SSH must negotiate an algorithm actually pinned for the exact host alias.
- Core 23 spork mutations use `sporkupdate`, not the read-only `spork` RPC.
- A committed journal write with a lost acknowledgement must accept an identical
  retry while still rejecting stale revisions, changed payloads and other owners.
- Tenderdash 1.8 HTTP GET can return bare objects instead of JSON-RPC envelopes.
  Read-only decoding is now separated from the immutable mutation recipe.
- Cancelled SSH setup must report cancellation, not a misleading routing error.
- Whole-fleet stop must withdraw the miner before validators.
- Final-health resume can verify an already-running fleet without repeating wallet
  or registration operations.
- A configurable observation window records progress over consensus round changes.
  Short-window failures were retained; nil-vote rounds/pauses were investigated.
  The successful acceptance observations used a 90-second gap with unchanged
  identity, quorum, DAPI, restart and block-agreement requirements.

## Operator assistance and remaining limits

Three intended devnet sporks were applied directly on the owning wallet after
confirming Core's RPC mismatch. Subsequent controller builds retained the exact
original mutation recipe while including reviewed journal/read-only fixes. Plans,
genesis, host ownership markers, identities and persistent data were not replaced
to force compatibility. Build provenance/checksums are retained with private evidence.
A controller egress change was repaired by rotating only the dedicated operator
`/32` ingress rules; no node services were restarted for that access issue.

The full run included investigation and interruptions, so its total duration is
not a clean deployment-speed measurement. Cold runtime/image preparation remains
a significant optimization opportunity. These results do **not** prove unattended
creation from the final PR binary, live version upgrades, managed-testnet rollout,
GitHub Actions/OIDC execution, generalized destroy, or dashboard integration.
