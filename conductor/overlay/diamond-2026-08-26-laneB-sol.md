Verdict: not safe to deploy unchanged. Four high-severity defects violate the input-isolation, rescue-Enter, and corruption requirements.

## Findings

1. **High — CLI option injection permits arbitrary readable-file disclosure** — [bridge_local.py:176](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:176)  
   The Telegram body precedes CLI flags without `--`. Agent-deck normalizes leading-dash arguments as options, so `/send target --message-file=/readable/path` reads that local file and sends its contents to the agent.  
   **Minimal fix:** put all flags first, then `--`, `sid`, and `message`.

2. **High — foreign drafts satisfy the rescue ownership test** — [bridge_local.py:121](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:121), [bridge_local.py:137](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:137)  
   Any eight-character prefix is accepted without evidence that capture truncation occurred: foreign draft `deploy p` matches sent message `deploy production`. `split_pane` also stops at the first blank line, hiding any foreign text below it.  
   **Minimal fix:** accept only the complete normalized message. Permit a prefix only with positive, structural evidence of pane truncation and after accounting for every composer line; otherwise do not rescue.

3. **High — unavoidable check/use race before `send-keys Enter`** — [bridge_local.py:190](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:190), [bridge_local.py:203](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:203)  
   An operator can replace the composer after `session output` returns and before the separate tmux process sends Enter. Then foreign text is submitted. Another read narrows but cannot close the race.  
   **Minimal fix:** remove the raw tmux rescue unless agent-deck provides an atomic guarded operation.

4. **High — failed patching can leave `bridge.py` corrupt** — [reapply.sh:73](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:73)  
   The script truncates and writes the live file, then compiles it. A write interruption, disk error, or compile failure leaves the invalid file installed.  
   **Minimal fix:** create a same-directory temporary file, validate the exact anchor count, compile it and `bridge_local.py`, then atomically replace `bridge.py`; restore automatically if a post-replacement step fails.

5. **Med — overlay failure can still remove stock handlers** — [bridge_local.py:301](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:301), [bridge-hook.diff:9](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge-hook.diff:9)  
   Registration is incremental. If a later registration raises, earlier overlay handlers remain despite the hook catching the exception. Once registered, `/sessions` and `/help` precede and shadow stock handlers; a runtime overlay failure does not fall through.  
   **Minimal fix:** build a separate Router and attach it only after complete validation. Preserve stock commands or provide an explicit stock fallback.

6. **Med — failed rescue can be reported as successful** — [bridge_local.py:203](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:203)  
   The tmux return code and second `session output` return code are ignored. If tmux or pane capture fails, an empty `composer2` produces a false ✅.  
   **Minimal fix:** require both commands to succeed and return only “attempted/unverified” unless positive delivery evidence exists.

7. **Med — session names containing spaces corrupt the outgoing message** — [bridge_local.py:269](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:269)  
   `/send foo bar hello` treats `foo` as the session and sends `bar hello`. If `foo` uniquely prefixes session `foo bar`, it silently targets that session with the wrong body.  
   **Minimal fix:** require an explicit delimiter such as `/send <session> -- <message>`, or match the longest active session prefix.

8. **Med — foreign operator text is disclosed to Telegram** — [bridge_local.py:197](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:197)  
   A mismatched composer is echoed back, potentially exposing an unsent password, token, or private draft.  
   **Minimal fix:** report only that foreign text exists; never include its contents.

9. **Med — marker-only idempotence accepts missing or broken overlays** — [reapply.sh:51](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:51), [reapply.sh:77](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:77)  
   Any occurrence of `# overlay-hook` is accepted. Duplicate, partial, commented-out hooks and a missing or syntax-invalid `bridge_local.py` are not detected. `OVERLAY` is never used.  
   **Minimal fix:** validate exactly one complete hook in the expected position and validate/import the overlay on every run.

10. **Med — launchctl verification has multiple false-positive paths** — [reapply.sh:22](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:22)  
    The timestamp has only second resolution and includes pre-restart lines from the same second. The health condition accepts either stock `Connection established` or `overlay: registered`; a broken overlay can therefore pass through healthy stock behavior.  
    **Minimal fix:** record the log byte offset before restart, inspect only appended lines, require both stock connection and overlay registration, reject overlay errors, and recheck PID stability after a short grace period.

11. **Med — settings rewrite is non-atomic and can lose existing configuration** — [reapply.sh:109](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:109)  
    The original is renamed before the replacement is written directly. Interruption leaves the settings missing or partial; the next run may treat it as absent and discard preserved top-level keys.  
    **Minimal fix:** write, validate, and fsync a mode-preserving temporary file before atomic replacement.

12. **Low — configured tmux socket resolution is an unstable private surface** — [bridge_local.py:39](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:39), [bridge_local.py:142](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:142)  
    It directly reads a hard-coded `~/.config` path, ignoring agent-deck’s XDG/legacy resolution, and silently defaults after any error. Rescue may target the wrong server.  
    **Minimal fix:** use a resolved value exposed by a stable surface, or fail closed and skip rescue.

13. **Low — some prerequisite failures are not loud** — [reapply.sh:87](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:87), [reapply.sh:118](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:118)  
    Invalid-JSON diagnostics are captured in `out`, but `set -e` exits before `say "$out"`. A missing plist or bridge-venv Python is silently skipped.  
    **Minimal fix:** print Python failures to stderr and explicitly fail when required plist/venv prerequisites are absent.

## Classes with no additional findings

- **HTML parse mode:** no injection found. `/peek` escapes both title and pane text; other responses are plain text under the stock bot configuration.
- **Shell/tmux metacharacter injection:** no shell-string interpolation found. Tmux receives only literal `Enter`, and its target is an argv element from session JSON. The CLI option injection above remains exploitable.
- **Aiogram timing race:** registration order is deterministic; the defect is partial registration and stock shadowing, not asynchronous ordering.
- **Direct Telegram-triggered daemon crash:** none found beyond the invalid `bridge.py` next-launch failure. The hook catches import/registration exceptions.
- **Secret leakage:** findings 1 and 8 are concrete. `/peek` also intentionally exports unredacted pane output, so secrets displayed in a pane inherently leave the host.

The files passed Bash/Python syntax checks and were not modified. An independent Claude-family second opinion was attempted but unavailable because that CLI is not authenticated.