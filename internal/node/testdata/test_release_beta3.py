"""Configuration contract for the exact Platform 5.0.0-beta.3 source release.

This tests the actual worker against upstream stored defaults, not the older
FakeHost render. It does not claim image availability or a running network.
"""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from test_worker import host, platform_request, SPORK, worker

FIXTURE = Path(__file__).with_name("dashmate-5.0.0-beta.3.json")


def configured(role, final=False):
    """Return a synthetic configured node for upstream schema validation too."""
    with tempfile.TemporaryDirectory(prefix="dashnet-beta3-") as tmp:
        w = host(tmp, role)
        config = json.loads(FIXTURE.read_text())
        config["configs"] = {w.config_name: config["configs"]["node"]}
        if role == "validator" and final:
            platform_request(w)
            w.atomic("platform/inputs.json", dict(
                genesisCoreHeight=4500,
                peers=[dict(name=v["name"], address=v["address"], nodeId=v["nodeId"])
                       for v in w.q["peers"]]))
        w.configure(config, SPORK if final else None)
        return config, copy.deepcopy(w.images)


class Beta3ConfigurationTests(unittest.TestCase):
    def test_release_rpc_permissions_survive_each_role(self):
        original = json.loads(FIXTURE.read_text())["configs"]["node"]
        users = original["core"]["rpc"]["users"]
        self.assertIn("getspecialtxes", users["drive_consensus"]["whitelist"])
        for role in ("validator", "wallet", "seed"):
            for final in (False, True):
                with self.subTest(role=role, final=final):
                    config, _ = configured(role, final)
                    actual = next(iter(config["configs"].values()))["core"]["rpc"]["users"]
                    for user, policy in users.items():
                        self.assertEqual(actual[user]["whitelist"], policy["whitelist"])
                        self.assertEqual(actual[user]["lowPriority"], policy["lowPriority"])
                    self.assertIsNone(actual["dashnet"]["whitelist"])

    def test_nullable_release_images_become_explicit_pins(self):
        original = json.loads(FIXTURE.read_text())["configs"]["node"]
        self.assertIsNone(original["platform"]["drive"]["abci"]["docker"]["image"])
        self.assertIsNone(original["platform"]["dapi"]["rsDapi"]["docker"]["image"])
        config, images = configured("validator", True)
        d = next(iter(config["configs"].values()))
        self.assertEqual(d["platform"]["drive"]["abci"]["docker"]["image"], images["drive"])
        self.assertEqual(d["platform"]["dapi"]["rsDapi"]["docker"]["image"], images["dapi"])
        td = d["platform"]["drive"]["tenderdash"]
        self.assertEqual(td["genesis"]["consensus_params"]["version"]["app_version"], "14")
        self.assertEqual(td["genesis"]["initial_core_chain_locked_height"], 4500)
        self.assertEqual(len(td["p2p"]["persistentPeers"]), 3)
        self.assertEqual(d["platform"]["drive"]["abci"]["epochTime"], 3600)

    def test_nonvalidators_keep_platform_disabled(self):
        for role in ("wallet", "seed"):
            config, _ = configured(role, True)
            d = next(iter(config["configs"].values()))
            self.assertFalse(d["platform"]["enable"])
            self.assertFalse(d["core"]["masternode"]["enable"])
            self.assertEqual(d["core"]["spork"]["privateKey"], "cSporkKey" if role == "wallet" else None)

    def test_missing_release_option_still_fails_closed(self):
        with tempfile.TemporaryDirectory(prefix="dashnet-beta3-") as tmp:
            w = host(tmp)
            config = json.loads(FIXTURE.read_text())
            config["configs"] = {w.config_name: config["configs"]["node"]}
            del config["configs"][w.config_name]["platform"]["drive"]["abci"]["epochTime"]
            with self.assertRaisesRegex(worker.Failure, "dashmate-option-missing"):
                w.configure(config, None)
