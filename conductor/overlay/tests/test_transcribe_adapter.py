"""The transcribe.py adapter: resolution, fail-closed, and rc mapping.

The engine's own behavior (isolation, allowlist env, single-flight, deadlines,
zero survivors) is pinned by the fnmendez/transcribe suite; these tests pin
what the ADAPTER promises the bridge: the CLI is found in the right order,
nothing is guessed on failure, and every exit code maps to the right refusal
with a precise reason.
"""

import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import transcribe as t  # noqa: E402


class FakeProc:
    def __init__(self, rc, stdout="", stderr=""):
        self.returncode = rc
        self.stdout = stdout
        self.stderr = stderr


def payload(**over):
    base = {
        "text": "hola mundo", "language": "es", "confidence": 0.88,
        "engine": "whisper.cpp Ultra local (whisper large)",
        "audio_seconds": 8.2, "isolated": True, "network_denied": True,
    }
    base.update(over)
    return json.dumps(base)


class BinaryResolution(unittest.TestCase):
    def test_path_wins(self):
        got = t.resolve_binary(which=lambda name: "/usr/local/bin/transcribe",
                               exists=lambda p: True)
        self.assertEqual(got, Path("/usr/local/bin/transcribe"))

    def test_local_bin_is_second(self):
        installed = Path.home() / ".local" / "bin" / "transcribe"
        got = t.resolve_binary(which=lambda name: None,
                               exists=lambda p: Path(p) == installed)
        self.assertEqual(got, installed)

    def test_dev_worktree_is_last_and_warned(self):
        with self.assertLogs("overlay.transcribe", level="WARNING") as logs:
            got = t.resolve_binary(which=lambda name: None,
                                   exists=lambda p: Path(p) == t._DEV_WORKTREE_BIN)
        self.assertEqual(got, t._DEV_WORKTREE_BIN)
        self.assertIn("make install", "".join(logs.output))

    def test_nothing_found_fails_closed_with_the_fix(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            t.resolve_binary(which=lambda name: None, exists=lambda p: False)
        msg = str(caught.exception)
        self.assertIn("make install", msg)
        self.assertIn("not installed", msg)


class RunAndMap(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(self.enterContext(__import__("tempfile").TemporaryDirectory()))
        self.audio = self.tmp / "note.ogg"
        self.audio.write_bytes(b"x")
        patcher = mock.patch.object(t, "resolve_binary",
                                    lambda **kw: Path("/fake/transcribe"))
        patcher.start()
        self.addCleanup(patcher.stop)

    def enterContext(self, cm):
        result = cm.__enter__()
        self.addCleanup(cm.__exit__, None, None, None)
        return result

    def run_with(self, proc):
        seen = {}

        def runner(cmd, timeout):
            seen["cmd"] = cmd
            seen["timeout"] = timeout
            return proc

        result = t.transcribe_voice(self.audio, runner=runner)
        return result, seen

    def test_success_maps_to_a_transcript_with_provenance(self):
        result, seen = self.run_with(FakeProc(0, payload()))
        self.assertEqual(result.text, "hola mundo")
        self.assertEqual(result.language, "es")
        self.assertAlmostEqual(result.confidence, 0.88)
        self.assertIn("Ultra local", result.engine)
        self.assertIn("confidence: 0.88", result.provenance())
        self.assertEqual(seen["cmd"][0], "/fake/transcribe")
        self.assertIn("--json", seen["cmd"])
        self.assertIn("--", seen["cmd"])
        self.assertNotIn("--allow-fallback", seen["cmd"],
                         "the conductor never opts into the fallback model")

    def test_missing_file_is_rejected_before_running(self):
        ran = []
        with self.assertRaises(t.JobRejected):
            t.transcribe_voice(self.tmp / "absent.ogg",
                               runner=lambda *a, **k: ran.append(a))
        self.assertEqual(ran, [])

    def test_non_isolated_result_is_refused(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            self.run_with(FakeProc(0, payload(isolated=False)))
        self.assertIn("non-isolated", str(caught.exception))

    def test_network_not_denied_is_refused(self):
        with self.assertRaises(t.TranscriptionUnavailable):
            self.run_with(FakeProc(0, payload(network_denied=False)))

    def test_unparseable_json_refuses_instead_of_guessing(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            self.run_with(FakeProc(0, "not json", "boom"))
        self.assertIn("could not be parsed", str(caught.exception))

    def test_rc7_is_single_flight_not_a_failure(self):
        with self.assertRaises(t.JobRejected) as caught:
            self.run_with(FakeProc(7, "", "another job is already running"))
        self.assertIn("already running", str(caught.exception))

    def test_rc3_carries_the_tools_exact_fix(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            self.run_with(FakeProc(3, "", "no Ultra local model; run make fetch-model"))
        self.assertIn("make fetch-model", str(caught.exception))

    def test_rc4_rejects_the_input(self):
        with self.assertRaises(t.JobRejected):
            self.run_with(FakeProc(4, "", "audio too long"))

    def test_rc5_reports_the_deadline(self):
        with self.assertRaises(t.JobRejected) as caught:
            self.run_with(FakeProc(5, "", "deadline exceeded after 92s"))
        self.assertIn("deadline", str(caught.exception))

    def test_rc6_discards_a_survivor_tainted_result(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            self.run_with(FakeProc(6, "", "2 survivors"))
        self.assertIn("survivor", str(caught.exception))

    def test_unknown_rc_is_unavailable_with_the_stderr_tail(self):
        with self.assertRaises(t.TranscriptionUnavailable) as caught:
            self.run_with(FakeProc(1, "", "line1\nreal reason"))
        self.assertIn("real reason", str(caught.exception))

    def test_a_hung_tool_is_reported_not_waited_forever(self):
        def runner(cmd, timeout):
            raise subprocess.TimeoutExpired(cmd, timeout)
        with self.assertRaises(t.JobRejected) as caught:
            t.transcribe_voice(self.audio, runner=runner)
        self.assertIn("did not finish", str(caught.exception))

    def test_dash_prefixed_filename_is_passed_as_data(self):
        weird = self.tmp / "--model.ogg"
        weird.write_bytes(b"x")
        seen = {}

        def runner(cmd, timeout):
            seen["cmd"] = cmd
            return FakeProc(0, payload())

        t.transcribe_voice(weird, runner=runner)
        self.assertGreater(seen["cmd"].index(str(weird)), seen["cmd"].index("--"),
                           "the filename must come after the -- separator")


if __name__ == "__main__":
    unittest.main()
