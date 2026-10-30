# Existing mainnet DAPI seed TLS adoption

This is **not** mainnet support in the network provisioner or managed upgrader.
The `seed-tls-*` commands operate an explicitly identified, existing AWS DAPI
bootstrap gateway on port 443. A DAPI bootstrap seed is not a Tenderdash P2P
seed. No Core, DAPI, Drive or Tenderdash version/configuration change is allowed.

## Supported layout and authority

- Exact AWS account/region/instance/public-IP binding, independently trusted SSH
  key under `<instance>.<region>.<account>.dashnet`, Ubuntu SSH user, root sudo.
- Existing Docker Compose-labelled `gateway`, `core`, `dapi_api` and
  `dapi_core_streams`; optional existing Drive/Tenderdash/rate-limiter services.
- Envoy's `/etc/envoy/hot-restarter.py /etc/envoy/start_envoy.sh`, public
  443 -> 10000, and separately bind-mounted `bundle.crt` / `private.key` files.
- A named Dashmate helper and an installed Debian Dashmate package with **no
  maintainer scripts, triggers, cron entries or systemd units**. Other layouts
  stop for adapter work; they are not rewritten speculatively.
- Core must report the main chain, no IBD, and matching blocks/headers.

The host's EC2 identity is checked again over IMDSv2. Inspect exports hashes and
identities only. Raw Docker definitions, environment and certificate/private-key
backups remain root-only on the host. The operator separately authorizes the
exact mainnet work; access alone is not authorization.

## Prepare and execute one seed

Example target (replace every value with verified inventory):

```json
{
  "name": "mainnet-seed-1",
  "account": "123456789012",
  "region": "us-west-2",
  "instance": "i-00000000000000001",
  "address": "8.8.8.8",
  "hostname": "seed-1.example.org",
  "gateway": "existing_mainnet-gateway-1",
  "helper": "existing_mainnet-dashmate_helper-1"
}
```

```sh
dashnet seed-tls-inspect --target seed.json --profile OPS \
  --ssh-key /secure/operator --known-hosts /secure/seed-hosts --out snapshot.json
dashnet seed-tls-plan --snapshot snapshot.json \
  --acme-image docker.io/goacme/lego@sha256:REVIEWED_DIGEST \
  --acme-email ops@example.org --out plan.json
dashnet seed-tls-apply --plan plan.json --confirm EXACT_PLAN_ID --profile OPS \
  --ssh-key /secure/operator --known-hosts /secure/seed-hosts --timeout 30m \
  --out applied.json
dashnet seed-tls-doctor --target seed.json --profile OPS \
  --ssh-key /secure/operator --known-hosts /secure/seed-hosts
```

Plans bind the worker/renewal recipe, immutable ACME image, exact target and
fresh (24-hour) source snapshot. Apply rejects configuration/container drift.
EC2 Instance Connect can provide temporary SSH access separately; the tool does
not install permanent SSH keys or change IAM/security groups/DNS.

Port 80 must be reachable for HTTP-01 and unused on the host. DNS must resolve
only to the bound IPv4 (no unrelated A or AAAA records). Check inherited CAA and
firewall policy before applying. The ACME account contact must be operational.

## What changes

1. Stage a reinstallable Dashmate rollback `.deb`, raw private Docker recovery
   definitions and original certificates under
   `/var/lib/dashnet/dapi-seeds/<name>/`; pull the digest-pinned ACME image.
2. Disable only the named helper's restart policy and stop it, preventing a
   competing certificate writer. Core and application containers stay running.
3. Issue a **staging** certificate into a separate account/storage directory to
   prove HTTP-01 and the combined DNS/IP SANs. Never serve that staging cert.
4. Obtain a production Let's Encrypt `shortlived` certificate containing both
   the explicit DNS hostname and public IP. Verify SANs, key match, trust chain
   and remaining lifetime. Preserve bind-mounted inodes; journal both old files
   before replacing either, then signal Envoy's hot restarter.
5. Require the actual served leaf to equal the new certificate with normal CA
   validation for both hostname and IP. Verify every preserved container ID,
   image/configuration fingerprint, start time and mounted configuration hash.
6. Remove the Dashmate package (not purge/autoremove), remove only its stopped
   helper (no `--volumes`), and enable an hourly renewal/check timer.

There is no Dashmate CLI, helper, renderer or Node runtime dependency in the new
TLS manager. Existing data volumes, networks, container names and rendered
configuration paths (including historical `.dashmate` directory names) remain
unchanged. Those files are preserved data, not a requirement to install Dashmate.
This operation does not promise future version upgrades/reprovisioning of the
adopted stack; it captures the existing definitions for explicit recovery.

## Renewal and monitoring

`dashnet-<name>-tls.timer` runs hourly with five-minute jitter. The pinned lego
container has no Docker socket and only the private ACME storage mount. The
host-local worker verifies the served certificate even when no renewal is due;
renewal starts with three days remaining. A lock prevents concurrent writers.
The generated certificate covers both hostname and IP so direct-IP clients keep
working. Normal system trust is mandatory; no insecure production success path.

Inspect `tls-status.json`, the systemd service result, and `seed-tls-doctor`.
Doctor fails on stale/failed renewal, missing timer, leftover Dashmate, TLS/name
failure, preservation drift or unsynced Core. Wire external DNS/TCP/TLS expiry
and actual DAPI/bootstrap probes into monitoring. A timer being active is not
proof that renewal succeeds, and TLS passing is not proof that DAPI works.

Before expanding past a canary, independently test the DAPI masternode-list
stream and a normal `new DAPIClient()` mainnet bootstrap without overrides or
TLS bypass. Observe a renewal/reload using staging or a controlled test; do not
force repeated production issuance merely to test a timer.

## Failure, interruption and rollback

Before retirement, a normal failure restores the original certificate pair and
helper restart policy. The operation record remains `rolled-back`; do not simply
edit/delete the record to retry. Inspect the failed service and retained evidence,
correct the cause, then prepare a reviewed reconciliation/new attempt.

After an interrupted certificate write, the renewal worker restores and reloads
the journalled pair before another issuance. It compares the exact old leaf;
an expired old cert is **not** reported as healthy. A leftover ACME container or
held lock blocks overlapping work rather than assuming the old process stopped.

An interrupted adoption does not auto-resume package removal. Use the exact
original plan and binary for explicit rollback/reconciliation:

```sh
dashnet seed-tls-rollback --plan plan.json --confirm EXACT_PLAN_ID --profile OPS \
  --ssh-key /secure/operator --known-hosts /secure/seed-hosts --timeout 15m
```

Rollback disables renewal, restores the original mounted certificate pair,
reinstalls the captured package if removed, and restores the exact original
helper definition using its local image ID. It refuses changed preserved
workloads, another operation ID, or an externally replaced helper. Backups and
data volumes are retained. It deliberately reports `healthy: false`: restoring
the previous expired certificate reverses the change, not the original outage.
