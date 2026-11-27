"""Detect a transport that silently replays mutations."""

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sdk = Path(__file__).resolve().parents[1]
guard = 'max_retries = self._max_retries if method.upper() in {"GET", "HEAD", "OPTIONS"} else 0'
with tempfile.TemporaryDirectory(prefix="devkit-retry-") as tmp:
    candidate = Path(tmp)
    shutil.copytree(sdk / "impreza", candidate / "impreza")
    (candidate / "tests").mkdir()
    test = candidate / "tests" / "verify_transport_mutation_retry.py"
    shutil.copy2(sdk / "tests" / test.name, test)
    for name in ("_http.py", "_http_async.py"):
        path = candidate / "impreza" / name
        source = path.read_text(encoding="utf-8")
        if source.count(guard) != 1:
            raise SystemExit("Expected one retry guard in " + name)
        path.write_text(
            source.replace(guard, "max_retries = self._max_retries"), encoding="utf-8", newline="\n"
        )
    result = subprocess.run(
        [sys.executable, "-B", "-I", str(test), "TestMutationRetry"],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    report = result.stdout + result.stderr
    if (
        result.returncode != 1
        or report.count("FAIL: test_mutations_do_not_replay_ambiguous_outcomes") != 12
    ):
        raise SystemExit("MUTATION_REPLAYED_AFTER_UNCERTAIN_RESPONSE survived\n" + report)
    print("KILLED MUTATION_REPLAYED_AFTER_UNCERTAIN_RESPONSE (12 cases)")
