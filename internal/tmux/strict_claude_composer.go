package tmux

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Claude 2.1.260 renders a structural NBSP after the marker. The optional
// ASCII space is a software cursor cell, not proof that the textarea is empty.
// A whitespace draft followed by Home can render exactly the same prompt row.
func strictClaudeNBSPPrompt(line string) bool {
	return line == "❯\u00a0" || line == "❯\u00a0 "
}

// In the measured bypass/status-line UI, this complete native hint is present
// for empty input. SPACE/NBSP drafts retain the mode label but lose the agents
// suffix, including after Home. Never substitute a substring or mode-only check.
const strictClaudeEmptyBypassHint = "⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents" // #nosec G101 -- Public Claude UI label, not a credential.

var strictClaudeAgentsBypassHint = regexp.MustCompile(`^⏵⏵ bypass permissions on \(shift\+tab to cycle\)( · PR #[1-9][0-9]{0,5})? · ← ([1-9]|[1-9][0-9]) agents$`)

func strictClaudeEmptyBypassHintValid(line string) bool {
	return line == strictClaudeEmptyBypassHint || strictClaudeAgentsBypassHint.MatchString(line)
}

// Claude 2.1.263-2.1.269 add one contextual idle-return tip after
// CLAUDE_CODE_IDLE_THRESHOLD_MINUTES (default 75) idle minutes when the context
// holds at least CLAUDE_CODE_IDLE_TOKEN_THRESHOLD (default 1e5) tokens. It is
// right-aligned in the hint row, ending at the same two-cell margin as the row
// indent. The count is en-US compact with one fraction digit, lowercased and
// with ".0" removed, so its k range is exactly 1k-999.9k; every other shape,
// including the m range, stays unrecognized. The tip is display data only: the
// complete native hint must still precede it.
const strictClaudeHintIndent = "  "

var strictClaudeIdleClearTipRow = regexp.MustCompile(`^([^ ].*[^ ]) +new task\? /clear to save [1-9][0-9]{0,2}(?:\.[1-9])?k tokens$`)

func strictClaudeIdleClearTipHint(row string, width int) bool {
	visible := strings.TrimRight(row, " ")
	if !strings.HasPrefix(visible, strictClaudeHintIndent) ||
		utf8.RuneCountInString(visible) != width-len(strictClaudeHintIndent) {
		return false
	}
	match := strictClaudeIdleClearTipRow.FindStringSubmatch(strings.TrimPrefix(visible, strictClaudeHintIndent))
	return match != nil && strictClaudeEmptyBypassHintValid(match[1])
}

// Claude 2.1.263 can put its session title inside the upper divider. Recognize
// only the measured full-width shape: thin rules, one ASCII space on each side
// of a bounded ASCII label, and one final rule. The label is display data, not
// identity or empty-input authority. The lower divider must remain plain.
func strictClaudeUpperDivider(line string, width int) bool {
	if strictDivider(line) {
		return true
	}
	if utf8.RuneCountInString(line) != width || !strings.HasSuffix(line, " ─") {
		return false
	}
	remainder := strings.TrimLeft(line, "─")
	if utf8.RuneCountInString(line)-utf8.RuneCountInString(remainder) < 10 || !strings.HasPrefix(remainder, " ") {
		return false
	}
	title := strings.TrimSuffix(strings.TrimPrefix(remainder, " "), " ─")
	if len(title) < 1 || len(title) > 64 {
		return false
	}
	for i, r := range title {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alphanumeric && (i == 0 || r != '-' && r != '_' && r != '.') {
			return false
		}
	}
	return true
}

// Claude 2.1.261, 2.1.263, 2.1.265, 2.1.268 and 2.1.269 (every installed build)
// end each completed turn with one transcript row from the same template: the
// ✻ glyph, a fixed past-tense verb, " for ", the optionless duration formatter
// output (0s-59s, Nm Ns, Nh Nm Ns or Nd Nh Nm, no leading zeros) and, when the
// completion time is known, " · done " plus the same-day en-US h:mm AM/PM.
// Pending background agents, running shells or monitors, budget and hidden
// message text, other locales and day-relative times change the row and stay
// unrecognized.
var strictClaudeCompletedTurnRow = regexp.MustCompile(`^✻ (?:Baked|Brewed|Churned|Cogitated|Cooked|Crunched|Saut\x{e9}ed|Worked) for ` +
	`(?:[1-5]?[0-9]s|(?:[1-9]|[1-5][0-9])m [1-5]?[0-9]s|(?:[1-9]|1[0-9]|2[0-3])h [1-5]?[0-9]m [1-5]?[0-9]s|[1-9][0-9]{0,2}d (?:1?[0-9]|2[0-3])h [1-5]?[0-9]m)` +
	`(?: · done (?:[1-9]|1[0-2]):[0-5][0-9] [AP]M)?$`)

// Only semicolon SGR is stripped; OSC 8 hyperlinks, colon SGR and every other
// escape leave the row unrecognized.
var strictSGRSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// strictClaudeLiveRegionStart returns the first row that can still hold a live
// menu, approval or interrupt hint above the empty bypass composer. Claude's
// spinner, todos, queued input, notification-area notices and dialogs render
// below all transcript, so when the only row between the upper divider and the
// transcript is blank and the last transcript row is a completed turn, quoted
// menus above it are not evidence. Transcript rows can still update in place
// (the usage-limit countdown, the turn row's running-task suffix); the caller
// keeps cancel hints whole-pane. Every other layout scans the whole pane.
func strictClaudeLiveRegionStart(s strictSnapshot, rawLines, lines []string) int {
	turn := s.y - 3
	if turn < 0 || s.y >= len(lines) || len(rawLines) != len(lines) || strings.Trim(lines[s.y-2], " ") != "" ||
		!strictClaudeCompletedTurnRow.MatchString(strings.TrimRight(strictSGRSequence.ReplaceAllString(rawLines[turn], ""), " ")) {
		return 0
	}
	return turn
}

func strictClaudeBypassComposer(s strictSnapshot, lines []string) string {
	if s.width <= s.x || s.height <= 0 || len(lines) != s.height || s.x != 2 || s.y < 1 || s.y+3 >= len(lines) {
		return "composer_geometry_unverified"
	}
	if !strictClaudeUpperDivider(lines[s.y-1], s.width) || !strictDivider(lines[s.y+1]) {
		return "composer_layout_unknown"
	}
	// The custom status line is mutable display data. Require its reserved row,
	// but none of its words can authorize delivery or replace the native hint.
	if strings.Trim(lines[s.y+2], " ") == "" {
		return "claude_status_layout_unknown"
	}
	if hint := lines[s.y+3]; !strictClaudeEmptyBypassHintValid(strings.Trim(hint, " ")) &&
		!strictClaudeIdleClearTipHint(hint, s.width) {
		return "claude_empty_hint_unverified"
	}
	for _, row := range lines[s.y+4:] {
		if strings.Trim(row, " ") != "" {
			return "claude_empty_hint_unverified"
		}
	}
	return ""
}
