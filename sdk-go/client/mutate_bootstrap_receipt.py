"""Detect accepting an unconfirmed bootstrap with incomplete credentials."""
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
source = sdk / "client" / "agent.go"
guard = "resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || "
with tempfile.TemporaryDirectory(prefix="devkit-ack-") as tmp:
    folder = Path(tmp)
    mutant = folder / source.name
    text = source.read_text(encoding="utf-8")
    if text.count(guard) != 1:
        raise SystemExit("Expected the bootstrap transport status guard")
    first = text.index("\tif strings.TrimSpace(out.AgentID)")
    last = text.index("\t// The token was sent to baseURL.", first)
    mutant.write_text((text[:first] + text[last:]).replace(guard, "").replace('\t"strings"\n', ""),
                      encoding="utf-8", newline="\n")
    overlay = folder / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(mutant)}}), encoding="utf-8")
    result = subprocess.run(
        [args.go, "test", "-count=1", "-overlay", str(overlay), "./client", "-run", "^TestBootstrapRequiresConfirmedCredentials$"],
        cwd=sdk, env={**os.environ, "GOWORK": "off"}, capture_output=True, text=True, timeout=90, check=False,
    )
    print(result.stdout, end="")
    if result.returncode != 1 or any(
        f"FAIL: TestBootstrapRequiresConfirmedCredentials/{path}" not in result.stdout
        for path in ("rejected_status", "missing_id", "missing_secret", "empty_receipt")
    ):
        raise SystemExit("BOOTSTRAP_ACCEPTS_UNCONFIRMED_CREDENTIALS survived")
    print("KILLED BOOTSTRAP_ACCEPTS_UNCONFIRMED_CREDENTIALS")
