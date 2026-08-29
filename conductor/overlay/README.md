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

## What the overlay adds

| Area | Behaviour |
|---|---|
| Delivery truth | `agent-deck session send` exits non-zero with `delivery=typed` when the body reached the pane but submission could not be confirmed. That verdict is *ambiguous*, not a failure, and the stock bridge treated it as one — which is why the conductor could receive a message, answer it, and still report "not delivered". `delivery.py` settles it from the session's own screen (baseline → send → observe) and the overlay rebinds `send_to_conductor` to report that truth. A message proven delivered rides the reply-pending path instead of being resent, so an ambiguous verdict can never duplicate it. |
| Inbound images | Photos and image documents are size/type bounded, stored under `<conductor>/inbox/images/`, and handed to the conductor as an absolute path it can open with its file tools. |
| Inbound audio | Voice notes are bounded and stored under `<conductor>/inbox/audio/`. Transcription runs through `transcribe.py`, which **fails closed**: see the note below. |
| `/peek` refresh | The snapshot carries an inline 🔄 button that edits the same message in place. Every press re-validates the target (token, Telegram user, session id **and** title, profile) and refuses stale, expired or forged callbacks. |

### Transcription is deliberately fail-closed

The installed Superwhisper CLI cannot transcribe a file — it is a history/search
tool (`superultrainc/superwhisper-cli-release` v0.1.0), and its MCP server exposes
only history/vocab/snippets. Every documented file-transcription path runs through
the GUI app:

* [File transcription](https://superwhisper.com/docs/get-started/transcribe-files) documents
  the "Command Line Method" as `open /path/audio.mp3 -a superwhisper` — LaunchServices
  handing the file to the running app, using whatever mode is active.
* [Advanced settings](https://superwhisper.com/docs/get-started/settings-advanced) confirms the
  app writes the clipboard, auto-pastes and simulates keystrokes.
* [Voice models](https://superwhisper.com/docs/models/voice) lists `Ultra (Cloud)` as an app model.
* The only public API ([enterprise overview](https://superwhisper.com/docs/enterprise/api/overview),
  live spec at `https://api.superwhisper.com/api/v1/openapi.json`) has three read-only stats
  endpoints; the OpenAPI linked from the docs index is Mintlify's sample Plant Store, not a real API.
* The [docs index](https://superwhisper.com/docs/llms.txt) publishes no CLI or transcription API.

So `transcribe.py` detects the capability at runtime and refuses with that exact
reason rather than driving the GUI or silently substituting another engine —
sending the operator's audio to an unapproved service would be the worse outcome.
The day the vendor ships a transcribe verb, `detect_capability` starts returning
available with no code change. Meanwhile the audio path still saves the file and
tells the conductor it arrived.

`canary_isolation.py` proves the isolation on a real machine (`--simulate` runs a
live stand-in engine through the same guard):

```
PASS frontmost app unchanged          PASS live database untouched
PASS clipboard untouched              PASS no job workspace left behind
PASS same superwhisper pids           PASS no stand-in engine descendants left
PASS no superwhisper restart          PASS no extra superwhisper process
```

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
    cd conductor/overlay && python3 -m unittest tests.test_bridge_local \
        tests.test_media_and_peek tests.test_transcribe_isolation tests.test_reapply_contract

## Morning checklist (Franco, from Telegram)
1. `/agents` → grouped list without archived sessions (`gsd` must be absent).
2. `/peek ops-main` → screen snapshot without the `❯` input box.
3. `/send ops-main hola desde telegram` → `✅ Sent to ops-main (delivery: submitted)`; try `/send ops x` → ambiguity list.
