"""Detect accepting an acknowledgement whose HTTP response is a failure."""
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
source = sdk / "client" / "client.go"
guard = "resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || "
with tempfile.TemporaryDirectory(prefix="devkit-ack-") as tmp:
    folder = Path(tmp)
    mutant = folder / source.name
    text = source.read_text(encoding="utf-8")
    if text.count(guard) != 2:
        raise SystemExit("Expected the guarded JSON and raw response paths")
    mutant.write_text(text.replace(guard, ""), encoding="utf-8", newline="\n")
    overlay = folder / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(mutant)}}), encoding="utf-8")
    result = subprocess.run(
        [args.go, "test", "-count=1", "-overlay", str(overlay), "./client", "-run", "^TestMutationAcknowledgementRequiresTransportSuccess$"],
        cwd=sdk, env={**os.environ, "GOWORK": "off"}, capture_output=True, text=True, timeout=90, check=False,
    )
    print(result.stdout, end="")
    if result.returncode != 1 or any(
        f"FAIL: TestMutationAcknowledgementRequiresTransportSuccess/403/{path}" not in result.stdout
        for path in ("agent_result", "artifact_receipt")
    ):
        raise SystemExit("MUTATION_HTTP_FAILURE_ACKNOWLEDGED survived")
    print("KILLED MUTATION_HTTP_FAILURE_ACKNOWLEDGED")
