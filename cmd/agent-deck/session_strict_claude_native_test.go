package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestStrictClaudeVersionedNativeIdentity(t *testing.T) {
	for _, fault := range []string{
		"none", "record_pid", "record_start", "record_version", "record_cwd", "record_tmux", "record_domain", "record_thread",
		"numeric_command", "other_version", "missing_process", "duplicate_process", "wrong_comm", "wrong_tty", "background", "stopped", "process_changed", "start_changed",
		"descriptor_pid", "descriptor_cwd", "wrong_path", "wrong_device", "wrong_inode", "missing_txt", "library_first", "duplicate_field", "duplicate_pid", "non_executable", "symlink", "path_replaced", "ancestor_replaced", "mapping_changed", "probe_error",
	} {
		t.Run(fault, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.263")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("native fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			file, _, err := strictOpenVerifiedPath(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			dev, ino, err := strictFileIdentity(file)
			if err != nil {
				t.Fatal(err)
			}
			id := tmux.StrictPaneIdentity{PID: "123", PaneID: "%2", SessionName: "target", WindowID: "@3", CWD: "/project", Command: "2.1.263", TTY: "/dev/ttys001"}
			started := "Tue Sep  8 18:28:28 2026"
			record := strictClaudeRecord{PID: 123, SessionID: "expected", CWD: "/project", Tmux: "target:@3.%2", ProcStart: started, PIDDomain: "darwin", Version: "2.1.263"}
			switch fault {
			case "record_pid":
				record.PID++
			case "record_start":
				record.ProcStart = "Tue Sep 8 15:28:28 2026"
			case "record_version":
				record.Version = "2.1.262"
			case "record_cwd":
				record.CWD = "/other"
			case "record_tmux":
				record.Tmux = "other:@3.%2"
			case "record_domain":
				record.PIDDomain = "linux"
			case "record_thread":
				record.SessionID = ""
			case "numeric_command":
				id.Command, record.Version = "123", "123"
			case "other_version":
				id.Command, record.Version = "9.9.9", "9.9.9"
			case "non_executable":
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			}
			psCalls, starts, mappings := 0, 0, 0
			run := func(name string, args ...string) ([]byte, error) {
				if fault == "probe_error" {
					return nil, fmt.Errorf("unavailable")
				}
				if name == "ps" && len(args) == 4 && args[3] == "lstart=" {
					starts++
					if fault == "start_changed" && starts == 2 {
						return []byte("Tue Sep 8 18:28:29 2026"), nil
					}
					return []byte(started), nil
				}
				if name == "ps" {
					if strings.Join(args, " ") != "-p 123 -o pid=,ppid=,pgid=,tpgid=,state=,tty=,comm=" {
						t.Fatalf("unexpected foreground probe %v", args)
					}
					psCalls++
					out := "123 12 123 123 S+ ttys001 claude\n"
					switch fault {
					case "missing_process":
						out = "124 12 123 123 S+ ttys001 claude\n"
					case "duplicate_process":
						out += out
					case "wrong_comm":
						out = strings.ReplaceAll(out, "claude", "other")
					case "wrong_tty":
						out = strings.ReplaceAll(out, "ttys001", "ttys002")
					case "background":
						out = strings.ReplaceAll(out, "123 123 S+", "123 456 S")
					case "stopped":
						out = strings.ReplaceAll(out, "S+", "T+")
					case "process_changed":
						if psCalls == 2 {
							out = strings.ReplaceAll(out, "123 12 ", "123 13 ")
						}
					}
					return []byte(out), nil
				}
				if name != "lsof" {
					t.Fatalf("unexpected probe %s", name)
				}
				mappings++
				out := fmt.Sprintf("p123\nfcwd\ntDIR\nn/project\nftxt\ntREG\nD0x%x\ni%d\nn%s\nftxt\ntREG\nn/usr/lib/dyld\n", dev, ino, path)
				switch fault {
				case "descriptor_pid":
					out = strings.ReplaceAll(out, "p123", "p124")
				case "descriptor_cwd":
					out = strings.ReplaceAll(out, "n/project", "n/other")
				case "wrong_path":
					out = strings.ReplaceAll(out, path, "/tmp/2.1.263")
				case "wrong_device":
					out = strings.ReplaceAll(out, fmt.Sprintf("D0x%x", dev), "D0x1")
				case "wrong_inode":
					out = strings.ReplaceAll(out, fmt.Sprintf("i%d", ino), "i1")
				case "missing_txt":
					out = "p123\nfcwd\ntDIR\nn/project\n"
				case "library_first":
					out = strings.Replace(out, "ftxt\n", "ftxt\ntREG\nn/usr/lib/dyld\nftxt\n", 1)
				case "duplicate_field":
					out = strings.Replace(out, "tREG\n", "tREG\ntREG\n", 1)
				case "duplicate_pid":
					out += "p123\n"
				case "mapping_changed":
					if mappings == 2 {
						out = strings.ReplaceAll(out, path, "/tmp/2.1.263")
					}
				case "path_replaced":
					if mappings == 2 {
						if err := os.Rename(path, path+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("replacement"), 0700); err != nil {
							t.Fatal(err)
						}
					}
				case "ancestor_replaced":
					if mappings == 2 {
						dir := filepath.Dir(path)
						if err := os.Rename(dir, dir+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(dir, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Link(filepath.Join(dir+".old", "2.1.263"), path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return []byte(out), nil
			}
			got := strictClaudeIdentityWithRun(record, id, "/project", "darwin", home, run)
			if got != (fault == "none") {
				t.Fatalf("identity accepted=%v", got)
			}
			if fault == "none" && (psCalls != 2 || starts != 2 || mappings != 2) {
				t.Fatal("proof was not re-observed")
			}
		})
	}
}

func TestStrictClaudeExactPIDProbeAllowlist(t *testing.T) {
	format := "pid=,ppid=,pgid=,tpgid=,state=,tty=,comm="
	for _, tc := range []struct {
		pid, format string
		want        bool
	}{
		{"123", format, true},
		{"123,124", format, false},
		{"123; echo unsafe", format, false},
		{"0", format, false},
		{"-1", format, false},
		{"", format, false},
		{"123", format + ",args=", false},
	} {
		cmd, err := strictNativeCommand(context.Background(), "ps", "-p", tc.pid, "-o", tc.format)
		if (err == nil) != tc.want {
			t.Fatalf("pid=%q format=%q accepted=%v", tc.pid, tc.format, err == nil)
		}
		if tc.want && (cmd.Path != "/bin/ps" || strings.Join(cmd.Env, "|") != "PATH=/usr/bin:/bin:/usr/sbin:/sbin|TZ=UTC|LC_ALL=C") {
			t.Fatalf("unexpected native command %v", cmd)
		}
	}
}

func TestStrictClaudeVersionRequiresRuntimeProof(t *testing.T) {
	id := tmux.StrictPaneIdentity{PID: "123", PaneID: "%2", SessionName: "target", WindowID: "@3", CWD: "/project", Command: "2.1.263"}
	started := "Tue Sep 8 18:28:28 2026"
	record := strictClaudeRecord{PID: 123, SessionID: "expected", CWD: "/project", Tmux: "target:@3.%2", ProcStart: started, PIDDomain: "darwin", Version: "2.1.263"}
	if strictClaudeIdentityMatches(record, id, "/project", started, "darwin") {
		t.Fatal("versioned name accepted without runtime proof")
	}
}
