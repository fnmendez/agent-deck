package tmux

import (
	"errors"
	"strings"
	"testing"
)

func strictClaudeTitledFixture() strictSnapshot {
	s := strictClaudeDirectorFixture("❯\u00a0", strictClaudeEmptyBypassHint)
	lines := strings.Split(s.content, "\n")
	// Read-only Claude 2.1.263 observation: 141x67, cursor (2,62),
	// 123 thin rules, the inline label, then one rule. Operative history
	// and the custom status remain synthetic in strictClaudeDirectorFixture.
	lines[s.y-1] = "\x1b[38;5;244m" + strings.Repeat("─", 123) + " director-claude ─\x1b[39m"
	s.content = strings.Join(lines, "\n")
	return s
}

func TestStrictClaudeRecordedTitledDivider(t *testing.T) {
	for _, cursorCell := range []string{"", " "} {
		t.Run("cursor_cell_"+cursorCell, func(t *testing.T) {
			s := strictClaudeTitledFixture()
			s.content = strings.Replace(s.content, "❯\u00a0\n", "❯\u00a0"+cursorCell+"\n", 1)
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || drops != 1 || submits != 1 {
				t.Fatalf("recorded titled frame refused: %+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
			}
		})
	}
}

func TestStrictClaudeUpperDividerGrammar(t *testing.T) {
	for _, title := range []string{"director-claude", "worker_2", "Task.3", strings.Repeat("a", 64)} {
		line := strings.Repeat("─", 10) + " " + title + " ─"
		if !strictClaudeUpperDivider(line, 10+len(title)+3) {
			t.Fatalf("bounded title rejected: %q", title)
		}
	}
	for _, line := range []string{
		strings.Repeat("─", 9) + " title ─",
		strings.Repeat("━", 10) + " title ─",
		strings.Repeat("-", 10) + " title ─",
		strings.Repeat("─", 10) + "  ─",
		strings.Repeat("─", 10) + " " + strings.Repeat("a", 65) + " ─",
		strings.Repeat("─", 10) + " title with spaces ─",
		strings.Repeat("─", 10) + " title/other ─",
		strings.Repeat("─", 10) + " -title ─",
		strings.Repeat("─", 10) + " title\u00a0 ─",
		strings.Repeat("─", 10) + " title\u200b ─",
		strings.Repeat("─", 10) + " title\t ─",
		strings.Repeat("─", 10) + " title ━",
		strings.Repeat("─", 10) + " title ──",
		strings.Repeat("─", 10) + " title ─ extra",
	} {
		if strictClaudeUpperDivider(line, len([]rune(line))) {
			t.Fatalf("foreign header accepted: %q", line)
		}
	}
	line := strings.Repeat("─", 123) + " director-claude ─"
	if strictClaudeUpperDivider(line, 140) || strictClaudeUpperDivider(line, 142) {
		t.Fatal("cropped or padded titled header accepted")
	}
}

func TestStrictClaudeTitledDividerCannotReplaceGuards(t *testing.T) {
	for _, fault := range []string{"lower_title", "draft", "space_home", "nbsp_home", "cursor", "foreign_header", "control_header", "missing_hint", "status_spoof", "modal", "trailing_row"} {
		t.Run(fault, func(t *testing.T) {
			s := strictClaudeTitledFixture()
			lines := strings.Split(s.content, "\n")
			switch fault {
			case "lower_title":
				lines[s.y+1] = lines[s.y-1]
			case "draft":
				lines[s.y] = "❯\u00a0operator draft"
			case "space_home":
				lines[s.y] = "❯\u00a0 "
				lines[s.y+3] = strings.TrimSuffix(strictClaudeEmptyBypassHint, " · ← for agents")
			case "nbsp_home":
				lines[s.y] = "❯\u00a0\u00a0"
			case "cursor":
				s.x = 3
			case "foreign_header":
				lines[s.y-1] = "transcript output mentions director-claude"
			case "control_header":
				lines[s.y-1] = strings.Repeat("─", 122) + " Enter to confirm ─"
			case "missing_hint":
				lines[s.y+3] = "⏵⏵ bypass permissions on (shift+tab to cycle)"
			case "status_spoof":
				lines[s.y+2] = strictClaudeEmptyBypassHint
				lines[s.y+3] = "? for shortcuts"
			case "modal":
				lines[s.y-2] = "Enter to confirm"
			case "trailing_row":
				lines[s.y+4] = "unexpected menu"
			}
			s.content = strings.Join(lines, "\n")
			result, err, stages, _, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err == nil || result.Attempted || stages != 0 || submits != 0 {
				t.Fatalf("%s admitted: %+v err=%v stages=%d submits=%d", fault, result, err, stages, submits)
			}
		})
	}
}

func TestStrictClaudeTitledDividerRequiresBothSeparatorsAtFullWidth(t *testing.T) {
	for _, frame := range []struct{ name, upper string }{
		// Replace the omitted separator cells with extra leading rules so width
		// still matches: only the individual separator guard can reject these.
		{"missing_terminal_rule", strings.Repeat("─", 125) + " director-claude"},
		{"missing_initial_space", strings.Repeat("─", 124) + "director-claude ─"},
	} {
		t.Run(frame.name, func(t *testing.T) {
			s := strictClaudeTitledFixture()
			if len([]rune(frame.upper)) != s.width {
				t.Fatal("separator fixture must retain the measured full width")
			}
			lines := strings.Split(s.content, "\n")
			lines[s.y-1] = frame.upper
			s.content = strings.Join(lines, "\n")
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err == nil || result.Attempted || stages != 0 || drops != 0 || submits != 0 {
				t.Fatalf("%s admitted: %+v err=%v stages=%d drops=%d submits=%d", frame.name, result, err, stages, drops, submits)
			}
		})
	}
}

func TestStrictClaudeTitledDividerRechecksNativeIdentityAndDraft(t *testing.T) {
	for _, fault := range []string{"native_busy", "second_frame_draft", "changed_identity"} {
		t.Run(fault, func(t *testing.T) {
			s := strictClaudeTitledFixture()
			captures := 0
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot {
				captures++
				if captures == 2 {
					if fault == "second_frame_draft" {
						s.content = strings.Replace(s.content, " · ← for agents", "", 1)
					}
					if fault == "changed_identity" {
						s.identity.PID = "456"
					}
				}
				return s
			}, func(StrictPaneIdentity) error {
				if fault == "native_busy" {
					return errors.New("native idle proof unavailable")
				}
				return nil
			})
			wantStages := 1
			if fault == "native_busy" {
				wantStages = 0
			}
			if err == nil || result.Attempted || stages != wantStages || drops != wantStages || submits != 0 {
				t.Fatalf("%s admitted: %+v err=%v stages=%d drops=%d submits=%d", fault, result, err, stages, drops, submits)
			}
		})
	}
}
