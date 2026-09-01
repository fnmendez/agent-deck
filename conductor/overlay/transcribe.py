"""Voice-note transcription for the conductor, via the `transcribe` CLI.

This module used to embed its own whisper.cpp engine. Franco decided the
conductor must use the shared tool from fnmendez/transcribe instead, so this
is now a thin adapter: it resolves the installed CLI, runs it with --json on
the saved note, and maps the result back onto the small API the bridge
already speaks (Transcript / TranscriptionUnavailable / JobRejected /
MIN_CONFIDENCE).

Isolation, the environment allowlist, single-flight, deadlines and the
zero-survivors guarantee live in the tool and are pinned by its own test
suite; what stays on this side is only what is not the tool's job: the
authorship fence, the bounded download, and the exactly-once ledger.

Nothing is ever substituted on failure: sending Franco's audio to a service
he did not approve would be a far worse outcome than no transcript.
"""

import json
import logging
import os
import shutil
import subprocess
from pathlib import Path

log = logging.getLogger("overlay.transcribe")

MIN_CONFIDENCE = 0.45

# Resolution order for the CLI. The linked worktree comes LAST and only as a
# development fallback: Franco removes linked worktrees after merging, so a
# hardcoded worktree path would break exactly when PR #1 of fnmendez/transcribe
# lands. PATH and ~/.local/bin (populated by `make install`) survive the merge.
_DEV_WORKTREE_BIN = Path(
    "/Users/francomendez/Software/tools/.worktrees/transcribe/feat%2Finitial-tool/bin/transcribe"
)
_TIMEOUT_MARGIN = 1900  # tool clamps its own deadline to <=1800s; this is a backstop


class TranscriptionUnavailable(Exception):
    """The tool or its engine/model cannot run here; message says the exact fix."""


class JobRejected(Exception):
    """This particular job was refused (bad input, busy, deadline); retry may help."""


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


def resolve_binary(which=shutil.which, exists=None) -> Path:
    """Find the transcribe CLI, or refuse with the precise reason.

    Order: `transcribe` on PATH, then ~/.local/bin/transcribe, then the
    development worktree (with a warning, because that copy disappears after
    the tool's PR merges). Fail-closed if none exists.
    """
    exists = exists or (lambda p: Path(p).is_file() and os.access(str(p), os.X_OK))
    found = which("transcribe")
    if found:
        return Path(found)
    installed = Path.home() / ".local" / "bin" / "transcribe"
    if exists(installed):
        return installed
    if exists(_DEV_WORKTREE_BIN):
        log.warning(
            "overlay transcribe: using the development worktree binary %s; "
            "run `make install` in fnmendez/transcribe so this survives the merge",
            _DEV_WORKTREE_BIN,
        )
        return _DEV_WORKTREE_BIN
    raise TranscriptionUnavailable(
        "the `transcribe` CLI is not installed: not on PATH, no %s, and no "
        "development worktree. Fix: merge fnmendez/transcribe#1 and run "
        "`make install` there (symlinks ~/.local/bin/transcribe)." % installed
    )


def _default_runner(cmd, timeout):
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)


def transcribe_voice(audio_path, engine=None, workspace_root=None, lock_path=None,
                     runner=None) -> Transcript:
    """Transcribe one voice note via the shared CLI, or refuse with a precise reason.

    `workspace_root`, `lock_path` and `engine` are accepted for compatibility
    with the previous embedded engine but ignored: the tool owns its own
    workspace, lock and engine now. `runner` injects a subprocess.run-shaped
    callable for tests.
    """
    audio_path = Path(audio_path)
    if not audio_path.is_file():
        raise JobRejected("audio file not found: %s" % audio_path)
    binary = resolve_binary()
    run = runner or _default_runner
    cmd = [str(binary), "--json", "--quiet", "--", str(audio_path)]
    try:
        proc = run(cmd, timeout=_TIMEOUT_MARGIN)
    except subprocess.TimeoutExpired:
        raise JobRejected("the transcription tool did not finish within %ds" % _TIMEOUT_MARGIN)
    except OSError as exc:
        raise TranscriptionUnavailable("could not execute %s: %s" % (binary, exc))

    rc = proc.returncode
    stderr = (proc.stderr or "").strip()
    detail = stderr.splitlines()[-1] if stderr else "no detail on stderr"
    if rc == 0:
        try:
            payload = json.loads((proc.stdout or "").strip().splitlines()[-1])
        except (ValueError, IndexError):
            raise TranscriptionUnavailable(
                "the tool exited 0 but its JSON output could not be parsed; "
                "refusing to guess (stderr: %s)" % detail
            )
        text = (payload.get("text") or "").strip()
        if not payload.get("isolated", False) or not payload.get("network_denied", False):
            # The tool only reports non-isolated runs for --dev-binary, which
            # this adapter never passes; treat it as a broken contract.
            raise TranscriptionUnavailable(
                "the tool reported a non-isolated run (isolated=%s network_denied=%s); "
                "refusing the result" % (payload.get("isolated"), payload.get("network_denied"))
            )
        return Transcript(
            text=text,
            language=payload.get("language"),
            confidence=payload.get("confidence"),
            engine=payload.get("engine") or "transcribe CLI",
            audio_seconds=payload.get("audio_seconds"),
        )
    if rc == 7:
        raise JobRejected("another transcription job is already running; try again in a moment")
    if rc == 3:
        raise TranscriptionUnavailable(detail)
    if rc == 4:
        raise JobRejected("the audio was rejected by the tool: %s" % detail)
    if rc == 5:
        raise JobRejected("the transcription hit its deadline: %s" % detail)
    if rc == 6:
        raise TranscriptionUnavailable(
            "the job left survivor processes and its result was discarded: %s" % detail
        )
    if rc == 130:
        raise JobRejected("the transcription was interrupted: %s" % detail)
    # 1 (internal), 2 (usage — would be a bug in this adapter), anything else.
    raise TranscriptionUnavailable("transcribe exited %d: %s" % (rc, detail))
