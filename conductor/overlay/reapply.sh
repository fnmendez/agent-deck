#!/usr/bin/env bash
# Re-apply local conductor customizations after `agent-deck update` / `conductor setup`.
# Idempotent. Usage: reapply.sh [--dry-run] [--rollback]
#   1. bridge.py       : insert the overlay hook (exactly once) before the stock /sessions handler
#   2. <conductor>/.claude/settings.json : modern, permissive (bypass; Edit() rules; no ask/deny)
#   3. LaunchAgent plist: python must be the bridge-venv interpreter
#   4. restart bridge if bridge.py / bridge_local.py / plist changed; verify new pid + log; rollback
set -euo pipefail
DATA="${AGENT_DECK_DATA_DIR:-$HOME/.local/share/agent-deck}"
CDIR="$DATA/conductor"; OVERLAY="$CDIR/overlay"; BRIDGE="$CDIR/bridge.py"
MODULE="$OVERLAY/bridge_local.py"; APPLIED="$OVERLAY/.applied-sha"
# Every module the bridge imports, not just the entry point: a change in any of
# them must restart the bridge, or it keeps serving the code already in memory.
MODULES="bridge_local.py delivery.py media.py transcribe.py"
VENV_PY="$DATA/bridge-venv/bin/python"
PLIST="$HOME/Library/LaunchAgents/com.agentdeck.conductor-bridge.plist"
LABEL="com.agentdeck.conductor-bridge"; LOG="$CDIR/bridge.log"
MARKER='# overlay-hook'; ANCHOR='    @dp.message(Command("sessions"))'
DRY=0; ROLLBACK=0
for a in "$@"; do case "$a" in --dry-run) DRY=1;; --rollback) ROLLBACK=1;; *) echo "unknown arg $a" >&2; exit 2;; esac; done
say() { printf '%s\n' "$*"; }
fail() { say "[FAIL] $*"; exit 1; }
CHANGED=0; PLIST_CHANGED=0

# ---- preflight ------------------------------------------------------------
[ -f "$BRIDGE" ] || fail "$BRIDGE missing"
[ -x "$VENV_PY" ] || fail "bridge venv python missing: $VENV_PY"
[ -f "$PLIST" ] || fail "LaunchAgent plist missing: $PLIST"
for module in $MODULES; do
  [ -f "$OVERLAY/$module" ] || fail "overlay module missing: $OVERLAY/$module"
  "$VENV_PY" -m py_compile "$OVERLAY/$module" || fail "$module does not compile"
done
"$VENV_PY" -c "import sys; sys.path.insert(0, '$OVERLAY'); import bridge_local" || fail "bridge_local.py does not import under the bridge venv"
MODULE_SHA="$(cd "$OVERLAY" && shasum -a 256 $MODULES | shasum -a 256 | cut -d' ' -f1)"

pid_of() { launchctl list | awk -v l="$LABEL" '$3==l{print $1}'; }

restart_and_verify() {
  local old_pid new_pid offset want
  old_pid="$(pid_of)"
  offset="$(stat -f %z "$LOG" 2>/dev/null || echo 0)"     # only lines appended after this point count
  want="overlay: registered"; [ "$ROLLBACK" = 1 ] && want="Run polling for bot"
  if [ "$PLIST_CHANGED" = 1 ]; then
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST"
  else
    launchctl kickstart -k "gui/$(id -u)/$LABEL"
  fi
  for _ in $(seq 1 40); do
    sleep 1
    new_pid="$(pid_of)"
    [ -n "$new_pid" ] && [ "$new_pid" != "-" ] && [ "$new_pid" != "$old_pid" ] || continue
    if tail -c "+$((offset + 1))" "$LOG" | grep -q "$want"; then
      sleep 3   # pid must be stable and no overlay error may follow
      [ "$(pid_of)" = "$new_pid" ] || { say "[FAIL] bridge pid $new_pid died right after start"; return 1; }
      if tail -c "+$((offset + 1))" "$LOG" | grep -q "overlay: bridge_local failed\|Traceback"; then
        say "[FAIL] bridge came back but the overlay reported an error:"; tail -c "+$((offset + 1))" "$LOG" | grep -A3 "overlay: bridge_local failed\|Traceback" | head -12; return 1
      fi
      say "[ok] bridge restarted: pid $old_pid -> $new_pid ($want)"
      [ "$ROLLBACK" = 1 ] || printf '%s\n' "$MODULE_SHA" > "$APPLIED"
      return 0
    fi
  done
  say "[FAIL] bridge did not log '$want' within 40s (old pid $old_pid, new '${new_pid:-}'). Check $LOG"; return 1
}

if [ "$ROLLBACK" = 1 ]; then
  [ -f "$BRIDGE.pre-overlay" ] || fail "no $BRIDGE.pre-overlay to restore"
  grep -q "$MARKER" "$BRIDGE" || fail "bridge.py has no overlay hook — nothing to roll back (refusing to downgrade a fresh update)"
  [ "$DRY" = 1 ] && { say "[dry] would restore $BRIDGE from .pre-overlay and restart"; exit 0; }
  cp "$BRIDGE.pre-overlay" "$BRIDGE"; rm -f "$APPLIED"; say "[ok] bridge.py restored from .pre-overlay"; restart_and_verify; exit $?
fi

# ---- 1. bridge.py hook ---------------------------------------------------
markers="$(grep -c "$MARKER" "$BRIDGE" || true)"
if [ "$markers" -gt 1 ]; then fail "bridge.py has $markers overlay markers (expected 1); restore from bridge.py.pre-overlay"; fi
if [ "$markers" -eq 1 ]; then
  "$VENV_PY" -m py_compile "$BRIDGE" || fail "bridge.py (with hook) does not compile"
  say "[ok] bridge.py: overlay hook present"
else
  anchors="$(grep -cF "$ANCHOR" "$BRIDGE" || true)"
  [ "$anchors" -eq 1 ] || fail "bridge.py: anchor found $anchors times (expected 1); upstream changed — patch reapply.sh"
  if [ "$DRY" = 1 ]; then say "[dry] bridge.py: would insert overlay hook before stock /sessions handler"
  else
    cp "$BRIDGE" "$BRIDGE.pre-overlay"
    "$VENV_PY" - "$BRIDGE" "$ANCHOR" <<'PYEOF'
import os, py_compile, sys
path, anchor = sys.argv[1], sys.argv[2]
hook = '''    # overlay-hook: local commands (/agents /peek /send), re-applied by overlay/reapply.sh
    try:
        sys.path.insert(0, str(Path(__file__).resolve().parent / "overlay"))
        import bridge_local
        bridge_local.register(dp, globals(), is_authorized)
    except Exception as _overlay_err:
        log.error("overlay: bridge_local failed to register: %s", _overlay_err)

'''
src = open(path, encoding="utf-8").read()
if src.count(anchor) != 1:
    sys.exit("anchor count != 1")
tmp = path + ".overlay-tmp"
with open(tmp, "w", encoding="utf-8") as f:
    f.write(src.replace(anchor, hook + anchor, 1))
try:
    py_compile.compile(tmp, doraise=True)
except py_compile.PyCompileError as exc:
    os.unlink(tmp); sys.exit(f"patched bridge.py does not compile: {exc}")
os.chmod(tmp, os.stat(path).st_mode)
os.replace(tmp, path)   # atomic: the live file is never truncated
PYEOF
    say "[ok] bridge.py: overlay hook inserted atomically (backup: bridge.py.pre-overlay)"
  fi
  CHANGED=1
fi

# ---- 1b. any overlay module changed since last applied? ------------------
if [ "$(cat "$APPLIED" 2>/dev/null || true)" != "$MODULE_SHA" ]; then
  say "[$([ "$DRY" = 1 ] && echo dry || echo ok)] overlay modules changed since last apply -> restart needed"
  CHANGED=1
fi

# ---- 2. conductor settings.json -----------------------------------------
for meta in "$CDIR"/*/meta.json; do
  [ -f "$meta" ] || continue
  d="$(dirname "$meta")"; s="$d/.claude/settings.json"
  set +e; out="$("$VENV_PY" - "$d" "$s" "$DRY" 2>&1 <<'PYEOF'
import json, os, shutil, sys, time
d, path, dry = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
name = os.path.basename(d)
want_perm = {
    "defaultMode": "bypassPermissions",
    "allow": ["Bash(agent-deck *)", "Bash(tmux *)", "Edit(//" + d + "/**)"],
}
root = {}
if os.path.exists(path):
    try:
        root = json.load(open(path))
    except json.JSONDecodeError as exc:
        sys.exit(f"[FAIL] {path}: invalid JSON ({exc}); fix or remove it first")
if root.get("permissions") == want_perm:
    print(f"[ok] {name}/.claude/settings.json: already modern+permissive"); sys.exit(0)
new = {k: v for k, v in root.items() if k != "permissions"}   # keep custom top-level keys
new["permissions"] = want_perm
if dry:
    print(f"[dry] {name}/.claude/settings.json: would rewrite permissions "
          f"(old keys: {sorted((root.get('permissions') or {}).keys())})"); sys.exit(0)
os.makedirs(os.path.dirname(path), exist_ok=True)
if os.path.exists(path):
    bak = path + ".bak-" + time.strftime("%Y%m%d-%H%M%S"); shutil.copy2(path, bak)
    print(f"     backup: {bak}")
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(new, f, indent=2); f.write("\n"); f.flush(); os.fsync(f.fileno())
json.load(open(tmp))
os.replace(tmp, path)
print(f"[ok] {name}/.claude/settings.json: rewritten atomically (bypass, Edit() rules, no ask/deny)")
PYEOF
)"; rc=$?; set -e
  say "$out"; [ $rc -eq 0 ] || exit $rc
done

# ---- 3. plist python -----------------------------------------------------
cur="$(plutil -extract ProgramArguments.0 raw -o - "$PLIST" 2>/dev/null || true)"
if [ "$cur" = "$VENV_PY" ]; then say "[ok] plist: python is bridge-venv"
elif [ "$DRY" = 1 ]; then say "[dry] plist: would set python $cur -> $VENV_PY"
else plutil -replace ProgramArguments.0 -string "$VENV_PY" "$PLIST"; say "[ok] plist: python set to $VENV_PY"; CHANGED=1; PLIST_CHANGED=1; fi

# ---- 4. restart if needed -----------------------------------------------
if [ "$DRY" = 1 ]; then say "[dry] no changes applied"; exit 0; fi
if [ "$CHANGED" = 1 ]; then restart_and_verify; else say "[ok] nothing changed; bridge left running"; fi
