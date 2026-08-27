# Conductor overlay (local customizations that survive `agent-deck update`)

## Where this lives

This directory is the **versioned source**. The overlay actually runs from the
conductor data dir, which `agent-deck update` and `conductor setup` regenerate:

```
conductor/overlay/                                 # this repo — canonical source
~/.local/share/agent-deck/conductor/overlay/       # deployed copy (what runs)
```

Install or update the deployed copy, then re-apply:

```sh
mkdir -p ~/.local/share/agent-deck/conductor/overlay
cp -R conductor/overlay/. ~/.local/share/agent-deck/conductor/overlay/
~/.local/share/agent-deck/conductor/overlay/reapply.sh --dry-run
~/.local/share/agent-deck/conductor/overlay/reapply.sh
```

The trailing `/.` copies the directory *contents*: plain `cp -R conductor/overlay/ <dest>/overlay/`
would nest a second `overlay/` inside an existing destination and leave the live copy stale.

`reapply.sh` restarts the bridge only when something actually changed (it tracks
the applied `bridge_local.py` sha in `.applied-sha`), so the copy above is safe to
repeat. Runtime files (`.applied-sha`, `__pycache__`) are not versioned.

Everything agent-deck regenerates on `update` / `conductor setup` is re-applied
from here with one idempotent command:

    ~/.local/share/agent-deck/conductor/overlay/reapply.sh [--dry-run] [--rollback]

What it re-applies (each step is a no-op when already in place):
0. Preflight: bridge-venv python, plist, and `bridge_local.py` must exist; the module
   must compile and import under the venv — otherwise nothing is touched.
1. `bridge.py` — inserts a 7-line `# overlay-hook` (exactly once) before the stock
   `/sessions` handler; written atomically (tmp + compile + rename; backup
   `bridge.py.pre-overlay`). Fails loudly if the anchor `@dp.message(Command("sessions"))`
   is missing/duplicated upstream. A changed `bridge_local.py` (sha in `.applied-sha`)
   also triggers a restart.
2. `<conductor>/.claude/settings.json` — `defaultMode: bypassPermissions`,
   `Edit(//<dir>/**)` file rule, no `ask`/`deny`, no legacy `Write()` rules.
   Custom top-level keys are kept; the previous file is saved as `settings.json.bak-<ts>`.
3. LaunchAgent plist — python interpreter = `~/.local/share/agent-deck/bridge-venv/bin/python`.
4. Restarts the bridge only if something changed (`launchctl kickstart -k`, or
   bootout/bootstrap when the plist changed) and verifies: new pid, `overlay: registered`
   in the lines appended to bridge.log after the restart, pid stable 3 s later, and no
   `overlay: bridge_local failed`/`Traceback` after it.

`--rollback` restores `bridge.py.pre-overlay` (== the binary's embedded bridge) and restarts
(verifying `Run polling for bot`). It refuses when bridge.py carries no hook, so it can never
downgrade a freshly updated bridge.

## After updating agent-deck
    agent-deck update            # or: agent-deck conductor setup slavna ...
    ~/.local/share/agent-deck/conductor/overlay/reapply.sh --dry-run
    ~/.local/share/agent-deck/conductor/overlay/reapply.sh

## Telegram commands added by `bridge_local.py`
| Command | Behaviour |
|---|---|
| `/agents [group]` (alias `/sessions`) | non-archived sessions, grouped by agent-deck group, `🟢 running 🟡 waiting ⚪ idle 🔴 error ⚫ stopped` |
| `/peek [session]` | one `session output --pane` snapshot, input box stripped, `<pre>`, ≤4 KB (default: the conductor) |
| `/send <session> <msg>` | `session send --no-wait --json --timeout 30s -- <id> <msg>` (flags first, `--` separator: Telegram text can never become a CLI option). `delivery: submitted` = ✅. Otherwise reads the composer: exactly our text (or a ≥60-char prefix of it) → re-capture, then one rescue `tmux send-keys Enter`, then re-check; anything else → ⚠️ (length only, never echoed) and Enter is NOT pressed; unverified delivery is reported ⚠️, never ✅ |
| `/help` | stock help + the above |

Session matching: exact title → case-insensitive `startswith`; `group:prefix`
restricts to a group; `"title with spaces"` may be quoted; ambiguity returns the candidate
list; archived sessions never match. `/peek` refuses screens where no `❯`/`›` input box can
be located (shell sessions) so an operator draft is never exposed.

Accepted residual risk (decision 2026-08-26, ops-main/Franco): the capture→Enter window of the
rescue Enter is not atomic through the agent-deck CLI; it is mitigated by an immediate
re-capture before Enter and the strict ownership rule above. Diamond review record:
`diamond-2026-08-26.md` in this directory.

Tests (no Telegram/agent-deck needed):
    cd ~/.local/share/agent-deck/conductor/overlay && ~/.local/share/agent-deck/bridge-venv/bin/python -m unittest tests.test_bridge_local

## Morning checklist (Franco, from Telegram)
1. `/agents` → grouped list without archived sessions (`gsd` must be absent).
2. `/peek ops-main` → screen snapshot without the `❯` input box.
3. `/send ops-main hola desde telegram` → `✅ Sent to ops-main (delivery: submitted)`; try `/send ops x` → ambiguity list.
