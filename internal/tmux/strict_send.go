package tmux

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// StrictSendResult describes terminal effects, never application consumption.
type StrictSendResult struct {
	Delivery  string `json:"delivery"`
	Attempted bool   `json:"attempted"`
	Reason    string `json:"reason"`
}

// StrictPaneIdentity is immutable for a single strict attempt. PID also detects
// respawn-pane, which retains the pane ID. ServerVersion and BracketPaste bind
// the narrow tmux 3.6 compatibility path. No environment or pane text is logged.
type StrictPaneIdentity struct {
	SessionID, PaneID, PID, SessionName, WindowID, CWD, Command, TTY string
	ServerVersion, BracketPaste                                      string
}
type strictSnapshot struct {
	identity      StrictPaneIdentity
	x, y          int
	width, height int
	content       string
	observedAt    time.Time
}
type strictOps struct {
	snapshot                func(string) (strictSnapshot, error)
	stage                   func(string) (string, error)
	drop                    func(string)
	submit                  func(string, string) error
	submitManualBracket     func(string, string) error
	manualBracketAuthorized func(StrictPaneIdentity) bool
}

// StrictSendOnce performs only an explicitly guarded, single terminal attempt.
// verify must re-read the native thread identity and idle evidence without any
// terminal mutation. There is an unavoidable race between observation and effect;
// even successful tmux commands return unknown, not delivered.
func (s *Session) StrictSendOnce(tool, message string, verify func(StrictPaneIdentity) error,
	manualBracketAuthorized func(StrictPaneIdentity) bool) (StrictSendResult, error) {
	if s.VimMode {
		return strictRefusal("vim_mode")
	}
	return strictSendOnce(tool, message, verify, strictOps{
		snapshot: s.strictSnapshot, manualBracketAuthorized: manualBracketAuthorized,
		stage: func(body string) (string, error) {
			name := pasteBufferName()
			cmd := keySenderExec(s.SocketName, "load-buffer", "-b", name, "-")
			cmd.Stdin = strings.NewReader(body)
			return name, runSendKeysBounded(cmd)
		},
		drop: func(name string) { _ = runSendKeysBounded(keySenderExec(s.SocketName, "delete-buffer", "-b", name)) },
		submit: func(pane, buffer string) error {
			// One server command list, no delay, extra Enter, mode switch or fallback.
			// Enter can be swallowed by an async paste handler: this remains unknown.
			return runSendKeysBounded(keySenderExec(s.SocketName, strictPasteSubmitArgs(pane, buffer, false)...))
		},
		submitManualBracket: func(pane, buffer string) error {
			// tmux 3.6 cannot expose bracket_paste_flag. The verified compatibility
			// path stages exactly one explicit bracket pair and pastes it raw so
			// multiline bytes never become independent Enter key events.
			return runSendKeysBounded(keySenderExec(s.SocketName, strictPasteSubmitArgs(pane, buffer, true)...))
		},
	})
}

func strictPasteSubmitArgs(pane, buffer string, manualBracket bool) []string {
	args := []string{"paste-buffer"}
	if !manualBracket {
		args = append(args, "-p")
	}
	return append(args, "-r", "-d", "-b", buffer, "-t", pane,
		";", "send-keys", "-t", pane, "Enter")
}

func strictRefusal(reason string) (StrictSendResult, error) {
	return StrictSendResult{Delivery: "refused", Reason: reason}, fmt.Errorf("strict send refused: %s", reason)
}

func strictSendOnce(tool, message string, verify func(StrictPaneIdentity) error, ops strictOps) (StrictSendResult, error) {
	if tool != "claude" && tool != "codex" {
		return strictRefusal("unsupported_tool")
	}
	if verify == nil {
		return strictRefusal("identity_verifier_missing")
	}
	if strings.TrimSpace(message) == "" || len(message) > 64*1024 || !utf8.ValidString(message) {
		return strictRefusal("invalid_message")
	}
	for _, r := range message {
		if (unicode.IsControl(r) && r != '\n') || r == '\u2028' || r == '\u2029' {
			return strictRefusal("control_character")
		}
	}
	first, err := ops.snapshot("")
	if err != nil {
		return strictRefusal("capture_failed")
	}
	if reason := strictEmptyComposer(tool, first); reason != "" {
		return strictRefusal(reason)
	}
	if err := verify(first.identity); err != nil {
		return strictRefusal("identity_or_idle_unverified")
	}
	// Staging is private tmux buffer memory, not a terminal effect. tmux 3.6
	// compatibility explicitly frames the already control-free payload instead
	// of treating its unavailable bracket_paste_flag as positive evidence.
	manualBracket := strictTmux36ManualBracket(first.identity)
	if manualBracket && (ops.manualBracketAuthorized == nil || !ops.manualBracketAuthorized(first.identity)) {
		return strictRefusal("bracket_paste_unverified")
	}
	stagedMessage := message
	if manualBracket {
		stagedMessage = "\x1b[200~" + message + "\x1b[201~"
	}
	buffer, err := ops.stage(stagedMessage)
	if buffer != "" {
		defer ops.drop(buffer)
	}
	if err != nil {
		return strictRefusal("staging_failed")
	}
	// Re-read native thread and idle status before the final fresh pane observation.
	if err := verify(first.identity); err != nil {
		return strictRefusal("identity_or_idle_changed")
	}
	last, err := ops.snapshot(first.identity.PaneID)
	if err != nil {
		return strictRefusal("capture_failed")
	}
	if first.identity != last.identity {
		return strictRefusal("pane_changed")
	}
	if reason := strictEmptyComposer(tool, last); reason != "" {
		return strictRefusal(reason)
	}
	// Bound observation age at command invocation, not OS/server processing time.
	if last.observedAt.IsZero() || time.Since(last.observedAt) > 500*time.Millisecond {
		return strictRefusal("snapshot_expired")
	}
	result := StrictSendResult{Delivery: "unknown", Attempted: true, Reason: "terminal_attempted"}
	submit := ops.submit
	if manualBracket {
		submit = ops.submitManualBracket
	}
	if submit == nil {
		return strictRefusal("transport_unavailable")
	}
	if err := submit(last.identity.PaneID, buffer); err != nil {
		result.Reason = "terminal_error"
		return result, fmt.Errorf("strict terminal attempt has unknown outcome")
	}
	return result, nil
}

const strictMetadataFormat = "#{session_id}|#{pane_id}|#{pane_pid}|#{cursor_x}|#{cursor_y}|#{pane_dead}|#{pane_in_mode}|#{cursor_flag}|#{pane_input_off}|#{alternate_on}|#{pane_width}|#{pane_height}|#{bracket_paste_flag}|#{pane_current_command}|#{session_name}|#{window_id}|#{pane_current_path}|#{pane_tty}|#{version}"

func strictTmux36ManualBracket(id StrictPaneIdentity) bool {
	return id.ServerVersion == "3.6" && id.BracketPaste == ""
}

func (s *Session) strictSnapshot(pinned string) (strictSnapshot, error) {
	return captureStrictSnapshot(s.Name, pinned, s.runBoundedOutput, func(target string) bool {
		discipline, err := s.paneLineDiscipline(target)
		return err == nil && !discipline.Canonical
	})
}

// StrictComposerError is a closed composer-guard refusal from a read-only
// strict observation. Reason is a fixed identifier, never pane text.
type StrictComposerError struct{ Reason string }

func (e StrictComposerError) Error() string { return "strict composer unavailable: " + e.Reason }

// StrictProbeIdentity performs one full read-only strict observation, including
// the composer guard, and returns metadata only. It cannot stage, submit, clear,
// restore or expose pane text. The command-level probe performs this twice.
func (s *Session) StrictProbeIdentity(tool string) (StrictPaneIdentity, error) {
	return strictProbeIdentity(tool, s.strictSnapshot)
}

func strictProbeIdentity(tool string, snapshot func(string) (strictSnapshot, error)) (StrictPaneIdentity, error) {
	observed, err := snapshot("")
	if err != nil {
		return StrictPaneIdentity{}, err
	}
	if reason := strictEmptyComposer(tool, observed); reason != "" {
		return StrictPaneIdentity{}, StrictComposerError{Reason: reason}
	}
	return observed.identity, nil
}

func captureStrictSnapshot(name, pinned string, read func(...string) ([]byte, error), rawMode func(string) bool) (strictSnapshot, error) {
	observedAt := time.Now()
	// Include both immutable target and dynamic active pane selection in each
	// sample: an active-pane switch must refuse, never redirect or hit a stale UI.
	before, err := read("display-message", "-p", "-t", name, strictMetadataFormat)
	if err != nil {
		return strictSnapshot{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(before)), "|")
	if len(fields) != 19 || !strings.HasPrefix(fields[0], "$") || !strings.HasPrefix(fields[1], "%") {
		return strictSnapshot{}, fmt.Errorf("invalid metadata")
	}
	if pinned != "" && fields[1] != pinned {
		return strictSnapshot{}, fmt.Errorf("active pane changed")
	}
	target := fields[1]
	bracketKnown := fields[12] == "1"
	bracket36Candidate := fields[12] == "" && fields[18] == "3.6"
	if fields[5] != "0" || fields[6] != "0" || fields[7] != "1" || fields[8] != "0" || !bracketKnown && !bracket36Candidate {
		return strictSnapshot{}, fmt.Errorf("pane unavailable")
	}
	if _, err := strconv.Atoi(fields[2]); err != nil {
		return strictSnapshot{}, fmt.Errorf("invalid pid")
	}
	x, e1 := strconv.Atoi(fields[3])
	y, e2 := strconv.Atoi(fields[4])
	width, e3 := strconv.Atoi(fields[10])
	height, e4 := strconv.Atoi(fields[11])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || x < 0 || x >= width || y < 0 || y >= height {
		return strictSnapshot{}, fmt.Errorf("invalid cursor")
	}
	clients, err := read("list-clients", "-t", fields[0], "-F", "#{client_control_mode}|#{client_activity}")
	if err != nil || !strictOperatorIdle(string(clients), time.Now()) {
		return strictSnapshot{}, fmt.Errorf("operator activity unverified or recent")
	}
	if !rawMode(target) {
		return strictSnapshot{}, fmt.Errorf("raw terminal mode unverified")
	}
	pane, err := read("capture-pane", "-p", "-e", "-t", target)
	if err != nil {
		return strictSnapshot{}, err
	}
	after, err := read("display-message", "-p", "-t", name, strictMetadataFormat)
	if err != nil || string(before) != string(after) {
		return strictSnapshot{}, fmt.Errorf("pane changed during capture")
	}
	return strictSnapshot{identity: StrictPaneIdentity{SessionID: fields[0], PaneID: fields[1], PID: fields[2],
		SessionName: fields[14], WindowID: fields[15], CWD: fields[16], Command: fields[13], TTY: fields[17],
		ServerVersion: fields[18], BracketPaste: fields[12]}, x: x, y: y, width: width, height: height,
		content: string(pane), observedAt: observedAt}, nil
}

// StrictThreadEnvironment reads only the native Codex thread anchor, uncached.
func (s *Session) StrictThreadEnvironment(id StrictPaneIdentity) (string, error) {
	data, err := s.runBoundedOutput("show-environment", "-t", id.SessionID, "CODEX_SESSION_ID")
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "CODEX_SESSION_ID=") {
		return "", fmt.Errorf("thread unavailable")
	}
	return strings.TrimPrefix(line, "CODEX_SESSION_ID="), nil
}

// Recognize a deliberately narrow layout. Codex requires the positively styled
// main placeholder; a bare marker is not evidence of an empty textarea.
func strictEmptyComposer(tool string, s strictSnapshot) string {
	rawLines := strings.Split(strings.TrimSuffix(s.content, "\n"), "\n")
	clean := StripANSI(s.content)
	lines := strings.Split(strings.TrimSuffix(clean, "\n"), "\n")
	if s.y < 0 || s.y >= len(lines) {
		return "cursor_unknown"
	}
	if tool == "codex" && !strictCodexGeometry(s, lines) {
		return "composer_geometry_unverified"
	}
	line := strings.TrimRight(lines[s.y], " ")
	placeholder := tool == "codex" && s.y < len(rawLines) && strictCodexPlaceholder(rawLines[s.y])
	claudeNBSP := tool == "claude" && strictClaudeNBSPPrompt(lines[s.y])
	// A bare Codex marker can be an all-space draft with Home pressed.
	if s.x != 2 || (tool == "codex" && !placeholder) || (tool != "codex" && line != "❯" && !claudeNBSP) {
		return "composer_not_empty_or_cursor_misplaced"
	}
	// Codex places its last remote-image row immediately above one blank separator.
	if tool == "codex" && s.y >= 2 && strictCodexImageRow.MatchString(lines[s.y-2]) {
		return "nontext_draft_present"
	}
	// Claude's usage-limit auto-resume countdown ("… · esc to cancel") updates
	// inside the transcript, so cancel hints stay whole-pane evidence. Menu,
	// approval and interrupt phrases count only in rows that can hold live UI.
	lower := strings.ToLower(clean)
	for _, indicator := range []string{"esc to cancel", "escape to cancel"} {
		if strings.Contains(lower, indicator) {
			return "busy_or_modal"
		}
	}
	if claudeNBSP {
		lower = strings.ToLower(strings.Join(lines[strictClaudeLiveRegionStart(s, rawLines, lines):], "\n"))
	}
	for _, indicator := range []string{"esc to interrupt", "ctrl+c to interrupt", "do you want to", "would you like to", "enter to confirm", "allow once", "approval required", "select an option", "❯ 1.", "› 1."} {
		if strings.Contains(lower, indicator) {
			return "busy_or_modal"
		}
	}
	if claudeNBSP {
		return strictClaudeBypassComposer(s, lines)
	}
	if tool == "claude" {
		if s.y < 1 || s.y+1 >= len(lines) || !strictDivider(lines[s.y-1]) || !strictDivider(lines[s.y+1]) {
			return "composer_layout_unknown"
		}
	}
	mainFooter := false
	for i := s.y + 1; i < len(lines); i++ {
		row := strings.TrimSpace(lines[i])
		if row == "" || (tool == "claude" && i == s.y+1 && strictDivider(row)) {
			continue
		}
		// Only known idle footer lines; modal/menu/help text fails closed.
		if tool == "claude" && row == "? for shortcuts" {
			continue
		}
		if tool == "codex" && !mainFooter && strictCodexStatusFooter.MatchString(row) {
			mainFooter = true
			continue
		}
		return "footer_unknown"
	}
	if tool == "codex" && !mainFooter {
		return "main_footer_unverified"
	}
	return ""
}
func strictDivider(s string) bool {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) < 10 {
		return false
	}
	for _, r := range s {
		if r != '─' && r != '━' && r != '-' {
			return false
		}
	}
	return true
}

// An attached human who used the terminal within the last minute owns input.
// Control-mode clients are excluded only with explicit positive identification.
func strictOperatorIdle(clients string, now time.Time) bool {
	for _, line := range strings.Split(strings.TrimSpace(clients), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 2 {
			return false
		}
		if fields[0] == "1" {
			continue
		}
		if fields[0] != "0" {
			return false
		}
		ts, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || ts <= 0 {
			return false
		}
		age := now.Sub(time.Unix(ts, 0))
		if age < 60*time.Second {
			return false
		}
	}
	return true
}
