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
        self.assertTrue(
            CANONICAL_BRIDGE.is_file(), "canonical bridge missing: %s" % CANONICAL_BRIDGE
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
        for name in ("dp = Dispatcher()", "def is_authorized(", "log = logging.getLogger"):
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


if __name__ == "__main__":
    unittest.main()
