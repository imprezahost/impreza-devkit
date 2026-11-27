"""Detect approving a destructive action despite incomplete confirmation IO."""
from pathlib import Path
import argparse
import os
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--go", required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parent
with tempfile.TemporaryDirectory(prefix="devkit-confirm-") as tmp:
    scratch = Path(tmp)
    source = (root / "confirm.go").read_text(encoding="utf-8")
    prompt = '''	if _, err := fmt.Fprintf(out, "%s [y/N] ", prompt); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}'''
    read = 'if err != nil && (!errors.Is(err, io.EOF) || line == "") {'
    if source.count(prompt) != 1 or source.count(read) != 1:
        raise SystemExit("Expected one read and one prompt guard")
    source = source.replace(prompt, '	fmt.Fprintf(out, "%s [y/N] ", prompt)').replace(
        read, 'if err != nil && line == "" {'
    ).replace('\t"errors"\n', '')
    (scratch / "confirm.go").write_text(source, encoding="utf-8", newline="\n")
    shutil.copy2(root / "confirm_io_test.go", scratch)
    result = subprocess.run(
        [args.go, "test", "confirm.go", "confirm_io_test.go"], cwd=scratch,
        env={**os.environ, "GOWORK": "off"}, capture_output=True, text=True, timeout=60, check=False,
    )
    if result.returncode != 1 or any(
        label + ": expected" not in result.stdout for label in ("partial_read", "prompt_write")
    ):
        raise SystemExit("CLI_CONFIRM_IO_FAILURE_APPROVES survived\n" + result.stdout + result.stderr)
    print("KILLED CLI_CONFIRM_IO_FAILURE_APPROVES (partial_read, prompt_write)")
