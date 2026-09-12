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

// F-005: the transcript quoted a reviewer's approval menu while the live
// Director Claude UI was idle. This synthetic history keeps the quoted menu on
// the reported visible row 1; operative history was not captured.
const strictClaudeF005QuotedMenu = "  quoting the reviewer's `❯ 1. Yes` from that approval prompt"

// Menu, approval and interrupt phrases render only in live UI below the
// transcript. Cancel hints stay whole-pane evidence (see the usage-limit test).
var strictLiveRegionIndicators = []string{
	"esc to interrupt", "ctrl+c to interrupt", "Do you want to proceed?", "Would you like to continue?",
	"Enter to confirm", "Allow once", "Approval required", "Select an option", "❯ 1. Yes", "› 1. Yes",
}

var strictWholePaneIndicators = []string{"Esc to cancel", "Escape to cancel"}

type strictClaudeFableRecording struct {
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	CursorX  int      `json:"cursor_x"`
	CursorY  int      `json:"cursor_y"`
	StartRow int      `json:"start_row"`
	Rows     []string `json:"rows"`
}

func loadStrictClaudeFableRecording(t *testing.T) strictClaudeFableRecording {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.263-fable-update-installed-idle.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording strictClaudeFableRecording
	if err := json.Unmarshal(data, &recording); err != nil {
		t.Fatal(err)
	}
	if recording.StartRow != recording.CursorY-3 || recording.StartRow+len(recording.Rows) != recording.Height {
		t.Fatalf("recorded geometry inconsistent: %+v", recording)
	}
	return recording
}

// strictClaudeF005Frame places the recorded completed-turn row, composer and
// footer with the cursor on row y, and fills every row above them with
// synthetic transcript history. Row 1 carries the quoted menu.
func strictClaudeF005Frame(t *testing.T, y int) strictSnapshot {
	t.Helper()
	recording := loadStrictClaudeFableRecording(t)
	lines := make([]string, recording.Height)
	for i := 0; i < y-3; i++ {
		lines[i] = fmt.Sprintf("  synthetic transcript history row %d", i)
	}
	lines[1] = strictClaudeF005QuotedMenu
	for i, row := range recording.Rows {
		if y-3+i < len(lines) {
			lines[y-3+i] = row
		}
	}
	return strictSnapshot{
		identity: StrictPaneIdentity{SessionID: "$2", PaneID: "%2", PID: "7861", Command: "2.1.263", TTY: "/dev/ttys004"},
		x:        recording.CursorX, y: y, width: recording.Width, height: recording.Height,
		content: strings.Join(lines, "\n") + "\n", observedAt: time.Now(),
	}
}

func strictSetRow(s *strictSnapshot, row int, value string) {
	lines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
	lines[row] = value
	s.content = strings.Join(lines, "\n") + "\n"
}

func strictFrameThroughCapture(s strictSnapshot, version string) func(string) (strictSnapshot, error) {
	metadata := fmt.Sprintf("%s|%s|%s|%d|%d|0|0|1|0|0|%d|%d|1|%s|agentdeck_director-claude_0c7acab5|@2|/fixture|%s|%s\n",
		s.identity.SessionID, s.identity.PaneID, s.identity.PID, s.x, s.y, s.width, s.height, s.identity.Command, s.identity.TTY, version)
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
	return func(pinned string) (strictSnapshot, error) {
		return captureStrictSnapshot("agentdeck_director-claude_0c7acab5", pinned, read, func(string) bool { return true })
	}
}

func TestStrictClaudeF005ScrollbackMenuQuoteThroughCapture(t *testing.T) {
	recording := loadStrictClaudeFableRecording(t)
	// The recorded frame (cursor row 62) and the F-005 report (cursor row 61,
	// two trailing blank rows) differ only by one trailing blank row.
	for _, y := range []int{recording.CursorY, recording.CursorY - 1} {
		t.Run(fmt.Sprintf("cursor_row_%d", y), func(t *testing.T) {
			s := strictClaudeF005Frame(t, y)
			lines := strings.Split(StripANSI(s.content), "\n")
			if !strings.Contains(lines[1], "❯ 1. Yes") || !strings.Contains(lines[y+2], "Fable 5.1") ||
				!strings.Contains(lines[y+2], "✔ Update installed · Restart to update") || lines[y+3] != "  "+strictClaudeEmptyBypassHint {
				t.Fatal("fixture no longer reproduces the F-005 screen")
			}
			assertStrictClaudeAdmitted(t, s)
			snapshot := strictFrameThroughCapture(s, "3.7b")
			if _, err := strictProbeIdentity("claude", snapshot); err != nil {
				t.Fatalf("read-only probe refused the idle F-005 screen: %v", err)
			}
			stages, submits := 0, 0
			result, err := strictSendOnce("claude", "fixture payload", func(StrictPaneIdentity) error { return nil }, strictOps{
				snapshot: snapshot,
				stage:    func(string) (string, error) { stages++; return "f", nil }, drop: func(string) {},
				submit: func(string, string) error { submits++; return nil },
			})
			if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || submits != 1 {
				t.Fatalf("F-005 screen refused through capture: %+v %v stages=%d submits=%d", result, err, stages, submits)
			}
		})
	}
}

func TestStrictClaudeTranscriptAboveCompletedTurnIsNotIndicatorEvidence(t *testing.T) {
	for _, phrase := range strictLiveRegionIndicators {
		// Row 58 is the last transcript row, directly above the completed turn.
		for _, row := range []int{0, 1, 30, 58} {
			t.Run(fmt.Sprintf("%s/row_%d", phrase, row), func(t *testing.T) {
				s := strictClaudeF005Frame(t, 62)
				strictSetRow(&s, row, "  transcript quoted: "+phrase)
				assertStrictClaudeAdmitted(t, s)
			})
		}
	}
}

// Claude's armed usage-limit auto-resume countdown is live UI inside the
// transcript: the rate-limited turn still appends its completed-turn row after
// it, and the countdown keeps updating above that row until it resumes.
func TestStrictClaudeUsageLimitCountdownAboveCompletedTurnRefuses(t *testing.T) {
	const y = 62
	rows := []string{
		"Continuing automatically at 3:00 PM · esc to cancel",
		"Continuing shortly · esc to cancel",
		"Continuing automatically when your limit resets · esc to cancel",
		"Usage limit reached · continuing shortly · esc to cancel",
		"Usage limit reached · continuing automatically at 3:00 PM · esc to cancel",
		"Usage limit reached · continuing automatically when it resets · esc to cancel",
	}
	for _, countdown := range rows {
		for _, row := range []int{1, 56, 58} {
			for _, prefix := range []string{"", "  ⎿  "} {
				t.Run(fmt.Sprintf("%s/row_%d/%q", countdown, row, prefix), func(t *testing.T) {
					s := strictClaudeF005Frame(t, y)
					strictSetRow(&s, 1, "  synthetic transcript history row 1")
					strictSetRow(&s, y-3, "✻ Worked for 45s · done 1:31 AM")
					strictSetRow(&s, row, prefix+countdown)
					assertStrictClaudeRefused(t, s, "busy_or_modal")
				})
			}
		}
	}
	for _, phrase := range strictWholePaneIndicators {
		t.Run("transcript_"+phrase, func(t *testing.T) {
			s := strictClaudeF005Frame(t, y)
			strictSetRow(&s, 1, "  synthetic transcript history row 1")
			strictSetRow(&s, 30, "  transcript quoted: "+phrase)
			assertStrictClaudeRefused(t, s, "busy_or_modal")
		})
	}
	t.Run("control_without_countdown", func(t *testing.T) {
		s := strictClaudeF005Frame(t, y)
		strictSetRow(&s, y-3, "✻ Worked for 45s · done 1:31 AM")
		strictSetRow(&s, 56, "  Claude usage limit reached; it resumed and finished this turn.")
		assertStrictClaudeAdmitted(t, s)
	})
}

func TestStrictClaudeLiveRegionStillRefusesIndicators(t *testing.T) {
	const y = 62
	for _, phrase := range append(append([]string{}, strictLiveRegionIndicators...), strictWholePaneIndicators...) {
		for name, row := range map[string]int{"turn_row": y - 3, "gap": y - 2, "status": y + 2, "hint": y + 3, "trailing": y + 4} {
			t.Run(name+"/"+phrase, func(t *testing.T) {
				s := strictClaudeF005Frame(t, y)
				lines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
				value := phrase
				if name == "turn_row" || name == "status" || name == "hint" {
					value = lines[row] + " " + phrase
				}
				strictSetRow(&s, row, value)
				assertStrictClaudeRefused(t, s, "busy_or_modal")
			})
		}
	}
	// Claude styles key hints per word. Matching reads visible text, so a phrase
	// split by SGR runs still refuses; the status row has no other validator.
	for name, row := range map[string]int{"status": y + 2, "hint": y + 3} {
		for _, phrase := range []string{"\x1b[1mEsc\x1b[22m to interrupt", "\x1b[1mctrl+c\x1b[22m to interrupt",
			"Do \x1b[2myou\x1b[0m want to proceed?", "\x1b[38;5;246m❯\x1b[39m 1. Yes", "\x1b[1mEsc\x1b[22m to cancel"} {
			t.Run("styled_"+name+"/"+phrase, func(t *testing.T) {
				s := strictClaudeF005Frame(t, y)
				lines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
				strictSetRow(&s, row, lines[row]+" "+phrase)
				assertStrictClaudeRefused(t, s, "busy_or_modal")
			})
		}
	}
}

func TestStrictClaudeLiveRegionRequiresExactCompletedTurnRow(t *testing.T) {
	const y = 62
	rows := map[string]string{
		"spinner_same_glyph":       "✻ Cascading… (3s · ↓ 114 tokens)",
		"spinner_other_glyph":      "✽ Shimmying… (3s · ↓ 114 tokens)",
		"spinner_no_counter":       "✶ Shimmying…",
		"spinner_ascii_glyph":      "* Cascading… (3s · ↓ 114 tokens)",
		"spinner_dot_glyph":        "· Bootstrapping… (3s · ↓ 114 tokens)",
		"retry_status":             "✢ Waiting for API response · next try in 5s · attempt 2",
		"tip_tree":                 "  ⎿  Tip: Use /btw to ask a quick side question without interrupting Claude's current work",
		"pending_agents":           "✻ Waiting for 1 background agent to finish",
		"monitor_still_running":    "✻ Baked for 24s · done 1:31 AM · 1 monitor still running",
		"shell_monitor_running":    "✻ Baked for 24s · done 1:31 AM · 1 shell, 1 monitor still running",
		"hidden_messages":          "✻ Baked for 24s · done 1:31 AM · 2 messages hidden (/focus to show)",
		"unknown_verb":             "✻ Frosted for 24s · done 1:31 AM",
		"lowercase_verb":           "✻ baked for 24s · done 1:31 AM",
		"gerund_verb":              "✻ Baking for 24s · done 1:31 AM",
		"decomposed_verb":          "✻ Saute\u0301ed for 24s · done 1:31 AM",
		"other_glyph":              "✢ Baked for 24s · done 1:31 AM",
		"missing_glyph":            "Baked for 24s · done 1:31 AM",
		"glyph_without_space":      "✻Baked for 24s · done 1:31 AM",
		"glyph_two_spaces":         "✻  Baked for 24s · done 1:31 AM",
		"nbsp_after_glyph":         "✻\u00a0Baked for 24s · done 1:31 AM",
		"indented":                 "  ✻ Baked for 24s · done 1:31 AM",
		"quoted_in_prose":          "  the reviewer saw ✻ Baked for 24s · done 1:31 AM",
		"trailing_text":            "✻ Baked for 24s · done 1:31 AM now",
		"trailing_separator":       "✻ Baked for 24s · done 1:31 AM ·",
		"other_separator":          "✻ Baked for 24s • done 1:31 AM",
		"missing_for":              "✻ Baked 24s · done 1:31 AM",
		"fractional_seconds":       "✻ Baked for 0.0s · done 1:31 AM",
		"decimal_seconds":          "✻ Baked for 24.0s · done 1:31 AM",
		"leading_zero_seconds":     "✻ Baked for 024s · done 1:31 AM",
		"sixty_seconds":            "✻ Baked for 60s · done 1:31 AM",
		"minute_without_seconds":   "✻ Baked for 1m · done 1:31 AM",
		"zero_minutes":             "✻ Baked for 0m 24s · done 1:31 AM",
		"sixty_minutes":            "✻ Baked for 60m 0s · done 1:31 AM",
		"minute_sixty_seconds":     "✻ Baked for 1m 60s · done 1:31 AM",
		"minute_padded_seconds":    "✻ Baked for 1m 05s · done 1:31 AM",
		"hour_without_seconds":     "✻ Baked for 1h 2m · done 1:31 AM",
		"twenty_four_hours":        "✻ Baked for 24h 0m 0s · done 1:31 AM",
		"zero_hours":               "✻ Baked for 0h 1m 0s · done 1:31 AM",
		"day_with_seconds":         "✻ Baked for 1d 0h 0m 0s · done 1:31 AM",
		"day_twenty_four_hours":    "✻ Baked for 1d 24h 0m · done 1:31 AM",
		"zero_days":                "✻ Baked for 0d 1h 0m · done 1:31 AM",
		"negative_duration":        "✻ Baked for -5s · done 1:31 AM",
		"compact_duration":         "✻ Baked for 1m24s · done 1:31 AM",
		"twenty_four_hour_time":    "✻ Baked for 24s · done 13:31",
		"lowercase_meridiem":       "✻ Baked for 24s · done 1:31 am",
		"narrow_nbsp_meridiem":     "✻ Baked for 24s · done 1:31\u202fAM",
		"padded_hour":              "✻ Baked for 24s · done 01:31 AM",
		"zero_hour":                "✻ Baked for 24s · done 0:31 AM",
		"thirteen_hour":            "✻ Baked for 24s · done 13:31 PM",
		"single_digit_minute":      "✻ Baked for 24s · done 1:3 AM",
		"sixty_minute_time":        "✻ Baked for 24s · done 1:60 AM",
		"weekday_time":             "✻ Baked for 24s · done Saturday 1:31 AM",
		"dated_time":               "✻ Baked for 24s · done Saturday, Sep 12, 1:31 AM",
		"missing_time":             "✻ Baked for 24s · done",
		"budget_suffix":            "✻ Baked for 24s · done 1:31 AM · 20k used (10k min)",
		"completed_row_with_esc":   "✻ Baked for 24s · done 1:31 AM esc to interrupt",
		"cursor_escape_inside":     "✻ Baked\x1b[1Cfor 24s · done 1:31 AM",
		"cursor_escape_spacing":    "✻ Baked\x1b[1C for 24s · done 1:31 AM",
		"erase_escape_inside":      "✻ Baked for 24s\x1b[K · done 1:31 AM",
		"osc_hyperlink_inside":     "✻ Baked for \x1b]8;;https://example.invalid\x07" + "24s\x1b]8;;\x07 · done 1:31 AM",
		"control_character_inside": "✻ Baked for 24s\x00 · done 1:31 AM",
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			s := strictClaudeF005Frame(t, y)
			strictSetRow(&s, y-3, row)
			// Without an exact completed-turn row the whole visible pane remains
			// evidence, so the quoted transcript menu refuses as before.
			assertStrictClaudeRefused(t, s, "busy_or_modal")
		})
	}
}

func TestStrictClaudeLiveRegionRequiresOneBlankRowAboveComposer(t *testing.T) {
	const y = 62
	turn := "✻ Baked for 24s · done 1:31 AM"
	for name, mutate := range map[string]func(*strictSnapshot){
		"gap_tip_row": func(s *strictSnapshot) { strictSetRow(s, y-2, "  ⎿  Tip: Run /install-github-app") },
		"gap_spinner": func(s *strictSnapshot) { strictSetRow(s, y-2, "✽ Shimmying… (3s · ↓ 114 tokens)") },
		"gap_nbsp":    func(s *strictSnapshot) { strictSetRow(s, y-2, "\u00a0") },
		"gap_tab":     func(s *strictSnapshot) { strictSetRow(s, y-2, "\t") },
		"no_gap": func(s *strictSnapshot) {
			strictSetRow(s, y-3, "  synthetic transcript history row 59")
			strictSetRow(s, y-2, turn)
		},
		"two_gap_rows": func(s *strictSnapshot) {
			strictSetRow(s, y-4, turn)
			strictSetRow(s, y-3, "")
		},
		"live_block_below_turn": func(s *strictSnapshot) {
			strictSetRow(s, y-5, turn)
			strictSetRow(s, y-4, "")
			strictSetRow(s, y-3, "✽ Shimmying… (3s · ↓ 114 tokens)")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := strictClaudeF005Frame(t, y)
			mutate(&s)
			assertStrictClaudeRefused(t, s, "busy_or_modal")
		})
	}
	t.Run("styled_blank_gap_control", func(t *testing.T) {
		s := strictClaudeF005Frame(t, y)
		strictSetRow(&s, y-2, "\x1b[39m  \x1b[0m")
		assertStrictClaudeAdmitted(t, s)
	})
}

// capture-pane -e never splits an escape across rows. If malformed bytes join
// two raw rows, raw and visible rows no longer align, and the completed-turn
// proof on a raw row cannot vouch for the visible row above the gap.
func TestStrictClaudeLiveRegionRequiresAlignedRawRows(t *testing.T) {
	const y = 62
	s := strictClaudeF005Frame(t, y)
	lines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
	raw := append([]string{"\x1b]8;;https://example.invalid", "\x07" + lines[1]}, lines[2:y-3]...)
	raw = append(raw, lines[y-3], "  synthetic transcript history row 59")
	raw = append(raw, lines[y-2:]...)
	s.content = strings.Join(raw, "\n") + "\n"
	visible := strings.Split(strings.TrimSuffix(StripANSI(s.content), "\n"), "\n")
	if len(raw) != s.height+1 || len(visible) != s.height || strictClaudeLiveRegionStart(s, visible, visible) != 0 ||
		!strings.Contains(visible[0], "❯ 1. Yes") || !strictClaudeNBSPPrompt(visible[y]) {
		t.Fatal("fixture must shift visible rows by one while keeping the composer on the cursor row")
	}
	assertStrictClaudeRefused(t, s, "busy_or_modal")
}

func TestStrictClaudeLiveRegionAdmitsSourceDerivedTurnRows(t *testing.T) {
	const y = 62
	verbs := []string{"Baked", "Brewed", "Churned", "Cogitated", "Cooked", "Crunched", "Sautéed", "Worked"}
	durations := []string{"0s", "9s", "24s", "59s", "1m 0s", "3m 17s", "59m 59s", "1h 0m 0s", "23h 59m 59s", "1d 0h 0m", "999d 23h 59m"}
	times := []string{"", " · done 1:31 AM", " · done 12:00 AM", " · done 9:05 PM", " · done 12:59 PM", " · done 10:50 PM"}
	for _, verb := range verbs {
		for _, duration := range durations {
			for _, done := range times {
				row := "✻ " + verb + " for " + duration + done
				t.Run(row, func(t *testing.T) {
					s := strictClaudeF005Frame(t, y)
					strictSetRow(&s, y-3, row)
					assertStrictClaudeAdmitted(t, s)
					// Styling and blank cells beyond the capture edge are display data.
					strictSetRow(&s, y-3, "\x1b[38;5;246m✻\x1b[39m \x1b[38;5;246m"+strings.TrimPrefix(row, "✻ ")+"\x1b[39m  ")
					assertStrictClaudeAdmitted(t, s)
				})
			}
		}
	}
}

func TestStrictClaudeLiveRegionKeepsDraftTitleAndFooterGuards(t *testing.T) {
	const y = 62
	for name, tc := range map[string]struct {
		mutate func(*strictSnapshot)
		reason string
	}{
		"text_draft":       {func(s *strictSnapshot) { strictSetRow(s, y, "❯\u00a0operator draft") }, "composer_not_empty_or_cursor_misplaced"},
		"nbsp_draft":       {func(s *strictSnapshot) { strictSetRow(s, y, "❯\u00a0\u00a0") }, "composer_not_empty_or_cursor_misplaced"},
		"cursor_moved":     {func(s *strictSnapshot) { s.x = 3 }, "composer_not_empty_or_cursor_misplaced"},
		"space_draft_home": {func(s *strictSnapshot) { strictSetRow(s, y+3, "  "+strictClaudeTestModeOnly) }, "claude_empty_hint_unverified"},
		"missing_hint":     {func(s *strictSnapshot) { strictSetRow(s, y+3, "") }, "claude_empty_hint_unverified"},
		"trailing_row":     {func(s *strictSnapshot) { strictSetRow(s, y+4, "unexpected menu") }, "claude_empty_hint_unverified"},
		"empty_status":     {func(s *strictSnapshot) { strictSetRow(s, y+2, "") }, "claude_status_layout_unknown"},
		"foreign_header":   {func(s *strictSnapshot) { strictSetRow(s, y-1, "transcript output mentions director-claude") }, "composer_layout_unknown"},
		"lower_title":      {func(s *strictSnapshot) { strictSetRow(s, y+1, strings.Repeat("─", 170)+" director-claude ─") }, "composer_layout_unknown"},
		"modal_title":      {func(s *strictSnapshot) { strictSetRow(s, y-1, strings.Repeat("─", 165)+" Enter to confirm ─") }, "busy_or_modal"},
		"height":           {func(s *strictSnapshot) { s.height-- }, "composer_geometry_unverified"},
		"crop": {func(s *strictSnapshot) {
			s.content = s.content[strings.IndexByte(s.content, '\n')+1:]
			s.y--
		}, "composer_geometry_unverified"},
	} {
		t.Run(name, func(t *testing.T) {
			s := strictClaudeF005Frame(t, y)
			tc.mutate(&s)
			assertStrictClaudeRefused(t, s, tc.reason)
		})
	}
}

func TestStrictClaudeLiveRegionCrossPassMutation(t *testing.T) {
	const y = 62
	cases := []struct {
		name     string
		row, to  string
		admitted bool
	}{
		{"turn_starts", "", "✽ Shimmying… (3s · ↓ 114 tokens)", false},
		{"background_agent_pending", "", "✻ Waiting for 1 background agent to finish", false},
		{"shell_starts", "", "✻ Baked for 24s · done 1:31 AM · 1 shell still running", false},
		{"menu_above_composer", "", "Do you want to proceed?", false},
		{"next_turn_completes", "", "✻ Worked for 3m 17s · done 1:35 AM", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := strictClaudeF005Frame(t, y)
			second := strictClaudeF005Frame(t, y)
			strictSetRow(&second, y-3, tc.to)
			captures := 0
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t, func() strictSnapshot {
				captures++
				if captures == 1 {
					return first
				}
				return second
			}, func(StrictPaneIdentity) error { return nil })
			if tc.admitted {
				if err != nil || !result.Attempted || stages != 1 || drops != 1 || submits != 1 {
					t.Fatalf("valid frames refused: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
				}
			} else if err == nil || result.Attempted || result.Reason != "busy_or_modal" || stages != 1 || drops != 1 || submits != 0 {
				t.Fatalf("live transition reached submit: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
			}
			passes := 0
			observe := func(string) (strictSnapshot, error) {
				passes++
				if passes == 1 {
					return first, nil
				}
				return second, nil
			}
			if _, err := strictProbeIdentity("claude", observe); err != nil {
				t.Fatalf("first probe pass refused the idle frame: %v", err)
			}
			_, err = strictProbeIdentity("claude", observe)
			if tc.admitted != (err == nil) {
				t.Fatalf("second probe pass admitted=%v err=%v", tc.admitted, err)
			}
			var composer StrictComposerError
			if !tc.admitted && (!errors.As(err, &composer) || composer.Reason != "busy_or_modal") {
				t.Fatalf("probe hid the composer reason: %v", err)
			}
		})
	}
}

// The measured completed-turn boundary belongs only to the Claude bypass UI.
// Codex and the legacy Claude footer keep whole-pane indicator evidence.
func TestStrictLiveRegionIsLimitedToClaudeBypassComposer(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		s := strictRecordedCodexComposer(t)
		lines := strings.Split(s.content, "\n")
		lines[60] = "✻ Baked for 24s · done 1:31 AM"
		lines[0] = strictClaudeF005QuotedMenu
		s.content = strings.Join(lines, "\n")
		if got := strictEmptyComposer("codex", s); got != "busy_or_modal" {
			t.Fatalf("codex reason=%q", got)
		}
	})
	t.Run("legacy_claude_footer", func(t *testing.T) {
		s := strictTestPane("claude")
		s.content = strictClaudeF005QuotedMenu + "\n✻ Baked for 24s · done 1:31 AM\n\n────────────\n❯ \n────────────\n? for shortcuts\n"
		s.y = 4
		if got := strictEmptyComposer("claude", s); got != "busy_or_modal" {
			t.Fatalf("legacy claude reason=%q", got)
		}
	})
}

func TestStrictProbeReportsClosedComposerReason(t *testing.T) {
	s := strictClaudeF005Frame(t, 62)
	strictSetRow(&s, 59, "✽ Shimmying… (3s · ↓ 114 tokens)")
	_, err := strictProbeIdentity("claude", func(string) (strictSnapshot, error) { return s, nil })
	var composer StrictComposerError
	if !errors.As(err, &composer) || composer.Reason != "busy_or_modal" || err.Error() != "strict composer unavailable: busy_or_modal" {
		t.Fatalf("probe composer error=%v", err)
	}
	// Observation failures before the composer guard are not composer reasons.
	_, err = strictProbeIdentity("claude", func(string) (strictSnapshot, error) {
		return strictSnapshot{}, errors.New("pane changed during capture")
	})
	if err == nil || errors.As(err, &composer) {
		t.Fatalf("capture failure became a composer reason: %v", err)
	}
}
