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
| Inbound voice | A voice note from the authorized account is transcribed locally and handed to the conductor **as a prompt**, and the conductor's answer comes back to the same chat — the same round trip as typing those words. See "The voice channel" below. |
| `/peek` refresh | The snapshot carries an inline 🔄 button that edits the same message in place. Every press re-validates the target (token, Telegram user, session id **and** title, profile) and refuses stale, expired or forged callbacks. |

### The voice channel

A voice note from Franco is **not** an attachment. It is him talking, and it is
executed like the text he types: transcribed, handed to the conductor as a prompt,
and answered in the same chat. That reclassifies the whole path — a transcript
attributed to the wrong audio is not a bad row in a table, it is the conductor
carrying out an order nobody gave, and it would be indistinguishable from a real
one. So the channel is built around four properties rather than around accuracy:

**The rule these turn on is authorship, not trust.** The untrusted fence was never
distrust of Franco; it was the answer to "who wrote this?" applied bluntly, by
fencing every transcript. The right question is narrower: *is the sender the
author?* When he speaks, he is, and fencing him would tell the conductor to ignore
its own operator. When he forwards someone else's note, he is not, and the fence is
exactly right. Anyone tempted to simplify the forwarding checks below into "he is
authorized, so run it" is answering the wrong question.

| | Property | How it holds |
|---|---|---|
| I8 | Only he can open it, and only when he is the author | The bridge's own single-user gate: `on_audio` returns before downloading anything for any other sender. That gate answers who *sent* the note, though, not who *recorded* it — so a note carrying any forwarding marker (`forward_origin`, the legacy `forward_*` fields, `is_automatic_forward`, `via_bot`, `sender_chat`) is transcribed and answered, but delivered behind the untrusted fence. A stranger's words are never run as his instruction. |
| I9 | The text provably belongs to *this* file | The transcript is the stdout/JSON of the process that read this file's private copy. There is no shared history to attribute from — the failure mode is absent by construction, not guarded against. |
| I10 | A mishearing cannot act on its own | The prompt carries a standing rule: restate and confirm before anything irreversible or outward-facing. Below `MIN_CONFIDENCE` the note is shown but never executed. A redelivered note is refused (durable ledger keyed by Telegram's `file_unique_id`, content hash when Telegram omits one). The ledger guards *execution* only, so re-forwarding a third party's note is not refused — nothing is being run. |
| I11 | The conductor knows what it is reading | Every prompt carries its provenance line: engine, audio length, detected language, mean token confidence. |

Franco also sees the transcript in Telegram before the answer arrives, so a
mishearing is visible to him and not only to the conductor.

### The engine: the shared `transcribe` CLI

Transcription is delegated to the `transcribe` tool from **fnmendez/transcribe**
(Franco's decision: one engine for every agent, not a private copy per bridge).
The tool owns the whole risky part — whisper.cpp arm64+Metal with the Ultra
local model, `sandbox-exec` network denial, a strict environment allowlist,
single-flight locking, deadlines, and the zero-survivors guarantee — and its
own test suite pins those invariants. `transcribe.py` here is only an adapter:
it resolves the CLI, runs `transcribe --json --quiet -- <file>`, and maps the
result onto the API the bridge speaks.

Binary resolution, in order — chosen so the channel survives the tool's PR
merging (linked worktrees are removed after merge):

1. `transcribe` on `PATH`
2. `~/.local/bin/transcribe` (created by `make install` in the tool repo)
3. the development worktree binary, with a logged warning — dev fallback only

If none exists the adapter fails closed and the Telegram reply names the exact
fix. Exit codes map to distinct refusals: `7` is "another job is already
running" (single-flight, not a failure), `3` carries the tool's own
install/model fix verbatim, `4`/`5` reject the input or report the deadline,
`6` discards a survivor-tainted result, anything else is reported with the
stderr tail. A result that does not claim `isolated` and `network_denied` is
refused even on rc 0. The adapter never passes `--allow-fallback`: nothing is
ever substituted on failure, because sending Franco's audio to a service he did
not approve would be a far worse outcome than no transcript.

What stays on this side of the boundary, because it is not the tool's job: the
authorship fence and forwarding rule (I8), the bounded download, the
exactly-once execution ledger (I10), and the prompt framing with provenance
(I11). Measured through the adapter on this machine: a real 15.2 s Spanish
note in 6.0 s (2.5× faster than real time) with the Ultra local large model.

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
        tests.test_media_and_peek tests.test_transcribe_isolation \
        tests.test_voice_prompt tests.test_voice_channel tests.test_reapply_contract

## Morning checklist (Franco, from Telegram)
1. `/agents` → grouped list without archived sessions (`gsd` must be absent).
2. `/peek ops-main` → screen snapshot without the `❯` input box.
3. `/send ops-main hola desde telegram` → `✅ Sent to ops-main (delivery: submitted)`; try `/send ops x` → ambiguity list.

## Handoff

### State on 2026-08-29

Deployed and running: bridge pid was 86736 after the last `reapply.sh`, with
`overlay: registered …` and `overlay: send_to_conductor now reports durable
delivery truth` in `bridge.log`. The deployed overlay matches the branch
`feat/conductor-inbound-media` (fork PR #2, unmerged).

### Rollback

Ordered from smallest to largest, each verified to leave the bridge running:

1. **Undo an overlay change only** — restore the previous overlay directory and
   re-apply. Timestamped copies are made before each deploy:
   ```sh
   cp -R ~/.local/share/agent-deck/conductor/overlay.bak-<stamp>/. \
         ~/.local/share/agent-deck/conductor/overlay/
   ~/.local/share/agent-deck/conductor/overlay/reapply.sh
   ```
2. **Remove the overlay entirely** — restore the stock bridge and restart:
   ```sh
   ~/.local/share/agent-deck/conductor/overlay/reapply.sh --rollback
   ```
   `bridge.py.pre-overlay` is the untouched stock bridge saved when the hook was
   first inserted, so this returns the conductor to vendor behaviour. It refuses
   when `bridge.py` carries no hook, so it can never downgrade a fresh update.
3. **Verify either one**: `launchctl list | grep conductor-bridge` shows a new
   pid, and the lines appended to `bridge.log` after the restart show
   `Run polling for bot` (stock) or `overlay: registered` (overlay).

### Two acceptances that need a person

Both are Telegram paths no automated run can trigger; everything behind them is
covered by unit tests, but the round trip has not been exercised by hand:

1. **Send Slavna one voice note.** Expected: `🎤 transcribing locally…`, then
   what it heard with its provenance line, then the conductor's actual answer to
   what he said. The file is under `<conductor>/inbox/audio/<date>/` and nothing
   is sent to any other service.
2. **Press 🔄 Refresh under a `/peek` reply.** Expected: the same Telegram
   message updates in place with a fresh snapshot. A button older than 30
   minutes, or one for a session that has gone, is refused with a short reason
   instead of acting on the wrong target.

### Audio transcription: decided

Option B (a local offline engine inside this same job harness) was taken on
2026-08-29. See "The engine, and why this one" above for the measurements behind
it. To undo: delete `<conductor>/stt-models/`, and the engine reports itself
unavailable again on its own — voice notes go back to being saved and announced,
never transcribed.
