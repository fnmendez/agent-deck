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

const strictFixtureThread = "00000000-0000-4000-8000-000000000001"
const strictFixtureProcesses = "10 1 10 10 Ss+ ttys012 /bin/node\n20 10 10 10 S+ ttys012 /bin/node\n30 20 10 10 S+ ttys012 /bin/codex\n99 1 99 99 S+ ttys999 /bin/codex\n"
const strictFixtureStarts = "30 Mon Sep 7 10:00:00 2026\n20 Mon Sep 7 10:00:00 2026\n10 Mon Sep 7 10:00:00 2026\n"

type strictNativeFixture struct {
	dir, path  string
	held       *os.File
	descriptor strictDescriptor
	id         tmux.StrictPaneIdentity
}

func makeStrictNativeFixture(t *testing.T) *strictNativeFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "rollout-now-"+strictFixtureThread+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, err := file.WriteString(strictFixtureHeader(strictFixtureThread, `"cli"`) + "\nMESSAGE RECORD MUST NOT BE READ\n"); err != nil {
		t.Fatal(err)
	}
	dev, ino, err := strictFileIdentity(file)
	if err != nil {
		t.Fatal(err)
	}
	return &strictNativeFixture{dir, path, file, strictDescriptor{"42", "u", path, dev, ino}, tmux.StrictPaneIdentity{PID: "10", Command: "node", TTY: "/dev/ttys012", CWD: dir}}
}
func strictFixtureHeader(id, source string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q,"source":%s}}`, id, id, source)
}
func (f *strictNativeFixture) lsof(descriptors ...strictDescriptor) string {
	out := "p30\nfcwd\na \ntDIR\nD0x1000010\ni175486709\nn" + f.dir + "\n"
	for _, d := range descriptors {
		out += fmt.Sprintf("f%s\na%s\ntREG\nD0x%x\ni%d\nn%s\n", d.FD, d.Access, d.Device, d.Inode, d.Path)
	}
	return out
}

func TestStrictNativeForegroundAndDescriptorAuthority(t *testing.T) {
	for _, fault := range []string{"none", "foreground_shell", "background_group", "stopped", "wrong_tty", "wrong_cwd", "readonly", "wrong_device", "wrong_inode", "path_replaced", "fd_closed", "fd_reused", "ancestor_replaced", "ancestry_changed", "start_changed", "lsof_failed"} {
		t.Run(fault, func(t *testing.T) {
			f := makeStrictNativeFixture(t)
			id := f.id
			descriptor := f.descriptor
			switch fault {
			case "foreground_shell":
				id.Command = "bash"
			case "readonly":
				descriptor.Access = "r"
			case "wrong_device":
				descriptor.Device++
			case "wrong_inode":
				descriptor.Inode++
			}
			psCalls, lsofCalls := 0, 0
			run := func(name string, args ...string) ([]byte, error) {
				if name == "ps" && args[0] == "-Ao" {
					psCalls++
					table := strictFixtureProcesses
					switch fault {
					case "background_group":
						table = strings.Replace(table, "30 20 10 10 S+", "30 20 30 10 S", 1)
					case "stopped":
						table = strings.Replace(table, "30 20 10 10 S+", "30 20 10 10 T+", 1)
					case "wrong_tty":
						table = strings.ReplaceAll(table, "ttys012", "ttys013")
					case "ancestry_changed":
						if psCalls > 1 {
							table = strings.Replace(table, "30 20 10", "30 10 10", 1)
						}
					}
					return []byte(table), nil
				}
				if name == "ps" {
					if fault == "start_changed" && psCalls > 1 {
						return []byte(strings.Replace(strictFixtureStarts, "10:00:00", "11:00:00", 1)), nil
					}
					return []byte(strictFixtureStarts), nil
				}
				if name != "lsof" {
					t.Fatalf("unexpected probe %s", name)
				}
				lsofCalls++
				out := f.lsof(descriptor)
				if fault == "lsof_failed" {
					return nil, fmt.Errorf("lsof failed")
				}
				if fault == "wrong_cwd" {
					out = strings.Replace(out, "n"+f.dir+"\n", "n/other\n", 1)
				}
				if fault == "path_replaced" && lsofCalls == 1 {
					if err := os.Rename(f.path, f.path+".held"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.path, []byte(strictFixtureHeader(strictFixtureThread, `"cli"`)+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "fd_closed" && lsofCalls == 2 {
					out = f.lsof()
				}
				if fault == "fd_reused" && lsofCalls == 2 {
					next := descriptor
					next.Inode++
					out = f.lsof(next)
				}
				if fault == "ancestor_replaced" && lsofCalls == 2 {
					old := filepath.Dir(f.path)
					moved := old + "-old"
					if err := os.Rename(old, moved); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(old, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(filepath.Join(moved, filepath.Base(f.path)), f.path); err != nil {
						t.Fatal(err)
					}
				}
				return []byte(out), nil
			}
			proof, err := strictCodexProofWithRun(id, strictFixtureThread, f.dir, run)
			if fault == "none" {
				if err != nil || !strings.Contains(proof, "42:u:") {
					t.Fatalf("valid Darwin descriptor rejected: proof=%q err=%v", proof, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe authority %s accepted: %s", fault, proof)
			}
		})
	}
}

func TestStrictRootAllowsChildrenButRequiresUniqueWritableMain(t *testing.T) {
	f := makeStrictNativeFixture(t)
	childID := "00000000-0000-4000-8000-000000000002"
	child := filepath.Join(filepath.Dir(f.path), "rollout-child-"+childID+".jsonl")
	writeChild := func(source string) strictDescriptor {
		if err := os.WriteFile(child, []byte(strings.Replace(strictFixtureHeader(childID, source), `"session_id":"`+childID+`"`, `"session_id":"`+strictFixtureThread+`"`, 1)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(child)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		dev, ino, err := strictFileIdentity(file)
		if err != nil {
			t.Fatal(err)
		}
		return strictDescriptor{"49", "u", child, dev, ino}
	}
	sub := writeChild(`{"subagent":{"thread_spawn":{"parent_thread_id":"00000000-0000-4000-8000-000000000001"}}}`)
	binding, held, err := strictRootDescriptor([]strictDescriptor{f.descriptor, sub}, strictFixtureThread)
	if err != nil {
		t.Fatal(err)
	}
	held.Close()
	if binding.FD != "42" {
		t.Fatal("child substituted for main")
	}
	other := writeChild(`"cli"`)
	if _, held, err := strictRootDescriptor([]strictDescriptor{f.descriptor, other}, strictFixtureThread); err == nil {
		held.Close()
		t.Fatal("second root accepted")
	}
	if _, held, err := strictRootDescriptor([]strictDescriptor{sub}, strictFixtureThread); err == nil {
		held.Close()
		t.Fatal("child-only root accepted")
	}
}

func TestStrictDescriptorParserRejectsMissingAndAmbiguousAuthority(t *testing.T) {
	f := makeStrictNativeFixture(t)
	good := f.lsof(f.descriptor)
	files, cwd, err := strictParseDescriptors(good, "30")
	if err != nil || len(files) != 1 || cwd != f.dir || files[0] != f.descriptor {
		t.Fatalf("measured Darwin shape rejected: %v", err)
	}
	for _, bad := range []string{strings.Replace(good, "p30", "p31", 1), strings.Replace(good, "au\n", "", 1), strings.Replace(good, "f42", "f42\nf42", 1), strings.Replace(good, "tREG", "tDIR", 1), strings.Replace(good, ".jsonl\n", ".jsonl (deleted)\n", 1), strings.Replace(good, "D0x", "Dunknown", -1)} {
		if _, _, err := strictParseDescriptors(bad, "30"); err == nil {
			t.Fatal("incomplete descriptor accepted")
		}
	}
}

func TestStrictPathRejectsSymlinkAncestorAndLeaf(t *testing.T) {
	f := makeStrictNativeFixture(t)
	link := filepath.Join(f.dir, "alias")
	if err := os.Symlink(filepath.Dir(f.path), link); err != nil {
		t.Fatal(err)
	}
	if file, _, err := strictOpenVerifiedPath(filepath.Join(link, filepath.Base(f.path))); err == nil {
		file.Close()
		t.Fatal("symlink ancestor accepted")
	}
	leaf := f.path + "-link"
	if err := os.Symlink(f.path, leaf); err != nil {
		t.Fatal(err)
	}
	if file, _, err := strictOpenVerifiedPath(leaf); err == nil {
		file.Close()
		t.Fatal("symlink leaf accepted")
	}
}

func TestStrictNativeProbeNormalizesTimezone(t *testing.T) {
	t.Setenv("TZ", "America/Santiago")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	cmd, err := strictNativeCommand(context.Background(), "ps", "-p", "123", "-o", "lstart=")
	if err != nil || strings.Join(cmd.Env, "|") != "PATH=/usr/bin:/bin:/usr/sbin:/sbin|TZ=UTC|LC_ALL=C" {
		t.Fatalf("native timestamp environment: %v %v", cmd, err)
	}
	if _, err := strictNativeCommand(context.Background(), "ps", "-p", "123; echo unsafe", "-o", "lstart="); err == nil {
		t.Fatal("nonnumeric pid accepted")
	}
	if _, err := strictNativeCommand(context.Background(), "sh", "-c", "true"); err == nil {
		t.Fatal("shell probe accepted")
	}
	for _, platform := range []string{"linux", "windows", "freebsd"} {
		if strictPlatformSupported(platform) {
			t.Fatalf("unsupported platform %s admitted", platform)
		}
	}
	if !strictPlatformSupported("darwin") {
		t.Fatal("Darwin unsupported")
	}
}
