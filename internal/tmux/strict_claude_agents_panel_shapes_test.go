package tmux

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type strictClaudePanelShape struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	CursorX int      `json:"cursor_x"`
	CursorY int      `json:"cursor_y"`
	Top     int      `json:"top"`
	Rows    []string `json:"rows"`
	Reason  string   `json:"reason"`
}

func loadStrictClaudePanelShapes(t *testing.T) map[string]strictClaudePanelShape {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.277-agents-panel-shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []strictClaudePanelShape `json:"cases"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	shapes := map[string]strictClaudePanelShape{}
	for _, shape := range doc.Cases {
		shapes[shape.Name] = shape
	}
	return shapes
}

// snapshot renders the recorded rows at their recorded position with blank rows
// above (no transcript), at the recorded cursor.
func (shape strictClaudePanelShape) snapshot(t *testing.T) strictSnapshot {
	t.Helper()
	if shape.Top+len(shape.Rows) != shape.Height {
		t.Fatalf("%s: rows do not end at the pane bottom", shape.Name)
	}
	lines := make([]string, shape.Height)
	copy(lines[shape.Top:], shape.Rows)
	return strictSnapshot{
		identity: StrictPaneIdentity{SessionID: "$5", PaneID: "%9", PID: "123", Command: "claude", TTY: "/dev/ttys015"},
		x:        shape.CursorX, y: shape.CursorY, width: shape.Width, height: shape.Height,
		content: strings.Join(lines, "\n") + "\n", observedAt: time.Now(),
	}
}

// withRow returns a copy with the row at offset i (from the upper divider)
// replaced, and the cursor placed on the recorded composer row.
func (shape strictClaudePanelShape) withRow(i int, row string) strictClaudePanelShape {
	shape.Rows = append([]string{}, shape.Rows...)
	shape.Rows[i] = row
	shape.CursorX, shape.CursorY = 2, shape.Top+1
	return shape
}

const (
	strictClaudeShapeHintRow = 4
	strictClaudeShapePanel   = 6
)

func TestStrictClaudeMeasuredPanelShapes(t *testing.T) {
	shapes := loadStrictClaudePanelShapes(t)
	if len(shapes) != 17 {
		t.Fatalf("fixture has %d shapes, want 17", len(shapes))
	}
	for _, shape := range shapes {
		t.Run(shape.Name, func(t *testing.T) {
			if shape.Reason == "" {
				assertStrictClaudeAdmitted(t, shape.snapshot(t))
			} else {
				assertStrictClaudeRefused(t, shape.snapshot(t), shape.Reason)
			}
		})
	}
}

// A focused footer or panel is refused even if the cursor were on the composer:
// the navigation hint replaces the native hint, and the focused rows fail the
// panel recognizer under a valid hint.
func TestStrictClaudeFocusedPanelRefusedAtEveryLayer(t *testing.T) {
	shapes := loadStrictClaudePanelShapes(t)
	validHint := shapes["cycle_form_panel"].Rows[strictClaudeShapeHintRow]
	for _, name := range []string{"focus_footer_pill", "focus_panel_main", "focus_panel_subagent"} {
		shape := shapes[name]
		t.Run(name+"/cursor_on_composer", func(t *testing.T) {
			onComposer := shape.withRow(strictClaudeShapeHintRow, shape.Rows[strictClaudeShapeHintRow])
			assertStrictClaudeRefused(t, onComposer.snapshot(t), "claude_empty_hint_unverified")
		})
		if name == "focus_footer_pill" {
			continue // Its panel rows are the unfocused ones; the pill lives in the hint.
		}
		t.Run(name+"/valid_hint", func(t *testing.T) {
			assertStrictClaudeRefused(t, shape.withRow(strictClaudeShapeHintRow, validHint).snapshot(t),
				"claude_agents_panel_unverified")
		})
	}
}

// Mutation guard: each case is one step wider than a measured admission and must
// stay refused. Widening the token arrow, the background segment or the tasks tip
// makes one of these fail.
func TestStrictClaudePanelShapesRefuseWiderVariants(t *testing.T) {
	shapes := loadStrictClaudePanelShapes(t)
	up := shapes["up_arrow_token_counter"]
	upRow := up.Rows[strictClaudeShapePanel+1]
	monitor := shapes["monitor_segment_tasks_tip"]
	monitorHint := monitor.Rows[strictClaudeShapeHintRow]
	both := shapes["shell_and_monitor_segment"]
	bothHint := both.Rows[strictClaudeShapeHintRow]
	nested := shapes["nested_subagent_count"]
	nestedRow := nested.Rows[strictClaudeShapePanel+1]
	tasks := shapes["tasks_tip_cycle_form"]
	tasksHint := tasks.Rows[strictClaudeShapeHintRow]
	if !strings.Contains(upRow, " · ↑ ") || !strings.Contains(monitorHint, "1 monitor") ||
		!strings.Contains(bothHint, "1 shell, 1 monitor") || !strings.Contains(tasksHint, " · /tasks to see subagents · ") {
		t.Fatal("fixture rows moved")
	}
	cases := []struct {
		name   string
		shape  strictClaudePanelShape
		reason string
	}{
		{"arrow_right", up.withRow(strictClaudeShapePanel+1, strings.Replace(upRow, " · ↑ ", " · → ", 1)), "claude_agents_panel_unverified"},
		{"arrow_both", up.withRow(strictClaudeShapePanel+1, strings.Replace(upRow, " · ↑ ", " · ↑↓ ", 1)), "claude_agents_panel_unverified"},
		{"arrow_missing", up.withRow(strictClaudeShapePanel+1, strings.Replace(upRow, " · ↑ ", " · ", 1)), "claude_agents_panel_unverified"},
		{"nested_zero", nested.withRow(strictClaudeShapePanel+1, strings.Replace(nestedRow, " (+1)", " (+0)", 1)), "claude_agents_panel_unverified"},
		{"nested_no_plus", nested.withRow(strictClaudeShapePanel+1, strings.Replace(nestedRow, " (+1)", " (1)", 1)), "claude_agents_panel_unverified"},
		{"nested_free_text", nested.withRow(strictClaudeShapePanel+1, strings.Replace(nestedRow, " (+1)", " (+1 more)", 1)), "claude_agents_panel_unverified"},
		{"monitor_bad_plural", monitor.withRow(strictClaudeShapeHintRow, strings.Replace(monitorHint, "1 monitor", "1 monitors", 1)), "claude_empty_hint_unverified"},
		{"monitor_zero", monitor.withRow(strictClaudeShapeHintRow, strings.Replace(monitorHint, "1 monitor", "0 monitors", 1)), "claude_empty_hint_unverified"},
		{"monitor_then_shell", both.withRow(strictClaudeShapeHintRow, strings.Replace(bothHint, "1 shell, 1 monitor", "1 monitor, 1 shell", 1)), "claude_empty_hint_unverified"},
		{"shell_dot_monitor", both.withRow(strictClaudeShapeHintRow, strings.Replace(bothHint, "1 shell, 1 monitor", "1 shell · 1 monitor", 1)), "claude_empty_hint_unverified"},
		{"unknown_background_kind", monitor.withRow(strictClaudeShapeHintRow, strings.Replace(monitorHint, "1 monitor", "1 task", 1)), "claude_empty_hint_unverified"},
		{"cycle_and_monitor", tasks.withRow(strictClaudeShapeHintRow, strings.Replace(tasksHint, " · /tasks to see subagents", " · 1 monitor", 1)), "claude_empty_hint_unverified"},
		{"tasks_tip_reworded", tasks.withRow(strictClaudeShapeHintRow, strings.Replace(tasksHint, "/tasks to see subagents", "/tasks to see agents", 1)), "claude_empty_hint_unverified"},
		{"tasks_tip_twice", tasks.withRow(strictClaudeShapeHintRow, strings.Replace(tasksHint, " · /tasks to see subagents", " · /tasks to see subagents · /tasks to see subagents", 1)), "claude_empty_hint_unverified"},
		{"tasks_tip_without_suffix", tasks.withRow(strictClaudeShapeHintRow, strings.Replace(tasksHint, " · ← 2 agents", "", 1)), "claude_empty_hint_unverified"},
		{"monitor_without_suffix", monitor.withRow(strictClaudeShapeHintRow, strings.Replace(monitorHint, " · /tasks to see subagents · ← 2 agents", "", 1)), "claude_empty_hint_unverified"},
		{"tasks_tip_after_suffix", tasks.withRow(strictClaudeShapeHintRow, strings.Replace(tasksHint, " · /tasks to see subagents · ← 2 agents", " · ← 2 agents · /tasks to see subagents", 1)), "claude_empty_hint_unverified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertStrictClaudeRefused(t, tc.shape.snapshot(t), tc.reason)
		})
	}
}
