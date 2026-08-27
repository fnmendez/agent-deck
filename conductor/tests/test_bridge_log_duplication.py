"""Regression test: bridge.log must not receive every record twice.

Both installed daemon units redirect the bridge's stdout into bridge.log
(launchd ``StandardOutPath``, systemd ``StandardOutput=append:``). The bridge
also attached its own ``FileHandler`` for that same path, so every line landed
in bridge.log twice with an identical timestamp. These tests drive the real
module in a subprocess under each stdout layout and count the marker.

Run: python -m unittest conductor.tests.test_bridge_log_duplication  (or pytest)
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

CANONICAL = (
    Path(__file__).resolve().parents[2] / "internal" / "session" / "conductor_bridge.py"
)

# Import the bridge under a private name, emit one record, flush every handler.
DRIVER = """
import importlib.util, logging, sys
spec = importlib.util.spec_from_file_location("bridge_under_test", sys.argv[1])
module = importlib.util.module_from_spec(spec)
sys.modules["bridge_under_test"] = module
spec.loader.exec_module(module)
module.log.info(sys.argv[2])
logging.shutdown()
sys.stdout.flush()
"""

MARKER = "log-duplication-marker"


def _run(conductor_dir: Path, stdout_path: Path) -> None:
    """Import the bridge with stdout redirected to `stdout_path`, like a daemon."""
    env = dict(os.environ)
    env["AGENT_DECK_CONDUCTOR_DIR"] = str(conductor_dir)
    with open(str(stdout_path), "a", encoding="utf-8") as out:
        proc = subprocess.run(
            [sys.executable, "-c", DRIVER, str(CANONICAL), MARKER],
            stdout=out,
            stderr=subprocess.PIPE,
            env=env,
        )
    if proc.returncode != 0:
        raise AssertionError(
            "bridge import failed: " + proc.stderr.decode("utf-8", "replace")
        )


def _count(path: Path) -> int:
    if not path.exists():
        return 0
    return path.read_text(encoding="utf-8", errors="replace").count(MARKER)


class BridgeLogDuplicationTest(unittest.TestCase):
    def test_stdout_redirected_to_bridge_log_logs_once(self):
        """The daemon layout: stdout IS bridge.log. Exactly one copy."""
        with tempfile.TemporaryDirectory() as tmp:
            conductor_dir = Path(tmp)
            log_path = conductor_dir / "bridge.log"
            log_path.touch()
            _run(conductor_dir, log_path)
            self.assertEqual(
                _count(log_path),
                1,
                "bridge.log must contain the record exactly once when stdout is "
                "already redirected to it (daemon layout)",
            )

    def test_separate_stdout_keeps_both_sinks(self):
        """Manual/terminal layout: stdout elsewhere. File and stdout both get it."""
        with tempfile.TemporaryDirectory() as tmp:
            conductor_dir = Path(tmp)
            log_path = conductor_dir / "bridge.log"
            other = conductor_dir / "stdout.txt"
            _run(conductor_dir, other)
            self.assertEqual(_count(log_path), 1, "bridge.log lost its file handler")
            self.assertEqual(_count(other), 1, "stdout lost its stream handler")

    def test_missing_conductor_dir_still_logs_to_stdout(self):
        """No data dir (CI/tests): import must not fail and stdout still logs."""
        with tempfile.TemporaryDirectory() as tmp:
            missing = Path(tmp) / "absent"
            out = Path(tmp) / "stdout.txt"
            _run(missing, out)
            self.assertEqual(_count(out), 1)
            self.assertFalse((missing / "bridge.log").exists())


if __name__ == "__main__":
    unittest.main()
