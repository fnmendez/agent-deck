Verdict: block deployment. I found 6 high-, 9 medium-, and 1 low-severity defects.

## Findings

1. **High — CLI option injection permits arbitrary local-file reads.**  
   [bridge_local.py:176–177](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:176)  
   Telegram text beginning `--` is passed before CLI flags. Agent-deck normalizes flags anywhere, so `/send target --message-file=/path` makes the CLI read that file and send its contents to the session. This was reproduced safely with a nonexistent path.  
   Minimal fix: place all flags first, then `--`, session ID, and message.

2. **High — arbitrary eight-character prefixes are treated as owned composer text.**  
   [bridge_local.py:133–139](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:133)  
   For message `schedule database migration`, foreign draft `schedule` passes. Whitespace normalization also makes distinct text equivalent. There is no evidence that the prefix was actually truncated by the UI.  
   Minimal fix: require exact, UI-aware equality. Permit prefixes only when parsing produces explicit truncation evidence; otherwise do not rescue.

3. **High — capture-to-Enter race can submit newly entered foreign text.**  
   [bridge_local.py:189–204](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:189)  
   An operator or concurrent `/send` can replace the composer after inspection but before `send-keys Enter`. A second capture only narrows, rather than closes, this race.  
   Minimal fix: remove the raw-tmux rescue. The strict “never foreign text” invariant cannot be made atomic through this interface.

4. **High — overlay failures suppress stock `/sessions` and `/help`.**  
   [bridge-hook.diff:9–16](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge-hook.diff:9), [bridge_local.py:241–304](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:241)  
   Aiogram 3.30 stops after the first matching handler. Because overlay handlers register first, runtime exceptions never fall through to stock handlers. Partial registration can also remain if a later registration fails.  
   Minimal fix: register only new commands externally. Add overlay behavior inside the stock `/sessions` and `/help` handlers under `try/except`, retaining their original bodies as fallback.

5. **High — `bridge.py` is truncated before validation.**  
   [reapply.sh:60–78](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:60)  
   The script writes directly to `bridge.py`, then compiles it. Interruption, write failure, or compile failure leaves the installed bridge damaged; `set -e` exits without restoring the backup.  
   Minimal fix: generate and compile a same-directory temporary file, then atomically `os.replace()` it.

6. **High — anchor and marker checks are not fail-closed.**  
   [reapply.sh:51–75](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:51)  
   Any occurrence of `# overlay-hook` is accepted, including stale, partial, or duplicate hooks. Anchor uniqueness uses `assert`; under `PYTHONOPTIMIZE=1` it disappears and `str.replace()` patches every matching anchor.  
   Minimal fix: explicitly require exact counts, use `replace(..., 1)`, verify the complete hook block, and compile even when the marker already exists.

7. **Medium — unverified delivery is reported as success.**  
   [bridge_local.py:178–194](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:178)  
   Agent-deck can return `success:true`, `delivery:"unverified"`, `submitted:false`. An empty composer then produces a green “Sent” response despite no positive submission evidence.  
   Minimal fix: only report success when `submitted is True` or `delivery == "submitted"`.

8. **Medium — rescue errors can crash the command or produce false success.**  
   [bridge_local.py:203–210](/private/tmp/claude-501/-Users/francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:203)  
   `FileNotFoundError` and `TimeoutExpired` escape the handler. A nonzero tmux result and the second pane command’s return code are ignored; an empty failed capture becomes success.  
   Minimal fix: catch subprocess errors and require both tmux and post-capture return codes to be zero before assessing delivery.

9. **Medium — `/peek` fails open and can expose live composer text.**  
   [bridge_local.py:104–130](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:104)  
   If a tool or updated UI uses a prompt other than `❯` or `›`, `split_pane()` returns the entire pane as output, including any operator draft.  
   Minimal fix: when the composer boundary cannot be proven, refuse the snapshot or restrict `/peek` to explicitly supported tool/UI shapes.

10. **Medium — foreign composer contents are sent back to Telegram.**  
    [bridge_local.py:197–199](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:197)  
    Up to 200 characters of another operator’s draft may be disclosed, potentially into a group chat.  
    Minimal fix: report only that foreign text was detected; never include its contents.

11. **Medium — launch verification has multiple false-positive paths.**  
    [reapply.sh:20–40](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:20)  
    Either `Connection established` or `overlay: registered` is enough. Thus stock connectivity can mask overlay failure, while registration before Telegram networking can mask an unusable bridge. Second-resolution timestamps may also accept an old line from the same second, and errors do not invalidate success.  
    Minimal fix: record the pre-restart log byte offset; require a stable new PID, a new exact overlay marker, and a platform-specific ready marker, with no subsequent failure marker. Rollback needs separate expectations.

12. **Medium — missing overlay, plist, or venv can be silently accepted.**  
    [reapply.sh:9–14](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:9), [reapply.sh:117–123](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/reapply.sh:117)  
    `OVERLAY` is unused, while absent plist/venv conditions are skipped without failure. The hook can therefore be installed without a loadable `bridge_local.py`, and verification can still pass.  
    Minimal fix: preflight the overlay file, compile it with the intended venv, and require the expected plist before modifying anything.

13. **Medium — `/send` cannot address session titles containing spaces.**  
    [bridge_local.py:268–275](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:268)  
    `/send my session do work` always resolves session `my`; agent-deck titles are free text.  
    Minimal fix: define quoting or an unambiguous delimiter, such as `/send "my session" -- do work`.

14. **Medium — overlay help is not “stock help + additions.”**  
    [bridge_local.py:286–304](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:286)  
    It omits `/sessions`, conductor names, and the stock default-conductor information.  
    Minimal fix: extend the stock handler rather than replacing it.

15. **Medium — rescue reads the wrong tmux socket under supported XDG layouts.**  
    [bridge_local.py:39–40](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:39), [bridge_local.py:142–148](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:142)  
    The stock bridge supports effective XDG/legacy config resolution, but the overlay hardcodes `~/.config`. A custom socket therefore falls back to `agent-deck`.  
    Minimal fix: consume the stock bridge’s resolved `CONFIG_PATH` through `ctx`.

16. **Low — `/peek` budgets only the body, not the complete HTML message.**  
    [bridge_local.py:151–166](/private/tmp/claude-501/-Users-francomendez-Software-forks-agent-deck/b5d769db-7963-4b5e-b154-ce294d0cf7ec/scratchpad/diamond/bridge_local.py:151)  
    An unusually long CLI-created title can push the final message over Telegram’s limit, causing `Bad Request`.  
    Minimal fix: budget or truncate the complete escaped payload, including header and tags.

No direct shell-metacharacter or tmux-command injection was found: subprocesses use argv arrays and tmux receives only constant `Enter`. No unescaped HTML injection was found; dynamic HTML is escaped. No unsupported agent-deck subcommand was found. I found no proven whole-daemon crash from Telegram input, but command-level exceptions and stock-handler suppression remain.

Static parsing passed for Python and Bash; ShellCheck’s only warning was the unused `OVERLAY`, which supports finding 12. No files were modified. An independent second model family was unavailable: Claude was not authenticated, and CodeRabbit cannot review this isolated non-Git directory.