package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func strictClaudeDirectorFixture(prompt, footer string) strictSnapshot {
	return strictClaudeFixtureGeometry(prompt, footer, 141, 67, 62)
}

func strictClaudeFixtureGeometry(prompt, footer string, width, height, y int) strictSnapshot {
	lines := make([]string, height)
	lines[y-1] = strings.Repeat("─", width)
	lines[y] = prompt
	lines[y+1] = lines[y-1]
	// Private operative status/history are not copied into tests. This row is
	// synthetic mutable status data in the parent-reported reserved position.
	lines[y+2] = "fixture · Opus 5 · high ██████░░░░ 61% · ✔ Update installed · Restart to update"
	lines[y+3] = footer
	return strictSnapshot{
		identity: StrictPaneIdentity{SessionID: "$5", PaneID: "%9", PID: "123", Command: "claude", TTY: "/dev/ttys015"},
		x:        2, y: y, width: width, height: height, content: strings.Join(lines, "\n") + "\n", observedAt: time.Now(),
	}
}

func strictClaudeFixtureAttempt(t *testing.T, snapshots func() strictSnapshot, verify func(StrictPaneIdentity) error) (StrictSendResult, error, int, int, int) {
	t.Helper()
	stages, drops, submits := 0, 0, 0
	result, err := strictSendOnce("claude", "fixture payload\n", verify, strictOps{
		snapshot: func(string) (strictSnapshot, error) {
			s := snapshots()
			s.observedAt = time.Now()
			return s, nil
		},
		stage: func(body string) (string, error) {
			if body != "fixture payload\n" {
				t.Fatal("prompt bytes changed")
			}
			stages++
			return "private-fixture", nil
		},
		drop:   func(string) { drops++ },
		submit: func(string, string) error { submits++; return nil },
	})
	return result, err, stages, drops, submits
}

func TestStrictClaudeRecordedWhitespaceAndEmptyFooters(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-2.1.260-whitespace-composer.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Mode       string `json:"mode"`
			Case       string `json:"case"`
			InputEmpty bool   `json:"input_empty"`
			CursorX    int    `json:"cursor_x"`
			RawPrompt  string `json:"raw_prompt"`
			RawFooter  string `json:"raw_footer"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Mode+"/"+item.Case, func(t *testing.T) {
			// Prompt/footer bytes are real UI observations. Geometry, history and
			// native idle authority here are synthetic; this is not a live send.
			s := strictClaudeDirectorFixture(item.RawPrompt, item.RawFooter)
			s.x = item.CursorX
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			want := item.Mode == "bypass" && item.InputEmpty
			if want {
				if err != nil || stages != 1 || drops != 1 || submits != 1 || !result.Attempted || result.Delivery != "unknown" {
					t.Fatalf("empty fixture: %+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
				}
			} else if err == nil || stages != 0 || drops != 0 || submits != 0 || result.Attempted || result.Delivery != "refused" {
				t.Fatalf("draft or unsupported footer admitted: %+v err=%v stages=%d submits=%d", result, err, stages, submits)
			}
		})
	}
}

func TestStrictClaudeRecordedResizePreservesEmptyHintGuard(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-2.1.260-whitespace-composer.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Case       string `json:"case"`
			InputEmpty bool   `json:"input_empty"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			CursorX    int    `json:"cursor_x"`
			CursorY    int    `json:"cursor_y"`
			RawPrompt  string `json:"raw_prompt"`
			RawFooter  string `json:"raw_footer"`
		} `json:"resize_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Case, func(t *testing.T) {
			s := strictClaudeFixtureGeometry(item.RawPrompt, item.RawFooter, item.Width, item.Height, item.CursorY)
			s.x = item.CursorX
			result, err, stages, _, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if item.InputEmpty {
				if err != nil || stages != 1 || submits != 1 || !result.Attempted || result.Delivery != "unknown" {
					t.Fatalf("recorded empty resize refused: %+v %v", result, err)
				}
			} else if err == nil || stages != 0 || submits != 0 || result.Attempted {
				t.Fatalf("recorded SPACE+Home resize admitted: %+v %v", result, err)
			}
		})
	}
}

func TestStrictClaude268MeasuredAgentsHint(t *testing.T) {
	for _, footer := range []string{
		"⏵⏵ bypass permissions on (shift+tab to cycle) · PR #978 · ← 2 agents",
		"⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agents",
		"⏵⏵ bypass permissions on (shift+tab to cycle) · PR #1 · ← 99 agents",
		"\x1b[2m⏵⏵ bypass permissions on (shift+tab to cycle) · PR #978 · ← 2 agents\x1b[0m",
	} {
		t.Run(footer, func(t *testing.T) {
			s := strictClaudeFixtureGeometry("❯\u00a0", footer, 200, 50, 46)
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || drops != 1 || submits != 1 {
				t.Fatalf("measured 2.1.268 hint refused: result=%+v err=%v stages=%d drops=%d submits=%d",
					result, err, stages, drops, submits)
			}
		})
	}
}

func TestStrictClaude268AgentsHintCounterexamplesHaveNoEffects(t *testing.T) {
	valid := "⏵⏵ bypass permissions on (shift+tab to cycle) · PR #978 · ← 2 agents"
	invalid := map[string]string{
		"substring_prefix":  "status: " + valid,
		"substring_suffix":  valid + " busy",
		"zero_agents":       strings.Replace(valid, "2 agents", "0 agents", 1),
		"leading_zero":      strings.Replace(valid, "2 agents", "02 agents", 1),
		"huge_agents":       strings.Replace(valid, "2 agents", "100 agents", 1),
		"negative_agents":   strings.Replace(valid, "2 agents", "-2 agents", 1),
		"singular_agent":    strings.Replace(valid, "2 agents", "1 agent", 1),
		"missing_agents":    strings.TrimSuffix(valid, " agents"),
		"zero_pr":           strings.Replace(valid, "PR #978", "PR #0", 1),
		"leading_zero_pr":   strings.Replace(valid, "PR #978", "PR #0978", 1),
		"huge_pr":           strings.Replace(valid, "PR #978", "PR #1000000", 1),
		"missing_pr_number": strings.Replace(valid, "PR #978", "PR #", 1),
		"lowercase_pr":      strings.Replace(valid, "PR #978", "pr #978", 1),
		"missing_separator": strings.Replace(valid, " · PR", " PR", 1),
		"foreign_arrow":     strings.Replace(valid, "←", "<", 1),
		"for_agents_suffix": strings.Replace(valid, "2 agents", "for agents", 1),
	}
	for name, footer := range invalid {
		t.Run(name, func(t *testing.T) {
			s := strictClaudeFixtureGeometry("❯\u00a0", footer, 200, 50, 46)
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err == nil || result.Attempted || stages != 0 || drops != 0 || submits != 0 {
				t.Fatalf("malformed 2.1.268 hint admitted: result=%+v err=%v stages=%d drops=%d submits=%d",
					result, err, stages, drops, submits)
			}
		})
	}
	t.Run("draft", func(t *testing.T) {
		s := strictClaudeFixtureGeometry("❯\u00a0operator draft", valid, 200, 50, 46)
		result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
			func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
		if err == nil || result.Attempted || stages != 0 || drops != 0 || submits != 0 {
			t.Fatalf("draft with valid agents suffix admitted: result=%+v err=%v", result, err)
		}
	})
}

func TestStrictClaudeDirectorNBSPThroughCapture(t *testing.T) {
	for _, cursorFlag := range []int{1, 0} {
		t.Run(fmt.Sprintf("cursor_flag_%d", cursorFlag), func(t *testing.T) {
			s := strictClaudeDirectorFixture("❯\u00a0", strictClaudeEmptyBypassHint)
			stages, submits := 0, 0
			metadata := fmt.Sprintf("$5|%%9|123|2|62|0|0|%d|0|0|141|67|1|claude|fixture|@2|/fixture|/dev/ttys015|3.7b\n", cursorFlag)
			read := func(args ...string) ([]byte, error) {
				switch args[0] {
				case "display-message":
					return []byte(metadata), nil
				case "list-clients":
					return nil, nil
				case "capture-pane":
					return []byte(s.content), nil
				}
				return nil, errors.New("unexpected fixture probe")
			}
			result, err := strictSendOnce("claude", "fixture payload", func(StrictPaneIdentity) error { return nil }, strictOps{
				snapshot: func(pinned string) (strictSnapshot, error) {
					return captureStrictSnapshot("fixture", pinned, read, func(string) bool { return true })
				},
				stage: func(string) (string, error) { stages++; return "f", nil }, drop: func(string) {},
				submit: func(string, string) error { submits++; return nil },
			})
			if cursorFlag == 1 {
				if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || submits != 1 {
					t.Fatalf("reported visible-cursor fixture refused: %+v %v", result, err)
				}
			} else if err == nil || result.Attempted || stages != 0 || submits != 0 {
				t.Fatal("software cursor fixture bypassed the existing visibility gate")
			}
		})
	}
}

func TestStrictClaudeNativeEmptyHintCannotComeFromCustomStatus(t *testing.T) {
	s := strictClaudeDirectorFixture("❯\u00a0", strings.TrimSuffix(strictClaudeEmptyBypassHint, " · ← for agents"))
	lines := strings.Split(s.content, "\n")
	lines[64] = strictClaudeEmptyBypassHint
	s.content = strings.Join(lines, "\n")
	result, err, stages, _, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
	if err == nil || result.Attempted || stages != 0 || submits != 0 {
		t.Fatal("custom status substituted for the native empty hint")
	}
	lines[64] = "unrelated mutable status text"
	lines[65] = strictClaudeEmptyBypassHint
	s.content = strings.Join(lines, "\n")
	if reason := strictEmptyComposer("claude", s); reason != "" {
		t.Fatalf("status text itself was treated as authority: %s", reason)
	}
}

func TestStrictClaudeNBSPLayoutAndModalRefusals(t *testing.T) {
	for _, fault := range []string{"width", "height", "cursor_row", "crop", "upper_divider", "lower_divider", "empty_status", "mode_only", "hint_suffix", "shifted_hint", "last_row", "extra_nbsp", "extra_spaces", "modal"} {
		t.Run(fault, func(t *testing.T) {
			s := strictClaudeDirectorFixture("❯\u00a0", strictClaudeEmptyBypassHint)
			lines := strings.Split(s.content, "\n")
			switch fault {
			case "width":
				s.width = s.x
			case "height":
				s.height--
			case "cursor_row":
				s.y++
			case "crop":
				lines = lines[1:]
			case "upper_divider":
				lines[61] = "unknown layout"
			case "lower_divider":
				lines[63] = "wrapped draft"
			case "empty_status":
				lines[64] = ""
			case "mode_only":
				lines[65] = "⏵⏵ bypass permissions on (shift+tab to cycle)"
			case "hint_suffix":
				lines[65] += " unknown"
			case "shifted_hint":
				lines[64], lines[65] = lines[65], lines[64]
			case "last_row":
				lines[66] = "unknown footer"
			case "extra_nbsp":
				lines[62] += "\u00a0"
			case "extra_spaces":
				lines[62] += "    "
			case "modal":
				lines[60] = "Enter to confirm"
			}
			s.content = strings.Join(lines, "\n")
			result, err, stages, _, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err == nil || result.Attempted || stages != 0 || submits != 0 {
				t.Fatalf("%s admitted: %+v %v", fault, result, err)
			}
		})
	}
}

func TestStrictClaudeEmptyHintRecheckedAndNativeProofRequired(t *testing.T) {
	for _, fault := range []string{"native_busy", "second_frame_draft"} {
		t.Run(fault, func(t *testing.T) {
			s := strictClaudeDirectorFixture("❯\u00a0", strictClaudeEmptyBypassHint)
			captures := 0
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot {
				captures++
				if fault == "second_frame_draft" && captures == 2 {
					s.content = strings.Replace(s.content, " · ← for agents", "", 1)
				}
				return s
			}, func(StrictPaneIdentity) error {
				if fault == "native_busy" {
					return errors.New("native idle proof unavailable")
				}
				return nil
			})
			wantStages := 0
			if fault == "second_frame_draft" {
				wantStages = 1
			}
			if err == nil || result.Attempted || stages != wantStages || drops != wantStages || submits != 0 {
				t.Fatalf("%s: %+v err=%v stages=%d drops=%d submits=%d", fault, result, err, stages, drops, submits)
			}
		})
	}
}
