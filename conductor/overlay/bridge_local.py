"""Local overlay commands for the agent-deck conductor Telegram bridge.

Loaded by a 7-line hook in bridge.py (see reapply.sh). Adds:
  /agents [group]        non-archived sessions grouped by agent-deck group
  /sessions              alias of /agents (replaces the stock listing)
  /peek [session]        one screen snapshot, input box stripped
  /send <session> <msg>  deliver a message; verified, guarded rescue Enter
  /help                  stock help + these commands

Only stable agent-deck CLI surfaces are used (list/session --json, session
output --pane). The single exception is a rescue `tmux send-keys Enter`,
issued only when the composer holds exactly the message we just sent (see
composer_is_ours). Accepted residual risk (design decision 2026-08-26): the
capture->Enter window cannot be made atomic through this interface; we
re-capture immediately before pressing Enter to keep it as small as possible.
"""
from __future__ import annotations

import asyncio
import functools
import html
import json
import re
import shlex
import subprocess
import time
from pathlib import Path

try:
    from aiogram.filters import Command
except ImportError:  # tests without aiogram
    Command = None  # type: ignore

STATUS_ICON = {
    "running": "\U0001f7e2", "waiting": "\U0001f7e1", "idle": "⚪",
    "error": "\U0001f534", "stopped": "⚫",
}
PROMPT_RE = re.compile(r"^\s*[❯›]")          # ❯ (claude) / › (codex)
RULE_RE = re.compile(r"^\s*[─━┄┈═\-]{8,}\s*$")
ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")
# Dim (ghost/placeholder) runs: opened by SGR 2 (alone or combined), closed by ANY SGR.
DIM_RE = re.compile(r"\x1b\[2(?:;[0-9;]*)?m.*?(?=\x1b\[[0-9;]*m|$)", re.S)
TG_LIMIT = 4096
PEEK_BUDGET = 3800
# A composer prefix counts as "ours" only when it is long enough that a
# coincidental foreign draft is implausible (truncation evidence by length).
PREFIX_MIN_LEN = 60
DEFAULT_CONFIG_PATH = Path.home() / ".config" / "agent-deck" / "config.toml"
SEND_USAGE = "Usage: /send <session|group:session|\"title with spaces\"> <message>"


# ---------------------------------------------------------------- helpers
def strip_ansi(text: str, drop_dim: bool = False) -> str:
    if drop_dim:
        text = DIM_RE.sub("", text)
    return ANSI_RE.sub("", text)


def active_sessions(ctx) -> list[tuple[str, dict]]:
    """(profile, session) for every non-archived session across profiles."""
    out = []
    for profile, s in ctx["get_sessions_list_all"](ctx["get_unique_profiles"]()):
        if isinstance(s, dict) and not s.get("archived"):
            out.append((profile, s))
    return out


def resolve_session(sessions: list[tuple[str, dict]], query: str, usage: str = SEND_USAGE):
    """Exact title, else case-insensitive startswith; 'group:name' filters.

    Returns (match, error_text). Ambiguity is an error listing candidates.
    """
    group = None
    name = query.strip()
    if ":" in name:
        group, name = name.split(":", 1)
        group, name = group.strip(), name.strip()
    if not name:
        return None, usage
    pool = [(p, s) for p, s in sessions if group is None or s.get("group") == group]
    exact = [(p, s) for p, s in pool if s.get("title") == name]
    if len(exact) == 1:
        return exact[0], None
    low = name.lower()
    pre = [(p, s) for p, s in pool if str(s.get("title", "")).lower().startswith(low)]
    if len(pre) == 1:
        return pre[0], None
    if not pre:
        scope = f" in group '{group}'" if group else ""
        return None, f"No active session matches '{name}'{scope}."
    names = ", ".join(f"{s.get('group') or '-'}:{s.get('title')}" for _, s in pre)
    return None, f"Ambiguous '{name}' — candidates: {names}"


def split_send_args(rest: str) -> tuple[str, str] | None:
    """'<session> <message>' -> (session, message). Session may be "quoted"."""
    rest = rest.strip()
    if not rest:
        return None
    if rest[0] in "\"'":
        try:
            lex = shlex.shlex(rest, posix=True)
            lex.whitespace_split = True
            first = lex.get_token()
            body = lex.instream.read().strip()
        except ValueError:
            return None
        return (first, body) if first and body else None
    parts = rest.split(None, 1)
    return (parts[0], parts[1].strip()) if len(parts) == 2 and parts[1].strip() else None


def format_agents(sessions: list[tuple[str, dict]], multi_profile: bool, only_group: str | None = None) -> str:
    groups: dict[str, list] = {}
    for p, s in sessions:
        g = s.get("group") or "(root)"
        if only_group and g != only_group:
            continue
        groups.setdefault(g, []).append((p, s))
    if not groups:
        return "No active sessions." if not only_group else f"No active sessions in group '{only_group}'."
    lines = []
    for g in sorted(groups, key=lambda x: (x != "conductor", x)):
        lines.append(f"\U0001f4c2 {g}")
        for p, s in sorted(groups[g], key=lambda ps: str(ps[1].get("title", "")).lower()):
            icon = STATUS_ICON.get(s.get("status", ""), "❓")
            tag = f"[{p}] " if multi_profile else ""
            lines.append(f"  {icon} {tag}{s.get('title', 'untitled')} ({s.get('tool', '?')})")
    return "\n".join(lines)


def split_pane(raw: str) -> tuple[list[str], str] | None:
    """Split a pane capture into (output_lines, composer_text).

    Composer = last prompt line (❯/›) and what follows until a rule/blank.
    Dim (ghost/placeholder) text is dropped before parsing. Returns None when
    no prompt line can be located (unknown UI): callers must fail closed.
    """
    clean = strip_ansi(raw, drop_dim=True)
    lines = clean.split("\n")
    idx = None
    for i in range(len(lines) - 1, -1, -1):
        if PROMPT_RE.match(lines[i]):
            idx = i
            break
    if idx is None:
        return None
    composer = [PROMPT_RE.sub("", lines[idx], count=1).strip()]
    for ln in lines[idx + 1:]:
        if RULE_RE.match(ln) or not ln.strip():
            break
        composer.append(ln.strip())
    cut = idx
    if idx > 0 and RULE_RE.match(lines[idx - 1]):
        cut = idx - 1
    output = [ln.rstrip() for ln in lines[:cut]]
    while output and not output[-1].strip():
        output.pop()
    return output, " ".join(c for c in composer if c).strip()


def norm(s: str) -> str:
    return re.sub(r"\s+", " ", s).strip()


def composer_is_ours(composer: str, message: str) -> bool:
    """Exact normalized match, or a long (>= PREFIX_MIN_LEN) prefix of it."""
    c, m = norm(composer), norm(message)
    if not c:
        return False
    return c == m or (len(c) >= PREFIX_MIN_LEN and m.startswith(c))


def tmux_socket_name(ctx) -> str:
    """Socket from agent-deck config, resolved via the bridge's own XDG-aware
    resolver when available (stable: it is the bridge that reads the same file)."""
    try:
        import toml
        resolver = ctx.get("resolve_config_path") if isinstance(ctx, dict) else None
        path = resolver("config.toml") if callable(resolver) else DEFAULT_CONFIG_PATH
        cfg = toml.load(path)
        return str(cfg.get("tmux", {}).get("socket_name") or "agent-deck")
    except Exception:
        return "agent-deck"


def render_peek(title: str, raw: str) -> str:
    parsed = split_pane(raw)
    header = f"\U0001f4f8 <b>{html.escape(str(title))}</b>\n"
    if parsed is None:
        return header + "\U0001f6ab can't locate the input box on this screen — refusing the snapshot."
    output, _ = parsed
    if not output:
        return header + "(no output above the input box)"
    budget = min(PEEK_BUDGET, TG_LIMIT - len(header.encode("utf-8")) - 40)
    esc = html.escape("\n".join(output))
    if len(esc.encode("utf-8")) > budget:
        kept: list[str] = []
        for ln in reversed(output):
            trial = html.escape("\n".join(["… (older lines trimmed)", ln, *kept]))
            if len(trial.encode("utf-8")) > budget:
                break
            kept.insert(0, ln)
        esc = html.escape("\n".join(["… (older lines trimmed)", *kept]))
    return header + "<pre>" + esc + "</pre>"


# ---------------------------------------------------------------- actions
def _capture(ctx, sid: str, profile: str):
    """Pane capture -> (output, composer) or None on failure/unknown UI."""
    res = ctx["run_cli"]("session", "output", "--pane", "-q", "--", sid, profile=profile, timeout=30)
    if res.returncode != 0 or not (res.stdout or "").strip():
        return None
    return split_pane(res.stdout)


def do_send(ctx, profile: str, sess: dict, message: str) -> str:
    """Send via agent-deck, verify delivery, guarded rescue Enter. Returns report."""
    run_cli = ctx["run_cli"]
    log = ctx["log"]
    sid = sess.get("id") or sess.get("title")
    title = sess.get("title")
    # Flags first, then "--": Telegram text can never be parsed as a CLI option.
    res = run_cli("session", "send", "--no-wait", "--json", "--timeout", "30s",
                  "--", sid, message, profile=profile, timeout=60)
    delivery, ok = "?", res.returncode == 0
    try:
        data = json.loads(res.stdout or "{}")
        delivery = str(data.get("delivery", delivery))
        ok = bool(data.get("success", ok))
    except json.JSONDecodeError:
        pass
    log.info("overlay /send -> %s: rc=%s delivery=%s", title, res.returncode, delivery)
    if ok and delivery == "submitted":
        return f"✅ Sent to {title} (delivery: submitted)"

    # Not confirmed submitted: inspect the composer before any rescue.
    parsed = _capture(ctx, sid, profile)
    if parsed is None:
        if ok:
            return f"⚠️ {title}: sent but delivery unverified ({delivery}); could not read the screen."
        err = (res.stderr or "").strip().splitlines()[-1:] or ["unknown error"]
        return f"❌ Send to {title} failed ({delivery}): {err[0]}"
    _, composer = parsed
    if not composer:
        if ok:
            return f"⚠️ {title}: sent, delivery unverified ({delivery}); composer is empty (probably submitted)."
        err = (res.stderr or "").strip().splitlines()[-1:] or ["unknown error"]
        return f"❌ Send to {title} failed ({delivery}): {err[0]}"
    if not composer_is_ours(composer, message):
        return (f"⚠️ {title}: message not submitted (delivery: {delivery}) and the composer holds "
                f"other text ({len(composer)} chars — possibly an operator draft or a collapsed paste). "
                f"Not touching Enter.")
    tmux_target = sess.get("tmux_session")
    if not tmux_target:
        return f"⚠️ {title}: left in composer (delivery: {delivery}); no tmux_session to rescue."
    # Re-capture right before Enter to shrink the check/use window.
    parsed = _capture(ctx, sid, profile)
    if parsed is None or not composer_is_ours(parsed[1], message):
        return f"⚠️ {title}: composer changed before rescue; not touching Enter (delivery: {delivery})."
    try:
        tm = subprocess.run(["tmux", "-L", tmux_socket_name(ctx), "send-keys", "-t", tmux_target, "Enter"],
                            capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError) as exc:
        return f"⚠️ {title}: rescue Enter failed ({exc.__class__.__name__}); message left in composer."
    if tm.returncode != 0:
        return f"⚠️ {title}: rescue Enter failed (tmux rc={tm.returncode}); message left in composer."
    time.sleep(1.5)
    parsed = _capture(ctx, sid, profile)
    if parsed is None:
        return f"⚠️ {title}: rescue Enter sent but the screen could not be re-read (delivery unverified)."
    if composer_is_ours(parsed[1], message):
        return f"⚠️ {title}: still in composer after rescue Enter (delivery: {delivery})."
    return f"✅ Sent to {title} (rescue Enter applied after {delivery})"


def do_peek(ctx, profile: str, sess: dict) -> str:
    sid = sess.get("id") or sess.get("title")
    title = html.escape(str(sess.get("title", "?")))
    if sess.get("status") == "stopped":
        return f"⚫ {title} isn't running — nothing to peek."
    res = ctx["run_cli"]("session", "output", "--pane", "-q", "--", sid, profile=profile, timeout=30)
    if res.returncode != 0 or not (res.stdout or "").strip():
        return f"\U0001f6ab {title}: no live screen."
    return render_peek(sess.get("title", "?"), res.stdout)


# ---------------------------------------------------------------- handlers
def register(dp, ctx: dict, is_authorized) -> None:
    """ctx: bridge globals (run_cli, get_sessions_list_all, get_unique_profiles,
    get_default_conductor, conductor_session_title, split_message, log,
    resolve_config_path, get_conductor_names)."""
    log = ctx["log"]

    async def _in_thread(fn, *a):
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(None, functools.partial(fn, *a))

    async def _reply_long(message, text: str, **kw):
        for chunk in ctx["split_message"](text):
            await message.answer(chunk, **kw)

    def _args(message) -> str:
        parts = (message.text or "").split(None, 1)
        return parts[1].strip() if len(parts) > 1 else ""

    def guarded(fn):
        """Never let an overlay bug leave a command unanswered / unlogged."""
        @functools.wraps(fn)
        async def wrapper(message):
            if not is_authorized(message):
                return
            try:
                await fn(message)
            except Exception as exc:  # noqa: BLE001
                log.exception("overlay %s failed", fn.__name__)
                await message.answer(f"⚠️ overlay {fn.__name__} failed: {exc.__class__.__name__}: {exc}")
        return wrapper

    @guarded
    async def cmd_agents(message):
        only = _args(message) or None
        sessions = await _in_thread(active_sessions, ctx)
        multi = len(ctx["get_unique_profiles"]()) > 1
        await _reply_long(message, format_agents(sessions, multi, only))

    @guarded
    async def cmd_peek(message):
        query = _args(message)
        sessions = await _in_thread(active_sessions, ctx)
        if not query:
            default = ctx["get_default_conductor"]()
            query = ctx["conductor_session_title"](default["name"]) if default else ""
        match, err = resolve_session(sessions, query, usage="Usage: /peek <session> (no default conductor found)")
        if err:
            await message.answer(err)
            return
        profile, sess = match
        text = await _in_thread(do_peek, ctx, profile, sess)
        await message.answer(text, parse_mode="HTML")

    @guarded
    async def cmd_send(message):
        split = split_send_args(_args(message))
        if split is None:
            await message.answer(SEND_USAGE)
            return
        query, body = split
        sessions = await _in_thread(active_sessions, ctx)
        match, err = resolve_session(sessions, query)
        if err:
            await message.answer(err)
            return
        profile, sess = match
        if sess.get("status") == "stopped":
            await message.answer(f"⚫ {sess.get('title')} is stopped; start it first.")
            return
        report = await _in_thread(do_send, ctx, profile, sess, body)
        await message.answer(report)

    @guarded
    async def cmd_help(message):
        names = []
        try:
            names = list(ctx["get_conductor_names"]())
        except Exception:  # noqa: BLE001
            pass
        await message.answer(
            "Conductor commands:\n"
            "/agents [group]  - Active sessions by group (alias: /sessions)\n"
            "/peek [session]  - Screen snapshot (input box stripped)\n"
            "/send <session> <msg> - Deliver a message (verified)\n"
            "   session = exact title, prefix, group:prefix, or \"title with spaces\"\n"
            "/status    - Aggregated status across all profiles\n"
            "/restart   - Restart a conductor (specify name)\n"
            "/help      - This message\n\n"
            f"Conductors: {', '.join(names) if names else 'none'}\n"
            "Route: <name>: <message>  (default: first conductor)"
        )

    # Build the full list first, then register: all-or-nothing.
    handlers = [
        (cmd_agents, Command("agents", "sessions")),
        (cmd_peek, Command("peek")),
        (cmd_send, Command("send")),
        (cmd_help, Command("help")),
    ]
    for fn, flt in handlers:
        dp.message.register(fn, flt)
    log.info("overlay: registered /agents /sessions /peek /send /help")
