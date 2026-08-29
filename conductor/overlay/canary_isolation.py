#!/usr/bin/env python3
"""Live canary: prove a transcription job leaves the operator's machine alone.

Run this on the machine, not in CI. It takes a before/after reading of the
things a badly-behaved job would disturb, runs a real job through the real code
path in between, and prints a verdict per property:

  * frontmost application (LaunchServices, read-only - no Accessibility events)
  * clipboard contents (hashed, never printed, never written)
  * the superwhisper GUI app's pid and start time (must be the same process)
  * the live transcription database: size, mtime and recording count
  * superwhisper process count (no second engine, no orphan)

Usage:  python3 canary_isolation.py [--audio /path/to/sample]
Exit 0 only when every property is unchanged.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import transcribe as t  # noqa: E402

LIVE_DB = Path.home() / "Library/Application Support/superwhisper/database/superwhisper.sqlite"


def run(cmd, timeout=20):
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return (result.stdout or "").strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def frontmost():
    """Frontmost app via LaunchServices. Read-only; no Accessibility, no events."""
    return run(["lsappinfo", "front"])


def clipboard_digest():
    """Hash the clipboard so a change is detectable without ever printing it."""
    data = run(["pbpaste"])
    return hashlib.sha256(data.encode("utf-8", "replace")).hexdigest()[:16]


def superwhisper_processes():
    out = run(["pgrep", "-f", "superwhisper.app/Contents/MacOS/superwhisper"])
    return sorted(p for p in out.split() if p.strip())


def process_start(pid):
    return run(["ps", "-o", "lstart=", "-p", str(pid)])


def db_fingerprint():
    if not LIVE_DB.exists():
        return {"present": False}
    stat = LIVE_DB.stat()
    count = run(["/usr/local/bin/superwhisper", "doctor"])
    recordings = ""
    for line in count.splitlines():
        if line.lower().startswith("recordings:"):
            recordings = line.split(":", 1)[1].strip()
    return {"present": True, "size": stat.st_size, "mtime": stat.st_mtime, "recordings": recordings}


def snapshot():
    pids = superwhisper_processes()
    return {
        "frontmost": frontmost(),
        "clipboard": clipboard_digest(),
        "sw_pids": pids,
        "sw_starts": {pid: process_start(pid) for pid in pids},
        "db": db_fingerprint(),
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--audio", default=None)
    parser.add_argument(
        "--simulate", action="store_true",
        help="also run a real process through the job guard (a stand-in engine), "
             "so isolation is proven while a job actually executes rather than "
             "only on the fail-closed path",
    )
    args = parser.parse_args()

    print("== superwhisper capability ==")
    capability = t.detect_capability()
    print("available: %s\nreason:    %s" % (capability.available, capability.reason))

    with tempfile.TemporaryDirectory() as tmp:
        audio = Path(args.audio) if args.audio else Path(tmp) / "canary.ogg"
        if not audio.exists():
            audio.write_bytes(b"OggS\x00canary")     # never transcribed; the job fails closed

        print("\n== before ==")
        before = snapshot()
        for key in ("frontmost", "clipboard", "sw_pids"):
            print("%-10s %s" % (key, before[key]))
        print("%-10s %s" % ("db", before["db"]))

        print("\n== running one real job through the production path ==")
        started = time.monotonic()
        try:
            text = t.transcribe_file(
                audio, workspace_root=Path(tmp) / "jobs", lock_path=Path(tmp) / "stt.lock",
            )
            print("transcript: %d chars" % len(text))
        except (t.TranscriptionUnavailable, t.JobRejected) as exc:
            print("refused (expected while no permitted engine exists): %s" % exc)
        print("elapsed: %.2fs" % (time.monotonic() - started))

        if args.simulate:
            # Same guard, same workspace, same teardown - but with a stand-in
            # engine that really runs and really spawns a descendant, so the
            # isolation properties are exercised by a live job.
            print("\n== simulated engine under the same guard ==")
            spawner = (
                "import subprocess,sys,time;"
                "subprocess.Popen(['sleep','90']);"
                "print('canary transcript');sys.stdout.flush();time.sleep(90)"
            )
            captured = {}

            def stand_in(cmd, env, deadline=None, cwd=None):
                captured["env"] = env
                captured["cwd"] = cwd
                return t.run_guarded([sys.executable, "-c", spawner], env, deadline=4.0, cwd=cwd)

            try:
                t.transcribe_file(
                    audio, workspace_root=Path(tmp) / "jobs2", lock_path=Path(tmp) / "stt2.lock",
                    capability=t.Capability(True, "simulated", verb="transcribe"),
                    runner=stand_in,
                )
            except (t.TranscriptionUnavailable, t.JobRejected) as exc:
                print("job ended as: %s" % exc)
            db_env = captured.get("env", {}).get("SUPERWHISPER_DB", "")
            print("job SUPERWHISPER_DB: %s" % db_env)
            print("points at the live DB: %s" % (str(LIVE_DB) in db_env))
            print("descendants left: %s" % run(["pgrep", "-f", "^sleep 90"]))

        print("\n== after ==")
        after = snapshot()
        # Check the roots the jobs above actually used, not just the system
        # temp dir - a job runs under workspace_root, so globbing /tmp alone
        # would pass without ever measuring anything.
        workspace_leftovers = []
        for root in (Path(tmp), Path(tmp) / "jobs", Path(tmp) / "jobs2", Path(tempfile.gettempdir())):
            workspace_leftovers.extend(str(x) for x in root.glob("conductor-stt-*"))
        print("workspace leftovers: %s" % (workspace_leftovers or "none"))

    checks = [
        ("frontmost app unchanged", before["frontmost"] == after["frontmost"]),
        ("clipboard untouched", before["clipboard"] == after["clipboard"]),
        ("same superwhisper pids", before["sw_pids"] == after["sw_pids"]),
        ("no superwhisper restart", before["sw_starts"] == after["sw_starts"]),
        ("no extra superwhisper process", len(after["sw_pids"]) <= len(before["sw_pids"])),
        ("live database untouched", before["db"] == after["db"]),
        ("no job workspace left behind", not workspace_leftovers),
        ("no stand-in engine descendants left", not run(["pgrep", "-f", "^sleep 90"])),
    ]
    print("\n== verdict ==")
    ok = True
    for label, passed in checks:
        print("%s %s" % ("PASS" if passed else "FAIL", label))
        ok = ok and passed
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
