"""Isolation guarantees for transcription jobs.

These are negative tests on purpose: each one describes a way a future edit
could start disturbing Franco's machine — driving the GUI app, touching the
pasteboard, simulating keys, reading his history, or leaving a second engine
running — and fails if that becomes possible.

The process tests spawn real processes (`sleep`) rather than mocks, because the
property under test is that a *descendant* cannot outlive its job.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import transcribe as t  # noqa: E402


class ForbiddenEntryPoints(unittest.TestCase):
    """Nothing may reach the GUI app, the pasteboard, or an input device."""

    def test_gui_launchers_are_refused(self):
        for cmd in (
            ["open", "-a", "superwhisper", "/tmp/a.mp3"],
            ["/usr/bin/open", "/tmp/a.mp3", "-a", "superwhisper"],
            ["osascript", "-e", 'tell application "superwhisper" to activate'],
        ):
            with self.assertRaises(t.JobRejected, msg=cmd):
                t.assert_safe_command(cmd)

    def test_url_scheme_is_refused(self):
        for arg in ("superwhisper://record", "superwhisper-debug://x"):
            with self.assertRaises(t.JobRejected):
                t.assert_safe_command(["/usr/local/bin/superwhisper", arg])

    def test_app_bundle_binary_is_refused(self):
        with self.assertRaises(t.JobRejected):
            t.assert_safe_command(["/Applications/superwhisper.app/Contents/MacOS/superwhisper", "x.mp3"])

    def test_clipboard_and_paste_flags_are_refused(self):
        for arg in ("--paste", "--auto-paste", "--clipboard", "--copy", "--keystroke", "--activate"):
            with self.assertRaises(t.JobRejected, msg=arg):
                t.assert_safe_command(["/usr/local/bin/superwhisper", "transcribe", "a.mp3", arg])

    def test_clipboard_binaries_are_refused(self):
        for binary in ("pbcopy", "pbpaste"):
            with self.assertRaises(t.JobRejected):
                t.assert_safe_command([binary])

    def test_microphone_capture_flags_are_refused(self):
        for arg in ("--record", "--mic", "--microphone"):
            with self.assertRaises(t.JobRejected):
                t.assert_safe_command(["/usr/local/bin/superwhisper", arg])

    def test_a_plain_cli_transcribe_is_allowed(self):
        t.assert_safe_command(["/usr/local/bin/superwhisper", "transcribe", "/tmp/job/a.ogg", "--model", "Ultra"])


class PrivateState(unittest.TestCase):
    """A job may never read or append to the live transcription history."""

    def test_job_env_points_superwhisper_state_into_the_workspace(self):
        with tempfile.TemporaryDirectory() as tmp:
            workspace = Path(tmp)
            env = t.job_environment(workspace, base_env={
                "SUPERWHISPER_DB": "/Users/someone/Library/.../superwhisper.sqlite",
                "SUPERWHISPER_SETTINGS": "/Users/someone/settings.json",
                "PATH": "/usr/bin",
            })
            self.assertTrue(env["SUPERWHISPER_DB"].startswith(str(workspace)))
            self.assertTrue(env["SUPERWHISPER_SETTINGS"].startswith(str(workspace)))
            self.assertEqual(env["PATH"], "/usr/bin")

    def test_inherited_superwhisper_pointers_are_dropped(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = t.job_environment(Path(tmp), base_env={"SUPERWHISPER_HISTORY_DIR": "/live"})
            self.assertNotIn("SUPERWHISPER_HISTORY_DIR", env)

    def test_workspace_is_private_and_removed(self):
        with tempfile.TemporaryDirectory() as root:
            with t.JobWorkspace(root) as workspace:
                created = workspace
                self.assertEqual(oct(os.stat(workspace).st_mode)[-3:], "700")
                self.assertTrue((workspace / "state").is_dir())
            self.assertFalse(created.exists(), "workspace must not outlive the job")


class SingleFlight(unittest.TestCase):
    def test_second_concurrent_job_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            lock = Path(tmp) / "stt.lock"
            with t.single_flight(lock):
                with self.assertRaises(t.JobRejected):
                    with t.single_flight(lock):
                        self.fail("two jobs admitted at once")
            with t.single_flight(lock):      # released, so the next one is admitted
                pass

    def test_stale_lock_from_a_dead_holder_is_reclaimed(self):
        with tempfile.TemporaryDirectory() as tmp:
            lock = Path(tmp) / "stt.lock"
            dead = subprocess.Popen([sys.executable, "-c", "pass"])
            dead.wait()
            lock.write_text("%d\n" % dead.pid)
            with t.single_flight(lock):
                pass
            self.assertFalse(lock.exists())


class ProcessTeardown(unittest.TestCase):
    """No job may leave a process — or a descendant — behind."""

    SPAWNER = (
        "import subprocess,sys,time;"
        "subprocess.Popen(['sleep','120']);"
        "sys.stdout.write('spawned');sys.stdout.flush();"
        "time.sleep(120)"
    )

    def test_deadline_kills_the_whole_group_including_descendants(self):
        with tempfile.TemporaryDirectory() as tmp:
            with t.JobWorkspace(tmp) as workspace:
                started = time.monotonic()
                result = t.run_guarded(
                    [sys.executable, "-c", self.SPAWNER],
                    t.job_environment(workspace), deadline=2.0,
                )
        self.assertTrue(result.timed_out)
        self.assertLess(time.monotonic() - started, 30, "deadline must be hard")
        self.assertFalse(result.survivors, "a descendant outlived the job")

    def test_zero_survivors_after_a_normal_exit(self):
        with tempfile.TemporaryDirectory() as tmp:
            with t.JobWorkspace(tmp) as workspace:
                result = t.run_guarded(
                    [sys.executable, "-c", "print('done')"],
                    t.job_environment(workspace), deadline=30,
                )
        self.assertEqual(result.returncode, 0)
        self.assertFalse(result.survivors)

    def test_cancellation_tears_the_group_down_before_propagating(self):
        seen = {}

        class Cancelled(BaseException):
            pass

        real_popen = subprocess.Popen

        def popen(cmd, **kwargs):
            proc = real_popen(cmd, **kwargs)
            seen["pgid"] = os.getpgid(proc.pid)
            original = proc.communicate

            def boom(*a, **k):
                raise Cancelled()

            proc.communicate = boom
            proc._original_communicate = original
            return proc

        with tempfile.TemporaryDirectory() as tmp:
            with t.JobWorkspace(tmp) as workspace:
                with self.assertRaises(Cancelled):
                    t.run_guarded([sys.executable, "-c", self.SPAWNER],
                                  t.job_environment(workspace), deadline=30, popen=popen)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and not t.group_is_gone(seen["pgid"]):
            time.sleep(0.1)
        self.assertTrue(t.group_is_gone(seen["pgid"]), "cancellation left the group alive")

    def test_a_command_that_is_not_safe_never_starts(self):
        started = []
        with tempfile.TemporaryDirectory() as tmp:
            with t.JobWorkspace(tmp) as workspace:
                with self.assertRaises(t.JobRejected):
                    t.run_guarded(["open", "-a", "superwhisper"], t.job_environment(workspace),
                                  popen=lambda *a, **k: started.append(a))
        self.assertEqual(started, [], "a forbidden command must be refused before spawning")


class FailClosed(unittest.TestCase):
    """Without a permitted engine, nothing is substituted and nothing is faked."""

    def test_capability_is_unavailable_for_a_history_only_cli(self):
        cap = t.detect_capability(
            cli=sys.executable,
            runner=lambda cmd: subprocess.CompletedProcess(cmd, 0, "Commands:\n  search\n  history\n", ""),
        )
        self.assertFalse(cap.available)
        self.assertIn("no transcribe command", cap.reason)

    def test_capability_is_detected_when_a_transcribe_verb_appears(self):
        cap = t.detect_capability(
            cli=sys.executable,
            runner=lambda cmd: subprocess.CompletedProcess(cmd, 0, "Commands:\n  transcribe  Transcribe a file\n", ""),
        )
        self.assertTrue(cap.available)
        self.assertEqual(cap.verb, "transcribe")

    def test_missing_cli_is_unavailable_not_a_crash(self):
        cap = t.detect_capability(cli="/nonexistent/superwhisper")
        self.assertFalse(cap.available)
        self.assertIn("not installed", cap.reason)

    def test_transcribe_refuses_instead_of_falling_back(self):
        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "a.ogg"
            audio.write_bytes(b"x")
            ran = []
            with self.assertRaises(t.TranscriptionUnavailable) as caught:
                t.transcribe_file(
                    audio, workspace_root=tmp, lock_path=Path(tmp) / "l",
                    capability=t.Capability(False, "no transcribe command"),
                    runner=lambda *a, **k: ran.append(a),
                )
            self.assertIn("no transcribe command", str(caught.exception))
            self.assertEqual(ran, [], "nothing may run when no permitted engine exists")

    def test_surviving_processes_invalidate_the_result(self):
        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "a.ogg"
            audio.write_bytes(b"x")
            with self.assertRaises(t.TranscriptionUnavailable) as caught:
                t.transcribe_file(
                    audio, workspace_root=tmp, lock_path=Path(tmp) / "l",
                    capability=t.Capability(True, "ok", verb="transcribe"),
                    runner=lambda *a, **k: t.GuardedResult(0, "text", "", False, survivors=True),
                )
            self.assertIn("left processes behind", str(caught.exception))

    def test_timeout_is_reported_not_silently_empty(self):
        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "a.ogg"
            audio.write_bytes(b"x")
            with self.assertRaises(t.TranscriptionUnavailable) as caught:
                t.transcribe_file(
                    audio, workspace_root=tmp, lock_path=Path(tmp) / "l",
                    capability=t.Capability(True, "ok", verb="transcribe"),
                    runner=lambda *a, **k: t.GuardedResult(None, "", "", True, survivors=False),
                )
            self.assertIn("deadline", str(caught.exception))

    def test_the_job_copies_audio_into_the_workspace_and_uses_ultra(self):
        seen = {}

        def runner(cmd, env, deadline=None, cwd=None):
            seen["cmd"] = cmd
            seen["cwd"] = cwd
            seen["copied"] = Path(cmd[2]).is_file()
            return t.GuardedResult(0, "  hola  ", "", False, False)

        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "note.ogg"
            audio.write_bytes(b"audio-bytes")
            text = t.transcribe_file(
                audio, workspace_root=tmp, lock_path=Path(tmp) / "l",
                capability=t.Capability(True, "ok", verb="transcribe", cli="/usr/local/bin/superwhisper"),
                runner=runner,
            )
        self.assertEqual(text, "hola")
        self.assertIn("--model", seen["cmd"])
        self.assertEqual(seen["cmd"][seen["cmd"].index("--model") + 1], "Ultra")
        self.assertTrue(seen["copied"], "the engine must read the workspace copy, not the inbox file")


if __name__ == "__main__":
    unittest.main()
