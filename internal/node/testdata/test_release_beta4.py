"""Exact beta.4 stored-default contract; no image/runtime compatibility claim."""
import json
from pathlib import Path
import tempfile
from unittest.mock import patch

import test_release_beta3 as beta3
from test_worker import host, SPORK

FIXTURE = Path(__file__).with_name("dashmate-5.0.0-beta.4.json")


class Beta4ConfigurationTests(beta3.Beta3ConfigurationTests):
    # Reuse the role, immutable-pin, RPC-policy and fail-closed assertions with
    # the new release's actual stored options, including its new required key.
    def setUp(self):
        fixture = patch.object(beta3, "FIXTURE", FIXTURE)
        fixture.start()
        self.addCleanup(fixture.stop)

    def test_required_quorum_cache_default_survives_all_roles(self):
        for role in ("validator", "wallet", "seed"):
            for final in (False, True):
                with self.subTest(role=role, final=final):
                    config, _ = beta3.configured(role, final)
                    d = next(iter(config["configs"].values()))
                    self.assertEqual(d["network"], "devnet")
                    self.assertEqual(d["platform"]["quorumList"]["cacheTtlSeconds"], 60)
                    # Do not accidentally enable an unsupported sidecar while
                    # preserving its required release-owned configuration.
                    self.assertFalse(d["platform"]["quorumList"]["enabled"])

    def test_operator_quorum_cache_and_tor_choices_are_preserved(self):
        for role in ("validator", "wallet", "seed"):
            with self.subTest(role=role), tempfile.TemporaryDirectory(prefix="dashnet-beta4-") as tmp:
                w = host(tmp, role)
                config = json.loads(FIXTURE.read_text())
                d = config["configs"].pop("node")
                config["configs"][w.config_name] = d
                d["platform"]["quorumList"]["cacheTtlSeconds"] = 5
                d["core"]["tor"]["enabled"] = False
                w.configure(config, SPORK)
                self.assertEqual(d["platform"]["quorumList"]["cacheTtlSeconds"], 5)
                self.assertFalse(d["core"]["tor"]["enabled"])
