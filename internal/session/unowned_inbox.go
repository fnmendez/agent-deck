package session

import "strings"

// Issue #1948 (review P1) — the drainable ledger for transitions whose parent
// is not on this machine.
//
// A worker created on host B by a conductor on host A has NO resolvable parent
// in B's registry: `parent_session_id` cannot point across machines. Every
// waiting / error / idle transition it produces therefore reaches
// resolveParentIDForInbox, is classified an ORPHAN (or parent_removed), and is
// dropped — logged to notifier-orphans.log and written to no inbox at all. Only
// COMPLETIONS survived, because they have a separate completion ledger.
//
// That is the exact scenario the field report is about — quota-stalled remote
// sessions nobody notices for hours — so a drain that returns completions but
// not stalls does not solve it.
//
// The fix is NOT to resolve the cross-machine parent. A remote worker having no
// local parent is a legitimate steady state, not damage to repair, and trying
// to reach across machines to find one is the push-relay design this feature
// exists to avoid. Instead the transition is PERSISTED IN A DRAINABLE FORM and
// left for whoever pulls.
//
// The store is the inbox directory under a reserved id, deliberately, so it
// inherits every property the inbox already has rather than growing a second
// set of them:
//
//   - WriteInboxEventIfNew's EventFingerprint dedup (one dedup rule, not two)
//   - the fsync'd append and the 16 MB scanner cap
//   - SweepInboxByTTL, which walks every inbox file, so growth stays bounded
//   - ExportPendingRecords, which walks every inbox file, so the drain already
//     exports it
//
// Session ids are `<hex>-<epoch>`, so the reserved id cannot collide with one.
const UnownedInboxID = "_unowned"

// unownedReasons are the terminal resolve outcomes that mean "no parent on THIS
// machine" — the cross-machine steady state, worth keeping for a puller.
//
// Everything else stays dropped on purpose: no_notify and self_conductor are
// deliberate suppressions (re-delivering them would override a user's explicit
// choice — review P2a), and child_removed cannot be attributed to anything a
// remote conductor could act on.
func isUnownedReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case deadLetterReasonOrphan, deadLetterReasonParentMissing:
		return true
	default:
		return false
	}
}

// recordUnownedTransition persists an event whose parent is not on this machine
// so a pulling conductor can still see it. Returns whether it was newly stored.
//
// TargetSessionID is deliberately left as-is (empty for these events): the
// record has no target on this host, and the draining conductor stamps itself
// as the target when it ingests. Best-effort by contract — the caller's
// dead-letter accounting is unchanged either way, so a failure here degrades to
// exactly the old behavior instead of breaking delivery.
func recordUnownedTransition(event TransitionNotificationEvent) (bool, error) {
	if strings.TrimSpace(event.ChildSessionID) == "" {
		return false, nil
	}
	event.DoneSummary = capDoneSummary(event.DoneSummary)
	if event.TurnFingerprint == "" {
		event.TurnFingerprint = TurnFingerprint(event)
	}
	return WriteInboxEventIfNew(UnownedInboxID, event)
}
