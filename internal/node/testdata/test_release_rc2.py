"""Exact RC.2 stored-default contract; no live Core activation claim."""
import json
from pathlib import Path
import tempfile
from unittest.mock import patch

import test_release_beta3 as beta3
import test_release_beta4 as beta4
from test_worker import host, SPORK

FIXTURE = Path(__file__).with_name("dashmate-5.0.0-rc.2.json")


class RC2ConfigurationTests(beta4.Beta4ConfigurationTests):
    def setUp(self):
        # Reuse role, image-pin, RPC-policy, TTL and fail-closed assertions
        # against this release's actual defaults rather than a synthetic host.
        for module in (beta3, beta4):
            fixture = patch.object(module, "FIXTURE", FIXTURE)
            fixture.start()
            self.addCleanup(fixture.stop)

    def test_core_deployment_permission_survives_all_roles(self):
        for role in ("validator", "wallet", "seed"):
            for final in (False, True):
                with self.subTest(role=role, final=final):
                    config, _ = beta3.configured(role, final)
                    users = next(iter(config["configs"].values()))["core"]["rpc"]["users"]
                    self.assertEqual(users["drive_consensus"]["whitelist"].count("getdeploymentinfo"), 1)
                    for name, user in users.items():
                        if name != "drive_consensus" and user["whitelist"] is not None:
                            self.assertNotIn("getdeploymentinfo", user["whitelist"])

    def test_operator_rpc_policy_is_not_replaced(self):
        for whitelist in (["getdeploymentinfo", "getblockhash", "custom_method"], None):
            with self.subTest(whitelist=whitelist), tempfile.TemporaryDirectory(prefix="dashnet-rc2-") as tmp:
                w = host(tmp)
                config = json.loads(FIXTURE.read_text())
                options = config["configs"].pop("node")
                config["configs"][w.config_name] = options
                user = options["core"]["rpc"]["users"]["drive_consensus"]
                user["whitelist"] = json.loads(json.dumps(whitelist))
                w.configure(config, SPORK)
                self.assertEqual(user["whitelist"], whitelist)
