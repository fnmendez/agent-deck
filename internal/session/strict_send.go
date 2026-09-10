package session

import (
	"encoding/json"
	"path/filepath"
	"time"
)

// StrictSendIdleEvidence carries native process evidence that can authorize a
// durable Claude Stop. Zero evidence preserves the historical fresh-hook path.
type StrictSendIdleEvidence struct {
	DurableClaudeStop bool
	ProcessStartedAt  time.Time
	CWD               string
}

// StrictSendIdleDecision is safe diagnostic metadata from the one bounded hook
// read. Send callers use Admitted only; the read-only probe also reports why.
type StrictSendIdleDecision struct {
	Admitted       bool
	DurableStop    bool
	HookAgeSeconds int64
	Reason         string
}

// StrictSendIdle reads only hook state; never use pane-diff fallback or treat
// a missing timestamp as fresh (the display-oriented reader permits that).
func StrictSendIdle(instanceID, tool, expectedThread string, evidence StrictSendIdleEvidence) StrictSendIdleDecision {
	return strictSendIdleAt(instanceID, tool, expectedThread, evidence, time.Now())
}

func strictSendIdleAt(instanceID, tool, expectedThread string, evidence StrictSendIdleEvidence, now time.Time) StrictSendIdleDecision {
	decision := StrictSendIdleDecision{Reason: "hook_unavailable"}
	data, err := readStatusFileNoFollow(hookStatusFilePath(instanceID))
	if err != nil {
		return decision
	}
	var raw struct {
		Status     string `json:"status"`
		Thread     string `json:"session_id"`
		Event      string `json:"event"`
		Timestamp  int64  `json:"ts"`
		CWD        string `json:"cwd"`
		Generation string `json:"hook_generation"`
		Sequence   uint64 `json:"sequence"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Timestamp <= 0 {
		decision.Reason = "hook_invalid"
		return decision
	}
	if raw.Thread != expectedThread {
		decision.Reason = "hook_thread_changed"
		return decision
	}
	if raw.Status != "waiting" && raw.Status != "idle" {
		decision.Reason = "hook_state_invalid"
		return decision
	}
	generation, authority := hookGenerationForInstance(instanceID)
	if !hookGenerationRecordAccepted(raw.Generation, generation, authority) {
		decision.Reason = "hook_generation_invalid"
		return decision
	}
	age := now.Sub(time.Unix(raw.Timestamp, 0))
	decision.HookAgeSeconds = int64(age / time.Second)
	if age < 0 {
		decision.Reason = "hook_timestamp_future"
		return decision
	}
	if age < hookFastPathFreshnessForTool(tool, raw.Status) {
		decision.Admitted = true
		decision.Reason = "fresh_hook"
		return decision
	}
	decision.Reason = "stale_hook"
	if tool != "claude" || raw.Status != "waiting" || raw.Event != "Stop" {
		return decision
	}
	if !evidence.DurableClaudeStop || evidence.ProcessStartedAt.IsZero() {
		decision.Reason = "durable_identity_unavailable"
		return decision
	}
	if time.Unix(raw.Timestamp, 0).Before(evidence.ProcessStartedAt) {
		decision.Reason = "hook_before_process"
		return decision
	}
	if raw.CWD == "" || evidence.CWD == "" || filepath.Clean(raw.CWD) != filepath.Clean(evidence.CWD) {
		decision.Reason = "hook_cwd_changed"
		return decision
	}
	if raw.Generation != "" && raw.Sequence == 0 {
		decision.Reason = "hook_sequence_invalid"
		return decision
	}
	decision.Admitted = true
	decision.DurableStop = true
	decision.Reason = "durable_stop"
	return decision
}
