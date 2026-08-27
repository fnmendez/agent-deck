"""Guard: the bridge suite must never touch the operator's live install.

The bridge resolves CONDUCTOR_DIR / CONFIG_PATH / LOG_PATH at import time and
attaches a FileHandler to LOG_PATH. Without the sandbox in conftest.py, running
this suite on a machine with a real conductor appended the tests' own log
records to the production bridge.log — the dedupe suite calls
``ensure_conductor_running("ops", "work")``, whose warnings then looked like a
misconfigured live conductor — and resolved CONFIG_PATH to the real
config.toml, bot token included.

These tests fail loudly if that sandbox is ever removed or bypassed.
"""

from __future__ import annotations

import logging
from pathlib import Path

import bridge


def _live_install_dirs():
    """Directories that belong to a real agent-deck install, never to tests."""
    home = Path.home()
    return [
        home / ".local" / "share" / "agent-deck",
        home / ".config" / "agent-deck",
        home / ".agent-deck",
    ]


def _assert_outside_live_install(path: Path, what: str) -> None:
    resolved = Path(path).resolve()
    for live in _live_install_dirs():
        live_resolved = live.resolve() if live.exists() else live
        assert resolved != live_resolved and live_resolved not in resolved.parents, (
            f"{what} resolves inside the live agent-deck install ({resolved}); "
            "the conftest sandbox is missing or was bypassed"
        )


def test_import_time_paths_are_sandboxed():
    for attr in ("CONDUCTOR_DIR", "LOG_PATH", "CONFIG_PATH"):
        _assert_outside_live_install(getattr(bridge, attr), f"bridge.{attr}")


def test_no_file_handler_writes_into_the_live_install():
    handlers = list(logging.getLogger().handlers)
    handlers.extend(bridge.log.handlers)
    for handler in handlers:
        filename = getattr(handler, "baseFilename", None)
        if filename:
            _assert_outside_live_install(Path(filename), f"log handler {filename}")
