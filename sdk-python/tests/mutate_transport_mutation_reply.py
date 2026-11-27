"""Detect a transport that accepts ambiguous acknowledgements."""

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sdk = Path(__file__).resolve().parents[1]
status_guard = "if 200 <= response.status_code < 300:"
envelope_guard = 'if payload.get("success") is not True or payload.get("error") is not None:'
with tempfile.TemporaryDirectory(prefix="devkit-envelope-") as tmp:
    candidate = Path(tmp)
    shutil.copytree(sdk / "impreza", candidate / "impreza")
    (candidate / "tests").mkdir()
    test = candidate / "tests" / "verify_transport_mutation_retry.py"
    shutil.copy2(sdk / "tests" / test.name, test)
    for name in ("_http.py", "_http_async.py"):
        path = candidate / "impreza" / name
        source = path.read_text(encoding="utf-8")
        if source.count(status_guard) != 1 or source.count(envelope_guard) != 1:
            raise SystemExit("Expected one status and envelope guard in " + name)
        path.write_text(
            source.replace(status_guard, "if response.status_code < 400:").replace(
                envelope_guard, 'if payload.get("success") is False:'
            ),
            encoding="utf-8",
            newline="\n",
        )
    result = subprocess.run(
        [sys.executable, "-B", "-I", str(test), "TestMutationReply"],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    report = result.stdout + result.stderr
    if (
        result.returncode != 1
        or report.count("FAIL: test_mutation_receipt_requires_explicit_protocol_success") != 8
    ):
        raise SystemExit("MUTATION_ACCEPTS_INDETERMINATE_ENVELOPE survived\n" + report)
    print("KILLED MUTATION_ACCEPTS_INDETERMINATE_ENVELOPE (8 cases)")
