"""Isolated, fail-closed transcription jobs.

Franco uses Superwhisper interactively all day. A bridge job must therefore be
invisible to that session: it may not touch the GUI app, the hotkey, the
microphone, the clipboard, the pasteboard, Accessibility, the foreground app, or
his transcription history. Anything that cannot be done under those rules is not
done at all — this module fails closed rather than reaching for a path that
would disturb him.

Isolation is enforced in four places:

* ``assert_safe_command`` refuses GUI/automation entry points outright, so a
  future edit cannot quietly reintroduce ``open -a superwhisper`` or an
  ``osascript`` keystroke.
* every job runs with ``SUPERWHISPER_DB`` / ``SUPERWHISPER_SETTINGS`` pointed
  inside a private workspace, so a job can never read or append to his history.
* ``single_flight`` admits one job at a time, so two voice notes can never put
  two engines on the machine at once.
* ``run_guarded`` gives each job its own process group and a hard deadline, then
  kills the whole group and *verifies* nothing survived.
"""

from __future__ import annotations

import errno
import os
import re
import shutil
import signal
import subprocess
import tempfile
import time
from pathlib import Path

SUPERWHISPER_CLI = "/usr/local/bin/superwhisper"
DEFAULT_MODEL = "Ultra"
DEFAULT_DEADLINE = 180.0
GRACE_SECONDS = 3.0

# Entry points that would run through Franco's live app or his input devices.
FORBIDDEN_BINARIES = {"open", "osascript", "automator", "shortcuts", "cliclick", "pbcopy", "pbpaste"}
FORBIDDEN_ARG_RE = re.compile(
    r"(superwhisper://|superwhisper-debug://|/Applications/|\.app/|--paste\b|--auto-paste\b|"
    r"--clipboard\b|--copy\b|--keystroke\b|--activate\b|--record\b|--mic\b|--microphone\b)",
    re.IGNORECASE,
)


class TranscriptionUnavailable(Exception):
    """No permitted transcription path exists. Carries an operator-facing reason."""


class JobRejected(Exception):
    """The job was refused before anything ran (single-flight, bounds, safety)."""


# --------------------------------------------------------------------- safety
def assert_safe_command(cmd) -> None:
    """Refuse any command that would reach the GUI, the devices, or the pasteboard."""
    if not cmd:
        raise JobRejected("empty command")
    binary = Path(str(cmd[0])).name.lower()
    if binary in FORBIDDEN_BINARIES:
        raise JobRejected(
            "refusing to run %r: it drives the GUI app, the pasteboard or an input device" % binary
        )
    for arg in cmd:
        if FORBIDDEN_ARG_RE.search(str(arg)):
            raise JobRejected(
                "refusing argument %r: it would invoke the app, the clipboard or a keystroke" % arg
            )


def job_environment(workspace: Path, base_env=None) -> dict:
    """Environment for a job: private Superwhisper state, no inherited pointers.

    The CLI documents ``SUPERWHISPER_DB`` and ``SUPERWHISPER_SETTINGS``; pointing
    both inside the workspace is what keeps a job out of the live history.
    """
    env = dict(os.environ if base_env is None else base_env)
    for leaked in [k for k in env if k.upper().startswith("SUPERWHISPER")]:
        del env[leaked]
    env["SUPERWHISPER_DB"] = str(workspace / "state" / "superwhisper.sqlite")
    env["SUPERWHISPER_SETTINGS"] = str(workspace / "state" / "settings.json")
    env["SUPERWHISPER_HEADLESS"] = "1"
    return env


# ---------------------------------------------------------------- single flight
class single_flight:
    """One job at a time, enforced by an exclusive lock file.

    A stale lock (holder gone) is reclaimed; a live one is refused rather than
    queued, because a second concurrent engine is exactly what must never exist.
    """

    def __init__(self, lock_path):
        self.lock_path = Path(lock_path)
        self.fd = None

    def __enter__(self):
        self.lock_path.parent.mkdir(parents=True, exist_ok=True)
        try:
            self.fd = os.open(str(self.lock_path), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except OSError as exc:
            if exc.errno != errno.EEXIST:
                raise
            if self._holder_alive():
                raise JobRejected("another transcription job is already running")
            self.lock_path.unlink(missing_ok=True)
            try:
                self.fd = os.open(str(self.lock_path), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            except FileExistsError:
                # Another job reclaimed the same stale lock first. Losing that
                # race is a refusal, not a crash - and never a second engine.
                raise JobRejected("another transcription job claimed the lock first")
        os.write(self.fd, ("%d\n" % os.getpid()).encode())
        return self

    def _holder_alive(self) -> bool:
        try:
            pid = int(self.lock_path.read_text().strip() or 0)
        except (OSError, ValueError):
            return False
        if pid <= 0:
            return False
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
        return True

    def __exit__(self, *exc):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
        self.lock_path.unlink(missing_ok=True)
        return False


# ------------------------------------------------------------------- workspace
class JobWorkspace:
    """A private directory per job, removed whatever happens."""

    def __init__(self, root=None, keep=False):
        self.root = root
        self.keep = keep
        self.path = None

    def __enter__(self):
        parent = str(self.root) if self.root else None
        if parent:
            Path(parent).mkdir(parents=True, exist_ok=True)
        self.path = Path(tempfile.mkdtemp(prefix="conductor-stt-", dir=parent))
        os.chmod(str(self.path), 0o700)
        (self.path / "state").mkdir()
        return self.path

    def __exit__(self, *exc):
        if self.path and not self.keep:
            shutil.rmtree(str(self.path), ignore_errors=True)
        return False


# ------------------------------------------------------------ guarded execution
def group_is_gone(pgid: int) -> bool:
    """True when no process remains in the group (the zero-survivor check)."""
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return True
    except PermissionError:
        return False
    return False


def kill_group(pgid: int, grace: float = GRACE_SECONDS) -> bool:
    """SIGTERM the group, then SIGKILL what is left. Returns True if none survive."""
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(pgid, sig)
        except ProcessLookupError:
            return True
        except PermissionError:
            return False
        deadline = time.monotonic() + (grace if sig == signal.SIGTERM else 1.0)
        while time.monotonic() < deadline:
            if group_is_gone(pgid):
                return True
            time.sleep(0.05)
    return group_is_gone(pgid)


class GuardedResult:
    def __init__(self, returncode, stdout, stderr, timed_out, survivors):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        self.timed_out = timed_out
        self.survivors = survivors


def run_guarded(cmd, env, deadline=DEFAULT_DEADLINE, cwd=None, popen=subprocess.Popen):
    """Run `cmd` in its own process group under a hard deadline.

    On timeout, failure or cancellation the entire group — the engine and every
    descendant it spawned — is torn down and the teardown is verified, so a job
    can never leave a second engine behind.
    """
    assert_safe_command(cmd)
    proc = popen(
        list(cmd), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
        cwd=str(cwd) if cwd else None, start_new_session=True, text=True,
    )
    pgid = os.getpgid(proc.pid) if hasattr(os, "getpgid") else proc.pid
    timed_out = False
    try:
        stdout, stderr = proc.communicate(timeout=deadline)
    except subprocess.TimeoutExpired:
        timed_out = True
        kill_group(pgid)
        try:
            stdout, stderr = proc.communicate(timeout=5)
        except Exception:
            stdout, stderr = "", "timed out"
    except BaseException:
        # Cancellation counts as failure: tear the group down before propagating.
        kill_group(pgid)
        raise
    finally:
        if proc.poll() is None:
            kill_group(pgid)
    survivors = not group_is_gone(pgid)
    return GuardedResult(proc.returncode, stdout or "", stderr or "", timed_out, survivors)


# ------------------------------------------------------------------- capability
class Capability:
    def __init__(self, available, reason, verb=None, cli=SUPERWHISPER_CLI):
        self.available = available
        self.reason = reason
        self.verb = verb
        self.cli = cli


TRANSCRIBE_VERBS = ("transcribe", "transcribe-file", "stt")


def detect_capability(cli=SUPERWHISPER_CLI, runner=None) -> Capability:
    """Ask the installed CLI whether it can transcribe a file at all.

    Detected at runtime rather than assumed: the day the vendor ships a
    transcribe verb, this starts returning available without a code change, and
    until then the reason string is precise instead of a silent no-op.
    """
    path = Path(cli)
    if not path.exists():
        return Capability(False, "superwhisper CLI not installed at %s" % cli, cli=cli)
    runner = runner or (lambda cmd: subprocess.run(
        cmd, capture_output=True, text=True, timeout=20))
    try:
        result = runner([str(path), "--help"])
    except (OSError, subprocess.SubprocessError) as exc:
        return Capability(False, "superwhisper CLI could not be queried: %s" % exc, cli=cli)
    text = ((result.stdout or "") + (result.stderr or "")).lower()
    for verb in TRANSCRIBE_VERBS:
        if re.search(r"^\s*%s\b" % re.escape(verb), text, re.MULTILINE):
            return Capability(True, "superwhisper CLI exposes '%s'" % verb, verb=verb, cli=cli)
    return Capability(
        False,
        "the installed superwhisper CLI has no transcribe command (it is a "
        "history/search tool); every documented file-transcription path goes "
        "through the GUI app, which this bridge is not permitted to drive",
        cli=cli,
    )


def transcribe_file(audio_path, workspace_root=None, model=DEFAULT_MODEL,
                    deadline=DEFAULT_DEADLINE, cli=SUPERWHISPER_CLI,
                    capability=None, runner=run_guarded, lock_path=None):
    """Transcribe `audio_path`, or refuse with a precise reason. Never falls back.

    A different engine is not substituted on failure: sending Franco's audio to
    an unapproved service would be a worse outcome than no transcript.
    """
    audio_path = Path(audio_path)
    if not audio_path.is_file():
        raise JobRejected("audio file not found: %s" % audio_path)
    cap = capability or detect_capability(cli)
    if not cap.available:
        raise TranscriptionUnavailable(cap.reason)

    lock = Path(lock_path) if lock_path else Path(tempfile.gettempdir()) / "conductor-stt.lock"
    with single_flight(lock):
        with JobWorkspace(workspace_root) as workspace:
            local_audio = workspace / audio_path.name
            shutil.copy2(str(audio_path), str(local_audio))
            cmd = [cap.cli, cap.verb, str(local_audio), "--model", model]
            result = runner(cmd, job_environment(workspace), deadline=deadline, cwd=workspace)
            if result.survivors:
                raise TranscriptionUnavailable(
                    "transcription job left processes behind; refusing the result"
                )
            if result.timed_out:
                raise TranscriptionUnavailable(
                    "transcription exceeded its %.0fs deadline and was terminated" % deadline
                )
            if result.returncode != 0:
                raise TranscriptionUnavailable(
                    "transcription failed (exit %s): %s"
                    % (result.returncode, (result.stderr or "").strip()[:300])
                )
            return (result.stdout or "").strip()
