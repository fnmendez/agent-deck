package tmux

import (
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
	if strings.Trim(lines[s.y+3], " ") != strictClaudeEmptyBypassHint {
		return "claude_empty_hint_unverified"
	}
	for _, row := range lines[s.y+4:] {
		if strings.Trim(row, " ") != "" {
			return "claude_empty_hint_unverified"
		}
	}
	return ""
}
