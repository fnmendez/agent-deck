package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type strictProbe func(string, ...string) ([]byte, error)

// Native start timestamps in Darwin session records are UTC, independent of
// the calling terminal's timezone. No inherited auth values are printed.
func strictNativeProbe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd, err := strictNativeCommand(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

func strictNativeCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	validPIDList := func(value string) bool {
		for _, pid := range strings.Split(value, ",") {
			if !strictValidPID(pid) {
				return false
			}
		}
		return value != ""
	}
	switch name {
	case "ps":
		table := len(args) == 2 && args[0] == "-Ao" && args[1] == "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm="
		starts := false
		if len(args) == 4 {
			format := args[3] // #nosec G602 -- guarded by len(args) == 4 immediately above; the independent two-argument branch is not this path.
			starts = args[0] == "-p" && args[2] == "-o" && ((validPIDList(args[1]) && (format == "pid=,lstart=" || format == "lstart=")) || (strictValidPID(args[1]) && format == "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm="))
		}
		if !table && !starts {
			return nil, fmt.Errorf("unsupported process probe")
		}
		// #nosec G204 G702 -- fixed system executable; exact argv-shape/format whitelist and positive numeric PID list above, never a shell.
		cmd = exec.CommandContext(ctx, "/bin/ps", args...)
	case "lsof":
		if len(args) != 5 || args[0] != "-a" || args[1] != "-p" || !strictValidPID(args[2]) || args[3] != "-F" || args[4] != "pfaDint" {
			return nil, fmt.Errorf("unsupported descriptor probe")
		}
		// #nosec G204 G702 -- fixed system executable with one validated numeric PID and exact read-only argv, never a shell.
		cmd = exec.CommandContext(ctx, "/usr/sbin/lsof", args...)
	default:
		return nil, fmt.Errorf("unsupported native probe")
	}
	// These system metadata tools need no inherited provider/agent environment.
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TZ=UTC", "LC_ALL=C"}
	return cmd, nil
}

// The initial release deliberately supports the measured Darwin native
// process/session formats only. Other platforms must refuse explicitly.
func strictPlatformSupported(platform string) bool { return platform == "darwin" }

func strictCodexRuntimeProof(id tmux.StrictPaneIdentity, expected, project string) (string, error) {
	if !strictPlatformSupported(runtime.GOOS) {
		return "", fmt.Errorf("unsupported strict platform")
	}
	return strictCodexProofWithRun(id, expected, project, strictNativeProbe)
}

func strictCodexProofWithRun(id tmux.StrictPaneIdentity, expected, project string, run strictProbe) (string, error) {
	if !strictValidPID(id.PID) || !strictWrapperCommand(id.Command) || id.TTY == "" || project == "" || id.CWD != project {
		return "", fmt.Errorf("unverified foreground target")
	}
	observeProcess := func() (string, string, error) {
		table, err := run("ps", "-Ao", "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm=")
		if err != nil {
			return "", "", fmt.Errorf("process ancestry unavailable")
		}
		chain, err := strictForegroundChain(string(table), id)
		if err != nil {
			return "", "", err
		}
		pids := []string{}
		signature := []string{}
		for _, p := range chain {
			pids = append(pids, p.PID)
			signature = append(signature, p.signature())
		}
		started, err := run("ps", "-p", strings.Join(pids, ","), "-o", "pid=,lstart=")
		if err != nil {
			return "", "", fmt.Errorf("process start unavailable")
		}
		starts := map[string]string{}
		for _, line := range strings.Split(string(started), "\n") {
			f := strings.Fields(line)
			if len(f) != 6 {
				continue
			}
			if _, ok := starts[f[0]]; ok {
				return "", "", fmt.Errorf("ambiguous process start")
			}
			starts[f[0]] = strings.Join(f[1:], " ")
		}
		for _, pid := range pids {
			if starts[pid] == "" {
				return "", "", fmt.Errorf("missing process start")
			}
			signature = append(signature, pid+"="+starts[pid])
		}
		return chain[0].PID, strings.Join(signature, "|"), nil
	}
	pid, before, err := observeProcess()
	if err != nil {
		return "", err
	}
	observeRoot := func() (strictRootBinding, *os.File, error) {
		opened, err := run("lsof", "-a", "-p", pid, "-F", "pfaDint")
		if err != nil {
			return strictRootBinding{}, nil, fmt.Errorf("open descriptor evidence unavailable")
		}
		files, cwd, err := strictParseDescriptors(string(opened), pid)
		if err != nil || cwd != project {
			return strictRootBinding{}, nil, fmt.Errorf("native descriptors or cwd unverified")
		}
		return strictRootDescriptor(files, expected)
	}
	first, held, err := observeRoot()
	if err != nil {
		return "", err
	}
	defer held.Close()
	// Holding our verified descriptor prevents inode reuse while re-observing the
	// runtime's descriptor table and ancestry. Child rollouts never authorize root.
	second, heldAgain, err := observeRoot()
	if err != nil {
		return "", err
	}
	defer heldAgain.Close()
	if first != second {
		return "", fmt.Errorf("root descriptor or ancestor changed")
	}
	afterPID, after, err := observeProcess()
	if err != nil || pid != afterPID || before != after {
		return "", fmt.Errorf("foreground process ancestry changed")
	}
	return before + "|" + first.signature(), nil
}

type strictProcess struct{ PID, PPID, PGID, Foreground, State, TTY, Command string }

func (p strictProcess) signature() string {
	return strings.Join([]string{p.PID, p.PPID, p.PGID, p.Foreground, p.TTY, p.Command}, ",")
}
func strictWrapperCommand(command string) bool {
	return command == "node" || command == "codex" || strings.HasPrefix(command, "codex-")
}
func strictForegroundChain(table string, id tmux.StrictPaneIdentity) ([]strictProcess, error) {
	processes := map[string]strictProcess{}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		if _, exists := processes[f[0]]; exists {
			return nil, fmt.Errorf("duplicate process")
		}
		processes[f[0]] = strictProcess{f[0], f[1], f[2], f[3], f[4], f[5], filepath.Base(strings.Join(f[6:], " "))}
	}
	root, ok := processes[id.PID]
	if !ok || !strictWrapperCommand(id.Command) || root.Command != id.Command || !strictValidPID(root.Foreground) {
		return nil, fmt.Errorf("foreground wrapper unavailable")
	}
	var result []strictProcess
	for pid, p := range processes {
		if p.Command != "codex" && !strings.HasPrefix(p.Command, "codex-") {
			continue
		}
		current := pid
		seen := map[string]bool{}
		chain := []strictProcess{}
		for current != "" && !seen[current] {
			next, ok := processes[current]
			if !ok {
				break
			}
			chain = append(chain, next)
			if current == id.PID {
				if result != nil {
					return nil, fmt.Errorf("multiple Codex runtimes")
				}
				result = chain
				break
			}
			seen[current] = true
			current = next.PPID
		}
	}
	if result == nil {
		return nil, fmt.Errorf("Codex runtime not bound to pane")
	}
	for _, p := range result {
		if !strictWrapperCommand(p.Command) || p.PGID != root.Foreground || p.Foreground != root.Foreground || p.TTY != strings.TrimPrefix(id.TTY, "/dev/") || strings.ContainsAny(p.State, "TXZ") || !strings.Contains(p.State, "+") {
			return nil, fmt.Errorf("Codex is not a live foreground TUI")
		}
	}
	return result, nil
}

type strictDescriptor struct {
	FD, Access, Path string
	Device, Inode    uint64
}
type strictRootBinding struct {
	strictDescriptor
	Ancestors string
}

func (r strictRootBinding) signature() string {
	return fmt.Sprintf("%s:%s:%x:%d:%s:%s", r.FD, r.Access, r.Device, r.Inode, r.Path, r.Ancestors)
}

// Darwin lsof field shape was measured against the current runtime: f42, au,
// tREG, D0x1000010, i262361546, n/absolute/.../rollout-...UUID.jsonl.
func strictParseDescriptors(output, pid string) ([]strictDescriptor, string, error) {
	records := []map[byte]string{}
	record := map[byte]string{}
	process := ""
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		key, value := line[0], line[1:]
		if key == 'p' {
			if process != "" || value != pid {
				return nil, "", fmt.Errorf("ambiguous lsof process")
			}
			process = value
			continue
		}
		if key == 'f' {
			if len(record) > 0 {
				records = append(records, record)
			}
			record = map[byte]string{}
		}
		if _, exists := record[key]; exists {
			return nil, "", fmt.Errorf("duplicate descriptor field")
		}
		record[key] = value
	}
	if len(record) > 0 {
		records = append(records, record)
	}
	if process != pid {
		return nil, "", fmt.Errorf("lsof process missing")
	}
	files := []strictDescriptor{}
	cwd := ""
	fds := map[string]bool{}
	for _, r := range records {
		if _, err := strconv.Atoi(r['f']); err == nil {
			if fds[r['f']] {
				return nil, "", fmt.Errorf("duplicate file descriptor")
			}
			fds[r['f']] = true
		}
		if r['f'] == "cwd" {
			if cwd != "" || r['t'] != "DIR" || !filepath.IsAbs(r['n']) {
				return nil, "", fmt.Errorf("cwd unavailable")
			}
			cwd = r['n']
			continue
		}
		path := r['n']
		base := filepath.Base(path)
		if !strings.Contains(filepath.ToSlash(path), "/sessions/") || !strings.HasPrefix(base, "rollout-") {
			continue
		}
		fd, err := strconv.Atoi(r['f'])
		if err != nil || fd < 0 || r['t'] != "REG" || !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasSuffix(base, ".jsonl") || len(base) < 49 || !strictThreadUUID.MatchString(base[len(base)-42:len(base)-6]) {
			return nil, "", fmt.Errorf("unsupported rollout descriptor")
		}
		dev, e1 := strconv.ParseUint(r['D'], 0, 64)
		ino, e2 := strconv.ParseUint(r['i'], 10, 64)
		if e1 != nil || e2 != nil || dev == 0 || ino == 0 || (r['a'] != "u" && r['a'] != "w" && r['a'] != "r") {
			return nil, "", fmt.Errorf("incomplete descriptor authority")
		}
		files = append(files, strictDescriptor{r['f'], r['a'], path, dev, ino})
	}
	return files, cwd, nil
}

func strictRootDescriptor(files []strictDescriptor, expected string) (strictRootBinding, *os.File, error) {
	var root strictRootBinding
	var held *os.File
	fail := func(reason string) (strictRootBinding, *os.File, error) {
		if held != nil {
			held.Close()
		}
		return strictRootBinding{}, nil, fmt.Errorf("%s", reason)
	}
	if len(files) == 0 || len(files) > 64 {
		return fail("rollout evidence unavailable or excessive")
	}
	for _, descriptor := range files {
		file, ancestors, err := strictOpenVerifiedPath(descriptor.Path)
		if err != nil {
			return fail("rollout path authority unavailable")
		}
		dev, ino, err := strictFileIdentity(file)
		if err != nil || dev != descriptor.Device || ino != descriptor.Inode {
			file.Close()
			return fail("opened file differs from native descriptor")
		}
		record, err := strictReadFirstMeta(file)
		if err != nil || record.Type != "session_meta" {
			file.Close()
			return fail("rollout metadata unverified")
		}
		var source string
		if json.Unmarshal(record.Payload.Source, &source) == nil {
			if source != "cli" || held != nil || descriptor.Access == "r" || record.Payload.ID != expected || record.Payload.SessionID != expected || !strings.HasSuffix(descriptor.Path, "-"+expected+".jsonl") {
				file.Close()
				return fail("writable unique CLI root unverified")
			}
			root = strictRootBinding{descriptor, ancestors}
			held = file
		} else {
			var source struct {
				Subagent struct {
					ThreadSpawn struct {
						ParentThreadID string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			}
			file.Close()
			if json.Unmarshal(record.Payload.Source, &source) != nil || !strictThreadUUID.MatchString(source.Subagent.ThreadSpawn.ParentThreadID) || record.Payload.SessionID != expected {
				return fail("unknown rollout source")
			}
		}
	}
	if held == nil {
		return fail("exactly one open CLI root required")
	}
	return root, held, nil
}

type strictSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID        string          `json:"id"`
		SessionID string          `json:"session_id"`
		Source    json.RawMessage `json:"source"`
	} `json:"payload"`
}

func strictValidPID(pid string) bool { n, err := strconv.Atoi(pid); return err == nil && n > 0 }
