"""Unit tests for the overlay (no Telegram, no agent-deck). Run: python -m unittest tests.test_bridge_local"""
import asyncio, json, logging, subprocess, sys, unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import bridge_local as bl  # noqa: E402

S = lambda title, group, status="idle", tool="claude", archived=False, id=None: {  # noqa: E731
    "title": title, "group": group, "status": status, "tool": tool,
    "archived": archived, "id": id or f"id-{title}", "tmux_session": f"tmux-{title}"}
SESS = [("operator", s) for s in [
    S("ops-main", "ops", "running"), S("ops-child", "ops"), S("opsy", "tools"),
    S("google", "tools", "waiting"), S("gsd", "ops", archived=True), S("conductor-slavna", "conductor", "stopped"),
    S("my session", "tmp"), S("fix <T>", "tmp")]]
ACTIVE = [(p, s) for p, s in SESS if not s["archived"]]
LONG = "overlay-test-1787721641 — mensaje de prueba del overlay /send (conductor-cmds). Confirma recepción en tu pane."


class Resolve(unittest.TestCase):
    def test_exact(self):
        m, e = bl.resolve_session(ACTIVE, "ops-main"); self.assertIsNone(e); self.assertEqual(m[1]["title"], "ops-main")
    def test_startswith_unique(self):
        m, e = bl.resolve_session(ACTIVE, "goo"); self.assertEqual(m[1]["title"], "google")
    def test_ambiguous_lists_candidates(self):
        m, e = bl.resolve_session(ACTIVE, "ops"); self.assertIsNone(m)
        self.assertIn("Ambiguous", e); self.assertIn("ops:ops-main", e); self.assertIn("tools:opsy", e)
    def test_group_prefix(self):
        m, e = bl.resolve_session(ACTIVE, "ops:ops-c"); self.assertEqual(m[1]["title"], "ops-child")
    def test_group_prefix_scopes(self):
        m, e = bl.resolve_session(ACTIVE, "tools:ops"); self.assertEqual(m[1]["title"], "opsy")
    def test_missing(self):
        m, e = bl.resolve_session(ACTIVE, "zzz"); self.assertIn("No active session", e)
    def test_archived_excluded(self):
        m, e = bl.resolve_session(ACTIVE, "gsd"); self.assertIn("No active session", e)
    def test_case_insensitive_prefix(self):
        m, e = bl.resolve_session(ACTIVE, "GOO"); self.assertEqual(m[1]["title"], "google")
    def test_quoted_title_with_spaces(self):
        self.assertEqual(bl.split_send_args('"my session" hola mundo'), ("my session", "hola mundo"))
        self.assertEqual(bl.split_send_args("ops-main hola"), ("ops-main", "hola"))
        self.assertIsNone(bl.split_send_args("ops-main")); self.assertIsNone(bl.split_send_args('"unterminated hola'))


class Format(unittest.TestCase):
    def test_groups_and_icons(self):
        out = bl.format_agents(ACTIVE, False)
        self.assertNotIn("gsd", out); self.assertIn("📂 conductor", out.splitlines()[0])
        self.assertIn("🟢 ops-main (claude)", out); self.assertIn("🟡 google (claude)", out); self.assertNotIn("[operator]", out)
    def test_only_group(self):
        out = bl.format_agents(ACTIVE, True, "tools"); self.assertIn("[operator] google", out); self.assertNotIn("ops-main", out)


CLAUDE_PANE = "line1\n⎿ done\n\n✳ Honking… (1m)\n\n" + "─" * 40 + "\n❯ hello there\n" + "─" * 40 + "\n  ctx 71%\n"
CODEX_PANE = "out\n\n\x1b[1m›\x1b[0m \x1b[2mImplement {feature}\x1b[0m\n\n  shopping · gpt\n"


class Pane(unittest.TestCase):
    def test_claude_split(self):
        out, comp = bl.split_pane(CLAUDE_PANE); self.assertEqual(comp, "hello there"); self.assertEqual(out[-1], "✳ Honking… (1m)")
    def test_codex_ghost_is_empty(self):
        out, comp = bl.split_pane(CODEX_PANE); self.assertEqual(comp, ""); self.assertEqual(out, ["out"])
    def test_dim_closed_by_other_sgr_and_combined_dim(self):
        out, comp = bl.split_pane("real line\n\x1b[2mghost\x1b[39m keep\n❯ \x1b[2;37mplaceholder\x1b[0m\n")
        self.assertEqual(out, ["real line", " keep"]); self.assertEqual(comp, "")
    def test_unknown_ui_fails_closed(self):
        self.assertIsNone(bl.split_pane("bash-5.2$ secret draft\n"))
        self.assertIn("refusing", bl.render_peek("x", "bash-5.2$ draft\n"))
    def test_ours_exact_and_long_prefix_only(self):
        self.assertTrue(bl.composer_is_ours("hello there", "hello  there"))
        self.assertFalse(bl.composer_is_ours("schedule", "schedule database migration"))   # short prefix = foreign
        self.assertFalse(bl.composer_is_ours("overlay-test-123 lo", "overlay-test-123 long message"))
        self.assertTrue(bl.composer_is_ours(LONG[:70], LONG))                                # long truncated prefix
        self.assertFalse(bl.composer_is_ours("ready and merge #33", "overlay-test")); self.assertFalse(bl.composer_is_ours("", "x"))
    def test_peek_render_and_escape(self):
        r = bl.render_peek("fix <T>", CLAUDE_PANE)
        self.assertTrue(r.startswith("📸 <b>fix &lt;T&gt;</b>\n<pre>")); self.assertNotIn("hello there", r)
    def test_peek_budget_whole_payload(self):
        r = bl.render_peek("t" * 500, "\n".join(f"l{i} " + "x" * 100 for i in range(200)) + "\n❯ \n")
        self.assertLess(len(r.encode()), 4096); self.assertIn("older lines trimmed", r); self.assertIn("l199", r)


class FakeCLI:
    def __init__(self, send_json, panes, pane_rc=0): self.send_json, self.panes, self.pane_rc, self.calls = send_json, list(panes), pane_rc, []
    def __call__(self, *args, profile=None, timeout=0):
        self.calls.append(args)
        if args[:2] == ("session", "send"):
            return subprocess.CompletedProcess(args, 0, json.dumps(self.send_json), "")
        return subprocess.CompletedProcess(args, self.pane_rc, self.panes.pop(0) if self.panes else "", "")


def ctx_with(cli):
    return {"run_cli": cli, "log": logging.getLogger("t"), "get_unique_profiles": lambda: ["operator"],
            "get_sessions_list_all": lambda p: SESS, "get_default_conductor": lambda: {"name": "slavna"},
            "conductor_session_title": lambda n: f"conductor-{n}", "split_message": lambda t: [t],
            "get_conductor_names": lambda: ["slavna"], "resolve_config_path": lambda n: "/nonexistent/" + n}


class Send(unittest.TestCase):
    def setUp(self):
        self.sent = []
        self._sub, self._time = bl.subprocess, bl.time
        bl.subprocess = SimpleNamespace(run=lambda cmd, **k: (self.sent.append(cmd), SimpleNamespace(returncode=0))[1],
                                        SubprocessError=subprocess.SubprocessError)
        bl.time = SimpleNamespace(sleep=lambda s: None)
    def tearDown(self):
        bl.subprocess, bl.time = self._sub, self._time
    def test_submitted_uses_dash_separator(self):
        cli = FakeCLI({"success": True, "delivery": "submitted"}, [])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), "--message-file /etc/hosts hi")
        self.assertTrue(r.startswith("✅")); a = cli.calls[0]
        self.assertEqual(a[a.index("--") + 1:], ("id-ops-main", "--message-file /etc/hosts hi")); self.assertLess(a.index("--no-wait"), a.index("--"))
    def test_foreign_composer_not_touched_and_not_echoed(self):
        cli = FakeCLI({"success": True, "delivery": "typed_not_submitted"}, ["x\n❯ ready and merge #33\n"])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), "overlay-test")
        self.assertTrue(r.startswith("⚠️")); self.assertNotIn("merge #33", r); self.assertEqual(self.sent, [])
    def test_rescue_enter_recaptures_first(self):
        cli = FakeCLI({"success": True, "delivery": "typed_not_submitted"}, [f"x\n❯ {LONG}\n", f"x\n❯ {LONG}\n", "x\n❯ \n"])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), LONG)
        self.assertTrue(r.startswith("✅")); self.assertIn("rescue", r)
        self.assertEqual(self.sent[0][-1], "Enter"); self.assertEqual(self.sent[0][-2], "tmux-ops-main")
        self.assertEqual(sum(1 for c in cli.calls if "output" in c), 3)
    def test_rescue_aborts_if_composer_changed(self):
        cli = FakeCLI({"success": True, "delivery": "typed_not_submitted"}, [f"x\n❯ {LONG}\n", "x\n❯ operator typing now\n"])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), LONG)
        self.assertIn("changed before rescue", r); self.assertEqual(self.sent, [])
    def test_unverified_is_not_green(self):
        cli = FakeCLI({"success": True, "delivery": "unverified"}, ["x\n❯ \n"])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), "hi"); self.assertTrue(r.startswith("⚠️")); self.assertIn("unverified", r)
    def test_capture_failure_after_send(self):
        cli = FakeCLI({"success": True, "delivery": "unverified"}, [], pane_rc=1)
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), "hi"); self.assertTrue(r.startswith("⚠️")); self.assertEqual(self.sent, [])
    def test_tmux_failure_reported(self):
        bl.subprocess = SimpleNamespace(run=lambda cmd, **k: SimpleNamespace(returncode=1), SubprocessError=subprocess.SubprocessError)
        cli = FakeCLI({"success": True, "delivery": "typed_not_submitted"}, [f"x\n❯ {LONG}\n", f"x\n❯ {LONG}\n"])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), LONG); self.assertIn("rescue Enter failed", r)


class Handlers(unittest.TestCase):
    """Drive the registered handlers with fake aiogram messages."""
    def run_cmd(self, text, cli=None, boom=False):
        handlers = {}
        class DP:
            class message:
                @staticmethod
                def register(fn, cmd): handlers[fn.__name__] = fn
        bl.Command = lambda *names: names
        ctx = ctx_with(cli or FakeCLI({"success": True, "delivery": "submitted"}, []))
        if boom:
            ctx["get_sessions_list_all"] = lambda p: 1 / 0
        bl.register(DP, ctx, lambda m: True)
        replies = []
        async def answer(t, **kw): replies.append(t)
        msg = SimpleNamespace(text=text, answer=answer, from_user=SimpleNamespace(id=1))
        name = {"/agents": "cmd_agents", "/sessions": "cmd_agents", "/peek": "cmd_peek", "/send": "cmd_send", "/help": "cmd_help"}[text.split()[0]]
        asyncio.run(handlers[name](msg))
        return replies
    def test_agents(self):
        r = self.run_cmd("/agents"); self.assertIn("📂 ops", r[0]); self.assertNotIn("gsd", r[0])
    def test_send_ambiguous(self):
        self.assertIn("Ambiguous", self.run_cmd("/send ops hola")[0])
    def test_send_missing_body(self):
        self.assertIn("Usage", self.run_cmd("/send ops-main")[0])
    def test_send_ok(self):
        self.assertTrue(self.run_cmd("/send ops:ops-m hola mundo")[0].startswith("✅"))
    def test_send_quoted(self):
        self.assertIn("my session", self.run_cmd('/send "my session" hola')[0])
    def test_send_stopped(self):
        self.assertIn("stopped", self.run_cmd("/send conductor-sl hola")[0])
    def test_peek_default_conductor_stopped(self):
        self.assertIn("isn't running", self.run_cmd("/peek")[0])
    def test_help_mentions_conductors(self):
        r = self.run_cmd("/help")[0]; self.assertIn("/send", r); self.assertIn("slavna", r); self.assertIn("/sessions", r)
    def test_handler_exception_is_answered_not_raised(self):
        r = self.run_cmd("/agents", boom=True); self.assertIn("overlay cmd_agents failed", r[0])


if __name__ == "__main__":
    unittest.main()
