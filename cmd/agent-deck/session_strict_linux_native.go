package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

const strictLinuxProcReadLimit = 64 * 1024
const strictLinuxClaudeNodeVersion = "v24.18.0"

var (
	strictLinuxMachineID        = regexp.MustCompile(`^[0-9a-f]{32}\n$`)
	strictLinuxPIDNamespace     = regexp.MustCompile(`^pid:\[[1-9][0-9]*\]$`)
	strictLinuxClaudeAtomicSlot = regexp.MustCompile(`^\.claude-code-[A-Za-z0-9]{8}$`)
)

type strictClaudeLinuxDeps struct {
	procRoot       string
	bind           func(string, int64, bool) (strictClaudeFileBinding, *os.File, []byte, error)
	readBounded    func(string, int64) ([]byte, error)
	readlink       func(string) (string, error)
	open           func(string) (*os.File, error)
	stat           func(string) (os.FileInfo, error)
	machineIDPath  string
	machineIDOwner int
	packageProof   func(string, int, int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error)
	uid, gid       int
}

func strictClaudeLinuxDefaultDeps() strictClaudeLinuxDeps {
	return strictClaudeLinuxDeps{
		procRoot: "/proc", bind: strictClaudeOwnedBinding, readBounded: strictLinuxReadBounded,
		readlink: os.Readlink, open: os.Open, stat: os.Stat, machineIDPath: "/etc/machine-id", machineIDOwner: 0,
		packageProof: strictClaudeLinuxPackageProof, uid: os.Getuid(), gid: os.Getgid(),
	}
}

type strictClaudeLinuxProcess struct {
	PID, PPID, PGID, SID, TPGID, TTY, Start, State, Executable, ExecutableSHA, MachineID, MachineIDProof string
	UID, Device, Inode, Size                                                                             uint64
	Mode                                                                                                 uint32
	ModTime                                                                                              int64
	StartedAt                                                                                            time.Time
}

func (p strictClaudeLinuxProcess) signature() string {
	return strings.Join([]string{p.PID, p.PPID, p.PGID, p.SID, p.TPGID, p.TTY, p.Start,
		strconv.FormatUint(p.UID, 10), p.Executable, fmt.Sprintf("%x", p.Device),
		strconv.FormatUint(p.Inode, 10), strconv.FormatUint(p.Size, 10), fmt.Sprintf("%o", p.Mode), strconv.FormatInt(p.ModTime, 10),
		p.StartedAt.Format(time.RFC3339Nano), p.ExecutableSHA, p.MachineID, p.MachineIDProof}, ",")
}

type strictClaudeLinuxProcessHandles struct {
	executable, machineID *os.File
}

func (h *strictClaudeLinuxProcessHandles) Close() {
	if h == nil {
		return
	}
	for _, file := range []*os.File{h.executable, h.machineID} {
		if file != nil {
			file.Close()
		}
	}
}

func strictClaudeLinuxThreadProof(inst *session.Instance, target *tmux.Session, id tmux.StrictPaneIdentity, home string) (strictClaudeNativeProof, error) {
	if runtime.GOARCH != "amd64" {
		return strictClaudeNativeProof{}, fmt.Errorf("unsupported native Linux architecture")
	}
	return strictClaudeLinuxThreadProofWithDeps(inst, target, id, home, strictClaudeLinuxDefaultDeps())
}

func strictClaudeLinuxThreadProofWithDeps(inst *session.Instance, target *tmux.Session, id tmux.StrictPaneIdentity,
	home string, deps strictClaudeLinuxDeps) (strictClaudeNativeProof, error) {
	if inst == nil || target == nil || inst.ID == "" || target.InstanceID != inst.ID ||
		inst.TmuxSocketName == "" || target.SocketName != inst.TmuxSocketName || target.Name == "" ||
		target.Name != id.SessionName || !filepath.IsAbs(inst.ProjectPath) || filepath.Clean(inst.ProjectPath) != inst.ProjectPath ||
		id.CWD != inst.ProjectPath {
		return strictClaudeNativeProof{}, fmt.Errorf("native AgentDeck target mismatch")
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || deps.procRoot == "" || deps.uid < 0 {
		return strictClaudeNativeProof{}, fmt.Errorf("native Linux root unavailable")
	}
	configDir := session.GetClaudeConfigDirForInstance(inst)
	canonicalConfig := filepath.Join(home, ".claude")
	if configDir != canonicalConfig {
		return strictClaudeNativeProof{}, fmt.Errorf("noncanonical native Linux config")
	}
	pid, err := strconv.Atoi(id.PID)
	if err != nil || pid <= 0 || id.Command != "claude" || id.TTY == "" {
		return strictClaudeNativeProof{}, fmt.Errorf("invalid native Linux pane")
	}
	recordPath := filepath.Join(canonicalConfig, "sessions", id.PID+".json")
	firstRecord, heldRecord, recordData, err := deps.bind(recordPath, 64*1024, true)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native session unavailable")
	}
	defer heldRecord.Close()
	record, err := strictDecodeClaudeLinuxRecord(recordData)
	if err != nil || !strictThreadUUID.MatchString(record.SessionID) || !strictClaudeNativeVersion.MatchString(record.Version) ||
		!strictClaudeLinuxRecordBounds(record) {
		return strictClaudeNativeProof{}, fmt.Errorf("invalid native record")
	}
	if record.PID != pid || filepath.Clean(record.CWD) != inst.ProjectPath || record.CWD != inst.ProjectPath ||
		record.Tmux != id.SessionName+":"+id.WindowID+"."+id.PaneID || record.ProcStart == "" ||
		record.Kind != "interactive" || record.Entrypoint != "cli" {
		return strictClaudeNativeProof{}, fmt.Errorf("native session identity mismatch")
	}
	firstRecordSignature := strictClaudeLinuxRecordSignature(firstRecord, record)
	// The record's version names the image THIS process runs, fixed for its lifetime.
	// The installed package names the generation installed NOW, which the in-place
	// auto-updater replaces under live sessions, so it cannot vouch for either.
	firstPackage, heldPackage, err := deps.packageProof(home, deps.uid, deps.gid)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native package unavailable: %w", err)
	}
	defer heldPackage.Close()
	before, heldExecutable, err := strictClaudeLinuxProcessProof(record, id, home, firstPackage, deps)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native session identity mismatch: %w", err)
	}
	defer heldExecutable.Close()
	rootPath := filepath.Join(canonicalConfig, "projects", session.ConvertToClaudeDirName(record.CWD), record.SessionID+".jsonl")
	firstRoot, heldRoot, _, err := deps.bind(rootPath, 0, false)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native root unavailable")
	}
	defer heldRoot.Close()
	secondRecord, heldRecordAgain, secondData, err := deps.bind(recordPath, 64*1024, true)
	if err != nil {
		if heldRecordAgain != nil {
			heldRecordAgain.Close()
		}
		return strictClaudeNativeProof{}, fmt.Errorf("native record changed")
	}
	defer heldRecordAgain.Close()
	secondDecoded, err := strictDecodeClaudeLinuxRecord(secondData)
	if err != nil || !strictClaudeLinuxRecordBounds(secondDecoded) ||
		firstRecordSignature != strictClaudeLinuxRecordSignature(secondRecord, secondDecoded) {
		return strictClaudeNativeProof{}, fmt.Errorf("native record changed")
	}
	secondPackage, heldPackageAgain, err := deps.packageProof(home, deps.uid, deps.gid)
	if err != nil || firstPackage.signature() != secondPackage.signature() {
		if heldPackageAgain != nil {
			heldPackageAgain.Close()
		}
		return strictClaudeNativeProof{}, fmt.Errorf("native package changed")
	}
	defer heldPackageAgain.Close()
	secondRoot, heldRootAgain, _, err := deps.bind(rootPath, 0, false)
	if err != nil || firstRoot != secondRoot {
		if heldRootAgain != nil {
			heldRootAgain.Close()
		}
		return strictClaudeNativeProof{}, fmt.Errorf("native root changed")
	}
	defer heldRootAgain.Close()
	after, heldExecutableAgain, err := strictClaudeLinuxProcessProof(secondDecoded, id, home, secondPackage, deps)
	if err != nil {
		return strictClaudeNativeProof{}, fmt.Errorf("native process changed")
	}
	defer heldExecutableAgain.Close()
	if before.signature() != after.signature() {
		return strictClaudeNativeProof{}, fmt.Errorf("native process changed")
	}
	signature := strings.Join([]string{inst.ID, target.SocketName, target.Name, id.SessionID, id.WindowID,
		id.PaneID, id.ServerVersion, id.BracketPaste, record.SessionID, record.CWD, before.signature(), firstRecordSignature,
		firstPackage.signature(), firstRoot.signature()}, "|")
	return strictClaudeNativeProof{Thread: record.SessionID, ProcessStartedAt: before.StartedAt,
		CWD: record.CWD, Signature: signature, DurableStop: true, NativeStatus: secondDecoded.Status,
		NativeStatusUpdatedAt: secondDecoded.StatusUpdatedAt, NativeStartedAt: secondDecoded.StartedAt,
		NativeStatusStable: record.Status == secondDecoded.Status && record.StatusUpdatedAt == secondDecoded.StatusUpdatedAt}, nil
}

func strictLinuxReadBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("bounded proc read failed")
	}
	return data, nil
}

func strictDecodeClaudeLinuxRecord(data []byte) (strictClaudeRecord, error) {
	allowed := map[string]bool{
		"cwd": true, "entrypoint": true, "kind": true, "messagingSocketPath": true, "name": true,
		"nameSince": true, "nameSource": true, "peerFeatures": true, "peerProtocol": true, "pid": true,
		"pidDomain": true, "procStart": true, "sessionId": true, "startedAt": true, "status": true,
		"statusUpdatedAt": true, "tmux": true, "updatedAt": true, "version": true,
	}
	seen := map[string]bool{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return strictClaudeRecord{}, fmt.Errorf("native record is not an object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return strictClaudeRecord{}, fmt.Errorf("unknown or duplicate native record field")
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return strictClaudeRecord{}, fmt.Errorf("malformed native record field")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return strictClaudeRecord{}, fmt.Errorf("malformed native record")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return strictClaudeRecord{}, fmt.Errorf("trailing native record data")
	}
	for required := range allowed {
		if !seen[required] {
			return strictClaudeRecord{}, fmt.Errorf("missing native record field")
		}
	}
	var record strictClaudeRecord
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if strict.Decode(&record) != nil {
		return strictClaudeRecord{}, fmt.Errorf("malformed native record")
	}
	return record, nil
}

func strictClaudeLinuxRecordBounds(record strictClaudeRecord) bool {
	const maxExactJSONInteger = int64(1<<53 - 1)
	if record.StartedAt <= 0 || record.StartedAt > maxExactJSONInteger || record.NameSince < record.StartedAt ||
		record.StatusUpdatedAt < record.StartedAt || record.UpdatedAt < record.StartedAt ||
		record.NameSince > maxExactJSONInteger || record.StatusUpdatedAt > maxExactJSONInteger || record.UpdatedAt > maxExactJSONInteger ||
		record.PeerProtocol < 0 || record.PeerProtocol > 64 || len(record.PeerFeatures) > 64 {
		return false
	}
	for _, feature := range record.PeerFeatures {
		if feature == "" || len(feature) > 128 {
			return false
		}
	}
	return len(record.MessagingSocketPath) <= 4096 && len(record.Name) <= 256 && len(record.NameSource) <= 64 &&
		len(record.Status) <= 64
}

func strictClaudeLinuxRecordSignature(binding strictClaudeFileBinding, record strictClaudeRecord) string {
	record.NameSince = 0
	record.StatusUpdatedAt = 0
	record.UpdatedAt = 0
	stable, _ := json.Marshal(record)
	return fmt.Sprintf("%s:%s:%x:%d:%d:%o:%x", binding.Path, binding.Ancestors, binding.Device,
		binding.Inode, binding.Nlink, binding.Mode, sha256.Sum256(stable))
}

type strictClaudeLinuxDirectoryBinding struct {
	Path, Ancestors      string
	Device, Inode, Nlink uint64
	ModTime              int64
	Mode                 uint32
}

func (b strictClaudeLinuxDirectoryBinding) signature() string {
	return fmt.Sprintf("%s:%s:%x:%d:%d:%d:%o", b.Path, b.Ancestors, b.Device, b.Inode, b.Nlink, b.ModTime, b.Mode)
}

type strictClaudeLinuxPackage struct {
	Directory  strictClaudeLinuxDirectoryBinding
	Executable strictClaudeFileBinding
}

func (p strictClaudeLinuxPackage) signature() string {
	return strings.Join([]string{p.Directory.signature(), p.Executable.signature()}, "|")
}

type strictClaudeLinuxPackageHandles struct {
	directory, executable *os.File
}

func (h *strictClaudeLinuxPackageHandles) Close() {
	if h == nil {
		return
	}
	for _, file := range []*os.File{h.executable, h.directory} {
		if file != nil {
			file.Close()
		}
	}
}

func strictClaudeLinuxPackagePath(home string) string {
	return filepath.Join(home, ".nvm", "versions", "node", strictLinuxClaudeNodeVersion,
		"lib", "node_modules", "@anthropic-ai", "claude-code")
}

func strictClaudeLinuxPackageProof(home string, uid, gid int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
	return strictClaudeLinuxPackageProofWithLinks(home, uid, gid, 4, 2)
}

// npm writes the package tree with the umask of whichever session ran the
// auto-update, so the same install is 0755 or 0775. A group-write bit is admitted
// only for the caller's own primary group; the parent @anthropic-ai directory,
// which controls replacing this one, carries the same bit and is not a boundary.
func strictClaudeLinuxPackageDirectoryMode(mode os.FileMode, directoryGID uint32, gid int) bool {
	return mode == 0755 || mode == 0775 && gid >= 0 && uint64(directoryGID) == uint64(gid)
}

func strictClaudeLinuxPackageProofWithLinks(home string, uid, gid int,
	directoryLinks, executableLinks uint64) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
	var proof strictClaudeLinuxPackage
	handles := &strictClaudeLinuxPackageHandles{}
	fail := func(reason string) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
		handles.Close()
		return strictClaudeLinuxPackage{}, nil, fmt.Errorf("%s", reason)
	}
	packagePath := strictClaudeLinuxPackagePath(home)
	directory, ancestors, err := strictOpenVerifiedDirectory(packagePath)
	if err != nil {
		return fail("native package directory unavailable")
	}
	handles.directory = directory
	directoryBefore, err := directory.Stat()
	if err != nil {
		return fail("native package directory stat unavailable")
	}
	directoryStat, ok := directoryBefore.Sys().(*syscall.Stat_t)
	if !ok {
		return fail("unsafe native package directory stat")
	}
	if !directoryBefore.IsDir() || !strictClaudeLinuxPackageDirectoryMode(directoryBefore.Mode().Perm(), directoryStat.Gid, gid) || int(directoryStat.Uid) != uid ||
		uint64(directoryStat.Nlink) != directoryLinks {
		return fail("unsafe native package directory")
	}
	device, inode, err := strictFileIdentity(directory)
	if err != nil {
		return fail("native package directory identity unavailable")
	}
	proof.Directory = strictClaudeLinuxDirectoryBinding{Path: packagePath, Ancestors: ancestors, Device: device,
		Inode: inode, Nlink: uint64(directoryStat.Nlink), ModTime: directoryBefore.ModTime().UnixNano(), Mode: uint32(directoryBefore.Mode().Perm())}
	executablePath := filepath.Join(packagePath, "bin", "claude.exe")
	executable, executableFile, _, err := strictClaudeLinuxOwnedPackageFile(executablePath, 512*1024*1024, false, 0755, executableLinks, uid)
	if err != nil {
		return fail("native package executable unavailable")
	}
	handles.executable = executableFile
	proof.Executable = executable
	directoryAfter, err := directory.Stat()
	if err != nil {
		return fail("native package directory changed")
	}
	afterStat, afterOK := directoryAfter.Sys().(*syscall.Stat_t)
	if !afterOK || directoryBefore.Mode() != directoryAfter.Mode() ||
		!directoryBefore.ModTime().Equal(directoryAfter.ModTime()) || directoryStat.Uid != afterStat.Uid ||
		directoryStat.Gid != afterStat.Gid || directoryStat.Nlink != afterStat.Nlink {
		return fail("native package directory changed")
	}
	return proof, handles, nil
}

func strictClaudeLinuxOwnedPackageFile(path string, maxSize int64, capture bool, mode os.FileMode,
	links uint64, uid int) (strictClaudeFileBinding, *os.File, []byte, error) {
	file, ancestors, err := strictOpenVerifiedPath(path)
	if err != nil {
		return strictClaudeFileBinding{}, nil, nil, err
	}
	fail := func(reason string) (strictClaudeFileBinding, *os.File, []byte, error) {
		file.Close()
		return strictClaudeFileBinding{}, nil, nil, fmt.Errorf("%s", reason)
	}
	before, err := file.Stat()
	if err != nil {
		return fail("native package file stat unavailable")
	}
	underlying, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxSize ||
		before.Mode().Perm() != mode || int(underlying.Uid) != uid || uint64(underlying.Nlink) != links {
		return fail("unsafe native package file")
	}
	hash := sha256.New()
	var data []byte
	if capture {
		data, err = io.ReadAll(io.LimitReader(file, maxSize+1))
		if err == nil {
			_, err = hash.Write(data)
		}
	} else {
		var count int64
		count, err = io.Copy(hash, io.LimitReader(file, maxSize+1))
		if err == nil && count != before.Size() {
			err = fmt.Errorf("native package file size changed")
		}
	}
	if err != nil || int64(len(data)) > maxSize || capture && int64(len(data)) != before.Size() {
		return fail("native package file read changed")
	}
	after, err := file.Stat()
	if err != nil {
		return fail("native package file changed")
	}
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !afterOK || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		underlying.Uid != afterStat.Uid || underlying.Nlink != afterStat.Nlink {
		return fail("native package file changed")
	}
	device, inode, err := strictFileIdentity(file)
	if err != nil {
		return fail("native package file identity unavailable")
	}
	return strictClaudeFileBinding{Path: path, Ancestors: ancestors, ContentSHA: fmt.Sprintf("%x", hash.Sum(nil)),
		Device: device, Inode: inode, Nlink: uint64(afterStat.Nlink), Size: after.Size(), ModTime: after.ModTime().UnixNano(),
		Mode: uint32(after.Mode().Perm())}, file, data, nil
}

func strictLinuxMachineIDProof(path string, owner int) (strictClaudeFileBinding, *os.File, []byte, error) {
	file, ancestors, err := strictOpenVerifiedPath(path)
	if err != nil {
		return strictClaudeFileBinding{}, nil, nil, err
	}
	fail := func(reason string) (strictClaudeFileBinding, *os.File, []byte, error) {
		file.Close()
		return strictClaudeFileBinding{}, nil, nil, fmt.Errorf("%s", reason)
	}
	before, err := file.Stat()
	if err != nil {
		return fail("machine identity stat unavailable")
	}
	underlying, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm() != 0444 || before.Size() != 33 ||
		int(underlying.Uid) != owner || underlying.Nlink != 1 {
		return fail("unsafe machine identity")
	}
	data, err := io.ReadAll(io.LimitReader(file, 34))
	if err != nil || int64(len(data)) != before.Size() || !strictLinuxMachineID.Match(data) {
		return fail("invalid machine identity")
	}
	after, err := file.Stat()
	if err != nil {
		return fail("machine identity changed")
	}
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !afterOK || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		underlying.Uid != afterStat.Uid || underlying.Nlink != afterStat.Nlink {
		return fail("machine identity changed")
	}
	device, inode, err := strictFileIdentity(file)
	if err != nil {
		return fail("machine identity file unavailable")
	}
	return strictClaudeFileBinding{Path: path, Ancestors: ancestors, ContentSHA: fmt.Sprintf("%x", sha256.Sum256(data)),
		Device: device, Inode: inode, Nlink: uint64(afterStat.Nlink), Size: after.Size(), ModTime: after.ModTime().UnixNano(),
		Mode: uint32(after.Mode().Perm())}, file, data, nil
}

func strictClaudeLinuxProcessProof(record strictClaudeRecord, id tmux.StrictPaneIdentity, home string, pkg strictClaudeLinuxPackage,
	deps strictClaudeLinuxDeps) (strictClaudeLinuxProcess, *strictClaudeLinuxProcessHandles, error) {
	handles := &strictClaudeLinuxProcessHandles{}
	fail := func(reason string) (strictClaudeLinuxProcess, *strictClaudeLinuxProcessHandles, error) {
		handles.Close()
		return strictClaudeLinuxProcess{}, nil, fmt.Errorf("%s", reason)
	}
	processRoot := filepath.Join(deps.procRoot, id.PID)
	statData, err := deps.readBounded(filepath.Join(processRoot, "stat"), strictLinuxProcReadLimit)
	if err != nil {
		return fail("process stat unavailable")
	}
	process, err := strictParseLinuxProcessStat(string(statData), id.PID)
	if err != nil || process.PGID != id.PID || process.SID != id.PID || process.TPGID != id.PID ||
		process.Start != record.ProcStart || !strictLinuxProcessStateAllowed(process.State) {
		return fail("foreground process unverified")
	}
	statusData, err := deps.readBounded(filepath.Join(processRoot, "status"), strictLinuxProcReadLimit)
	if err != nil || !strictMatchLinuxProcessStatus(string(statusData), process, deps.uid) {
		return fail("process ownership unverified")
	}
	tty, err := deps.readlink(filepath.Join(processRoot, "fd", "0"))
	if err != nil || tty != id.TTY || !filepath.IsAbs(tty) || filepath.Clean(tty) != tty {
		return fail("process tty unverified")
	}
	ttyInfo, err := deps.stat(tty)
	if err != nil {
		return fail("process tty unavailable")
	}
	ttyStat, ok := ttyInfo.Sys().(*syscall.Stat_t)
	if !ok || strconv.FormatUint(uint64(ttyStat.Rdev), 10) != process.TTY {
		return fail("process tty changed")
	}
	cwd, err := deps.readlink(filepath.Join(processRoot, "cwd"))
	if err != nil || cwd != record.CWD || cwd != id.CWD {
		return fail("process cwd changed")
	}
	procStatData, procStatErr := deps.readBounded(filepath.Join(deps.procRoot, "stat"), 1024*1024)
	pidNamespace, namespaceErr := deps.readlink(filepath.Join(processRoot, "ns", "pid"))
	bootTime, bootTimeErr := strictLinuxBootTime(string(procStatData))
	startTicks, startTicksErr := strconv.ParseInt(process.Start, 10, 64)
	processStart := bootTime.Add(time.Duration(startTicks) * time.Second / 100)
	recordStart := time.UnixMilli(record.StartedAt).UTC()
	recordDelay := recordStart.Sub(processStart)
	if procStatErr != nil || namespaceErr != nil || bootTimeErr != nil || startTicksErr != nil || startTicks <= 0 ||
		record.StartedAt <= 0 || recordDelay < 0 || recordDelay > 2*time.Second ||
		!strictLinuxPIDNamespace.MatchString(pidNamespace) {
		return fail("process domain unverified")
	}
	machineBinding, machineFile, machineData, err := strictLinuxMachineIDProof(deps.machineIDPath, deps.machineIDOwner)
	if err != nil {
		return fail("machine identity unavailable")
	}
	handles.machineID = machineFile
	machineID := strings.TrimSuffix(string(machineData), "\n")
	if record.PIDDomain != "linux:"+machineID+":"+pidNamespace {
		return fail("process domain unverified")
	}
	process.StartedAt = processStart.UTC()
	process.MachineID, process.MachineIDProof = machineID, machineBinding.signature()
	executableLink := filepath.Join(processRoot, "exe")
	executable, err := deps.readlink(executableLink)
	stableExecutable := executable == pkg.Executable.Path
	atomicResidue := strictLinuxClaudeAtomicResidue(executable, home)
	if err != nil || !stableExecutable && !atomicResidue {
		return fail("process executable unverified")
	}
	held, err := deps.open(executableLink)
	if err != nil {
		return fail("process executable unavailable")
	}
	handles.executable = held
	info, err := held.Stat()
	if err != nil {
		return fail("process executable stat unavailable")
	}
	underlying, ok := info.Sys().(*syscall.Stat_t)
	expectedLinks := pkg.Executable.Nlink
	if atomicResidue {
		expectedLinks = 0
	}
	// An in-place upgrade unlinks the image a live session runs and installs a
	// different one, so only the stable image can equal the installed size; the
	// residue keeps the install's owner, mode and device and a bounded size.
	sizeValid := info.Size() == pkg.Executable.Size
	if atomicResidue {
		sizeValid = info.Size() > 0 && info.Size() <= 512*1024*1024
	}
	if !ok || !info.Mode().IsRegular() || !sizeValid || info.Mode().Perm() != os.FileMode(pkg.Executable.Mode) ||
		int(underlying.Uid) != deps.uid || uint64(underlying.Nlink) != expectedLinks {
		return fail("unsafe process executable")
	}
	device, inode, err := strictFileIdentity(held)
	if err != nil || device != pkg.Executable.Device || stableExecutable && inode != pkg.Executable.Inode ||
		atomicResidue && (inode == 0 || inode == pkg.Executable.Inode) {
		return fail("process executable identity unavailable")
	}
	executableSHA := pkg.Executable.ContentSHA
	if atomicResidue {
		// Nothing on disk can equal an image the upgrade replaced, and the kernel
		// refuses writes to a running text file (ETXTBSY), so its unchanged inode
		// identity is its content proof. A byte comparison would only ever refuse
		// an honest upgrade: anyone able to plant a residue in this tree can as
		// well replace the stable executable, which keeps its full content proof.
		executableSHA = ""
		after, statErr := held.Stat()
		if statErr != nil {
			return fail("atomic-update executable changed or differs")
		}
		afterUnderlying, afterOK := after.Sys().(*syscall.Stat_t)
		afterDevice, afterInode, identityErr := strictFileIdentity(held)
		if !afterOK || after.Mode() != info.Mode() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) ||
			afterUnderlying.Uid != underlying.Uid || afterUnderlying.Nlink != underlying.Nlink ||
			afterDevice != device || afterInode != inode || identityErr != nil {
			return fail("atomic-update executable changed or differs")
		}
	}
	executableAgain, err := deps.readlink(executableLink)
	if err != nil || executableAgain != executable {
		return fail("process executable changed")
	}
	process.Executable, process.ExecutableSHA = executable, executableSHA
	process.UID, process.Device, process.Inode = uint64(deps.uid), device, inode // #nosec G115 -- uid equality with the kernel stat value above proves a valid non-negative OS uid.
	process.Size, process.Mode = uint64(info.Size()), uint32(info.Mode().Perm()) // #nosec G115 -- the held descriptor is a regular file with the non-negative bounded size proved above.
	// Without a content hash for the residue, its modification time carries any
	// change to the held image across the two passes, as its size and inode do.
	process.ModTime = info.ModTime().UnixNano()
	return process, handles, nil
}

func strictLinuxClaudeAtomicResidue(value, home string) bool {
	const deletedSuffix = " (deleted)"
	if !strings.HasSuffix(value, deletedSuffix) {
		return false
	}
	path := strings.TrimSuffix(value, deletedSuffix)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	parent := filepath.Dir(strictClaudeLinuxPackagePath(home))
	relative, err := filepath.Rel(parent, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	return len(parts) == 3 && strictLinuxClaudeAtomicSlot.MatchString(parts[0]) && parts[1] == "bin" && parts[2] == "claude.exe"
}

func strictLinuxProcessStateAllowed(state string) bool {
	return state == "R" || state == "S" || state == "D" || state == "I"
}

func strictParseLinuxProcessStat(value, expectedPID string) (strictClaudeLinuxProcess, error) {
	open := strings.IndexByte(value, '(')
	close := strings.LastIndex(value, ") ")
	if open < 1 || close <= open || strings.TrimSpace(value[:open]) != expectedPID || value[open+1:close] != "claude" {
		return strictClaudeLinuxProcess{}, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(value[close+2:])
	if len(fields) < 20 || fields[0] == "" {
		return strictClaudeLinuxProcess{}, fmt.Errorf("incomplete process stat")
	}
	for _, index := range []int{1, 2, 3, 4, 5, 19} {
		if _, err := strconv.ParseInt(fields[index], 10, 64); err != nil {
			return strictClaudeLinuxProcess{}, fmt.Errorf("invalid process stat")
		}
	}
	return strictClaudeLinuxProcess{PID: expectedPID, PPID: fields[1], PGID: fields[2], SID: fields[3],
		TTY: fields[4], TPGID: fields[5], Start: fields[19], State: fields[0]}, nil
}

func strictMatchLinuxProcessStatus(value string, process strictClaudeLinuxProcess, uid int) bool {
	wanted := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "Name" && key != "State" && key != "Tgid" && key != "Pid" && key != "PPid" && key != "Uid" {
			continue
		}
		if _, duplicate := wanted[key]; duplicate {
			return false
		}
		wanted[key] = strings.Join(fields[1:], " ")
	}
	uidText := strconv.Itoa(uid)
	return wanted["Name"] == "claude" && strings.HasPrefix(wanted["State"], process.State+" ") &&
		wanted["Tgid"] == process.PID && wanted["Pid"] == process.PID && wanted["PPid"] == process.PPID &&
		wanted["Uid"] == strings.Join([]string{uidText, uidText, uidText, uidText}, " ")
}

func strictLinuxBootTime(value string) (time.Time, error) {
	boot := ""
	for _, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			if boot != "" {
				return time.Time{}, fmt.Errorf("duplicate Linux boot time")
			}
			boot = fields[1]
		}
	}
	seconds, err := strconv.ParseInt(boot, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, fmt.Errorf("Linux boot time unavailable")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func strictLinuxClaudeExecutable(value, home string) bool {
	return value == filepath.Join(strictClaudeLinuxPackagePath(home), "bin", "claude.exe")
}
