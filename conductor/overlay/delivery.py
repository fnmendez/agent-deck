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


def baseline_count(ctx, sid: str, profile, message: str) -> int:
    """Occurrences of the message's needle BEFORE sending.

    A pre-send baseline is what makes a short message ("ok") decidable: the
    verdict is that the count *grew*, not that the text appears somewhere.
    """
    parsed = capture(ctx, sid, profile)
    if parsed is None:
        return 0
    return count_token(parsed[0], delivery_token(message))


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


def resolve_truth(ctx, sid: str, profile, message: str, baseline: int):
    """Decide what actually happened to `message`. Returns (truth, composer).

    Order matters: the composer is checked first because a body still sitting
    there is the one case that is provably NOT submitted and is the only case a
    rescue Enter may touch.
    """
    parsed = capture(ctx, sid, profile)
    if parsed is None:
        return UNKNOWN, ""
    output, composer = parsed
    if composer_is_ours(composer, message):
        return IN_COMPOSER, composer
    if count_token(output, delivery_token(message)) > baseline:
        return DELIVERED, composer
    status = ctx["get_session_status"](sid, profile=profile)
    if status in BUSY_STATUSES:
        # The composer is clean and the agent started working: it took the body
        # even though the transcript has not rendered it yet.
        return DELIVERED, composer
    return ABSENT, composer
