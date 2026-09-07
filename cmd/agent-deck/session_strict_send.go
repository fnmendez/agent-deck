package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var strictThreadUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func strictNativeThread(inst *session.Instance, target *tmux.Session, id tmux.StrictPaneIdentity) (string, error) {
	if inst.Tool == "codex" {
		return target.StrictThreadEnvironment(id)
	}
	pid, err := strconv.Atoi(id.PID)
	if err != nil || pid <= 0 {
		return "", fmt.Errorf("invalid native pid")
	}

	path := filepath.Join(session.GetClaudeConfigDirForInstance(inst), "sessions", id.PID+".json")
	file, _, err := strictOpenVerifiedPath(path)
	if err != nil {
		return "", fmt.Errorf("native session unavailable")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64*1024 {
		return "", fmt.Errorf("invalid native record")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return "", fmt.Errorf("invalid native record")
	}
	var record strictClaudeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", fmt.Errorf("native session unavailable")
	}
	started, err := strictNativeProbe("ps", "-p", id.PID, "-o", "lstart=")
	if err != nil || !strictClaudeIdentityMatches(record, id, inst.ProjectPath, string(started), runtime.GOOS) {
		return "", fmt.Errorf("native session identity mismatch")
	}

	return record.SessionID, nil
}

func handleStrictSessionSend(out *CLIOutput, inst *session.Instance, expectedThread, message string) {
	result := tmux.StrictSendResult{Delivery: "refused", Reason: "target_unavailable"}
	var sendErr error = fmt.Errorf("strict target unavailable")
	target := inst.GetTmuxSession()
	if !strictPlatformSupported(runtime.GOOS) {
		result.Reason = "unsupported_platform"
		sendErr = fmt.Errorf("strict send currently supports Darwin only")
	}
	if strictPlatformSupported(runtime.GOOS) && target != nil && strictThreadUUID.MatchString(expectedThread) && (inst.Tool == "claude" || inst.Tool == "codex") {
		previousProof := ""
		result, sendErr = target.StrictSendOnce(inst.Tool, message, func(id tmux.StrictPaneIdentity) error {
			if inst.Tool == "codex" {
				proof, err := strictCodexRuntimeProof(id, expectedThread, inst.ProjectPath)
				if err != nil || (previousProof != "" && previousProof != proof) {
					return fmt.Errorf("native Codex identity unverified")
				}
				previousProof = proof
			}
			thread, err := strictNativeThread(inst, target, id)
			if err != nil || thread != expectedThread || !session.StrictSendIdle(inst.ID, inst.Tool, expectedThread) {
				return fmt.Errorf("thread or idle status unverified")
			}
			return nil
		})
	}
	data := map[string]interface{}{"success": sendErr == nil, "delivery": result.Delivery, "attempted": result.Attempted, "reason": result.Reason, "session_id": inst.ID, "expected_thread": expectedThread}
	if sendErr != nil {
		out.ErrorWithData(sendErr.Error(), ErrCodeDeliveryFailed, data)
		os.Exit(1)
	}
	out.Success("Terminal attempt completed; session consumption is unknown", data)
}

type strictClaudeRecord struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Tmux      string `json:"tmux"`
	ProcStart string `json:"procStart"`
	PIDDomain string `json:"pidDomain"`
}

func strictClaudeIdentityMatches(record strictClaudeRecord, id tmux.StrictPaneIdentity, project, started, domain string) bool {
	if domain != "darwin" {
		return false
	}
	if id.Command != "claude" && !strings.HasPrefix(id.Command, "claude-") {
		return false
	}
	pid, err := strconv.Atoi(id.PID)
	if err != nil || pid <= 0 || record.PID != pid || record.SessionID == "" || record.PIDDomain != domain || record.ProcStart == "" || strings.TrimSpace(started) == "" {
		return false
	}
	if strings.Join(strings.Fields(record.ProcStart), " ") != strings.Join(strings.Fields(started), " ") {
		return false
	}
	if project == "" || id.CWD == "" || record.CWD == "" || filepath.Clean(record.CWD) != filepath.Clean(project) || filepath.Clean(id.CWD) != filepath.Clean(project) {
		return false
	}
	return record.Tmux == id.SessionName+":"+id.WindowID+"."+id.PaneID
}

// Strict input is bounded and byte-preserving; the legacy resolver trims CR/LF.
func resolveStrictMessageInput(inline, file string, stdin io.Reader) (string, error) {
	if file == "" {
		if len(inline) > 64*1024 {
			return "", fmt.Errorf("message exceeds 64 KiB")
		}
		return inline, nil
	}
	if inline != "" {
		return "", fmt.Errorf("use either inline message or --message-file")
	}
	input := stdin
	if file != "-" {
		opened, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("message file unavailable")
		}
		defer opened.Close()
		input = opened
	}
	data, err := io.ReadAll(io.LimitReader(input, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return "", fmt.Errorf("message unavailable or exceeds 64 KiB")
	}
	return string(data), nil
}
