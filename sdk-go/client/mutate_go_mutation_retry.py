"""Detect accepting replaying a mutation whose result is uncertain."""
from pathlib import Path
import argparse
import json
import os
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--go", required=True)
args = parser.parse_args()
sdk = Path(__file__).resolve().parents[1]
source = sdk / "client" / "retry.go"
guard = "(!readOnly && req.Context().Value(allowIdempotentReplay{}) != true)"
with tempfile.TemporaryDirectory(prefix="devkit-ack-") as tmp:
    folder = Path(tmp)
    mutant = folder / source.name
    text = source.read_text(encoding="utf-8")
    if text.count(guard) != 1:
        raise SystemExit("Expected one mutation replay guard")
    mutant.write_text(text.replace(guard, "(!readOnly && false)"), encoding="utf-8", newline="\n")
    overlay = folder / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(mutant)}}), encoding="utf-8")
    result = subprocess.run(
        [args.go, "test", "-count=1", "-overlay", str(overlay), "./client", "-run", "^TestTransportDoesNotReplayUnconfirmedMutations$"],
        cwd=sdk, env={**os.environ, "GOWORK": "off"}, capture_output=True, text=True, timeout=90, check=False,
    )
    print(result.stdout, end="")
    if result.returncode != 1 or any(
        f"FAIL: TestTransportDoesNotReplayUnconfirmedMutations/{path}/timeout" not in result.stdout
        for path in ("POST", "PUT", "DELETE")
    ):
        raise SystemExit("GO_MUTATION_REPLAYED_AFTER_UNCERTAIN_RESPONSE survived")
    print("KILLED GO_MUTATION_REPLAYED_AFTER_UNCERTAIN_RESPONSE")
