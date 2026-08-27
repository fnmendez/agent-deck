Lane C — Claude (Fable 5 general-purpose subagent), 2026-08-26 01:36, artifact SHA256SUMS in this dir. Completed.
1 MED bridge_local.py:176 message starting with '-' parsed as agent-deck flags (normalizeArgs hoists any -token). Fix: flags first, then "--", sid, message.
2 LOW bridge_local.py:216,219 unescaped title under parse_mode=HTML. Fix html.escape.
3 LOW bridge_local.py:37 DIM_RE over/under-eats (stops only at [0m/[22m; misses [2;37m). Fix: stop at any SGR; opener \x1b\[2(;[0-9;]*)?m.
4 LOW bridge_local.py:139 multi-line paste collapsed by Claude -> reported as foreign text (fail-safe, misleading wording).
5 LOW bridge_local.py:69 /peek with no query returns /send usage text.
6 MED reapply.sh verify greps "Connection established" which aiogram logs only after a failed poll -> --rollback always reports FAIL; network flap could mask overlay failure. Fix: "Run polling for bot" for rollback, "overlay: registered" otherwise.
7 MED reapply.sh hook write non-atomic; py_compile failure leaves broken bridge.py (crash-loop on next respawn). Fix: tmp+compile+mv or restore on failure.
8 LOW reapply.sh settings python prints [FAIL] to stdout inside $(...) so message is swallowed. Fix: stderr.
9 LOW reapply.sh --rollback after update-before-reapply restores an older stock. Fix: refuse rollback when no hook marker.
No findings: registration/ordering, injection, secret leakage, idempotence, hook diff.
