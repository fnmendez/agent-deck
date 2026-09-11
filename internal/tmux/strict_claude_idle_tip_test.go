package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	strictClaudeTestNBSPPrompt  = "❯\xc2\xa0"
	strictClaudeMeasuredIdleTip = "new task? /clear to save 546.6k tokens"
	strictClaudeTestModeOnly    = "⏵⏵ bypass permissions on (shift+tab to cycle)"
)

type strictClaudeIdleTipRecording struct {
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CursorX   int    `json:"cursor_x"`
	CursorY   int    `json:"cursor_y"`
	RawPrompt string `json:"raw_prompt"`
	RawFooter string `json:"raw_footer"`
}

func loadStrictClaudeIdleTipRecording(t *testing.T) strictClaudeIdleTipRecording {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.263-idle-clear-tip.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording strictClaudeIdleTipRecording
	if err := json.Unmarshal(data, &recording); err != nil {
		t.Fatal(err)
	}
	return recording
}

// strictClaudeTipGap is the space run Claude renders between an already indented
// left part and the idle tip so the visible row ends two cells before the edge.
func strictClaudeTipGap(left, right string, width int) int {
	return width - 2 - utf8.RuneCountInString(StripANSI(left)) - utf8.RuneCountInString(StripANSI(right))
}

func strictClaudeTipRow(t *testing.T, left, right string, width int) string {
	t.Helper()
	gap := strictClaudeTipGap(left, right, width)
	if gap < 1 {
		t.Fatalf("fixture does not fit: width=%d left=%q right=%q", width, left, right)
	}
	return left + strings.Repeat(" ", gap) + right
}

func strictClaudeIdleTipSnapshot(prompt, footer string, width int) strictSnapshot {
	return strictClaudeFixtureGeometry(prompt, footer, width, 67, 62)
}

func assertStrictClaudeAdmitted(t *testing.T, s strictSnapshot) {
	t.Helper()
	if reason := strictEmptyComposer("claude", s); reason != "" {
		t.Fatalf("composer refused: %s", reason)
	}
	result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
		func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
	if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || drops != 1 || submits != 1 {
		t.Fatalf("admission failed: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
	}
}

func assertStrictClaudeRefused(t *testing.T, s strictSnapshot, reason string) {
	t.Helper()
	if got := strictEmptyComposer("claude", s); got != reason {
		t.Fatalf("composer reason = %q, want %q", got, reason)
	}
	result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
		func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
	if err == nil || result.Attempted || result.Delivery != "refused" || stages != 0 || drops != 0 || submits != 0 {
		t.Fatalf("refusal had effects: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
	}
}

func TestStrictClaudeMeasuredIdleClearTipThroughCapture(t *testing.T) {
	recording := loadStrictClaudeIdleTipRecording(t)
	visible := strings.TrimRight(StripANSI(recording.RawFooter), " ")
	if utf8.RuneCountInString(visible) != recording.Width-2 || !strings.HasSuffix(visible, strictClaudeMeasuredIdleTip) ||
		!strings.HasPrefix(visible, "  "+strictClaudeEmptyBypassHint+" ") {
		t.Fatalf("recording is not the measured right-aligned idle tip: %q", visible)
	}
	s := strictClaudeFixtureGeometry(recording.RawPrompt, recording.RawFooter, recording.Width, recording.Height, recording.CursorY)
	s.x = recording.CursorX
	assertStrictClaudeAdmitted(t, s)

	metadata := fmt.Sprintf("$5|%%9|123|%d|%d|0|0|1|0|0|%d|%d|1|claude|fixture|@2|/fixture|/dev/ttys015|3.7b\n",
		recording.CursorX, recording.CursorY, recording.Width, recording.Height)
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
	snapshot := func(pinned string) (strictSnapshot, error) {
		return captureStrictSnapshot("fixture", pinned, read, func(string) bool { return true })
	}
	if _, err := strictProbeIdentity("claude", snapshot); err != nil {
		t.Fatalf("read-only probe refused the measured idle tip: %v", err)
	}
	stages, submits := 0, 0
	result, err := strictSendOnce("claude", "fixture payload", func(StrictPaneIdentity) error { return nil }, strictOps{
		snapshot: snapshot,
		stage:    func(string) (string, error) { stages++; return "f", nil }, drop: func(string) {},
		submit: func(string, string) error { submits++; return nil },
	})
	if err != nil || !result.Attempted || result.Delivery != "unknown" || stages != 1 || submits != 1 {
		t.Fatalf("measured idle tip refused through capture: %+v %v stages=%d submits=%d", result, err, stages, submits)
	}
}

func TestStrictClaudeIdleClearTipAfterEachAuthority(t *testing.T) {
	authorities := []string{
		strictClaudeEmptyBypassHint,
		"⏵⏵ bypass permissions on (shift+tab to cycle) · PR #978 · ← 2 agents",
		"⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agents",
		"\x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · ← for agents\x1b[39m",
	}
	tips := []string{
		strictClaudeMeasuredIdleTip,
		"new task? /clear to save 100k tokens",
		"new task? /clear to save 1k tokens",
		"new task? /clear to save 999.9k tokens",
		"\x1b[38;5;246mnew task? \x1b[38;5;153m/clear\x1b[38;5;246m to save \x1b[38;5;153m100.1k tokens\x1b[39m",
	}
	for _, width := range []int{141, 188, 200} {
		for a, authority := range authorities {
			for n, tip := range tips {
				t.Run(fmt.Sprintf("w%d/authority%d/tip%d", width, a, n), func(t *testing.T) {
					footer := strictClaudeTipRow(t, "  "+authority, tip, width)
					assertStrictClaudeAdmitted(t, strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, footer, width))
					// Blank cells beyond the trimmed capture edge do not change the visible row.
					assertStrictClaudeAdmitted(t, strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, footer+"  ", width))
				})
			}
		}
	}
}

func TestStrictClaudeIdleClearTipCounterexamplesHaveNoEffects(t *testing.T) {
	const width = 188
	authority := "  " + strictClaudeEmptyBypassHint
	tip := func(count string) string { return "new task? /clear to save " + count + " tokens" }
	aligned := func(left, right string) string { return strictClaudeTipRow(t, left, right, width) }
	gap := strictClaudeTipGap(authority, strictClaudeMeasuredIdleTip, width)
	withGap := func(separator string) string { return authority + separator + strictClaudeMeasuredIdleTip }
	rows := map[string]string{
		"missing_authority":     aligned("  "+strictClaudeTestModeOnly, strictClaudeMeasuredIdleTip),
		"authority_prefix_only": aligned("  "+strictClaudeTestModeOnly+" · ←", strictClaudeMeasuredIdleTip),
		"tip_without_authority": aligned("  ", strictClaudeMeasuredIdleTip),
		"reordered":             aligned("  "+strictClaudeMeasuredIdleTip, strictClaudeEmptyBypassHint),
		"duplicated_tip":        aligned(authority, strictClaudeMeasuredIdleTip+" "+strictClaudeMeasuredIdleTip),
		"authority_substring":   aligned("  status: "+strictClaudeEmptyBypassHint, strictClaudeMeasuredIdleTip),
		"singular_agent":        aligned("  "+strings.Replace(strictClaudeEmptyBypassHint, "for agents", "1 agent", 1), strictClaudeMeasuredIdleTip),
		"zero_count":            aligned(authority, tip("0k")),
		"zero_integer_fraction": aligned(authority, tip("0.5k")),
		"leading_zero":          aligned(authority, tip("0546.6k")),
		"dot_zero_fraction":     aligned(authority, tip("546.0k")),
		"two_fraction_digits":   aligned(authority, tip("546.66k")),
		"trailing_dot":          aligned(authority, tip("546.k")),
		"leading_dot":           aligned(authority, tip(".6k")),
		"comma_decimal":         aligned(authority, tip("546,6k")),
		"huge_count":            aligned(authority, tip("1000k")),
		"huger_count":           aligned(authority, tip("12345.6k")),
		"negative_count":        aligned(authority, tip("-546.6k")),
		"signed_count":          aligned(authority, tip("+546.6k")),
		"exponent_count":        aligned(authority, tip("5e2k")),
		"fullwidth_digits":      aligned(authority, tip("５４６.６k")),
		"uppercase_unit":        aligned(authority, tip("546.6K")),
		"million_unit":          aligned(authority, tip("1m")),
		"missing_unit":          aligned(authority, tip("546")),
		"spaced_unit":           aligned(authority, tip("546.6 k")),
		"doubled_unit":          aligned(authority, tip("546.6kk")),
		"compact_command":       aligned(authority, "new task? /compact to save 546.6k tokens"),
		"new_command":           aligned(authority, "new task? /new to save 546.6k tokens"),
		"bare_command":          aligned(authority, "new task? clear to save 546.6k tokens"),
		"capital_command":       aligned(authority, "new task? /Clear to save 546.6k tokens"),
		"capital_phrase":        aligned(authority, "New task? /clear to save 546.6k tokens"),
		"other_verb":            aligned(authority, "new task? /clear to free 546.6k tokens"),
		"singular_tokens":       aligned(authority, "new task? /clear to save 546.6k token"),
		"double_space":          aligned(authority, "new task?  /clear to save 546.6k tokens"),
		"trailing_text":         aligned(authority, strictClaudeMeasuredIdleTip+" now"),
		"trailing_separator":    aligned(authority, strictClaudeMeasuredIdleTip+" ·"),
		"trailing_punctuation":  aligned(authority, strictClaudeMeasuredIdleTip+"."),
		"nbsp_gap":              withGap(strings.Repeat("\xc2\xa0", gap)),
		"tab_gap":               withGap(strings.Repeat("\t", gap)),
		"mixed_nbsp_gap":        withGap(strings.Repeat(" ", gap-1) + "\xc2\xa0"),
		"tab_before_tip":        withGap(strings.Repeat(" ", gap-1) + "\t"),
		"indent_zero":           strictClaudeEmptyBypassHint + strings.Repeat(" ", gap+2) + strictClaudeMeasuredIdleTip,
		"one_cell_short":        withGap(strings.Repeat(" ", gap-1)),
		"one_cell_long":         withGap(strings.Repeat(" ", gap+1)),
		"flush_right":           withGap(strings.Repeat(" ", gap+2)),
		"indent_one":            " " + strictClaudeEmptyBypassHint + strings.Repeat(" ", gap+1) + strictClaudeMeasuredIdleTip,
		"indent_three":          "   " + strictClaudeEmptyBypassHint + strings.Repeat(" ", gap-1) + strictClaudeMeasuredIdleTip,
		"cursor_escape_padding": withGap(fmt.Sprintf("\x1b[%dC", gap)),
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			assertStrictClaudeRefused(t, strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, row, width), "claude_empty_hint_unverified")
		})
	}
	t.Run("zero_gap_even_when_aligned", func(t *testing.T) {
		row := authority + strictClaudeMeasuredIdleTip
		fit := utf8.RuneCountInString(row) + 2
		assertStrictClaudeRefused(t, strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, row, fit), "claude_empty_hint_unverified")
	})
	t.Run("valid_control", func(t *testing.T) {
		assertStrictClaudeAdmitted(t, strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, withGap(strings.Repeat(" ", gap)), width))
	})
}

func TestStrictClaudeIdleClearTipCannotAdmitDrafts(t *testing.T) {
	const width = 188
	full := strictClaudeTipRow(t, "  "+strictClaudeEmptyBypassHint, strictClaudeMeasuredIdleTip, width)
	modeOnly := strictClaudeTipRow(t, "  "+strictClaudeTestModeOnly, strictClaudeMeasuredIdleTip, width)
	t.Run("text_draft", func(t *testing.T) {
		assertStrictClaudeRefused(t, strictClaudeIdleTipSnapshot("❯\xc2\xa0operator draft", full, width), "composer_not_empty_or_cursor_misplaced")
	})
	t.Run("space_draft_loses_authority", func(t *testing.T) {
		assertStrictClaudeRefused(t, strictClaudeIdleTipSnapshot("❯\xc2\xa0 ", modeOnly, width), "claude_empty_hint_unverified")
	})
	t.Run("cursor_moved", func(t *testing.T) {
		s := strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, full, width)
		s.x = 3
		assertStrictClaudeRefused(t, s, "composer_not_empty_or_cursor_misplaced")
	})
	data, err := os.ReadFile("testdata/claude-2.1.260-whitespace-composer.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Cases []struct {
			Mode       string `json:"mode"`
			Case       string `json:"case"`
			InputEmpty bool   `json:"input_empty"`
			CursorX    int    `json:"cursor_x"`
			RawPrompt  string `json:"raw_prompt"`
			RawFooter  string `json:"raw_footer"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	drafts := 0
	for _, item := range recorded.Cases {
		if item.Mode != "bypass" || item.InputEmpty {
			continue
		}
		drafts++
		t.Run("recorded_"+item.Case, func(t *testing.T) {
			// Real SPACE/NBSP draft prompt and hint bytes, with the idle tip appended.
			s := strictClaudeIdleTipSnapshot(item.RawPrompt, strictClaudeTipRow(t, item.RawFooter, strictClaudeMeasuredIdleTip, width), width)
			s.x = item.CursorX
			if reason := strictEmptyComposer("claude", s); reason == "" {
				t.Fatal("recorded draft admitted with the idle tip")
			}
			result, err, stages, drops, submits := strictClaudeFixtureAttempt(t,
				func() strictSnapshot { return s }, func(StrictPaneIdentity) error { return nil })
			if err == nil || result.Attempted || stages != 0 || drops != 0 || submits != 0 {
				t.Fatalf("recorded draft had effects: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
			}
		})
	}
	if drafts == 0 {
		t.Fatal("recorded bypass draft cases are missing")
	}
}

func TestStrictClaudeIdleClearTipCrossPassMutation(t *testing.T) {
	const width = 188
	plain := "  " + strictClaudeEmptyBypassHint
	valid := strictClaudeTipRow(t, plain, strictClaudeMeasuredIdleTip, width)
	gap := strictClaudeTipGap(plain, strictClaudeMeasuredIdleTip, width)
	cases := []struct {
		name          string
		first, second string
		secondPrompt  string
		admitted      bool
	}{
		{"authority_lost", valid, strictClaudeTipRow(t, "  "+strictClaudeTestModeOnly, strictClaudeMeasuredIdleTip, width), "", false},
		{"trailing_text", valid, strictClaudeTipRow(t, plain, strictClaudeMeasuredIdleTip+" now", width), "", false},
		{"malformed_count", valid, strictClaudeTipRow(t, plain, "new task? /clear to save 546.66k tokens", width), "", false},
		{"alternate_command", valid, strictClaudeTipRow(t, plain, "new task? /compact to save 546.6k tokens", width), "", false},
		{"misaligned", valid, plain + strings.Repeat(" ", gap-1) + strictClaudeMeasuredIdleTip, "", false},
		{"text_draft", valid, valid, "❯\xc2\xa0x", false},
		{"tip_appears", plain, valid, "", true},
		{"tip_clears", valid, plain, "", true},
		{"count_changes", valid, strictClaudeTipRow(t, plain, "new task? /clear to save 546.7k tokens", width), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := strictClaudeIdleTipSnapshot(strictClaudeTestNBSPPrompt, tc.first, width)
			prompt := strictClaudeTestNBSPPrompt
			if tc.secondPrompt != "" {
				prompt = tc.secondPrompt
			}
			second := strictClaudeIdleTipSnapshot(prompt, tc.second, width)
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
			} else if err == nil || result.Attempted || stages != 1 || drops != 1 || submits != 0 {
				t.Fatalf("mutated final frame reached submit: result=%+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
			}
			// The read-only probe reevaluates the complete frame on each pass.
			passes := 0
			observe := func(string) (strictSnapshot, error) {
				passes++
				if passes == 1 {
					return first, nil
				}
				return second, nil
			}
			if _, err := strictProbeIdentity("claude", observe); err != nil {
				t.Fatalf("first probe pass refused a valid frame: %v", err)
			}
			if _, err := strictProbeIdentity("claude", observe); tc.admitted != (err == nil) {
				t.Fatalf("second probe pass admitted=%v err=%v", tc.admitted, err)
			}
		})
	}
}
