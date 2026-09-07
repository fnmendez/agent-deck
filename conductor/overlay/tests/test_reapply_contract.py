"""Contract between reapply.sh and the canonical bridge it patches.

reapply.sh inserts the overlay hook at one exact anchor in
``internal/session/conductor_bridge.py``. If a stock bridge change moves,
duplicates or renames that anchor — or the names the hook closes over — the
script fails *after* an ``agent-deck update`` has already replaced bridge.py,
which is precisely when the local Telegram commands are needed. These tests run
the same patch the script runs, so that breakage lands in CI instead.

The anchor, marker and hook text are extracted from reapply.sh itself, so this
test and the script can never drift apart.

Run: python -m unittest tests.test_reapply_contract  (from conductor/overlay)
"""

from __future__ import annotations

import py_compile
import re
import tempfile
import unittest
from pathlib import Path

OVERLAY_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = OVERLAY_DIR.parents[1]
REAPPLY = OVERLAY_DIR / "reapply.sh"
CANONICAL_BRIDGE = REPO_ROOT / "internal" / "session" / "conductor_bridge.py"


def _script() -> str:
    return REAPPLY.read_text(encoding="utf-8")


def _extract(pattern: str, what: str) -> str:
    match = re.search(pattern, _script(), re.DOTALL)
    if not match:
        raise AssertionError(
            "could not read the {} out of reapply.sh; the script's shape "
            "changed and this contract test must be updated with it".format(what)
        )
    return match.group(1)


def anchor() -> str:
    return _extract(r"ANCHOR='([^']+)'", "anchor")


def marker() -> str:
    return _extract(r"MARKER='([^']+)'", "marker")


def hook() -> str:
    return _extract(r"hook = '''(.*?)'''", "hook body")


class TrackedModulesTest(unittest.TestCase):
    """reapply.sh must restart on a change to ANY module the bridge imports.

    It used to hash only bridge_local.py, so editing delivery.py deployed the
    file and left the bridge serving the code already in memory - a silent
    no-op deploy.
    """

    def tracked(self):
        match = re.search(r'^MODULES="([^"]+)"', _script(), re.MULTILINE)
        self.assertIsNotNone(match, "reapply.sh no longer declares MODULES")
        return set(match.group(1).split())

    def imported_by_overlay(self):
        """Local modules bridge_local.py imports from the overlay directory."""
        source = (OVERLAY_DIR / "bridge_local.py").read_text(encoding="utf-8")
        local = {p.stem for p in OVERLAY_DIR.glob("*.py")}
        found = {"bridge_local.py"}
        for name in re.findall(r"^(?:import|from)\s+([a-z_][a-z0-9_]*)", source, re.MULTILINE):
            if name in local:
                found.add(name + ".py")
        return found

    def test_every_imported_module_is_tracked(self):
        missing = self.imported_by_overlay() - self.tracked()
        self.assertFalse(
            missing,
            "reapply.sh would not restart the bridge for a change in %s" % sorted(missing),
        )

    def test_tracked_modules_all_exist(self):
        for module in self.tracked():
            self.assertTrue((OVERLAY_DIR / module).is_file(), "%s is tracked but missing" % module)


class ReapplyContractTest(unittest.TestCase):
    def setUp(self):
        if not CANONICAL_BRIDGE.is_file():
            # The deployed overlay has no repository beside it, so this contract
            # simply does not apply there. It is a skip, not a failure — but only
            # ever when the file is genuinely absent, so CI (where the repo is
            # present) still runs every case.
            raise unittest.SkipTest(
                "no canonical bridge at %s — this suite pins the repository "
                "contract and only applies inside a checkout" % CANONICAL_BRIDGE
            )
        self.source = CANONICAL_BRIDGE.read_text(encoding="utf-8")

    def test_anchor_appears_exactly_once(self):
        """reapply.sh refuses on any count but one - so CI must hold it at one."""
        self.assertEqual(
            self.source.count(anchor()),
            1,
            "the reapply anchor %r must appear exactly once in the canonical "
            "bridge; reapply.sh fails closed otherwise and the overlay would be "
            "lost on the next agent-deck update" % anchor(),
        )

    def test_stock_bridge_carries_no_marker(self):
        self.assertNotIn(
            marker(),
            self.source,
            "the canonical bridge must not ship the overlay marker; reapply.sh "
            "would treat a fresh stock bridge as already patched",
        )

    def test_hook_closes_over_names_the_bridge_defines(self):
        """The hook uses sys, Path, dp, is_authorized and log from the bridge."""
        for needed in ("import sys", "from pathlib import Path"):
            self.assertIn(needed, self.source, "bridge no longer has %r" % needed)
        for name in ('authorized_user = config["telegram"]["user_id"]', "dp = Dispatcher()", "def is_authorized(", "log = logging.getLogger"):
            self.assertIn(name, self.source, "bridge no longer defines %r" % name)

    def test_patched_bridge_compiles_and_is_idempotent(self):
        patched = self.source.replace(anchor(), hook() + anchor(), 1)
        self.assertEqual(patched.count(marker()), 1, "hook inserted more than once")
        self.assertIn(
            hook() + anchor(), patched, "hook must sit immediately before the anchor"
        )
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "bridge_patched.py"
            target.write_text(patched, encoding="utf-8")
            try:
                py_compile.compile(str(target), doraise=True)
            except py_compile.PyCompileError as exc:
                self.fail("bridge with the overlay hook does not compile: %s" % exc)
        # Second application: reapply.sh short-circuits on the marker, so the
        # file it would leave behind is byte-identical to the first pass.
        self.assertEqual(patched.count(marker()), 1)
        self.assertEqual(patched.count(anchor()), 1)

    def test_hook_is_indented_for_its_enclosing_function(self):
        """The anchor sits inside create_telegram_bot; the hook must match it."""
        indent = len(anchor()) - len(anchor().lstrip())
        for line in hook().splitlines():
            if line.strip():
                self.assertTrue(
                    line.startswith(" " * indent),
                    "hook line is not indented to the anchor's level: %r" % line,
                )

class OverlayOnlyDeployTest(unittest.TestCase):
    """Run the actual installer against fake launchctl/plutil, never live services."""

    def setUp(self):
        import os
        import shutil
        import sys
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.data = self.root / "data"
        self.cdir = self.data / "conductor"
        self.overlay = self.cdir / "overlay"
        self.overlay.mkdir(parents=True)
        for name in ("bridge_local.py", "documents.py", "delivery.py", "media.py", "transcribe.py", "reapply.sh"):
            shutil.copy2(OVERLAY_DIR / name, self.overlay / name)
        self.bridge = self.cdir / "bridge.py"
        self.stock = CANONICAL_BRIDGE.read_text()
        self.old_hook = hook().replace(
            ", authorized_user_id=authorized_user", "")
        self.bridge.write_text(self.stock.replace(anchor(), self.old_hook + anchor(), 1))
        self.backup = self.cdir / "bridge.py.pre-overlay"
        self.backup.write_text(self.stock)
        self.venv = self.data / "bridge-venv" / "bin" / "python"
        self.venv.parent.mkdir(parents=True)
        self.venv.symlink_to(sys.executable)
        self.fake_home = self.root / "home"
        self.plist = self.fake_home / "Library/LaunchAgents/com.agentdeck.conductor-bridge.plist"
        self.plist.parent.mkdir(parents=True)
        self.plist.write_text("fixture plist must remain unchanged")
        settings = self.cdir / "slavna/.claude/settings.json"
        settings.parent.mkdir(parents=True)
        settings.write_text("deliberately invalid fixture: settings must not be read")
        (settings.parent.parent / "meta.json").write_text("{}")
        self.settings = settings
        self.pid = self.root / "pid"
        self.pid.write_text("100")
        self.log = self.cdir / "bridge.log"
        self.log.write_text("")
        self.calls = self.root / "calls"
        self.calls.write_text("")
        self.bindir = self.root / "bin"
        self.bindir.mkdir()
        def executable(name, code):
            target = self.bindir / name
            target.write_text("#!" + sys.executable + "\n" + code)
            target.chmod(0o700)
        executable("plutil", "import sys\n"
                   "if sys.argv[1:3] != ['-extract', 'ProgramArguments.0']: sys.exit(9)\n"
                   "print(" + repr(str(self.venv)) + ")\n")
        executable("launchctl", """import sys
from pathlib import Path
pid = Path(%r)
log = Path(%r)
calls = Path(%r)
if sys.argv[1:] == ['list']:
    print(pid.read_text() + ' 0 com.agentdeck.conductor-bridge')
elif sys.argv[1:3] == ['kickstart', '-k']:
    with calls.open('a') as f: f.write('restart\\n')
    pid.write_text(str(int(pid.read_text()) + 1))
    with log.open('a') as f: f.write('overlay: registered\\noverlay: document outbox ready\\n')
else:
    sys.exit(8)
""" % (str(self.pid), str(self.log), str(self.calls)))
        executable("sleep", "pass\n")
        executable("stat", "import os, sys\nprint(os.path.getsize(sys.argv[-1]))\n")
        self.env = dict(os.environ, HOME=str(self.fake_home), AGENT_DECK_DATA_DIR=str(self.data),
                        PATH=str(self.bindir) + os.pathsep + os.environ.get("PATH", ""),
                        PYTHONDONTWRITEBYTECODE="1")

    def run_apply(self, *extra):
        import subprocess
        return subprocess.run(["bash", str(self.overlay / "reapply.sh"), "--overlay-only", *extra],
                              env=self.env, capture_output=True, text=True, timeout=20)

    def test_overlay_only_updates_old_hook_restarts_once_and_preserves_settings_plist(self):
        original_settings = self.settings.read_bytes()
        original_plist = self.plist.read_bytes()
        before = self.bridge.read_bytes()
        dry = self.run_apply("--dry-run")
        self.assertEqual(dry.returncode, 0, dry.stdout + dry.stderr)
        self.assertEqual(self.bridge.read_bytes(), before)
        self.assertEqual(self.calls.read_text(), "")
        applied = self.run_apply()
        self.assertEqual(applied.returncode, 0, applied.stdout + applied.stderr)
        self.assertIn(hook() + anchor(), self.bridge.read_text())
        self.assertEqual(self.backup.read_text(), self.stock)
        self.assertEqual(self.settings.read_bytes(), original_settings)
        self.assertEqual(self.plist.read_bytes(), original_plist)
        self.assertEqual(self.calls.read_text(), "restart\n")
        again = self.run_apply()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertEqual(self.calls.read_text(), "restart\n")

    def test_unknown_hook_is_refused_without_settings_or_service_mutation(self):
        self.bridge.write_text(self.bridge.read_text().replace(
            "bridge_local.register(dp, globals(), is_authorized)", "bridge_local.register(dp, globals(), wrong_gate)"))
        before = self.bridge.read_bytes()
        result = self.run_apply()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported overlay registration", result.stderr)
        self.assertEqual(self.bridge.read_bytes(), before)
        self.assertEqual(self.calls.read_text(), "")

    def test_stock_hook_install_and_wrong_interpreter_fail_closed(self):
        self.bridge.write_text(self.stock)
        applied = self.run_apply()
        self.assertEqual(applied.returncode, 0, applied.stdout + applied.stderr)
        self.assertIn(hook() + anchor(), self.bridge.read_text())
        (self.bindir / "plutil").write_text("#!/bin/sh\necho /wrong/python\n")
        result = self.run_apply()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires the existing bridge-venv interpreter", result.stdout)
        self.assertEqual(self.calls.read_text(), "restart\n")


if __name__ == "__main__":
    unittest.main()
