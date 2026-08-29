"""Durable delivery truth for agent-deck sends.

`agent-deck session send` exits non-zero with ``delivery=typed`` when the body
reached the pane but submission could not be confirmed (session_cmd.go). That
verdict is *ambiguous*, not a failure: the agent very often did take the message
and is already answering. Reporting it as a failure makes the bridge tell the
user "not delivered" about a message that was delivered, and any retry built on
that verdict delivers the body a second time.

This module answers the only question that matters after an ambiguous send —
*did the agent actually take it?* — from the session's own screen:

    baseline (before send) -> send -> observe

  * the body sits in the composer               -> IN_COMPOSER (never submitted)
  * the body appears in the transcript region   -> DELIVERED
  * the turn started (running/active)           -> DELIVERED
  * nothing anywhere                            -> ABSENT

Only IN_COMPOSER is safe to rescue with an Enter; DELIVERED must never be
retried, and ABSENT is a real failure the caller may resend.
"""

from __future__ import annotations

import json
import re

# Truth values.
DELIVERED = "delivered"
IN_COMPOSER = "in_composer"
ABSENT = "absent"
UNKNOWN = "unknown"

PROMPT_RE = re.compile(r"^\s*[❯›]")          # ❯ (claude) / › (codex)
RULE_RE = re.compile(r"^\s*[─━┄┈═\-]{8,}\s*$")
ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")
# Dim (ghost/placeholder) runs: opened by SGR 2 (alone or combined), closed by any SGR.
DIM_RE = re.compile(r"\x1b\[2(?:;[0-9;]*)?m.*?(?=\x1b\[[0-9;]*m|$)", re.S)

# A composer prefix counts as "ours" only when long enough that a coincidental
# operator draft is implausible.
PREFIX_MIN_LEN = 60
# Panes wrap and indent, so the needle must be short enough to survive wrapping
# once whitespace is collapsed, yet long enough to be distinctive.
TOKEN_LEN = 48
TOKEN_MIN_LEN = 8

BUSY_STATUSES = ("running", "active", "starting")


def strip_ansi(text: str, drop_dim: bool = False) -> str:
    if drop_dim:
        text = DIM_RE.sub("", text)
    return ANSI_RE.sub("", text)


def norm(text: str) -> str:
    """Collapse whitespace so pane wrapping and indentation stop mattering."""
    return re.sub(r"\s+", " ", text).strip()


def split_pane(raw: str):
    """(output_lines, composer_text), or None when no input box can be located.

    Returning None is a refusal, not an empty result: callers must fail closed
    rather than treat a whole unparsed screen as transcript.
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
    for line in lines[idx + 1:]:
        if RULE_RE.match(line) or not line.strip():
            break
        composer.append(line.strip())
    cut = idx
    if idx > 0 and RULE_RE.match(lines[idx - 1]):
        cut = idx - 1
    output = [line.rstrip() for line in lines[:cut]]
    while output and not output[-1].strip():
        output.pop()
    return output, " ".join(part for part in composer if part).strip()


def composer_is_ours(composer: str, message: str) -> bool:
    """Exact normalized match, or a long prefix of it (a truncated capture)."""
    current, sent = norm(composer), norm(message)
    if not current:
        return False
    return current == sent or (
        len(current) >= PREFIX_MIN_LEN and sent.startswith(current)
    )


def delivery_token(message: str) -> str:
    """A short, distinctive, wrap-proof needle taken from the message.

    Prefers the longest line so a boilerplate first line ("hola") does not
    become the needle when the body carries something more specific.
    """
    lines = [norm(line) for line in message.splitlines()]
    lines = [line for line in lines if line]
    if not lines:
        return ""
    needle = max(lines, key=len)
    return needle[:TOKEN_LEN]


def count_token(output_lines, token: str) -> int:
    """Occurrences of `token` in the transcript, immune to pane wrapping."""
    if not token:
        return 0
    return norm("\n".join(output_lines)).count(token)


def capture(ctx, sid: str, profile):
    """Pane split for a session, or None when it cannot be read or parsed."""
    result = ctx["run_cli"](
        "session", "output", "--pane", "-q", "--", sid, profile=profile, timeout=30
    )
    if result.returncode != 0 or not (result.stdout or "").strip():
        return None
    return split_pane(result.stdout)


def baseline(ctx, sid: str, profile, message: str):
    """State BEFORE sending: (needle count, was the session already busy).

    A pre-send baseline is what makes a short message ("ok") decidable — the
    verdict is that the count *grew*, not that the text appears somewhere — and
    the busy flag is what stops an unrelated turn that was already running from
    being read as proof that our message started it.
    """
    parsed = capture(ctx, sid, profile)
    count = 0 if parsed is None else count_token(parsed[0], delivery_token(message))
    try:
        busy = ctx["get_session_status"](sid, profile=profile) in BUSY_STATUSES
    except Exception:  # noqa: BLE001 - a status probe must never break a send
        # Unknown, not idle: assuming idle here would let a post-send busy
        # status stand in as proof of delivery for a send that never landed.
        busy = None
    return count, busy


def baseline_count(ctx, sid: str, profile, message: str) -> int:
    """Backwards-compatible count-only baseline."""
    return baseline(ctx, sid, profile, message)[0]


def parse_send_result(result):
    """(ok, delivery) from a `session send --json` CompletedProcess."""
    delivery, ok = "?", result.returncode == 0
    try:
        data = json.loads(result.stdout or "{}")
        delivery = str(data.get("delivery", delivery))
        if data.get("submitted") is True:
            ok = True
        elif "success" in data:
            ok = bool(data.get("success"))
    except (json.JSONDecodeError, TypeError):
        pass
    return ok, delivery


def resolve_truth(ctx, sid: str, profile, message: str, baseline, was_busy=False):
    """Decide what actually happened to `message`. Returns (truth, composer).

    `baseline` is either the pre-send needle count or the (count, was_busy) pair
    from `baseline()`. Order matters: the composer is checked first because a
    body still sitting there is the one case that is provably NOT submitted and
    the only case a rescue Enter may touch.
    """
    if isinstance(baseline, tuple):
        baseline, was_busy = baseline
    parsed = capture(ctx, sid, profile)
    if parsed is None:
        return UNKNOWN, ""
    output, composer = parsed
    if composer_is_ours(composer, message):
        return IN_COMPOSER, composer
    if count_token(output, delivery_token(message)) > baseline:
        return DELIVERED, composer
    if was_busy is not False:
        # Either it was already working before we sent, or the pre-send probe
        # failed and we cannot say. Both make a busy status now worthless as
        # evidence about *our* message, and a guess here would report a lost
        # message as delivered.
        return UNKNOWN, composer
    try:
        status = ctx["get_session_status"](sid, profile=profile)
    except Exception:  # noqa: BLE001 - a failed probe is ambiguity, not a crash
        return UNKNOWN, composer
    if status in BUSY_STATUSES:
        # It was positively idle before and is working now with a clean
        # composer: it took the body, the transcript just has not rendered it.
        return DELIVERED, composer
    return ABSENT, composer


# --------------------------------------------------------------- reply freshness
# Lines the agents draw around a turn: spinners, timers, tool markers. They are
# chrome, not the answer.
CHROME_RE = re.compile(r"^\s*(?:[✻✳✽·⎿⏺•]|\[STATUS\]\s*$|Working\b|Esc to interrupt)")
STATUS_TAIL_RE = re.compile(r"^\s*(?:[✻✳✽]\s|.*·\s*done\s|\s*$)")
FRESHNESS_PROBE = 40


def pane_reply_after(output_lines, message: str) -> str:
    """Whatever the agent printed after our message appeared in the transcript.

    The pane is the only place that shows ordering, so it is what tells a fresh
    answer from an older one that a stale cache may still be serving.
    """
    token = delivery_token(message)
    if not token:
        return ""
    index = None
    for position, line in enumerate(output_lines):
        if token in norm(line):
            index = position
    if index is None:
        return ""
    reply = []
    for line in output_lines[index + 1:]:
        stripped = line.strip()
        if not stripped:
            continue
        if STATUS_TAIL_RE.match(line) and reply:
            break                      # the turn's closing timer ends the answer
        if CHROME_RE.match(line):
            cleaned = re.sub(r"^\s*[⎿⏺•]\s*", "", line).strip()
            if cleaned and not STATUS_TAIL_RE.match(line):
                reply.append(cleaned)
            continue
        reply.append(stripped)
    return "\n".join(reply).strip()


def reply_is_fresh(stock_reply: str, pane_reply: str, output_lines, message: str) -> bool:
    """True when the cached reply really belongs to the turn we just triggered."""
    if not stock_reply:
        return False
    probe = norm(stock_reply)[:FRESHNESS_PROBE]
    if not probe:
        return False
    if pane_reply and probe in norm(pane_reply):
        return True
    # The answer may have scrolled past the top of the pane; accept the cached
    # reply only when it appears after our own message, never before it.
    joined = norm("\n".join(output_lines))
    token = delivery_token(message)
    if not token or token not in joined:
        return False
    return probe in joined.split(token)[-1]


def fresh_reply(ctx, sid: str, profile, message: str, stock_reply: str):
    """(reply_text, source). Never returns a reply that predates `message`.

    A stale answer is worse than an honest gap: it reads as the conductor
    ignoring what was asked and answering something else entirely.
    """
    parsed = capture(ctx, sid, profile)
    if parsed is None:
        # Without the pane there is no way to tell a fresh reply from an old
        # one, and relaying a possibly-stale answer is the failure this exists
        # to prevent.
        return "", "none"
    output, _composer = parsed
    pane = pane_reply_after(output, message)
    if reply_is_fresh(stock_reply, pane, output, message):
        return stock_reply, "cached"
    if pane:
        return pane, "pane"
    return "", "none"
