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

The engine that actually runs is ``WhisperCpp``: ffmpeg decodes the voice note
into a 16 kHz mono WAV inside the private workspace, and ``whisper-cli`` reads
only that copy. Both are plain CLI binaries — no GUI, no clipboard, no
Accessibility, no microphone, no network — so the transcript comes back on the
stdout/JSON of the very process that produced it. That is what makes the
transcript *provably* the transcript of this file: there is no shared history to
attribute the wrong text from.
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


# ------------------------------------------------------------------ the engine
# Resolved at call time, never at import: the deployed overlay and the repo
# checkout sit in different places, and a test must be able to point elsewhere.
WHISPER_CLI = "/usr/local/bin/whisper-cli"
FFMPEG = "/usr/local/bin/ffmpeg"
MODEL_DIR_NAME = "stt-models"
DEFAULT_MODEL_FILE = "ggml-small.bin"
DEFAULT_LANGUAGE = "auto"
MAX_AUDIO_SECONDS = 480.0
# Measured on this machine (M1 Max, x86 binary under Rosetta, SSE4.2 backend):
# ggml-small greedy runs at ~1.0-1.7x real time. The multiplier is the slack on
# top of that, so a slow run is still finished rather than killed mid-sentence.
DEADLINE_BASE = 60.0
DEADLINE_PER_AUDIO_SECOND = 4.0
DEADLINE_CAP = 1800.0
DECODE_DEADLINE = 120.0
WAV_RATE = 16000
WAV_BYTES_PER_SAMPLE = 2
# Below this mean token probability the audio was not understood well enough to
# be acted on. It is a floor for garbled audio, not a quality bar.
MIN_CONFIDENCE = 0.45


class Transcript:
    """What one job produced, with everything needed to weigh it."""

    def __init__(self, text, language=None, confidence=None, engine="", audio_seconds=None):
        self.text = text
        self.language = language
        self.confidence = confidence
        self.engine = engine
        self.audio_seconds = audio_seconds

    def provenance(self) -> str:
        """One line describing where this text came from and how sure it is."""
        bits = ["engine: %s (local, offline)" % self.engine]
        if self.audio_seconds is not None:
            bits.append("audio: %.1fs" % self.audio_seconds)
        if self.language:
            bits.append("detected language: %s" % self.language)
        bits.append(
            "confidence: %.2f" % self.confidence if self.confidence is not None
            else "confidence: not reported"
        )
        return " · ".join(bits)


def deadline_for(audio_seconds) -> float:
    if not audio_seconds or audio_seconds <= 0:
        return DEADLINE_BASE
    return min(DEADLINE_CAP, DEADLINE_BASE + DEADLINE_PER_AUDIO_SECOND * float(audio_seconds))


def default_thread_count() -> int:
    return max(1, min(8, (os.cpu_count() or 4) - 2))


def conductor_root() -> Path:
    """The deployed overlay lives in <conductor>/overlay/, so the parent is it."""
    return Path(__file__).resolve().parent.parent


def _mean_token_probability(segments) -> float:
    """Mean probability over real tokens; whisper's own confidence signal."""
    values = []
    for segment in segments or []:
        for token in segment.get("tokens") or []:
            text = str(token.get("text", ""))
            probability = token.get("p")
            # [_BEG_], [_TT_123] and friends are control tokens, not speech.
            if text.startswith("[_") or not isinstance(probability, (int, float)):
                continue
            values.append(float(probability))
    return round(sum(values) / len(values), 4) if values else None


class WhisperCpp:
    """whisper.cpp behind ffmpeg. Two plain binaries, no GUI and no network."""

    def __init__(self, binary=None, ffmpeg=None, model=None, threads=None,
                 language=DEFAULT_LANGUAGE):
        self.binary = Path(binary or os.environ.get("CONDUCTOR_STT_WHISPER") or WHISPER_CLI)
        self.ffmpeg = Path(ffmpeg or os.environ.get("CONDUCTOR_STT_FFMPEG") or FFMPEG)
        self.model = Path(
            model or os.environ.get("CONDUCTOR_STT_MODEL")
            or conductor_root() / MODEL_DIR_NAME / DEFAULT_MODEL_FILE
        )
        self.threads = int(threads or os.environ.get("CONDUCTOR_STT_THREADS")
                           or default_thread_count())
        self.language = str(language or os.environ.get("CONDUCTOR_STT_LANGUAGE")
                            or DEFAULT_LANGUAGE)

    @property
    def name(self) -> str:
        return "whisper.cpp %s" % self.model.stem.replace("ggml-", "")

    def available(self):
        """(ok, reason). The reason names the exact missing piece, with its fix."""
        if not self.ffmpeg.is_file():
            return False, "ffmpeg not found at %s (brew install ffmpeg)" % self.ffmpeg
        if not self.binary.is_file():
            return False, "whisper-cli not found at %s (brew install whisper-cpp)" % self.binary
        if not self.model.is_file():
            return False, (
                "the speech model is missing at %s — download it with: curl -fL -o %s "
                "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s"
                % (self.model, self.model, self.model.name)
            )
        return True, "%s ready" % self.name

    def _decode(self, workspace: Path, audio: Path, runner) -> Path:
        """Opus/OGG/M4A -> 16 kHz mono PCM WAV, which is all whisper.cpp reads."""
        wav = workspace / "audio.wav"
        cmd = [
            str(self.ffmpeg), "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
            "-i", str(audio), "-vn", "-sn", "-dn", "-map", "0:a:0",
            "-ac", "1", "-ar", str(WAV_RATE), "-c:a", "pcm_s16le", "-f", "wav", str(wav),
        ]
        result = runner(cmd, job_environment(workspace), deadline=DECODE_DEADLINE, cwd=workspace)
        if result.survivors:
            raise TranscriptionUnavailable("the decoder left processes behind; refusing the result")
        if result.timed_out:
            raise TranscriptionUnavailable("decoding the audio exceeded its deadline")
        if result.returncode != 0 or not wav.is_file():
            raise TranscriptionUnavailable(
                "could not decode that audio (ffmpeg exit %s): %s"
                % (result.returncode, (result.stderr or "").strip()[:200])
            )
        return wav

    @staticmethod
    def wav_seconds(wav: Path) -> float:
        """Duration straight from the PCM size — no second probe process."""
        try:
            payload = max(0, wav.stat().st_size - 44)   # canonical WAV header
        except OSError:
            return 0.0
        return round(payload / float(WAV_RATE * WAV_BYTES_PER_SAMPLE), 2)

    def run(self, workspace: Path, audio: Path, runner=None) -> Transcript:
        runner = runner or run_guarded
        wav = self._decode(workspace, audio, runner)
        seconds = self.wav_seconds(wav)
        if seconds > MAX_AUDIO_SECONDS:
            raise JobRejected(
                "that note is %.0fs long; this bridge transcribes up to %.0fs"
                % (seconds, MAX_AUDIO_SECONDS)
            )
        stem = workspace / "result"
        cmd = [
            str(self.binary), "-m", str(self.model), "-f", str(wav),
            "-l", self.language, "-t", str(self.threads),
            "-bo", "1", "-bs", "1",         # greedy: ~2x faster, same text here
            "-nt", "-np", "-ojf", "-of", str(stem),
        ]
        result = runner(cmd, job_environment(workspace),
                        deadline=deadline_for(seconds), cwd=workspace)
        if result.survivors:
            raise TranscriptionUnavailable(
                "transcription job left processes behind; refusing the result"
            )
        if result.timed_out:
            raise TranscriptionUnavailable(
                "transcription exceeded its %.0fs deadline and was terminated"
                % deadline_for(seconds)
            )
        if result.returncode != 0:
            raise TranscriptionUnavailable(
                "transcription failed (exit %s): %s"
                % (result.returncode, (result.stderr or "").strip()[:300])
            )
        return self._read_result(stem, result, seconds)

    def _read_result(self, stem: Path, result, seconds: float) -> Transcript:
        """Prefer the JSON the run wrote; fall back to its own stdout."""
        report = stem.with_suffix(".json")
        language = None
        confidence = None
        text = (result.stdout or "").strip()
        if report.is_file():
            try:
                import json
                data = json.loads(report.read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise TranscriptionUnavailable("unreadable transcription output: %s" % exc)
            segments = data.get("transcription") or []
            joined = "".join(str(s.get("text", "")) for s in segments).strip()
            if joined:
                text = joined
            language = ((data.get("result") or {}).get("language")) or None
            confidence = _mean_token_probability(segments)
        if not text:
            raise TranscriptionUnavailable("the audio produced no speech")
        return Transcript(text, language=language, confidence=confidence,
                          engine=self.name, audio_seconds=seconds)


def transcribe_voice(audio_path, engine=None, workspace_root=None, lock_path=None,
                     runner=None) -> Transcript:
    """Transcribe one voice note locally, or refuse with a precise reason.

    Nothing is ever substituted on failure: sending Franco's audio to a service
    he did not approve would be a far worse outcome than no transcript.
    """
    audio_path = Path(audio_path)
    if not audio_path.is_file():
        raise JobRejected("audio file not found: %s" % audio_path)
    engine = engine or WhisperCpp()
    ok, reason = engine.available()
    if not ok:
        raise TranscriptionUnavailable(reason)
    lock = Path(lock_path) if lock_path else Path(tempfile.gettempdir()) / "conductor-stt.lock"
    with single_flight(lock):
        with JobWorkspace(workspace_root) as workspace:
            local_audio = workspace / ("note" + (audio_path.suffix or ".ogg"))
            shutil.copy2(str(audio_path), str(local_audio))
            return engine.run(workspace, local_audio, runner=runner)
