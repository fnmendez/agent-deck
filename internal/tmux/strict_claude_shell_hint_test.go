package tmux

import (
	"encoding/json"
	"os"
	"testing"
)

type strictClaudeShellHintRecording struct {
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	CursorY      int    `json:"cursor_y"`
	RawHint      string `json:"raw_hint"`
	RawDraftHint string `json:"raw_draft_hint"`
	RawPRHint    string `json:"raw_pr_hint"`
}

func loadStrictClaudeShellHintRecording(t *testing.T) strictClaudeShellHintRecording {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.276-background-shell.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording strictClaudeShellHintRecording
	if err := json.Unmarshal(data, &recording); err != nil {
		t.Fatal(err)
	}
	return recording
}

func strictClaudeShellHintSnapshot(t *testing.T, hint string) strictSnapshot {
	t.Helper()
	recording := loadStrictClaudeShellHintRecording(t)
	return strictClaudeFixtureGeometry(strictClaudeTestNBSPPrompt, hint, recording.Width, recording.Height, recording.CursorY)
}

func TestStrictClaudeMeasuredBackgroundShellHint(t *testing.T) {
	recording := loadStrictClaudeShellHintRecording(t)
	assertStrictClaudeAdmitted(t, strictClaudeShellHintSnapshot(t, recording.RawHint))
	// The PR segment is an OSC 8 hyperlink; only its visible text is grammar.
	assertStrictClaudeAdmitted(t, strictClaudeShellHintSnapshot(t, recording.RawPRHint))
	for _, visible := range []string{
		"⏵⏵ bypass permissions on · 2 shells · ← 2 agents",
		"⏵⏵ bypass permissions on · 1 shell · ← for agents",
		"⏵⏵ bypass permissions on · PR #1 · 12 shells · ← 99 agents",
	} {
		if !strictClaudeEmptyBypassHintValid(visible) {
			t.Fatalf("background-shell hint refused: %q", visible)
		}
	}
}

// Measured: every draft drops the complete agents suffix in this layout too.
func TestStrictClaudeBackgroundShellHintDraftsAndCounterexamplesRefuse(t *testing.T) {
	recording := loadStrictClaudeShellHintRecording(t)
	assertStrictClaudeRefused(t, strictClaudeShellHintSnapshot(t, recording.RawDraftHint), "claude_empty_hint_unverified")
	for _, visible := range []string{
		"⏵⏵ bypass permissions on · 1 shell",
		"⏵⏵ bypass permissions on · ← 2 agents",
		"⏵⏵ bypass permissions on (shift+tab to cycle) · 1 shell · ← 2 agents",
		"⏵⏵ bypass permissions on · 0 shells · ← 2 agents",
		"⏵⏵ bypass permissions on · 1 shells · ← 2 agents",
		"⏵⏵ bypass permissions on · 2 shell · ← 2 agents",
		"⏵⏵ bypass permissions on · 100 shells · ← 2 agents",
		"⏵⏵ bypass permissions on · 1 shell · PR #978 · ← 2 agents",
		"⏵⏵ bypass permissions on · 1 shell · ← 2 agents · 1 shell",
		"⏵⏵ bypass permissions on · 1 shell · ← 1 agent",
		"⏵⏵ bypass permissions on · 1 shell · ← 0 agents",
		"⏵⏵ accept edits on · 1 shell · ← 2 agents",
	} {
		if strictClaudeEmptyBypassHintValid(visible) {
			t.Fatalf("unmeasured background-shell hint admitted: %q", visible)
		}
		assertStrictClaudeRefused(t, strictClaudeShellHintSnapshot(t, "  "+visible), "claude_empty_hint_unverified")
	}
}
