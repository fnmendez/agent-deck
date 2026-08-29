"""Unit tests for the overlay (no Telegram, no agent-deck). Run: python -m unittest tests.test_bridge_local"""
import asyncio, json, logging, subprocess, sys, unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import bridge_local as bl  # noqa: E402
import delivery as dl  # noqa: E402

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
    """Serves `session output --pane` from a queue and records every call."""

    def __init__(self, send_results, panes, pane_rc=0):
        self.send_results = list(send_results)
        self.panes = list(panes)
        self.pane_rc = pane_rc
        self.calls = []

    @property
    def sends(self):
        return [c for c in self.calls if c[:2] == ("session", "send")]

    def __call__(self, *args, profile=None, timeout=0):
        self.calls.append(args)
        if args[:2] == ("session", "send"):
            payload = self.send_results.pop(0) if self.send_results else {}
            rc = 0 if payload.get("delivery") == "submitted" else 1
            return subprocess.CompletedProcess(args, rc, json.dumps(payload), payload.pop("_stderr", ""))
        pane = self.panes.pop(0) if self.panes else ""
        return subprocess.CompletedProcess(args, self.pane_rc, pane, "")


def pane(transcript="", composer=""):
    return "%s\n%s\n❯ %s\n%s\n  status\n" % (transcript, "-" * 40, composer, "-" * 40)


def ctx_with(cli, status="idle"):
    return {
        "run_cli": cli,
        "log": logging.getLogger("t"),
        "get_unique_profiles": lambda: ["operator"],
        "get_sessions_list_all": lambda p: SESS,
        "get_default_conductor": lambda: {"name": "slavna"},
        "conductor_session_title": lambda n: "conductor-%s" % n,
        "split_message": lambda t: [t],
        "get_conductor_names": lambda: ["slavna"],
        "resolve_config_path": lambda n: "/nonexistent/" + n,
        "get_session_status": lambda s, profile=None: status,
    }


class Truth(unittest.TestCase):
    """delivery.resolve_truth: what actually happened to the body."""

    MSG = "overlay truth probe uno dos tres cuatro cinco seis siete ocho"

    def test_token_survives_pane_wrapping(self):
        token = dl.delivery_token(self.MSG)
        wrapped = "  overlay truth probe uno dos tres\n  cuatro cinco seis siete ocho"
        self.assertIn(token, dl.norm(wrapped))

    def test_token_prefers_the_longest_line(self):
        self.assertTrue(dl.delivery_token("hola\n" + self.MSG).startswith("overlay truth"))

    def test_delivered_when_transcript_count_grows(self):
        cli = FakeCLI([], [pane(transcript=self.MSG)])
        truth, _ = dl.resolve_truth(ctx_with(cli), "id", "operator", self.MSG, baseline=0)
        self.assertEqual(truth, dl.DELIVERED)

    def test_not_delivered_when_the_text_was_already_there(self):
        """A repeated body is decided by growth, not by mere presence."""
        cli = FakeCLI([], [pane(transcript=self.MSG)])
        truth, _ = dl.resolve_truth(ctx_with(cli), "id", "operator", self.MSG, baseline=1)
        self.assertEqual(truth, dl.ABSENT)

    def test_composer_beats_transcript(self):
        cli = FakeCLI([], [pane(transcript=self.MSG, composer=self.MSG)])
        truth, _ = dl.resolve_truth(ctx_with(cli), "id", "operator", self.MSG, baseline=0)
        self.assertEqual(truth, dl.IN_COMPOSER)

    def test_busy_session_with_clean_composer_counts_as_delivered(self):
        cli = FakeCLI([], [pane()])
        truth, _ = dl.resolve_truth(ctx_with(cli, status="running"), "id", "operator", self.MSG, 0)
        self.assertEqual(truth, dl.DELIVERED)

    def test_unreadable_screen_is_unknown_not_absent(self):
        cli = FakeCLI([], [], pane_rc=1)
        truth, _ = dl.resolve_truth(ctx_with(cli), "id", "operator", self.MSG, 0)
        self.assertEqual(truth, dl.UNKNOWN)

    def test_unparseable_screen_is_unknown(self):
        cli = FakeCLI([], ["bash-5.2$ operator draft\n"])
        truth, _ = dl.resolve_truth(ctx_with(cli), "id", "operator", self.MSG, 0)
        self.assertEqual(truth, dl.UNKNOWN)

    def test_parse_send_result_trusts_submitted_flag(self):
        ok, delivery = dl.parse_send_result(
            subprocess.CompletedProcess([], 1, json.dumps({"success": False, "submitted": True, "delivery": "submitted"}), "")
        )
        self.assertTrue(ok)
        self.assertEqual(delivery, "submitted")


class Send(unittest.TestCase):
    """do_send: report the truth, never resend."""

    MSG = LONG

    def setUp(self):
        self.sent = []
        self._sub = bl.subprocess
        bl.subprocess = SimpleNamespace(
            run=lambda cmd, **k: (self.sent.append(cmd), SimpleNamespace(returncode=0, stderr=""))[1],
            SubprocessError=subprocess.SubprocessError,
        )

    def tearDown(self):
        bl.subprocess = self._sub

    def test_submitted_is_a_single_send_with_separator(self):
        cli = FakeCLI([{"delivery": "submitted", "submitted": True}], [pane()])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), "--message-file /etc/hosts hi")
        self.assertTrue(r.startswith("✅"))
        self.assertEqual(len(cli.sends), 1)
        args = cli.sends[0]
        self.assertEqual(args[args.index("--") + 1:], ("id-ops-main", "--message-file /etc/hosts hi"))
        self.assertLess(args.index("--no-wait"), args.index("--"))

    def test_ambiguous_but_arrived_is_reported_delivered_without_resending(self):
        cli = FakeCLI([{"delivery": "typed", "success": False}], [pane(), pane(transcript=self.MSG)])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), self.MSG)
        self.assertTrue(r.startswith("✅"))
        self.assertIn("could not confirm", r)
        self.assertEqual(len(cli.sends), 1, "an ambiguous verdict must never trigger a second send")
        self.assertEqual(self.sent, [], "a delivered message must not get a rescue Enter")

    def test_in_composer_is_rescued(self):
        cli = FakeCLI([{"delivery": "typed", "success": False}],
                      [pane(), pane(composer=self.MSG), pane(composer=self.MSG)])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), self.MSG)
        self.assertTrue(r.startswith("✅"))
        self.assertEqual(self.sent[0][-1], "Enter")
        self.assertEqual(len(cli.sends), 1)

    def test_foreign_composer_is_never_touched_or_echoed(self):
        cli = FakeCLI([{"delivery": "typed_not_submitted", "success": False}],
                      [pane(), pane(composer="ready and merge #33")])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), self.MSG)
        self.assertTrue(r.startswith("❌"))
        self.assertNotIn("merge #33", r)
        self.assertEqual(self.sent, [])

    def test_absent_is_an_honest_failure(self):
        cli = FakeCLI([{"delivery": "no_evidence", "success": False, "_stderr": "session not found"}],
                      [pane(), pane()])
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), self.MSG)
        self.assertTrue(r.startswith("❌"))
        self.assertEqual(len(cli.sends), 1)

    def test_unreadable_screen_never_claims_delivery_and_never_resends(self):
        cli = FakeCLI([{"delivery": "typed", "success": False}], [], pane_rc=1)
        r = bl.do_send(ctx_with(cli), "operator", S("ops-main", "ops"), self.MSG)
        self.assertTrue(r.startswith("⚠️"))
        self.assertIn("Nothing was resent", r)
        self.assertEqual(len(cli.sends), 1)


class ConductorSend(unittest.TestCase):
    """The conductor path: the reason Slavna appeared not to answer."""

    MSG = "conductor probe " + LONG

    def setUp(self):
        self.sent = []
        self._sub = bl.subprocess
        bl.subprocess = SimpleNamespace(
            run=lambda cmd, **k: (self.sent.append(cmd), SimpleNamespace(returncode=0, stderr=""))[1],
            SubprocessError=subprocess.SubprocessError,
        )

    def tearDown(self):
        bl.subprocess = self._sub

    def _ctx(self, cli, status="idle"):
        ctx = ctx_with(cli, status=status)
        self.queued = []
        ctx["_enqueue_message"] = lambda s, m, p, cb: self.queued.append((s, m))
        ctx["_is_still_running_timeout"] = lambda e: "still running" in e.lower()
        ctx["get_session_output"] = lambda s, profile=None: "reply text"
        ctx["RESPONSE_TIMEOUT"] = 300
        ctx["send_to_conductor"] = lambda *a, **k: None
        return ctx

    def test_ambiguous_wait_send_awaits_the_reply_instead_of_failing(self):
        """delivery=typed + the body visible in the transcript = delivered."""
        cli = FakeCLI([{"delivery": "typed", "success": False,
                        "_stderr": "message reached 'conductor-slavna' but was never confirmed submitted"}],
                      [pane(), pane(transcript=self.MSG)])
        ctx = self._ctx(cli)
        ok, text, still_running = bl.make_send_to_conductor(ctx)(
            "conductor-slavna", self.MSG, profile="operator", wait_for_reply=True)
        self.assertFalse(ok)
        self.assertTrue(still_running, "a delivered message must ride the reply-pending path")
        self.assertEqual(len(cli.sends), 1, "never resend an ambiguous delivery")
        self.assertEqual(self.queued, [], "never queue a message that already arrived")

    def test_genuinely_absent_wait_send_still_fails(self):
        cli = FakeCLI([{"delivery": "no_evidence", "success": False, "_stderr": "session not found"}],
                      [pane(), pane()])
        ok, _t, still_running = bl.make_send_to_conductor(self._ctx(cli))(
            "conductor-slavna", self.MSG, profile="operator", wait_for_reply=True)
        self.assertFalse(ok)
        self.assertFalse(still_running)

    def test_still_running_timeout_keeps_its_stock_meaning(self):
        cli = FakeCLI([{"delivery": "?", "success": False, "_stderr": "agent still running after 5m0s"}], [pane()])
        ok, _t, still_running = bl.make_send_to_conductor(self._ctx(cli))(
            "conductor-slavna", self.MSG, profile="operator", wait_for_reply=True)
        self.assertFalse(ok)
        self.assertTrue(still_running)
        self.assertEqual(len(cli.sends), 1)

    def test_unreadable_screen_never_resends_and_never_lies(self):
        cli = FakeCLI([{"delivery": "typed", "success": False, "_stderr": "unconfirmed"}], [], pane_rc=1)
        ok, _t, still_running = bl.make_send_to_conductor(self._ctx(cli))(
            "conductor-slavna", self.MSG, profile="operator", wait_for_reply=True)
        self.assertFalse(ok)
        self.assertTrue(still_running)
        self.assertEqual(len(cli.sends), 1)
        self.assertEqual(self.queued, [])

    def test_successful_wait_send_returns_the_reply(self):
        cli = FakeCLI([{"delivery": "submitted", "submitted": True}], [pane()])
        ok, text, still_running = bl.make_send_to_conductor(self._ctx(cli))(
            "conductor-slavna", self.MSG, profile="operator", wait_for_reply=True)
        self.assertTrue(ok)
        self.assertEqual(text, "reply text")
        self.assertFalse(still_running)

    def test_nowait_ambiguous_but_arrived_is_success_not_a_queue(self):
        cli = FakeCLI([{"delivery": "typed", "success": False}], [pane(), pane(transcript=self.MSG)])
        ctx = self._ctx(cli)
        ok, _t, _s = bl.make_send_to_conductor(ctx)("conductor-slavna", self.MSG, profile="operator")
        self.assertTrue(ok)
        self.assertEqual(self.queued, [])
        self.assertEqual(len(cli.sends), 1)

    def test_busy_conductor_is_queued_without_sending(self):
        cli = FakeCLI([], [])
        ctx = self._ctx(cli, status="running")
        ok, _t, _s = bl.make_send_to_conductor(ctx)("conductor-slavna", self.MSG, profile="operator")
        self.assertTrue(ok)
        self.assertEqual(len(self.queued), 1)
        self.assertEqual(len(cli.sends), 0)

    def test_force_queue_short_circuits(self):
        cli = FakeCLI([], [])
        ctx = self._ctx(cli)
        ok, _t, _s = bl.make_send_to_conductor(ctx)(
            "conductor-slavna", self.MSG, profile="operator", force_queue=True)
        self.assertTrue(ok)
        self.assertEqual(len(self.queued), 1)
        self.assertEqual(len(cli.sends), 0)

    def test_nowait_composer_rescue_reports_success(self):
        cli = FakeCLI([{"delivery": "typed", "success": False}],
                      [pane(), pane(composer=self.MSG), pane(composer=self.MSG)])
        ctx = self._ctx(cli)
        ok, _t, _s = bl.make_send_to_conductor(ctx)("conductor-slavna", self.MSG, profile="operator")
        self.assertTrue(ok)
        self.assertEqual(self.sent[0][-1], "Enter")


class Install(unittest.TestCase):
    def test_rebinds_the_module_global(self):
        ctx = ctx_with(FakeCLI([], []))
        ctx.update({"_enqueue_message": lambda *a: None, "_is_still_running_timeout": lambda e: False,
                    "get_session_output": lambda *a, **k: "", "RESPONSE_TIMEOUT": 300})
        stock = lambda *a, **k: ("stock", "", False)
        ctx["send_to_conductor"] = stock
        self.assertTrue(bl.install_conductor_send(ctx))
        self.assertIsNot(ctx["send_to_conductor"], stock)

    def test_fails_closed_when_the_bridge_shape_changed(self):
        ctx = ctx_with(FakeCLI([], []))          # missing _enqueue_message et al.
        self.assertFalse(bl.install_conductor_send(ctx))


class Handlers(unittest.TestCase):
    """Drive the registered handlers with fake aiogram messages."""
    def run_cmd(self, text, cli=None, boom=False):
        handlers = {}
        class DP:
            class message:
                @staticmethod
                def register(fn, cmd): handlers[fn.__name__] = fn
        bl.Command = lambda *names: names
        ctx = ctx_with(cli or FakeCLI([{"delivery": "submitted", "submitted": True}], [pane(), pane()]))
        if boom:
            ctx["get_sessions_list_all"] = lambda p: 1 / 0
        ctx.setdefault("_enqueue_message", lambda *a: None)
        ctx.setdefault("_is_still_running_timeout", lambda e: False)
        ctx.setdefault("get_session_output", lambda *a, **k: "")
        ctx.setdefault("RESPONSE_TIMEOUT", 300)
        ctx.setdefault("send_to_conductor", lambda *a, **k: None)
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
