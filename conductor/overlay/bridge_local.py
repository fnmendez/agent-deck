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
import os
import secrets
import shlex
import subprocess
import time
from pathlib import Path

try:
    from aiogram.filters import Command
    from aiogram import F
    from aiogram.types import InlineKeyboardButton, InlineKeyboardMarkup
except ImportError:  # tests without aiogram
    Command = None  # type: ignore
    F = None  # type: ignore
    InlineKeyboardButton = InlineKeyboardMarkup = None  # type: ignore

import media
import transcribe
from delivery import (  # shared truth resolution (same dir, added by the hook to sys.path)
    ABSENT,
    DELIVERED,
    IN_COMPOSER,
    UNKNOWN,
    baseline_count,
    capture,
    composer_is_ours,
    delivery_token,
    fresh_reply,
    norm,
    parse_send_result,
    resolve_truth,
    split_pane,
    strip_ansi,
)

STATUS_ICON = {
    "running": "\U0001f7e2", "waiting": "\U0001f7e1", "idle": "⚪",
    "error": "\U0001f534", "stopped": "⚫",
}
TG_LIMIT = 4096
PEEK_BUDGET = 3800
SEND_USAGE = "Usage: /send <session|group:session|\"title with spaces\"> <message>"


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
def rescue_enter(ctx, sess: dict, message: str) -> tuple[bool, str]:
    """Press Enter for a body that is provably still sitting in the composer.

    Re-captures immediately before the keystroke to keep the check/use window as
    small as this interface allows, and refuses the moment the composer stops
    matching what we sent.
    """
    sid = sess.get("id") or sess.get("title")
    profile = sess.get("profile")
    target = sess.get("tmux_session")
    if not target:
        return False, "no tmux_session to rescue"
    parsed = capture(ctx, sid, profile)
    if parsed is None or not composer_is_ours(parsed[1], message):
        return False, "composer changed before rescue"
    try:
        result = subprocess.run(
            ["tmux", "-L", tmux_socket_name(ctx), "send-keys", "-t", target, "Enter"],
            capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        return False, "rescue Enter failed (%s)" % exc.__class__.__name__
    if result.returncode != 0:
        return False, "rescue Enter failed (tmux rc=%d)" % result.returncode
    return True, "rescue Enter sent"


def send_with_truth(ctx, profile, sess: dict, message: str, extra_args=()):
    """Send, then settle what actually happened. Returns (truth, delivery, detail).

    Never retries a send: an ambiguous CLI verdict is resolved by looking at the
    session, and the only corrective action taken is an Enter for a body proven
    to be still unsent in the composer.
    """
    run_cli = ctx["run_cli"]
    log = ctx["log"]
    sid = sess.get("id") or sess.get("title")
    title = sess.get("title")

    base = baseline_count(ctx, sid, profile, message)
    # Flags first, then "--": message text can never be parsed as a CLI option.
    args = ["session", "send", "--no-wait", "--json", "--timeout", "30s"]
    args.extend(extra_args)
    args.extend(["--", sid, message])
    result = run_cli(*args, profile=profile, timeout=60)
    ok, delivery = parse_send_result(result)
    if ok and delivery == "submitted":
        log.info("overlay send -> %s: delivery=submitted", title)
        return DELIVERED, delivery, "submitted"

    truth, _composer = resolve_truth(ctx, sid, profile, message, base)
    detail = ""
    if truth == IN_COMPOSER:
        rescued, detail = rescue_enter(ctx, dict(sess, profile=profile), message)
        if rescued:
            truth = DELIVERED
    log.info(
        "overlay send -> %s: cli delivery=%s rc=%s -> truth=%s %s",
        title, delivery, result.returncode, truth, detail,
    )
    if truth == ABSENT and not detail:
        stderr = (result.stderr or "").strip().splitlines()[-1:]
        detail = stderr[0] if stderr else "no evidence the message reached the session"
    return truth, delivery, detail


def do_send(ctx, profile, sess: dict, message: str) -> str:
    """User-facing /send report: says what is true, and never resends."""
    title = sess.get("title")
    truth, delivery, detail = send_with_truth(ctx, profile, sess, message)
    if truth == DELIVERED:
        if delivery == "submitted":
            return "✅ Sent to %s (delivery: submitted)" % title
        return "✅ Delivered to %s — the CLI could not confirm it (delivery: %s), the session shows it arrived%s" % (
            title, delivery, "; " + detail if detail else "",
        )
    if truth == IN_COMPOSER:
        return "⚠️ %s: your message is sitting unsent in the composer (delivery: %s) and could not be submitted%s. Nothing was resent." % (
            title, delivery, "; " + detail if detail else "",
        )
    if truth == UNKNOWN:
        return "⚠️ %s: could not read the session screen, so delivery is unverified (delivery: %s). Nothing was resent — check the pane before retrying." % (
            title, delivery,
        )
    return "❌ Not delivered to %s (delivery: %s): %s" % (title, delivery, detail)


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
# ------------------------------------------------------------- media ingestion
def conductor_dir(ctx):
    """The deployed conductor's data dir (where inbound files are stored)."""
    return ctx.get("CONDUCTOR_DIR") or Path.home() / ".local/share/agent-deck/conductor"


def pick_image(message):
    """(file_obj, mime, name, size, meta) for a photo or an image document."""
    photos = getattr(message, "photo", None)
    if photos:
        best = photos[-1]                      # Telegram sorts smallest-first
        return best, "image/jpeg", "photo.jpg", getattr(best, "file_size", None), {
            "width": getattr(best, "width", None), "height": getattr(best, "height", None),
        }
    doc = getattr(message, "document", None)
    if doc is not None:
        mime = (getattr(doc, "mime_type", "") or "").lower()
        if not mime.startswith("image/"):
            raise media.MediaRejected(
                "that document is %s; this bridge accepts images and voice notes"
                % (mime or "an unknown type")
            )
        return doc, mime, getattr(doc, "file_name", "image"), getattr(doc, "file_size", None), {}
    raise media.MediaRejected("no image found in that message")


def pick_audio(message):
    """(file_obj, mime, name, size, meta) for a voice note or an audio file."""
    for attr, default_mime in (("voice", "audio/ogg"), ("audio", "audio/mpeg")):
        obj = getattr(message, attr, None)
        if obj is not None:
            return (
                obj,
                (getattr(obj, "mime_type", "") or default_mime).lower(),
                getattr(obj, "file_name", None) or "%s" % attr,
                getattr(obj, "file_size", None),
                {"duration": getattr(obj, "duration", None)},
            )
    doc = getattr(message, "document", None)
    if doc is not None and (getattr(doc, "mime_type", "") or "").lower().startswith("audio/"):
        return (doc, doc.mime_type.lower(), getattr(doc, "file_name", "audio"),
                getattr(doc, "file_size", None), {})
    raise media.MediaRejected("no audio found in that message")


def media_target(ctx, kind: str, extension: str, stamp=None, token=None) -> Path:
    """Absolute destination path for an inbound attachment."""
    stamp = stamp or time.strftime("%Y-%m-%d")
    directory = media.media_dir(conductor_dir(ctx), kind, stamp)
    name = "%s-%s%s" % (time.strftime("%H%M%S"), token or secrets.token_hex(3), extension)
    return directory / name


# --------------------------------------------------------------- peek refresh
# token -> the exact target a refresh is allowed to act on. A callback carries
# only an opaque token: the session identity is re-read from this record and
# re-validated against the live session list on every press, so a replayed or
# hand-crafted callback cannot redirect a refresh at another session.
PEEK_TOKENS: dict[str, dict] = {}
PEEK_TOKEN_TTL = 30 * 60
PEEK_TOKEN_MAX = 64


def _prune_peek_tokens(now=None):
    now = now if now is not None else time.time()
    for token, record in list(PEEK_TOKENS.items()):
        if now - record["created_at"] > PEEK_TOKEN_TTL:
            del PEEK_TOKENS[token]
    while len(PEEK_TOKENS) > PEEK_TOKEN_MAX:
        oldest = min(PEEK_TOKENS, key=lambda t: PEEK_TOKENS[t]["created_at"])
        del PEEK_TOKENS[oldest]


def issue_peek_token(sess: dict, profile, user_id: int, now=None) -> str:
    token = secrets.token_urlsafe(9)
    PEEK_TOKENS[token] = {
        "session_id": sess.get("id"),
        "title": sess.get("title"),
        "profile": profile,
        "user_id": user_id,
        "created_at": now if now is not None else time.time(),
    }
    # Prune after inserting so the store is bounded including the new token.
    _prune_peek_tokens(now)
    return token


def validate_peek_callback(ctx, token: str, user_id: int, now=None):
    """(session, profile) for a refresh press, or (None, reason).

    Rejects unknown/expired tokens, a different Telegram user, and any session
    whose identity no longer matches the one the button was issued for.
    """
    now = now if now is not None else time.time()
    record = PEEK_TOKENS.get(token)
    if record is None:
        return None, "this button is stale — run /peek again"
    if now - record["created_at"] > PEEK_TOKEN_TTL:
        del PEEK_TOKENS[token]
        return None, "this button expired — run /peek again"
    if record["user_id"] != user_id:
        return None, "not your button"
    for profile, sess in active_sessions(ctx):
        if sess.get("id") == record["session_id"] and sess.get("title") == record["title"]:
            if profile != record["profile"]:
                return None, "session moved profile — run /peek again"
            return dict(sess, profile=profile), None
    return None, "that session is gone — run /peek again"


def peek_keyboard(token: str):
    """Inline keyboard with the refresh button, or None without aiogram."""
    if InlineKeyboardMarkup is None or InlineKeyboardButton is None:
        return None
    return InlineKeyboardMarkup(inline_keyboard=[[
        InlineKeyboardButton(text="🔄 Refresh", callback_data="pk:%s" % token)
    ]])


def make_send_to_conductor(ctx):
    """A send_to_conductor that reports delivery truth instead of the CLI verdict.

    Same signature and return contract as the stock function
    ``(success, response_text, still_running)``; only the ambiguous branch
    changes. The stock version treats every non-zero exit as a failure unless
    stderr mentions a still-running turn, so ``delivery=typed`` — the body
    reached the pane but submission could not be confirmed — was reported to the
    user as "not delivered" for messages the conductor had in fact taken and
    answered. That is the conductor-not-answering symptom.

    ``still_running=True`` is the contract's own "delivered, await the reply
    asynchronously" signal, so a message proven delivered rides that path rather
    than being resent.
    """
    run_cli = ctx["run_cli"]
    log = ctx["log"]
    RESPONSE_TIMEOUT = ctx["RESPONSE_TIMEOUT"]

    def _session(session: str, profile):
        """Minimal session dict for the truth helpers (title doubles as id)."""
        for _p, item in active_sessions(ctx):
            if item.get("title") == session:
                return dict(item, profile=profile)
        return {"id": session, "title": session, "profile": profile}

    def send_to_conductor(
        session, message, profile=None, wait_for_reply=False,
        response_timeout=RESPONSE_TIMEOUT, reply_callback=None, force_queue=False,
    ):
        enqueue = ctx["_enqueue_message"]
        get_status = ctx["get_session_status"]

        if not wait_for_reply:
            if force_queue:
                log.info("Conductor %s: force-queueing message", session)
                enqueue(session, message, profile, reply_callback)
                return True, "", False
            status = get_status(session, profile=profile)
            if status in ("running", "active", "starting"):
                log.info("Conductor %s is busy (%s), queueing message", session, status)
                enqueue(session, message, profile, reply_callback)
                return True, "", False
            sess = _session(session, profile)
            base = baseline_count(ctx, sess.get("id") or session, profile, message)
            result = run_cli(
                "session", "send", "--no-wait", "--json", "--timeout", "30s",
                "--", sess.get("id") or session, message,
                profile=profile, timeout=60,
            )
            ok, delivery = parse_send_result(result)
            if ok and delivery == "submitted":
                return True, "", False
            truth, _c = resolve_truth(ctx, sess.get("id") or session, profile, message, base)
            if truth == IN_COMPOSER:
                rescued, detail = rescue_enter(ctx, sess, message)
                log.info("Conductor %s: composer rescue -> %s (%s)", session, rescued, detail)
                if rescued:
                    return True, "", False
            if truth == DELIVERED:
                log.info(
                    "Conductor %s: CLI said delivery=%s but the session shows the "
                    "message arrived; not resending", session, delivery,
                )
                return True, "", False
            stderr = (result.stderr or "").strip()
            if truth == ABSENT and ("timeout" in stderr.lower() or "not ready" in stderr.lower()):
                log.info("Conductor %s became busy during send, queueing message", session)
                enqueue(session, message, profile, reply_callback)
                return True, "", False
            log.error(
                "Failed to send to conductor %s (delivery=%s truth=%s): %s",
                session, delivery, truth, stderr,
            )
            return False, "", False

        # Blocking path: heartbeats and the idle user-message flow.
        sess = _session(session, profile)
        sid = sess.get("id") or session
        base = baseline_count(ctx, sid, profile, message)
        result = run_cli(
            "session", "send", "--wait", "--timeout", "%ss" % response_timeout, "-q",
            "--", sid, message,
            profile=profile, timeout=max(response_timeout + 30, 60),
        )
        if result.returncode == 0:
            stock = ctx["get_session_output"](session, profile=profile)
            text, source = fresh_reply(ctx, sid, profile, message, stock)
            if source == "none":
                # A stale answer reads as the conductor ignoring the question and
                # replying to something else. Await the real one instead.
                log.info(
                    "Conductor %s: no reply newer than the message yet; awaiting it "
                    "rather than returning a stale one", session,
                )
                return False, "", True
            if source == "pane":
                log.info("Conductor %s: cached reply was stale, using the pane reply", session)
            return True, text, False

        stderr = (result.stderr or "").strip()
        if ctx["_is_still_running_timeout"](stderr):
            log.info(
                "Conductor %s: --wait timed out but agent still running "
                "(message delivered, reply pending)", session,
            )
            return False, "", True

        truth, _c = resolve_truth(ctx, sid, profile, message, base)
        if truth == IN_COMPOSER:
            rescued, detail = rescue_enter(ctx, sess, message)
            log.info("Conductor %s: composer rescue -> %s (%s)", session, rescued, detail)
            if rescued:
                truth = DELIVERED
        if truth == DELIVERED:
            log.info(
                "Conductor %s: submission was never confirmed but the session shows "
                "the message arrived; awaiting the reply instead of failing", session,
            )
            return False, "", True
        if truth == UNKNOWN:
            # Could not read the screen. Never resend on a guess: await the reply
            # and let the pending-reply watcher time out if nothing comes.
            log.warning(
                "Conductor %s: delivery unverifiable (screen unreadable); "
                "awaiting a reply rather than resending", session,
            )
            return False, "", True
        log.error("Failed to send to conductor %s (truth=%s): %s", session, truth, stderr)
        return False, "", False

    return send_to_conductor


def install_conductor_send(ctx) -> bool:
    """Rebind the bridge's module-level send_to_conductor to the truthful one.

    ``ctx`` is the bridge module's own globals(), so rebinding the name here is
    what every later call site resolves. Verified rather than assumed: a copied
    mapping would make this a silent no-op.
    """
    log = ctx["log"]
    required = (
        "run_cli", "get_session_status", "get_session_output",
        "_enqueue_message", "_is_still_running_timeout", "RESPONSE_TIMEOUT",
        "send_to_conductor",
    )
    missing = [name for name in required if name not in ctx]
    if missing:
        log.error("overlay: cannot install truthful send_to_conductor, missing %s", missing)
        return False
    patched = make_send_to_conductor(ctx)
    ctx["send_to_conductor"] = patched
    if ctx.get("send_to_conductor") is not patched:
        log.error("overlay: send_to_conductor rebind did not take effect")
        return False
    log.info("overlay: send_to_conductor now reports durable delivery truth")
    return True


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
        log.info("overlay /agents%s: %d active sessions", " " + only if only else "", len(sessions))
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
        log.info("overlay /peek %s", sess.get("title"))
        text = await _in_thread(do_peek, ctx, profile, sess)
        token = issue_peek_token(sess, profile, message.from_user.id)
        await message.answer(text, parse_mode="HTML", reply_markup=peek_keyboard(token))

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

    async def _route_to_conductor(message, body: str) -> str:
        """Hand data to the default conductor, reusing the bridge's own transport."""
        conductor = ctx["get_default_conductor"]()
        if conductor is None:
            return "[No conductors configured.]"
        profile = conductor["profile"]
        title = ctx["conductor_session_title"](conductor["name"])
        ok, _text, pending = await _in_thread(
            functools.partial(ctx["send_to_conductor"], title, body, profile=profile)
        )
        if ok or pending:
            return "→ handed to %s" % title
        return "⚠️ could not hand it to %s — it is saved on disk" % title

    def _is_audio_document(message) -> bool:
        doc = getattr(message, "document", None)
        mime = (getattr(doc, "mime_type", "") or "").lower() if doc is not None else ""
        return mime.startswith("audio/") or mime in media.AUDIO_EXTENSIONS

    async def on_photo(message):
        if not is_authorized(message):
            return
        if _is_audio_document(message):
            # A voice note forwarded as a file still belongs to the audio path.
            await on_audio(message)
            return
        try:
            file_obj, mime, name, size, meta = pick_image(message)
            media.check_size(size, media.MAX_IMAGE_BYTES, "that image")
        except media.MediaRejected as exc:
            await message.answer("⚠️ %s" % exc)
            return
        extension = media.safe_extension(mime, name, media.IMAGE_EXTENSIONS, ".jpg")
        target = media_target(ctx, "images", extension)
        try:
            await message.bot.download(file_obj, destination=str(target))
        except Exception as exc:  # noqa: BLE001 - surface, never crash the bridge
            log.exception("overlay: image download failed")
            await message.answer("⚠️ could not download that image: %s" % exc.__class__.__name__)
            return
        meta["size"] = target.stat().st_size
        if meta["size"] > media.MAX_IMAGE_BYTES:
            target.unlink(missing_ok=True)
            await message.answer("⚠️ that image is over the size limit for this bridge")
            return
        body = media.describe_image(target, getattr(message, "caption", "") or "", meta)
        log.info("overlay image: %s bytes -> %s", meta["size"], target)
        status = await _route_to_conductor(message, body)
        await message.answer("🖼 Image saved: %s\n%s" % (target, status))

    async def on_audio(message):
        if not is_authorized(message):
            return
        try:
            file_obj, mime, name, size, meta = pick_audio(message)
            media.check_size(size, media.MAX_AUDIO_BYTES, "that audio")
        except media.MediaRejected as exc:
            await message.answer("⚠️ %s" % exc)
            return
        extension = media.safe_extension(mime, name, media.AUDIO_EXTENSIONS, ".ogg")
        target = media_target(ctx, "audio", extension)
        try:
            await message.bot.download(file_obj, destination=str(target))
        except Exception as exc:  # noqa: BLE001
            log.exception("overlay: audio download failed")
            await message.answer("⚠️ could not download that audio: %s" % exc.__class__.__name__)
            return
        try:
            transcript = await _in_thread(
                functools.partial(
                    transcribe.transcribe_file, target,
                    workspace_root=conductor_dir(ctx) / "inbox" / "jobs",
                    lock_path=conductor_dir(ctx) / "inbox" / "stt.lock",
                )
            )
        except (transcribe.TranscriptionUnavailable, transcribe.JobRejected) as exc:
            log.warning("overlay audio: saved %s but not transcribed: %s", target, exc)
            await message.answer(
                "🎤 Voice note saved: %s\n"
                "⚠️ Not transcribed — %s\n"
                "The audio file is on disk; nothing was sent to any other service."
                % (target, exc)
            )
            return
        body = media.describe_audio(target, transcript, getattr(message, "caption", "") or "", meta)
        log.info("overlay audio: %s -> transcript of %d chars", target, len(transcript))
        status = await _route_to_conductor(message, body)
        await message.answer(
            "🎤 Voice note transcribed (%d chars): %s\n%s"
            % (len(transcript), target, status)
        )

    async def on_peek_refresh(callback):
        """Refresh a /peek snapshot in place, re-validating the target each press."""
        user = getattr(callback.from_user, "id", None)
        data = getattr(callback, "data", "") or ""
        if not data.startswith("pk:"):
            await callback.answer("unknown action")
            return
        sess, reason = await _in_thread(
            functools.partial(validate_peek_callback, ctx, data[3:], user)
        )
        if sess is None:
            log.info("overlay peek refresh refused: %s", reason)
            await callback.answer(reason, show_alert=True)
            return
        text = await _in_thread(do_peek, ctx, sess["profile"], sess)
        token = issue_peek_token(sess, sess["profile"], user)
        try:
            await callback.message.edit_text(
                text, parse_mode="HTML", reply_markup=peek_keyboard(token)
            )
        except Exception as exc:  # noqa: BLE001 - "message is not modified" et al.
            log.info("overlay: peek refresh edit skipped: %s", exc.__class__.__name__)
            await callback.answer("no change since the last snapshot")
            return
        await callback.answer("refreshed")

    # Build the full list first, then register: all-or-nothing.
    handlers = [
        (cmd_agents, Command("agents", "sessions")),
        (cmd_peek, Command("peek")),
        (cmd_send, Command("send")),
        (cmd_help, Command("help")),
    ]
    for fn, flt in handlers:
        dp.message.register(fn, flt)
    if F is not None:
        # Media handlers must precede the stock catch-all, which drops every
        # non-text message with `if not message.text: return`.
        dp.message.register(on_audio, F.voice | F.audio)
        dp.message.register(on_photo, F.photo | F.document)
        dp.callback_query.register(on_peek_refresh, F.data.startswith("pk:"))
    install_conductor_send(ctx)
    log.info(
        "overlay: registered /agents /sessions /peek /send /help + audio, image "
        "and peek-refresh handlers"
    )
