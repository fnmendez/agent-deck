package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var strictThreadUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func safeStrictProbeReason(value string) bool {
	if len(value) < 1 || len(value) > 80 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if character != '_' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

type strictClaudeNativeProof struct {
	Thread           string
	ProcessStartedAt time.Time
	CWD              string
	Signature        string
	DurableStop      bool
}

type strictNativeThreadProof struct {
	Thread string
	Claude strictClaudeNativeProof
}

type strictAdmissionObservation struct {
	Thread        string
	NativeProof   string
	IdleDecision  session.StrictSendIdleDecision
	ProcessStart  time.Time
	DurableNative bool
}

type strictAdmissionDeps struct {
	native func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error)
	codex  func(tmux.StrictPaneIdentity, string, string) (string, error)
	idle   func(string, string, string, session.StrictSendIdleEvidence) session.StrictSendIdleDecision
}

type strictSessionAdmissionVerifier struct {
	inst           *session.Instance
	target         *tmux.Session
	expected       string
	previousProof  string
	previousStrong bool
	passes         int
	deps           strictAdmissionDeps
}

func strictNativeThread(inst *session.Instance, target *tmux.Session, id tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
	if inst.Tool == "codex" {
		if !strictCodexPlatformSupported(runtime.GOOS) {
			return strictNativeThreadProof{}, fmt.Errorf("unsupported strict Codex platform")
		}
		thread, err := target.StrictThreadEnvironment(id)
		return strictNativeThreadProof{Thread: thread}, err
	}
	proof, err := strictClaudeThreadProof(inst, target, id)
	return strictNativeThreadProof{Thread: proof.Thread, Claude: proof}, err
}

func newStrictSessionAdmissionVerifier(inst *session.Instance, target *tmux.Session, expected string) *strictSessionAdmissionVerifier {
	return &strictSessionAdmissionVerifier{inst: inst, target: target, expected: expected, deps: strictAdmissionDeps{
		native: strictNativeThread,
		codex:  strictCodexRuntimeProof,
		idle:   session.StrictSendIdle,
	}}
}

func (v *strictSessionAdmissionVerifier) verify(id tmux.StrictPaneIdentity) (strictAdmissionObservation, error) {
	var observed strictAdmissionObservation
	if v == nil || v.inst == nil || v.target == nil || v.passes >= 2 {
		return observed, fmt.Errorf("admission_verifier_unavailable")
	}
	v.passes++
	if v.inst.Tool == "codex" {
		if v.expected == "" {
			return observed, fmt.Errorf("codex_thread_unavailable")
		}
		proof, err := v.deps.codex(id, v.expected, v.inst.ProjectPath)
		if err != nil || (v.previousProof != "" && v.previousProof != proof) {
			return observed, fmt.Errorf("codex_identity_unverified")
		}
		v.previousProof = proof
	}
	native, err := v.deps.native(v.inst, v.target, id)
	if err != nil {
		if v.inst.Tool == "claude" {
			switch err.Error() {
			case "native root unavailable", "native root changed":
				return observed, fmt.Errorf("native_root_changed")
			case "native record changed":
				return observed, fmt.Errorf("native_record_changed")
			}
		}
		return observed, fmt.Errorf("native_thread_unverified")
	}
	if !strictThreadUUID.MatchString(native.Thread) {
		return observed, fmt.Errorf("native_thread_unverified")
	}
	if v.expected == "" {
		if v.passes != 1 {
			return observed, fmt.Errorf("native_thread_changed")
		}
		v.expected = native.Thread
	}
	if native.Thread != v.expected {
		return observed, fmt.Errorf("native_thread_changed")
	}
	evidence := session.StrictSendIdleEvidence{}
	if v.inst.Tool == "claude" {
		strong := native.Claude.DurableStop && native.Claude.Signature != ""
		observed.NativeProof = native.Claude.Signature
		observed.ProcessStart = native.Claude.ProcessStartedAt
		observed.DurableNative = strong
		if v.passes == 1 {
			v.previousStrong = strong
			if strong {
				v.previousProof = native.Claude.Signature
			}
		} else {
			if v.previousStrong != strong {
				return observed, fmt.Errorf("identity_strength_changed")
			}
			if strong && v.previousProof != native.Claude.Signature {
				return observed, fmt.Errorf("native_root_changed")
			}
		}
		if strong {
			evidence = session.StrictSendIdleEvidence{DurableClaudeStop: true,
				ProcessStartedAt: native.Claude.ProcessStartedAt, CWD: native.Claude.CWD}
		}
	}
	decision := v.deps.idle(v.inst.ID, v.inst.Tool, v.expected, evidence)
	observed.IdleDecision = decision
	if !decision.Admitted {
		return observed, fmt.Errorf("%s", decision.Reason)
	}
	observed.Thread = native.Thread
	return observed, nil
}

func (v *strictSessionAdmissionVerifier) callback(id tmux.StrictPaneIdentity) error {
	_, err := v.verify(id)
	return err
}

func handleStrictSessionSend(out *CLIOutput, inst *session.Instance, expectedThread, message string) {
	result := tmux.StrictSendResult{Delivery: "refused", Reason: "target_unavailable"}
	var sendErr error = fmt.Errorf("strict target unavailable")
	target := inst.GetTmuxSession()
	if !strictToolPlatformSupported(runtime.GOOS, runtime.GOARCH, inst.Tool) {
		result.Reason = "unsupported_platform"
		sendErr = fmt.Errorf("strict send is unsupported on this platform")
	}
	if strictToolPlatformSupported(runtime.GOOS, runtime.GOARCH, inst.Tool) && target != nil && strictThreadUUID.MatchString(expectedThread) {
		verifier := newStrictSessionAdmissionVerifier(inst, target, expectedThread)
		result, sendErr = target.StrictSendOnce(inst.Tool, message, verifier.callback)
	}
	data := map[string]interface{}{"success": sendErr == nil, "delivery": result.Delivery, "attempted": result.Attempted, "reason": result.Reason, "session_id": inst.ID, "expected_thread": expectedThread}
	if sendErr != nil {
		out.ErrorWithData(sendErr.Error(), ErrCodeDeliveryFailed, data)
		os.Exit(1)
	}
	out.Success("Terminal attempt completed; session consumption is unknown", data)
}

func strictProofDigest(proof string) string {
	if proof == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(proof)))
}

func strictProbeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func runStrictAdmissionProbe(verifier *strictSessionAdmissionVerifier, id tmux.StrictPaneIdentity) ([2]strictAdmissionObservation, error) {
	var observations [2]strictAdmissionObservation
	var err error
	for index := range observations {
		observations[index], err = verifier.verify(id)
		if err != nil {
			return observations, err
		}
	}
	if verifier.passes != 2 {
		return observations, fmt.Errorf("strict admission second pass missing")
	}
	return observations, nil
}

func printStrictProbeUsage() {
	fmt.Println("Usage: agent-deck session strict-probe <full-session-id> [--json]")
	fmt.Println()
	fmt.Println("Run the strict native/idle verifier twice without staging or terminal input.")
}

func parseStrictProbeArguments(args []string) (string, bool, error) {
	fs := flag.NewFlagSet("session strict-probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		return "", false, err
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return "", false, fmt.Errorf("one full session ID is required")
	}
	return remaining[0], *jsonOutput, nil
}

func loadStrictProbeRegistryTarget(profile, identifier string) (*session.Instance, error) {
	storage, err := session.OpenStorageReadOnlyWithProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("read-only session registry unavailable")
	}
	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		if closeErr := storage.Close(); closeErr != nil {
			return nil, fmt.Errorf("read-only session registry close failed")
		}
		return nil, fmt.Errorf("read-only session registry is incompatible")
	}
	inst, _, _ := ResolveSession(identifier, instances)
	if err := storage.Close(); err != nil {
		return nil, fmt.Errorf("read-only session registry close failed")
	}
	if inst == nil || identifier != inst.ID {
		return nil, fmt.Errorf("strict probe requires one exact session ID")
	}
	return inst, nil
}

func handleStrictSessionProbe(profile string, args []string) {
	identifier, jsonOutput, parseErr := parseStrictProbeArguments(args)
	if errors.Is(parseErr, flag.ErrHelp) {
		printStrictProbeUsage()
		return
	}
	out := NewCLIOutput(jsonOutput, false)
	if parseErr != nil {
		out.Error(parseErr.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	inst, err := loadStrictProbeRegistryTarget(profile, identifier)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	if !strictToolPlatformSupported(runtime.GOOS, runtime.GOARCH, inst.Tool) || inst.IsSSH() || !inst.Exists() {
		out.Error("strict probe requires one exact live local Claude session ID", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	target := inst.GetTmuxSession()
	if target == nil {
		out.Error("strict probe target unavailable", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	identity, err := target.StrictProbeIdentity()
	if err != nil {
		out.Error("strict probe identity unavailable", ErrCodeDeliveryFailed)
		os.Exit(1)
	}
	verifier := newStrictSessionAdmissionVerifier(inst, target, "")
	observations, probeErr := runStrictAdmissionProbe(verifier, identity)
	reason := ""
	if probeErr != nil && safeStrictProbeReason(probeErr.Error()) {
		reason = probeErr.Error()
	}
	if reason == "" {
		reason = observations[1].IdleDecision.Reason
	}
	if reason == "" {
		reason = observations[0].IdleDecision.Reason
	}
	if reason == "" {
		reason = "admission_refused"
	}
	data := map[string]interface{}{
		"success": probeErr == nil, "admitted": probeErr == nil,
		"session_id": inst.ID, "thread": verifier.expected,
		"durable_stop":       probeErr == nil && observations[1].IdleDecision.DurableStop,
		"hook_age_seconds":   []int64{observations[0].IdleDecision.HookAgeSeconds, observations[1].IdleDecision.HookAgeSeconds},
		"process_started_at": strictProbeTime(observations[1].ProcessStart),
		"signature_pass1":    strictProofDigest(observations[0].NativeProof),
		"signature_pass2":    strictProofDigest(observations[1].NativeProof),
		"reason":             reason,
	}
	if probeErr != nil {
		out.ErrorWithData("strict admission probe refused", ErrCodeDeliveryFailed, data)
		os.Exit(1)
	}
	out.Success("Strict admission probe passed without terminal effects", data)
}

func strictClaudeThreadProof(inst *session.Instance, target *tmux.Session, id tmux.StrictPaneIdentity) (strictClaudeNativeProof, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native home unavailable")
	}
	if runtime.GOOS == "linux" {
		return strictClaudeLinuxThreadProof(inst, target, id, home)
	}
	return strictClaudeThreadProofWithRun(session.GetClaudeConfigDirForInstance(inst), inst.ProjectPath,
		id, runtime.GOOS, home, strictNativeProbe)
}

func strictClaudeThreadProofWithRun(configDir, project string, id tmux.StrictPaneIdentity,
	domain, home string, run strictProbe) (strictClaudeNativeProof, error) {
	return strictClaudeThreadProofWithDeps(configDir, project, id, domain, home, run, strictClaudeOwnedBinding)
}

func strictClaudeThreadProofWithDeps(configDir, project string, id tmux.StrictPaneIdentity,
	domain, home string, run strictProbe,
	bind func(string, int64, bool) (strictClaudeFileBinding, *os.File, []byte, error)) (strictClaudeNativeProof, error) {
	pid, err := strconv.Atoi(id.PID)
	if err != nil || pid <= 0 {
		return strictClaudeNativeProof{}, fmt.Errorf("invalid native pid")
	}
	recordPath := filepath.Join(configDir, "sessions", id.PID+".json")
	if id.Command == "claude" || strings.HasPrefix(id.Command, "claude-") {
		file, _, err := strictOpenVerifiedPath(recordPath)
		if err != nil {
			return strictClaudeNativeProof{}, fmt.Errorf("native session unavailable")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
			return strictClaudeNativeProof{}, fmt.Errorf("invalid native record")
		}
		data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
		if err != nil || len(data) > 64*1024 {
			return strictClaudeNativeProof{}, fmt.Errorf("invalid native record")
		}
		var record strictClaudeRecord
		if json.Unmarshal(data, &record) != nil || !strictClaudeIdentityWithRun(record, id, project, domain, home, run) {
			return strictClaudeNativeProof{}, fmt.Errorf("native session identity mismatch")
		}
		return strictClaudeNativeProof{Thread: record.SessionID, CWD: record.CWD}, nil
	}

	firstRecord, heldRecord, recordData, err := bind(recordPath, 64*1024, true)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native session unavailable")
	}
	defer heldRecord.Close()
	var record strictClaudeRecord
	if json.Unmarshal(recordData, &record) != nil || !strictThreadUUID.MatchString(record.SessionID) {
		return strictClaudeNativeProof{}, fmt.Errorf("invalid native record")
	}
	runtimeProof, err := strictClaudeRuntimeProofWithRun(record, id, project, domain, home, run)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native session identity mismatch")
	}
	rootPath := filepath.Join(configDir, "projects", session.ConvertToClaudeDirName(record.CWD), record.SessionID+".jsonl")
	firstRoot, heldRoot, _, err := bind(rootPath, 0, false)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native root unavailable")
	}
	defer heldRoot.Close()
	secondRecord, heldRecordAgain, _, err := bind(recordPath, 64*1024, true)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native record changed")
	}
	defer heldRecordAgain.Close()
	secondRoot, heldRootAgain, _, err := bind(rootPath, 0, false)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native root changed")
	}
	defer heldRootAgain.Close()
	if firstRecord != secondRecord {
		return strictClaudeNativeProof{}, fmt.Errorf("native record changed")
	}
	if firstRoot != secondRoot {
		return strictClaudeNativeProof{}, fmt.Errorf("native root changed")
	}
	startedAgain, err := run("ps", "-p", id.PID, "-o", "lstart=")
	if err != nil || !strictClaudeIdentityMatches(record, runtimeProof.NativeID, project, string(startedAgain), domain) {
		return strictClaudeNativeProof{}, fmt.Errorf("native process start changed")
	}
	parsedAgain, err := strictParseClaudeProcessStart(string(startedAgain))
	if err != nil || !parsedAgain.Equal(runtimeProof.ProcessStartedAt) {
		return strictClaudeNativeProof{}, fmt.Errorf("native process start changed")
	}
	signature := strings.Join([]string{record.SessionID, filepath.Clean(record.CWD), runtimeProof.Signature,
		firstRecord.signature(), firstRoot.signature()}, "|")
	return strictClaudeNativeProof{Thread: record.SessionID, ProcessStartedAt: runtimeProof.ProcessStartedAt,
		CWD: record.CWD, Signature: signature, DurableStop: true}, nil
}

type strictClaudeRecord struct {
	PID                 int      `json:"pid"`
	SessionID           string   `json:"sessionId"`
	CWD                 string   `json:"cwd"`
	Tmux                string   `json:"tmux"`
	ProcStart           string   `json:"procStart"`
	PIDDomain           string   `json:"pidDomain"`
	Version             string   `json:"version"`
	Entrypoint          string   `json:"entrypoint"`
	Kind                string   `json:"kind"`
	MessagingSocketPath string   `json:"messagingSocketPath"`
	Name                string   `json:"name"`
	NameSince           int64    `json:"nameSince"`
	NameSource          string   `json:"nameSource"`
	PeerFeatures        []string `json:"peerFeatures"`
	PeerProtocol        int64    `json:"peerProtocol"`
	StartedAt           int64    `json:"startedAt"`
	Status              string   `json:"status"`
	StatusUpdatedAt     int64    `json:"statusUpdatedAt"`
	UpdatedAt           int64    `json:"updatedAt"`
}

var strictClaudeNativeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type strictClaudeRuntimeProof struct {
	NativeID         tmux.StrictPaneIdentity
	ProcessStartedAt time.Time
	Signature        string
}

type strictClaudeFileBinding struct {
	Path, Ancestors, ContentSHA string
	Device, Inode, Nlink        uint64
	Size, ModTime               int64
	Mode                        uint32
}

func (b strictClaudeFileBinding) signature() string {
	return fmt.Sprintf("%s:%s:%x:%d:%d:%d:%d:%o:%s", b.Path, b.Ancestors, b.Device,
		b.Inode, b.Nlink, b.Size, b.ModTime, b.Mode, b.ContentSHA)
}

func strictClaudeOwnedBinding(path string, maxSize int64, readContent bool) (strictClaudeFileBinding, *os.File, []byte, error) {
	file, ancestors, err := strictOpenVerifiedPath(path)
	if err != nil {
		return strictClaudeFileBinding{}, nil, nil, err
	}
	fail := func(reason string) (strictClaudeFileBinding, *os.File, []byte, error) {
		file.Close()
		return strictClaudeFileBinding{}, nil, nil, fmt.Errorf("%s", reason)
	}
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || maxSize > 0 && before.Size() > maxSize || before.Mode().Perm()&0022 != 0 {
		return fail("unsafe native file")
	}
	underlying, ok := before.Sys().(*syscall.Stat_t)
	uid := os.Getuid()
	if !ok || uid < 0 || int64(underlying.Uid) != int64(uid) || underlying.Nlink != 1 {
		return fail("unsafe native file ownership")
	}
	device, inode, err := strictFileIdentity(file)
	if err != nil {
		return fail("native file identity unavailable")
	}
	var data []byte
	contentSHA := ""
	if readContent {
		data, err = io.ReadAll(io.LimitReader(file, maxSize+1))
		if err != nil || int64(len(data)) != before.Size() || int64(len(data)) > maxSize {
			return fail("native file read changed")
		}
		contentSHA = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	after, err := file.Stat()
	if err != nil {
		return fail("native file changed")
	}
	afterUnderlying, afterOK := after.Sys().(*syscall.Stat_t)
	if !afterOK || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		underlying.Uid != afterUnderlying.Uid || underlying.Nlink != afterUnderlying.Nlink {
		return fail("native file changed")
	}
	return strictClaudeFileBinding{Path: path, Ancestors: ancestors, ContentSHA: contentSHA,
		Device: device, Inode: inode, Nlink: uint64(afterUnderlying.Nlink), Size: after.Size(), ModTime: after.ModTime().UnixNano(),
		Mode: uint32(after.Mode().Perm())}, file, data, nil
}

func strictFoldClaudeProcessStart(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func strictParseClaudeProcessStart(value string) (time.Time, error) {
	folded := strictFoldClaudeProcessStart(value)
	if folded == "" {
		return time.Time{}, fmt.Errorf("empty process start")
	}
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", folded, time.UTC)
}

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
	_, err = strictClaudeRuntimeProofWithStarted(record, id, project, domain, home, string(started), run)
	return err == nil
}

func strictClaudeRuntimeProofWithRun(record strictClaudeRecord, id tmux.StrictPaneIdentity, project, domain, home string, run strictProbe) (strictClaudeRuntimeProof, error) {
	if domain != "darwin" || !strictValidPID(id.PID) {
		return strictClaudeRuntimeProof{}, fmt.Errorf("unsupported native identity")
	}
	started, err := run("ps", "-p", id.PID, "-o", "lstart=")
	if err != nil {
		return strictClaudeRuntimeProof{}, err
	}
	return strictClaudeRuntimeProofWithStarted(record, id, project, domain, home, string(started), run)
}

func strictClaudeRuntimeProofWithStarted(record strictClaudeRecord, id tmux.StrictPaneIdentity, project, domain, home, started string, run strictProbe) (strictClaudeRuntimeProof, error) {
	if !strictClaudeNativeVersion.MatchString(id.Command) || record.Version != id.Command || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude version unverified")
	}
	// Normalize only after choosing the separately corroborated native path;
	// retain every existing record/PID/start/CWD/tmux requirement.
	nativeID := id
	nativeID.Command = "claude"
	if !strictClaudeIdentityMatches(record, nativeID, project, started, domain) {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude record mismatch")
	}
	parsedStart, err := strictParseClaudeProcessStart(started)
	if err != nil || strictFoldClaudeProcessStart(record.ProcStart) != strictFoldClaudeProcessStart(started) {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude process start invalid")
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
		return strictClaudeRuntimeProof{}, err
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
		return strictClaudeRuntimeProof{}, err
	}
	defer held.Close()
	second, heldAgain, err := observeExecutable()
	if err != nil {
		return strictClaudeRuntimeProof{}, err
	}
	defer heldAgain.Close()
	after, err := observeProcess()
	if err != nil || before != after || first != second {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude process or executable changed")
	}
	startedAgain, err := run("ps", "-p", id.PID, "-o", "lstart=")
	if err != nil || !strictClaudeIdentityMatches(record, nativeID, project, string(startedAgain), domain) {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude process start changed")
	}
	parsedAgain, err := strictParseClaudeProcessStart(string(startedAgain))
	if err != nil || !parsedAgain.Equal(parsedStart) {
		return strictClaudeRuntimeProof{}, fmt.Errorf("native Claude process start changed")
	}
	return strictClaudeRuntimeProof{NativeID: nativeID, ProcessStartedAt: parsedStart,
		Signature: strings.Join([]string{before, strictFoldClaudeProcessStart(started), first.signature()}, "|")}, nil
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
