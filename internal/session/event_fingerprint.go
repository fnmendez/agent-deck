package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// EventFingerprint returns a stable identifier for a transition event, keyed on
// its intrinsic identity: WHICH child, WHICH status flip (or which completion
// outcome), and WHICH turn produced it. Two attempts to persist the same logical
// event collapse to the same fingerprint and are deduplicated by the inbox
// writer and the notifier-missed log.
//
// The emit instant is deliberately NOT part of it (issue #1948, review P2b).
// It used to be — Timestamp.UnixNano() — on the reasoning that the daemon
// stamps an event once and reuses it. That holds within ONE producer and fails
// across two: a one-shot completion is written to the completion ledger with
// CompletionRecord.FinishedAt and committed to the inbox with a later
// time.Now(), and the replay path re-stamps time.Now() again. Keying on the
// instant meant those copies of a single completion never compared equal, so
// the cross-machine drain reported and persisted one completion twice while
// appearing to be deduplicated. Identity cannot depend on which code path
// happened to observe the event.
//
// What keeps genuinely distinct events distinct now:
//
//   - LastOutputHash for transitions — the child's pane content at the flip,
//     which advances once per turn. Two real stalls with different output are
//     two fingerprints; a re-emit of one stall is one.
//   - Kind + DoneStatus + DoneSummary for completions (issue #1186), so
//     distinct completions never collapse into each other.
//
// Two identical flips for one child with identical output are, by construction,
// the same assertion twice — which is exactly what this dedup exists to drop
// (issue #824: one logical event persisted 13 times). Dedup is also scoped to
// records still PENDING in a file: a drain removes the file and the fingerprint
// set with it, so a later recurrence of the same flip lands normally.
//
// Related but distinct: TurnFingerprint (inbox_outbox.go) identifies a child's
// completed TURN for exactly-once consumer effects and deliberately ignores the
// from→to flip. This identifies the EVENT. Neither is derivable from the other;
// they are two questions, not two copies of one rule.
//
// The fingerprint is a hex SHA-256 so it can safely be embedded in a JSON
// string field without escaping concerns and is cheap to grep for.
func EventFingerprint(e TransitionNotificationEvent) string {
	var b strings.Builder
	b.Grow(len(e.ChildSessionID) + len(e.FromStatus) + len(e.ToStatus) + len(e.LastOutputHash) + 32)
	b.WriteString(strings.TrimSpace(e.ChildSessionID))
	b.WriteByte('|')
	b.WriteString(strings.ToLower(strings.TrimSpace(e.FromStatus)))
	b.WriteByte('|')
	b.WriteString(strings.ToLower(strings.TrimSpace(e.ToStatus)))
	b.WriteByte('|')
	// The turn signal. Empty for a completion (no pane hash is carried on that
	// path) and for a transition emitted without one, in which case the flip
	// itself is the identity.
	b.WriteString(strings.TrimSpace(e.LastOutputHash))
	// Issue #1186: finished events carry no from→to transition, so without
	// the kind + outcome the fingerprint would collapse distinct completions
	// (and could collide with a same-child transition). Append them so each
	// completion assertion is its own logical event.
	if e.Kind != "" {
		b.WriteByte('|')
		b.WriteString(e.Kind)
		b.WriteByte('|')
		b.WriteString(strings.ToLower(strings.TrimSpace(e.DoneStatus)))
		b.WriteByte('|')
		b.WriteString(strings.TrimSpace(e.DoneSummary))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
