"""Acceptance-only fault injection, never a normal rollout path."""
import json
import os
import pathlib
import signal
import subprocess
import sys
import time

root = pathlib.Path(sys.argv[1])
log = root / "operator.log"
with log.open("w") as output:
    child = subprocess.Popen(
        [str(root / "dashnet"), "upgrade", *sys.argv[2:]],
        stdout=output, stderr=subprocess.STDOUT, start_new_session=True,
    )
    interrupted = False
    while child.poll() is None:
        if not interrupted and log.read_text().count("verified; whole fleet healthy") >= 2:
            os.killpg(child.pid, signal.SIGINT)
            interrupted = True
        time.sleep(1)
(root / "interruption.json").write_text(json.dumps({
    "interrupted": interrupted, "exitCode": child.returncode,
    "verifiedTargets": log.read_text().count("verified; whole fleet healthy"),
}))
if not interrupted or not child.returncode:
    raise SystemExit("Expected checkpointed interruption was not observed; inspect private evidence")
