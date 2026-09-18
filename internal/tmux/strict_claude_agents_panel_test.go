package tmux

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type strictClaudeAgentsPanelRecording struct {
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	CursorX  int      `json:"cursor_x"`
	CursorY  int      `json:"cursor_y"`
	RawHint  string   `json:"raw_hint"`
	RawPanel []string `json:"raw_panel"`
}

func loadStrictClaudeAgentsPanelRecording(t *testing.T) strictClaudeAgentsPanelRecording {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.274-agents-panel.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording strictClaudeAgentsPanelRecording
	if err := json.Unmarshal(data, &recording); err != nil {
		t.Fatal(err)
	}
	return recording
}

// strictClaudeAgentsPanelSnapshot renders the measured empty bypass composer with
// the given hint and the given raw rows directly below it.
func strictClaudeAgentsPanelSnapshot(t *testing.T, hint string, panel []string) strictSnapshot {
	t.Helper()
	recording := loadStrictClaudeAgentsPanelRecording(t)
	// Claude keeps the panel at the bottom and moves the composer up by one row
	// per extra panel row; the recorded panel sits exactly on the last rows.
	y := recording.CursorY - (len(panel) - len(recording.RawPanel))
	if y > recording.CursorY {
		y = recording.CursorY
	}
	s := strictClaudeFixtureGeometry(strictClaudeTestNBSPPrompt, hint, recording.Width, recording.Height, y)
	lines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
	copy(lines[y+4:], panel)
	s.content = strings.Join(lines, "\n") + "\n"
	return s
}

func strictClaudeAgentsPanelSubagent(t *testing.T, name string) string {
	t.Helper()
	recording := loadStrictClaudeAgentsPanelRecording(t)
	row := recording.RawPanel[2]
	return strings.Replace(row, "  ◯ general-purpose", "  ◯ "+name, 1)
}

func TestStrictClaudeMeasuredAgentsPanelIsDisplayData(t *testing.T) {
	recording := loadStrictClaudeAgentsPanelRecording(t)
	assertStrictClaudeAdmitted(t, strictClaudeAgentsPanelSnapshot(t, recording.RawHint, recording.RawPanel))

	// Every running subagent adds one row.
	panel := append(append([]string{}, recording.RawPanel...), strictClaudeAgentsPanelSubagent(t, "Explore"))
	assertStrictClaudeAdmitted(t, strictClaudeAgentsPanelSnapshot(t, recording.RawHint, panel))
	// The hint's count is not the panel's: Claude counts other sessions that need
	// input, so the panel also appears under the count-free literal hint and under
	// the idle-return tip.
	assertStrictClaudeAdmitted(t, strictClaudeAgentsPanelSnapshot(t, "  "+strictClaudeEmptyBypassHint, recording.RawPanel))
	tip := strictClaudeTipRow(t, "  "+strictClaudeEmptyBypassHint, "new task? /clear to save 546.6k tokens", recording.Width)
	assertStrictClaudeAdmitted(t, strictClaudeAgentsPanelSnapshot(t, tip, recording.RawPanel))
}

func TestStrictClaudeAgentsPanelRefusesEveryOtherShape(t *testing.T) {
	recording := loadStrictClaudeAgentsPanelRecording(t)
	main, subagent := recording.RawPanel[1], recording.RawPanel[2]
	panel := func(rows ...string) []string { return append([]string{""}, rows...) }
	cases := []struct {
		name, hint string
		rows       []string
		reason     string
	}{
		// Rows that do not show the panel's main row stay the budgeted reason.
		{"rows_without_the_panel", recording.RawHint, []string{"", "stray"}, "claude_empty_hint_unverified"},
		{"subagent_rows_without_main", recording.RawHint, panel(subagent), "claude_empty_hint_unverified"},
		{"main_only", recording.RawHint, panel(main), "claude_agents_panel_unverified"},
		{"more_than_bounded_agents", recording.RawHint, panel(append([]string{main}, strings.Split(strings.Repeat(subagent+"\n", 33), "\n")[:33]...)...), "claude_agents_panel_unverified"},
		{"no_separator", recording.RawHint, []string{main, subagent}, "claude_agents_panel_unverified"},
		{"main_not_first", recording.RawHint, panel(subagent, main), "claude_agents_panel_unverified"},
		// A focused or selected panel renders differently from the measured rows.
		{"main_unstyled", recording.RawHint, panel("  ● main", subagent), "claude_agents_panel_unverified"},
		{"main_inverse", recording.RawHint, panel("\x1b[7m  ● main\x1b[0m", subagent), "claude_agents_panel_unverified"},
		{"subagent_selected", recording.RawHint, panel(main, "\x1b[1m"+subagent+"\x1b[0m"), "claude_agents_panel_unverified"},
		{"subagent_pointer", recording.RawHint, panel(main, strings.Replace(subagent, "  ◯ ", "❯ ◯ ", 1)), "claude_agents_panel_unverified"},
		{"finished_glyph", recording.RawHint, panel(main, strings.Replace(subagent, "◯", "✓", 1)), "claude_agents_panel_unverified"},
		{"unmeasured_token_unit", recording.RawHint, panel(main, strings.Replace(subagent, "81.6k tokens", "1.2M tokens", 1)), "claude_agents_panel_unverified"},
		{"row_after_panel", recording.RawHint, panel(main, subagent, "", "stray"), "claude_agents_panel_unverified"},
		// Whole-pane interrupt and approval evidence still wins over the panel.
		{"interrupt_in_task_text", recording.RawHint, panel(main, strings.Replace(subagent, "Reading the fixture", "esc to interrupt", 1)), "busy_or_modal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertStrictClaudeRefused(t, strictClaudeAgentsPanelSnapshot(t, tc.hint, tc.rows), tc.reason)
		})
	}
}
