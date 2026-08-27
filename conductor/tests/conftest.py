"""Test fixtures for the conductor bridge.

The canonical bridge script lives at ``internal/session/conductor_bridge.py``
(embedded into the binary via ``//go:embed``); there is no ``conductor/bridge.py``
checked into the repo. To keep these tests running against the one canonical
file — and to preserve the existing ``from bridge import ...`` /
``mock.patch("bridge.<attr>")`` usage in the test bodies — load that file under
the module name ``bridge`` before any test module is imported.

Loading it also has to be *sandboxed*. The bridge resolves ``CONDUCTOR_DIR``,
``CONFIG_PATH`` and ``LOG_PATH`` at import time and immediately attaches a
``FileHandler`` to ``LOG_PATH``, so importing it with the host's real
environment bound the suite to the operator's live ``bridge.log`` and to the
real ``config.toml`` (bot token included). Tests that drive conductor code
paths — ``ensure_conductor_running("ops", "work")`` in the dedupe suite, the
hook tests — then wrote their log records into a production log. Every host
path the bridge reads at import is therefore pointed into a temp dir first.
"""

from __future__ import annotations

import atexit
import importlib.util
import os
import shutil
import sys
import tempfile
from pathlib import Path

# repo_root/conductor/tests/conftest.py -> repo_root/internal/session/conductor_bridge.py
_CANONICAL = (
    Path(__file__).resolve().parents[2]
    / "internal"
    / "session"
    / "conductor_bridge.py"
)


def _load_canonical_bridge() -> None:
    if "bridge" in sys.modules:
        return
    if not _CANONICAL.is_file():
        raise FileNotFoundError(
            f"canonical bridge source not found at {_CANONICAL}; "
            "it should live at internal/session/conductor_bridge.py"
        )
    spec = importlib.util.spec_from_file_location("bridge", _CANONICAL)
    module = importlib.util.module_from_spec(spec)
    # Register before exec so the module can be patched/imported as "bridge".
    sys.modules["bridge"] = module
    spec.loader.exec_module(module)


_SANDBOX = Path(tempfile.mkdtemp(prefix="agent-deck-bridge-tests-"))
atexit.register(shutil.rmtree, str(_SANDBOX), ignore_errors=True)


def _sandbox_bridge_environment() -> None:
    """Point the bridge's import-time path resolution inside ``_SANDBOX``.

    ``AGENT_DECK_CONDUCTOR_DIR`` governs ``CONDUCTOR_DIR`` (and therefore
    ``LOG_PATH``); the XDG variables govern ``CONFIG_PATH``. The conductor dir
    is created so the bridge takes its normal file-logging path against the
    sandbox rather than silently skipping it — the suite exercises the same
    shape as production, just never the production files.

    ``test_bridge_paths.py`` builds its own subprocess environment and pops
    these keys, so its host-resolution scenarios are unaffected.
    """
    conductor_dir = _SANDBOX / "conductor"
    conductor_dir.mkdir(parents=True, exist_ok=True)
    os.environ["AGENT_DECK_CONDUCTOR_DIR"] = str(conductor_dir)
    os.environ["XDG_DATA_HOME"] = str(_SANDBOX / "data")
    os.environ["XDG_CONFIG_HOME"] = str(_SANDBOX / "config")


_sandbox_bridge_environment()
_load_canonical_bridge()
