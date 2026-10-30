"""RC.3 stock-image migration must not leave dashnet on mutable family tags.

RC.3 stored defaults equal the RC.2 fixture. Exact upstream migration tests
are recorded in the version-bound compatibility report; helper execution here
is simulated to test the worker's migration/configuration/render ordering.
"""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_worker import host

FIXTURE = Path(__file__).with_name("dashmate-5.0.0-rc.2.json")


class RC3ImageSelectionTests(unittest.TestCase):
    def config(self, w):
        config = json.loads(FIXTURE.read_text())
        config["configs"] = {w.config_name: config["configs"]["node"]}
        return config

    def assert_pins(self, w, config):
        options = config["configs"][w.config_name]
        self.assertEqual(options["platform"]["drive"]["abci"]["docker"]["image"], w.images["drive"])
        self.assertEqual(options["platform"]["dapi"]["rsDapi"]["docker"]["image"], w.images["dapi"])
        self.assertIn("@sha256:", w.images["drive"])
        self.assertIn("@sha256:", w.images["dapi"])

    def test_explicit_pins_replace_inherited_stock_choices(self):
        for role in ("validator", "wallet", "seed"):
            for tag in (None, "4", "4-dev", "4-beta", "5-beta", "5-rc"):
                with self.subTest(role=role, tag=tag), tempfile.TemporaryDirectory(prefix="dashnet-rc3-") as tmp:
                    w = host(tmp, role)
                    config = self.config(w)
                    options = config["configs"][w.config_name]
                    options["platform"]["drive"]["abci"]["docker"]["image"] = "dashpay/drive:" + tag if tag else None
                    options["platform"]["dapi"]["rsDapi"]["docker"]["image"] = "dashpay/rs-dapi:" + tag if tag else None
                    w.configure(config, None)
                    if role == "validator":
                        self.assert_pins(w, config)
                    else:
                        self.assertFalse(options["platform"]["enable"])
                        self.assertEqual(options["platform"]["drive"]["abci"]["docker"]["image"], "dashpay/drive:" + tag if tag else None)
                        self.assertEqual(options["platform"]["dapi"]["rsDapi"]["docker"]["image"], "dashpay/rs-dapi:" + tag if tag else None)

    def test_migration_unsets_before_worker_repins_and_renders(self):
        with tempfile.TemporaryDirectory(prefix="dashnet-rc3-order-") as tmp:
            w = host(tmp)
            w.version = "5.0.0-rc.3"
            w.home.mkdir(parents=True, exist_ok=True)
            (w.home / "config.json").write_text(json.dumps(self.config(w)))
            original_helper = w.helper
            calls = []

            def helper(stage, mode):
                calls.append(mode)
                path = stage / "config.json"
                if mode == "migrate":
                    config = json.loads(path.read_text())
                    options = config["configs"][w.config_name]
                    options["platform"]["drive"]["abci"]["docker"]["image"] = None
                    options["platform"]["dapi"]["rsDapi"]["docker"]["image"] = None
                    path.write_text(json.dumps(config))
                else:
                    self.assertEqual(mode, "render")
                    self.assert_pins(w, json.loads(path.read_text()))
                    original_helper(stage, mode)

            with patch.object(w, "helper", side_effect=helper):
                result = w.render()
            self.assertEqual(calls, ["migrate", "render"])
            self.assert_pins(w, result.config)
            self.assertEqual(w.containers, {}, "configuration rendering must not start services")
