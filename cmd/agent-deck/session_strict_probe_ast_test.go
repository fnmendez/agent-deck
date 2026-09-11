package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

type probeASTFunction struct {
	id, path, name, receiver string
}

func probeCalls(values ...string) []string {
	sort.Strings(values)
	return values
}

var probeASTClosure = []probeASTFunction{
	{"cmd.handleStrictSessionProbe", "session_strict_send.go", "handleStrictSessionProbe", ""},
	{"cmd.loadStrictProbeRegistryTarget", "session_strict_send.go", "loadStrictProbeRegistryTarget", ""},
	{"cmd.runStrictAdmissionProbe", "session_strict_send.go", "runStrictAdmissionProbe", ""},
	{"cmd.newStrictSessionAdmissionVerifier", "session_strict_send.go", "newStrictSessionAdmissionVerifier", ""},
	{"cmd.verifier.verify", "session_strict_send.go", "verify", "strictSessionAdmissionVerifier"},
	{"cmd.strictNativeThread", "session_strict_send.go", "strictNativeThread", ""},
	{"cmd.strictClaudeThreadProof", "session_strict_send.go", "strictClaudeThreadProof", ""},
	{"cmd.strictClaudeLinuxThreadProof", "session_strict_linux_native.go", "strictClaudeLinuxThreadProof", ""},
	{"cmd.strictClaudeLinuxDefaultDeps", "session_strict_linux_native.go", "strictClaudeLinuxDefaultDeps", ""},
	{"cmd.strictClaudeLinuxThreadProofWithDeps", "session_strict_linux_native.go", "strictClaudeLinuxThreadProofWithDeps", ""},
	{"cmd.strictClaudeLinuxPackageProof", "session_strict_linux_native.go", "strictClaudeLinuxPackageProof", ""},
	{"cmd.strictClaudeLinuxPackageProofWithLinks", "session_strict_linux_native.go", "strictClaudeLinuxPackageProofWithLinks", ""},
	{"cmd.strictClaudeLinuxOwnedPackageFile", "session_strict_linux_native.go", "strictClaudeLinuxOwnedPackageFile", ""},
	{"cmd.strictClaudeLinuxProcessProof", "session_strict_linux_native.go", "strictClaudeLinuxProcessProof", ""},
	{"cmd.strictLinuxMachineIDProof", "session_strict_linux_native.go", "strictLinuxMachineIDProof", ""},
	{"cmd.strictLinuxReadBounded", "session_strict_linux_native.go", "strictLinuxReadBounded", ""},
	{"cmd.strictClaudeThreadProofWithRun", "session_strict_send.go", "strictClaudeThreadProofWithRun", ""},
	{"cmd.strictClaudeThreadProofWithDeps", "session_strict_send.go", "strictClaudeThreadProofWithDeps", ""},
	{"cmd.strictClaudeRuntimeProofWithRun", "session_strict_send.go", "strictClaudeRuntimeProofWithRun", ""},
	{"cmd.strictClaudeRuntimeProofWithStarted", "session_strict_send.go", "strictClaudeRuntimeProofWithStarted", ""},
	{"cmd.strictClaudeOwnedBinding", "session_strict_send.go", "strictClaudeOwnedBinding", ""},
	{"cmd.strictClaudeIdentityWithRun", "session_strict_send.go", "strictClaudeIdentityWithRun", ""},
	{"cmd.strictClaudeIdentityMatches", "session_strict_send.go", "strictClaudeIdentityMatches", ""},
	{"cmd.strictClaudeExecutableDescriptor", "session_strict_send.go", "strictClaudeExecutableDescriptor", ""},
	{"cmd.strictFoldClaudeProcessStart", "session_strict_send.go", "strictFoldClaudeProcessStart", ""},
	{"cmd.strictParseClaudeProcessStart", "session_strict_send.go", "strictParseClaudeProcessStart", ""},
	{"cmd.strictProofDigest", "session_strict_send.go", "strictProofDigest", ""},
	{"cmd.strictProbeTime", "session_strict_send.go", "strictProbeTime", ""},
	{"cmd.safeStrictProbeReason", "session_strict_send.go", "safeStrictProbeReason", ""},
	{"cmd.strictNativeProbe", "session_strict_codex.go", "strictNativeProbe", ""},
	{"cmd.strictNativeCommand", "session_strict_codex.go", "strictNativeCommand", ""},
	{"cmd.strictCodexRuntimeProof", "session_strict_codex.go", "strictCodexRuntimeProof", ""},
	{"cmd.strictCodexProofWithRun", "session_strict_codex.go", "strictCodexProofWithRun", ""},
	{"cmd.strictOpenVerifiedPath", "session_strict_files.go", "strictOpenVerifiedPath", ""},
	{"cmd.strictOpenVerifiedDirectory", "session_strict_files.go", "strictOpenVerifiedDirectory", ""},
	{"cmd.strictFileIdentity", "session_strict_files.go", "strictFileIdentity", ""},
	{"tmux.StrictProbeIdentity", "../../internal/tmux/strict_send.go", "StrictProbeIdentity", "Session"},
	{"tmux.strictProbeIdentity", "../../internal/tmux/strict_send.go", "strictProbeIdentity", ""},
	{"tmux.strictSnapshot", "../../internal/tmux/strict_send.go", "strictSnapshot", "Session"},
	{"tmux.captureStrictSnapshot", "../../internal/tmux/strict_send.go", "captureStrictSnapshot", ""},
	{"tmux.strictOperatorIdle", "../../internal/tmux/strict_send.go", "strictOperatorIdle", ""},
	{"tmux.paneLineDiscipline", "../../internal/tmux/canonical_line.go", "paneLineDiscipline", "Session"},
	{"tmux.ttyLineDiscipline", "../../internal/tmux/canonical_line_unix.go", "ttyLineDiscipline", ""},
	{"tmux.canonicalBufferBytes", "../../internal/tmux/canonical_line_unix.go", "canonicalBufferBytes", ""},
	{"tmux.runBoundedOutput.method", "../../internal/tmux/socket.go", "runBoundedOutput", "Session"},
	{"tmux.runBoundedOutput.function", "../../internal/tmux/socket.go", "runBoundedOutput", ""},
	{"tmux.tmuxCmdContext", "../../internal/tmux/socket.go", "tmuxCmdContext", "Session"},
	{"tmux.tmuxExecContext", "../../internal/tmux/socket.go", "tmuxExecContext", ""},
	{"tmux.tmuxArgs", "../../internal/tmux/socket.go", "tmuxArgs", ""},
	{"tmux.assertTmuxSpawnIsolated", "../../internal/tmux/default_socket_guard.go", "assertTmuxSpawnIsolated", ""},
	{"tmux.Exists", "../../internal/tmux/tmux.go", "Exists", "Session"},
	{"session.StrictSendIdle", "../../internal/session/strict_send.go", "StrictSendIdle", ""},
	{"session.strictSendIdleAt", "../../internal/session/strict_send.go", "strictSendIdleAt", ""},
	{"session.OpenStorageReadOnlyWithProfile", "../../internal/session/storage.go", "OpenStorageReadOnlyWithProfile", ""},
	{"statedb.OpenReadOnly", "../../internal/statedb/statedb.go", "OpenReadOnly", ""},
	{"statedb.snapshotReadOnlyRegistry", "../../internal/statedb/statedb.go", "snapshotReadOnlyRegistry", ""},
	{"statedb.snapshotReadOnlyRegistryWithHook", "../../internal/statedb/statedb.go", "snapshotReadOnlyRegistryWithHook", ""},
	{"statedb.registrySnapshotSources", "../../internal/statedb/statedb.go", "registrySnapshotSources", ""},
	{"statedb.fingerprintRegistrySource", "../../internal/statedb/statedb.go", "fingerprintRegistrySource", ""},
}

var probeASTExpectedCalls = map[string][]string{
	"cmd.handleStrictSessionProbe":      probeCalls("NewCLIOutput", "errors.Is", "inst.Exists", "inst.GetTmuxSession", "inst.IsSSH", "loadStrictProbeRegistryTarget", "newStrictSessionAdmissionVerifier", "os.Exit", "out.Error", "out.ErrorWithData", "out.Success", "parseStrictProbeArguments", "printStrictProbeUsage", "runStrictAdmissionProbe", "safeStrictProbeReason", "strictProbeTargetSupported", "strictProbeTime", "strictProofDigest", "target.StrictProbeIdentity"),
	"cmd.loadStrictProbeRegistryTarget": probeCalls("ResolveSession", "session.OpenStorageReadOnlyWithProfile", "storage.Close", "storage.LoadWithGroups"),
	"cmd.runStrictAdmissionProbe":       probeCalls("observe", "verifier.verify"),
	"cmd.verifier.verify":               probeCalls("strictBracketPasteAdmission", "strictLinuxNativeIdleAdmission", "v.deps.codex", "v.deps.idle", "v.deps.native"),
	"cmd.strictNativeThread":            probeCalls("strictClaudeThreadProof", "strictCodexPlatformSupported", "target.StrictThreadEnvironment"),
	"cmd.strictClaudeThreadProof":       probeCalls("os.UserHomeDir", "session.GetClaudeConfigDirForInstance", "strictClaudeLinuxThreadProof", "strictClaudeThreadProofWithRun"),
	"cmd.strictClaudeLinuxThreadProof":  probeCalls("strictClaudeLinuxDefaultDeps", "strictClaudeLinuxThreadProofWithDeps"),
	"cmd.strictClaudeLinuxDefaultDeps":  probeCalls("os.Getuid"),
	"cmd.strictClaudeLinuxThreadProofWithDeps": probeCalls("after.signature", "before.signature", "deps.bind", "deps.packageProof", "firstPackage.signature", "firstRoot.signature",
		"heldExecutable.Close", "heldExecutableAgain.Close", "heldPackage.Close", "heldPackageAgain.Close", "heldRecord.Close", "heldRecordAgain.Close",
		"heldRoot.Close", "heldRootAgain.Close", "secondPackage.signature", "session.ConvertToClaudeDirName", "session.GetClaudeConfigDirForInstance",
		"strictClaudeLinuxProcessProof", "strictClaudeLinuxRecordBounds", "strictClaudeLinuxRecordSignature", "strictDecodeClaudeLinuxRecord"),
	"cmd.strictClaudeLinuxPackageProof":          probeCalls("strictClaudeLinuxPackageProofWithLinks"),
	"cmd.strictClaudeLinuxPackageProofWithLinks": probeCalls("directory.Stat", "directoryAfter.ModTime", "directoryAfter.Mode", "directoryAfter.Sys", "directoryBefore.IsDir", "directoryBefore.ModTime", "directoryBefore.ModTime().Equal", "directoryBefore.ModTime().UnixNano", "directoryBefore.Mode", "directoryBefore.Mode().Perm", "directoryBefore.Sys", "fail", "handles.Close", "strictClaudeLinuxOwnedPackageFile", "strictClaudeLinuxPackageMetadata", "strictClaudeLinuxPackagePath", "strictFileIdentity", "strictOpenVerifiedDirectory", "uint64"),
	"cmd.strictClaudeLinuxOwnedPackageFile":      probeCalls("after.Sys", "fail", "file.Close", "file.Stat", "hash.Write", "io.Copy", "io.LimitReader", "io.ReadAll", "strictFileIdentity", "strictOpenVerifiedPath", "uint64"),
	"cmd.strictClaudeLinuxProcessProof": probeCalls("after.ModTime().Equal", "after.Sys", "bootTime.Add", "deps.open", "deps.readBounded", "deps.readlink", "deps.stat", "fail", "handles.Close", "held.Stat",
		"info.Mode().Perm", "io.Copy", "io.LimitReader", "machineBinding.signature", "os.FileMode", "processStart.UTC", "recordStart.Sub", "strconv.FormatUint", "strconv.ParseInt",
		"strictFileIdentity", "strictLinuxBootTime", "strictLinuxClaudeAtomicResidue", "strictLinuxMachineIDProof", "strictLinuxPIDNamespace.MatchString", "strictLinuxProcessStateAllowed",
		"strictMatchLinuxProcessStatus", "strictParseLinuxProcessStat", "strings.TrimSuffix", "time.Duration", "time.UnixMilli",
		"time.UnixMilli(record.StartedAt).UTC", "ttyInfo.Sys", "uint64"),
	"cmd.strictLinuxMachineIDProof":            probeCalls("after.Sys", "fail", "file.Close", "file.Stat", "io.LimitReader", "io.ReadAll", "strictFileIdentity", "strictLinuxMachineID.Match", "strictOpenVerifiedPath", "uint64"),
	"cmd.strictLinuxReadBounded":               probeCalls("file.Close", "io.LimitReader", "io.ReadAll", "os.Open"),
	"cmd.strictClaudeThreadProofWithRun":       probeCalls("strictClaudeThreadProofWithDeps"),
	"cmd.strictClaudeThreadProofWithDeps":      probeCalls("bind", "file.Close", "file.Stat", "firstRecord.signature", "firstRoot.signature", "heldRecord.Close", "heldRecordAgain.Close", "heldRoot.Close", "heldRootAgain.Close", "io.LimitReader", "io.ReadAll", "run", "session.ConvertToClaudeDirName", "strictClaudeIdentityMatches", "strictClaudeIdentityWithRun", "strictClaudeRuntimeProofWithRun", "strictOpenVerifiedPath", "strictParseClaudeProcessStart"),
	"cmd.strictClaudeRuntimeProofWithRun":      probeCalls("run", "strictClaudeRuntimeProofWithStarted", "strictValidPID"),
	"cmd.strictClaudeRuntimeProofWithStarted":  probeCalls("file.Close", "file.Stat", "first.signature", "held.Close", "heldAgain.Close", "observeExecutable", "observeProcess", "run", "strictClaudeExecutableDescriptor", "strictClaudeIdentityMatches", "strictFileIdentity", "strictFoldClaudeProcessStart", "strictOpenVerifiedPath", "strictParseClaudeProcessStart", "strictValidPID"),
	"cmd.strictClaudeOwnedBinding":             probeCalls("after.Sys", "fail", "file.Close", "file.Stat", "io.LimitReader", "io.ReadAll", "os.Getuid", "strictFileIdentity", "strictOpenVerifiedPath", "uint64"),
	"cmd.strictClaudeIdentityWithRun":          probeCalls("run", "strictClaudeIdentityMatches", "strictClaudeRuntimeProofWithStarted", "strictValidPID"),
	"cmd.strictParseClaudeProcessStart":        probeCalls("strictFoldClaudeProcessStart"),
	"cmd.strictNativeProbe":                    probeCalls("cancel", "cmd.Output", "context.Background", "context.WithTimeout", "strictNativeCommand"),
	"cmd.strictNativeCommand":                  probeCalls("exec.CommandContext", "strictValidPID", "validPIDList"),
	"cmd.strictCodexRuntimeProof":              probeCalls("strictCodexPlatformSupported", "strictCodexProofWithRun"),
	"cmd.strictCodexProofWithRun":              probeCalls("first.signature", "held.Close", "heldAgain.Close", "observeProcess", "observeRoot", "p.signature", "run", "strictForegroundChain", "strictParseDescriptors", "strictRootDescriptor", "strictValidPID", "strictWrapperCommand"),
	"cmd.strictOpenVerifiedPath":               probeCalls("file.Close", "file.Stat", "os.NewFile", "unix.Close", "unix.Fstat", "unix.Open", "unix.Openat"),
	"cmd.strictOpenVerifiedDirectory":          probeCalls("file.Close", "file.Stat", "os.NewFile", "unix.Close", "unix.Fstat", "unix.Open", "unix.Openat"),
	"cmd.strictFileIdentity":                   probeCalls("file.Fd", "unix.Fstat"),
	"tmux.StrictProbeIdentity":                 probeCalls("strictProbeIdentity"),
	"tmux.strictProbeIdentity":                 probeCalls("snapshot", "strictEmptyComposer"),
	"tmux.strictSnapshot":                      probeCalls("captureStrictSnapshot", "s.paneLineDiscipline"),
	"tmux.captureStrictSnapshot":               probeCalls("rawMode", "read", "strictOperatorIdle"),
	"tmux.strictOperatorIdle":                  probeCalls("strconv.ParseInt"),
	"tmux.paneLineDiscipline":                  probeCalls("errors.New", "s.runBoundedOutput", "ttyLineDiscipline"),
	"tmux.ttyLineDiscipline":                   probeCalls("canonicalBufferBytes", "func() { _ = unix.Close(fd) }", "unix.Close", "unix.IoctlGetTermios", "unix.Open"),
	"tmux.canonicalBufferBytes":                probeCalls(),
	"tmux.runBoundedOutput.method":             probeCalls("runBoundedOutput"),
	"tmux.runBoundedOutput.function":           probeCalls("cancel", "context.Background", "context.WithTimeout", "tmuxExecContext", "tmuxExecContext(ctx, socketName, args...).Output"),
	"tmux.tmuxCmdContext":                      probeCalls("tmuxExecContext"),
	"tmux.tmuxExecContext":                     probeCalls("assertTmuxSpawnIsolated", "exec.CommandContext", "tmuxArgs"),
	"tmux.tmuxArgs":                            probeCalls("panic", "tmuxutf8.Prepend", "uint"),
	"tmux.assertTmuxSpawnIsolated":             probeCalls("assertTmuxSpawnIsolatedFor", "looksLikeGoTestBinary", "os.Getuid"),
	"tmux.Exists":                              probeCalls("DefaultSocketName", "GetPipeManager", "cancel", "context.Background", "context.WithTimeout", "pm.IsConnected", "s.tmuxCmdContext", "s.tmuxCmdContext(ctx, \"has-session\", \"-t\", s.Name).Run", "sessionExistsFromCache", "socketHasProtocolMismatch"),
	"session.StrictSendIdle":                   probeCalls("strictSendIdleAt"),
	"session.strictSendIdleAt":                 probeCalls("hookFastPathFreshnessForTool", "hookGenerationForInstance", "hookGenerationRecordAccepted", "hookStatusFilePath", "readStatusFileNoFollow"),
	"session.OpenStorageReadOnlyWithProfile":   probeCalls("GetProfileDir", "ResolveProfileForStorage", "os.Lstat", "statedb.OpenReadOnly"),
	"statedb.OpenReadOnly":                     probeCalls("db.Close", "db.Ping", "db.QueryRow", "db.QueryRow(\"PRAGMA query_only\").Scan", "db.QueryRow(`SELECT value FROM metadata WHERE key = 'schema_version'`).Scan", "db.SetMaxOpenConns", "fail", "os.Getpid", "os.Lstat", "os.RemoveAll", "snapshotReadOnlyRegistry", "sql.Open"),
	"statedb.snapshotReadOnlyRegistry":         probeCalls("snapshotReadOnlyRegistryWithHook"),
	"statedb.snapshotReadOnlyRegistryWithHook": probeCalls("afterCopy", "cleanup", "destination.Close", "destination.Sync", "fingerprintRegistrySource", "os.Lstat", "os.MkdirTemp", "os.OpenFile", "os.RemoveAll", "registrySnapshotSources"),
	"statedb.registrySnapshotSources":          probeCalls("os.IsNotExist", "os.Lstat"),
	"statedb.fingerprintRegistrySource":        probeCalls("file.Close", "file.Stat", "io.Copy", "io.MultiWriter", "metadata", "os.Lstat", "os.OpenFile"),
}

var probeASTExpectedSinks = map[string][]string{
	"cmd.strictClaudeThreadProofWithDeps":     probeCalls(`run("ps", "-p", id.PID, "-o", "lstart=")`),
	"cmd.strictClaudeRuntimeProofWithRun":     probeCalls(`run("ps", "-p", id.PID, "-o", "lstart=")`),
	"cmd.strictClaudeRuntimeProofWithStarted": probeCalls(`run("lsof", "-a", "-p", id.PID, "-F", "pfaDint")`, `run("ps", "-p", id.PID, "-o", "lstart=")`, `run("ps", "-p", id.PID, "-o", "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm=")`),
	"cmd.strictClaudeIdentityWithRun":         probeCalls(`run("ps", "-p", id.PID, "-o", "lstart=")`),
	"cmd.strictNativeCommand":                 probeCalls(`exec.CommandContext(ctx, "/bin/ps", args...)`, `exec.CommandContext(ctx, "/usr/sbin/lsof", args...)`),
	"cmd.strictCodexProofWithRun":             probeCalls(`run("lsof", "-a", "-p", pid, "-F", "pfaDint")`, `run("ps", "-Ao", "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm=")`, `run("ps", "-p", strings.Join(pids, ","), "-o", "pid=,lstart=")`),
	"cmd.strictOpenVerifiedPath":              probeCalls(`unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)`),
	"cmd.strictOpenVerifiedDirectory":         probeCalls(`unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)`),
	"tmux.captureStrictSnapshot":              probeCalls(`read("capture-pane", "-p", "-e", "-t", target)`, `read("display-message", "-p", "-t", name, strictMetadataFormat)`, `read("display-message", "-p", "-t", name, strictMetadataFormat)`, `read("list-clients", "-t", fields[0], "-F", "#{client_control_mode}|#{client_activity}")`),
	"tmux.paneLineDiscipline":                 probeCalls(`s.runBoundedOutput("display-message", "-t", target, "-p", "#{pane_tty}")`),
	"tmux.ttyLineDiscipline":                  probeCalls(`unix.Open(tty, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)`),
	"tmux.tmuxExecContext":                    probeCalls(`exec.CommandContext(ctx, "tmux", tmuxArgs(socketName, args...)...)`),
	"tmux.Exists":                             probeCalls(`s.tmuxCmdContext(ctx, "has-session", "-t", s.Name)`),
}

var probeASTPureCalls = map[string]bool{
	"[]byte": true, "append": true, "copy": true, "int": true, "int64": true,
	"io.Writer": true, "len": true, "make": true, "string": true, "uint32": true, "uintptr": true,
	"err.Error": true, "parseErr.Error": true, "probeErr.Error": true,
	"fmt.Errorf": true, "fmt.Sprintf": true, "fmt.Sprint": true,
	"filepath.Clean": true, "filepath.IsAbs": true, "filepath.Join": true, "filepath.ToSlash": true,
	"json.Unmarshal": true, "sha256.New": true, "sha256.Sum256": true,
	"strconv.Atoi": true, "strconv.Itoa": true, "strconv.ParseUint": true,
	"strings.Contains": true, "strings.ContainsAny": true, "strings.Fields": true,
	"strings.HasPrefix": true, "strings.Join": true, "strings.Split": true,
	"strings.TrimPrefix": true, "strings.TrimSpace": true,
	"time.Now": true, "time.ParseInLocation": true, "time.Unix": true,
	"after.ModTime": true, "after.ModTime().UnixNano": true, "after.Mode": true,
	"after.Mode().Perm": true, "after.Size": true,
	"before.ModTime": true, "before.ModTime().Equal": true, "before.Mode": true,
	"before.Mode().IsRegular": true, "before.Mode().Perm": true, "before.Size": true, "before.Sys": true,
	"ctx.Err": true, "evidence.ProcessStartedAt.IsZero": true, "hash.Sum": true,
	"info.IsDir": true, "info.ModTime": true, "info.ModTime().UnixNano": true,
	"info.Mode": true, "info.Mode().IsRegular": true, "info.Size": true, "info.Sys": true,
	"now.Sub": true, "parsedAgain.Equal": true, "stat.Mode": true, "stat.Mode().Perm": true,
	"strictClaudeNativeVersion.MatchString": true, "strictThreadUUID.MatchString": true,
	"time.Unix(raw.Timestamp, 0).Before": true, "value.Format": true, "value.IsZero": true,
}

func probeASTExpr(fset *token.FileSet, expression ast.Node) string {
	var output bytes.Buffer
	if err := printer.Fprint(&output, fset, expression); err != nil {
		return "<print-error>"
	}
	return output.String()
}

func probeASTReceiver(fset *token.FileSet, declaration *ast.FuncDecl) string {
	if declaration.Recv == nil || len(declaration.Recv.List) != 1 {
		return ""
	}
	value := probeASTExpr(fset, declaration.Recv.List[0].Type)
	return strings.TrimPrefix(value, "*")
}

func probeASTFindFunction(t *testing.T, spec probeASTFunction, source []byte) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, spec.path, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", spec.path, err)
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == spec.name && probeASTReceiver(fset, function) == spec.receiver {
			return fset, function
		}
	}
	t.Fatalf("function %s (%s) not found in %s", spec.name, spec.receiver, spec.path)
	return nil, nil
}

func probeASTCalls(t *testing.T, spec probeASTFunction, source []byte) ([]string, []string) {
	t.Helper()
	fset, function := probeASTFindFunction(t, spec, source)
	calls := []string{}
	sinks := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := probeASTExpr(fset, call.Fun)
		calls = append(calls, callee)
		sink := callee == "read" || callee == "run" || callee == "s.runBoundedOutput" ||
			callee == "s.tmuxCmdContext" || callee == "exec.CommandContext" || callee == "unix.Open"
		if sink {
			sinks = append(sinks, probeASTExpr(fset, call))
		}
		return true
	})
	sort.Strings(calls)
	sort.Strings(sinks)
	return calls, sinks
}

func probeASTNonPure(calls []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, call := range calls {
		if probeASTPureCalls[call] || seen[call] {
			continue
		}
		seen[call] = true
		result = append(result, call)
	}
	return result
}

func TestStrictProbeTransitiveASTAllowlist(t *testing.T) {
	for _, spec := range probeASTClosure {
		source, err := os.ReadFile(spec.path)
		if err != nil {
			t.Fatalf("read %s: %v", spec.path, err)
		}
		calls, sinks := probeASTCalls(t, spec, source)
		calls = probeASTNonPure(calls)
		if !slices.Equal(calls, probeASTExpectedCalls[spec.id]) || !slices.Equal(sinks, probeASTExpectedSinks[spec.id]) {
			t.Errorf("%s allowlist drift\n calls: %#v\n sinks: %#v", spec.id, calls, sinks)
		}
	}
}

func mutateProbeASTFunction(t *testing.T, spec probeASTFunction, source []byte, statement string) []byte {
	t.Helper()
	fset, function := probeASTFindFunction(t, spec, source)
	offset := fset.Position(function.Body.Lbrace).Offset + 1
	mutated := make([]byte, 0, len(source)+len(statement)+2)
	mutated = append(mutated, source[:offset]...)
	mutated = append(mutated, '\n')
	mutated = append(mutated, statement...)
	mutated = append(mutated, '\n')
	mutated = append(mutated, source[offset:]...)
	return mutated
}

func TestStrictProbeASTAllowlistRejectsEffectMutants(t *testing.T) {
	byID := map[string]probeASTFunction{}
	for _, spec := range probeASTClosure {
		byID[spec.id] = spec
	}
	for _, mutant := range []struct {
		name, function, statement string
	}{
		{"verifier_ctrl_u", "cmd.verifier.verify", `_ = v.target.SendCtrlU()`},
		{"verifier_named_key", "cmd.verifier.verify", `_ = v.target.SendNamedKey("Escape")`},
		{"verifier_command", "cmd.verifier.verify", `_ = v.target.SendCommand("display-message")`},
		{"handler_kill", "cmd.handleStrictSessionProbe", `_ = target.Kill()`},
		{"snapshot_respawn", "tmux.strictSnapshot", `_ = s.RespawnPane("true")`},
		{"snapshot_environment", "tmux.strictSnapshot", `_ = s.SetEnvironment("K", "V")`},
	} {
		t.Run(mutant.name, func(t *testing.T) {
			spec, ok := byID[mutant.function]
			if !ok {
				t.Fatal("mutation target is outside the reviewed closure")
			}
			source, err := os.ReadFile(spec.path)
			if err != nil {
				t.Fatal(err)
			}
			mutated := mutateProbeASTFunction(t, spec, source, mutant.statement)
			calls, sinks := probeASTCalls(t, spec, mutated)
			calls = probeASTNonPure(calls)
			if slices.Equal(calls, probeASTExpectedCalls[spec.id]) && slices.Equal(sinks, probeASTExpectedSinks[spec.id]) {
				t.Fatal("effect mutant remained inside the closed allowlist")
			}
		})
	}
}
