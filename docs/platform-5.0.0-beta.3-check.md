# Platform 5.0.0-beta.3 source compatibility check

## Identity and scope

- Platform release: `407035786`, tag `v5.0.0-beta.3`.
- Immutable Platform commit: `bb78fa2220ae0d58f5a9b5c0f6b0027a292cc3d0`.
- dash-network-go baseline: `24e215c082dcbe31736d428fc9959b7f87543bc0`.
- This is a source/configuration test addition, not a live upgrade or a complete
  runtime compatibility certification. No default/deployed image pins changed.

## Fixture provenance

`internal/node/testdata/dashmate-5.0.0-beta.3.json` is the stored-options output
of this exact release's unmodified `getBaseConfigFactory()()` and real `Config`
class/schema. The wrapper is `{configFormatVersion: "5.0.0", configs: {node: ...}}`.
Source files and their relative import closure came from the immutable commit.
Real Ajv, ajv-formats, lodash and semver dependencies were used; no Config stub,
schema bypass, helper emulation or Platform implementation patch was used.
The source checkout root in build-only paths was normalized to `/platform`, the
helper's layout. Null image defaults are deliberately retained as stored by the
release, rather than replaced by the effective `5-beta` defaults.

To reproduce the options in an isolated checkout with its dependencies:

```js
import getBaseConfigFactory from './packages/dashmate/configs/defaults/getBaseConfigFactory.js';
const options = getBaseConfigFactory()().getStoredOptions();
console.log(JSON.stringify({configFormatVersion: '5.0.0', configs: {node: options}}, null, 2));
```

The added Python tests run the real dash-network-go `Worker.configure` against
these options. They cover initial/final validator, wallet and seed configuration,
release RPC whitelist preservation (including `getspecialtxes`), explicit image
pins replacing nullable defaults, PV14 genesis/anchor/peers, nonvalidator service
selection and fail-closed missing options. They do not call FakeHost's renderer.

```sh
python3 -m unittest discover -s internal/node/testdata -p 'test_*.py'
```

## Findings and remaining gate

The complete beta.2-to-beta.3 Git trees show no Dashmate config, template,
Compose or migration code changes; only Dashmate's package version changed.
The inherited beta.2 whitelist migration and Core `deprecatedrpc=service`
template remain relevant. Existing rollout restrictions still apply: an older
network requiring a Core configuration migration is not silently admitted to a
Core-preserving rollout, and an activated protocol differing from the immutable
bootstrap plan remains a separate upgrade-gate limitation on this baseline.

At the 2026-10-08 check, exact Docker Hub `5.0.0-beta.3` tags for `dashpay/drive`,
`dashpay/rs-dapi` and `dashpay/dashmate-helper` returned `MANIFEST_UNKNOWN`.
The release workflow was queued. This is missing publication evidence, not a
proven product incompatibility. Do not replace these with mutable `5-beta` tags
and claim the exact release was tested.

After publication, resolve the exact artifacts to immutable index and both
Linux architecture digests, then validate actual helper rendering/Compose and
bounded isolated runtime contracts. Retain fail-closed rollout guards. No live
network operation is authorized by this check or its release trigger.
