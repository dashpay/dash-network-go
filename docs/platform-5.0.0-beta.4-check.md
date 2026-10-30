# Platform 5.0.0-beta.4 source compatibility check

## Bound revisions

- Release `408056424`, tag `v5.0.0-beta.4`.
- Platform commit `a35c4ce8c75a0ac4e745766e469649ceba360b09`.
- dash-network-go upstream baseline `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- This work builds on the beta.3 test-only review commit
  `15d53e89238300a84b2e28811e359cd7cf1ef441`; it does not change production code,
  default/deployed image pins or installed binaries.

## Changed release contracts

Beta.4 adds required `platform.quorumList.cacheTtlSeconds` to the Config schema,
base defaults, migration and quorum-list Compose environment. The default is
60 seconds, or 1 for the local preset. Its migration fills a missing value but
preserves an explicit operator value. Devnet remains on the nonlocal default.
The dashnet worker already asks the target helper to migrate before configuring
the node and does not overwrite this release-owned setting.

The Core template also adds `bind=127.0.0.1=onion` beside the network-specific
wildcard bind when Tor is enabled. Tor-disabled output has only the wildcard
bind. This is not a reason to ignore changed Core fingerprints: the baseline's
Platform-only rollout deliberately refuses a changed Core configuration.
Its fingerprint normalization removes verified random salts, not comments or
bind directives. A source/configuration pass therefore does not certify an
existing beta.3 network's Core-preserving upgrade to beta.4.

## Fixture and regression coverage

`internal/node/testdata/dashmate-5.0.0-beta.4.json` contains the exact release's
real `Config.getStoredOptions()` from `getBaseConfigFactory()()`; source imports
were fetched and verified by Git blob identity at the immutable commit above.
The wrapper `{configFormatVersion: "5.0.0", configs: {node: ...}}` is a test
envelope, not a capture of the helper CLI's persisted file. Build-root paths
are normalized to `/platform`. Null version-derived image defaults are kept.

The existing beta.3 pin, role, RPC-whitelist and fail-closed tests are reused
against this new fixture. Added assertions preserve required TTL60 and disabled
quorum-list selection across all six initial/final role cases, as well as an
operator's TTL5 and disabled-Tor choices. Tests call the real Worker.configure,
not FakeHost's renderer. The full node-worker suite passes 51 tests:

```sh
python3 -m unittest discover -s internal/node/testdata -p 'test_*.py'
```

The separate exact-source harness validates the old fixture's rejection before
migration and acceptance afterward, local/nonlocal/operator TTL handling, and
the original Core template's Tor bind semantics. It uses unmodified upstream
Config/Ajv/schema/migration/template code, not stubs or relaxed validation.
Schema coercion follows upstream's existing Ajv settings; it is not disabled
or treated as a product defect by these checks.

## Incomplete release gate

At the 2026-10-09 check, exact `5.0.0-beta.4` tags for `dashpay/drive`,
`dashpay/rs-dapi` and `dashpay/dashmate-helper` returned `MANIFEST_UNKNOWN`.
Release Platform workflow `37952820355` was queued. This is missing artifact
publication, not proof of product incompatibility. No mutable tag substitution
or live upgrade was attempted.

After publication, resolve exact image digests for both Linux architectures and
validate the actual helper/Compose and isolated runtime contracts. Preserve the
Core-change guard; a reviewed Core configuration migration path is needed for
existing networks whose rendered Core config changes. Separate unmerged
network-specific operational work is not incorporated by this test-only check.
Do not infer complete compatibility or live-upgrade authority from this document.
