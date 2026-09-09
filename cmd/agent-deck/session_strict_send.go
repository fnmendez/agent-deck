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
	home, err := os.UserHomeDir()
	if err != nil || !strictClaudeIdentityWithRun(record, id, inst.ProjectPath, runtime.GOOS, home, strictNativeProbe) {
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
	Version   string `json:"version"`
}

var strictClaudeNativeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Native installs expose their version as tmux's command, while Darwin's ps
// still reports claude. A version alone is never executable identity evidence.
func strictClaudeIdentityWithRun(record strictClaudeRecord, id tmux.StrictPaneIdentity, project, domain, home string, run strictProbe) bool {
	if domain != "darwin" || !strictValidPID(id.PID) {
		return false
	}
	started, err := run("ps", "-p", id.PID, "-o", "lstart=")
	if err != nil {
		return false
	}
	if id.Command == "claude" || strings.HasPrefix(id.Command, "claude-") {
		return strictClaudeIdentityMatches(record, id, project, string(started), domain)
	}
	if !strictClaudeNativeVersion.MatchString(id.Command) || record.Version != id.Command || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return false
	}
	// Normalize only after choosing the separately corroborated native path;
	// retain every existing record/PID/start/CWD/tmux requirement.
	nativeID := id
	nativeID.Command = "claude"
	if !strictClaudeIdentityMatches(record, nativeID, project, string(started), domain) {
		return false
	}
	observeProcess := func() (string, error) {
		table, err := run("ps", "-p", id.PID, "-o", "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm=")
		if err != nil {
			return "", err
		}
		matched := ""
		for _, line := range strings.Split(string(table), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] != id.PID {
				continue
			}
			if matched != "" || len(f) != 7 || f[6] != "claude" || !strictValidPID(f[2]) || f[2] != f[3] || id.TTY == "" || f[5] != strings.TrimPrefix(id.TTY, "/dev/") || !strings.Contains(f[4], "+") || strings.ContainsAny(f[4], "TXZ") {
				return "", fmt.Errorf("native Claude foreground process unverified")
			}
			matched = strings.Join(f[:4], " ") + " " + strings.Join(f[5:], " ")
		}
		if matched == "" {
			return "", fmt.Errorf("native Claude process missing")
		}
		return matched, nil
	}
	before, err := observeProcess()
	if err != nil {
		return false
	}
	executable := filepath.Join(home, ".local", "share", "claude", "versions", id.Command)
	observeExecutable := func() (strictRootBinding, *os.File, error) {
		output, err := run("lsof", "-a", "-p", id.PID, "-F", "pfaDint")
		if err != nil {
			return strictRootBinding{}, nil, err
		}
		descriptor, err := strictClaudeExecutableDescriptor(string(output), id.PID, project, executable)
		if err != nil {
			return strictRootBinding{}, nil, err
		}
		file, ancestors, err := strictOpenVerifiedPath(executable)
		if err != nil {
			return strictRootBinding{}, nil, err
		}
		dev, ino, err := strictFileIdentity(file)
		stat, statErr := file.Stat()
		if err != nil || statErr != nil || stat.Mode().Perm()&0111 == 0 || dev != descriptor.Device || ino != descriptor.Inode {
			file.Close()
			return strictRootBinding{}, nil, fmt.Errorf("native Claude executable replaced")
		}
		return strictRootBinding{descriptor, ancestors}, file, nil
	}
	first, held, err := observeExecutable()
	if err != nil {
		return false
	}
	defer held.Close()
	second, heldAgain, err := observeExecutable()
	if err != nil {
		return false
	}
	defer heldAgain.Close()
	after, err := observeProcess()
	if err != nil || before != after || first != second {
		return false
	}
	startedAgain, err := run("ps", "-p", id.PID, "-o", "lstart=")
	return err == nil && strictClaudeIdentityMatches(record, nativeID, project, string(startedAgain), domain)
}

// Darwin lsof lists the loaded executable as the first txt mapping. Later txt
// mappings may be libraries or data files; they cannot authorize a binary.
func strictClaudeExecutableDescriptor(output, pid, project, executable string) (strictDescriptor, error) {
	var records []map[byte]string
	var record map[byte]string
	process := ""
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		key, value := line[0], line[1:]
		if key == 'p' {
			if process != "" || value != pid || record != nil {
				return strictDescriptor{}, fmt.Errorf("ambiguous native Claude process")
			}
			process = value
			continue
		}
		if key == 'f' {
			record = map[byte]string{}
			records = append(records, record)
		}
		if process == "" || record == nil {
			return strictDescriptor{}, fmt.Errorf("missing native Claude descriptor")
		}
		if _, exists := record[key]; exists {
			return strictDescriptor{}, fmt.Errorf("duplicate native Claude descriptor field")
		}
		record[key] = value
	}
	var first map[byte]string
	cwd := ""
	for _, r := range records {
		if r['f'] == "cwd" {
			if cwd != "" || r['t'] != "DIR" || r['n'] != project {
				return strictDescriptor{}, fmt.Errorf("native Claude cwd unverified")
			}
			cwd = r['n']
		}
		if r['f'] == "txt" && first == nil {
			first = r
		}
	}
	dev, e1 := strconv.ParseUint(first['D'], 0, 64)
	ino, e2 := strconv.ParseUint(first['i'], 10, 64)
	if process != pid || cwd == "" || first['t'] != "REG" || first['n'] != executable || e1 != nil || e2 != nil || dev == 0 || ino == 0 {
		return strictDescriptor{}, fmt.Errorf("native Claude executable unverified")
	}
	return strictDescriptor{FD: "txt", Path: executable, Device: dev, Inode: ino}, nil
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
