package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

const strictLinuxFixtureThread = "00000000-0000-4000-8000-000000000001"

type strictClaudeLinuxFixture struct {
	home, procRoot, recordPath, rootPath      string
	packagePath, manifestPath, executablePath string
	inst                                      *session.Instance
	target                                    *tmux.Session
	id                                        tmux.StrictPaneIdentity
	record                                    strictClaudeRecord
	deps                                      strictClaudeLinuxDeps
}

func newStrictClaudeLinuxFixture(t *testing.T) *strictClaudeLinuxFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(name, "")
	}
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	procRoot := filepath.Join(home, "proc")
	processRoot := filepath.Join(procRoot, "123")
	for _, directory := range []string{filepath.Join(processRoot, "fd"), filepath.Join(processRoot, "ns"), filepath.Join(procRoot, "sys", "kernel", "random")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ttyInfo, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	ttyStat := ttyInfo.Sys().(*syscall.Stat_t)
	statLine := fmt.Sprintf("123 (claude) S 12 123 123 %d 123 0 0 0 0 0 0 0 0 0 20 0 1 0 10000\n", ttyStat.Rdev)
	if err := os.WriteFile(filepath.Join(processRoot, "stat"), []byte(statLine), 0600); err != nil {
		t.Fatal(err)
	}
	status := "Name:\tclaude\nState:\tS (sleeping)\nTgid:\t123\nPid:\t123\nPPid:\t12\nUid:\t" + strings.Repeat(strconv.Itoa(os.Getuid())+"\t", 4) + "\n"
	if err := os.WriteFile(filepath.Join(processRoot, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	for path, target := range map[string]string{
		filepath.Join(processRoot, "fd", "0"):   "/dev/null",
		filepath.Join(processRoot, "cwd"):       project,
		filepath.Join(processRoot, "ns", "pid"): "pid:[4026532219]",
	} {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	packagePath := strictClaudeLinuxPackagePath(home)
	manifestPath := filepath.Join(packagePath, "package.json")
	executable := filepath.Join(packagePath, "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(executable), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(packagePath, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(`{"name":"@anthropic-ai/claude-code","version":"2.1.268"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("native Linux Claude fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executable, filepath.Join(packagePath, "bin", "claude.exe.package-link")); err != nil {
		t.Fatal(err)
	}
	packageInfo, err := os.Stat(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	packageLinks := uint64(packageInfo.Sys().(*syscall.Stat_t).Nlink)
	executableLinks := uint64(executableInfo.Sys().(*syscall.Stat_t).Nlink)
	fixturePackageProof := func(home, version string, uid int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
		return strictClaudeLinuxPackageProofWithLinks(home, version, uid, packageLinks, executableLinks)
	}
	if err := os.Symlink(executable, filepath.Join(processRoot, "exe")); err != nil {
		t.Fatal(err)
	}
	bootID := "e5656c24-f93c-4c9e-b4cf-e4941b9eabd0"
	if err := os.WriteFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"), []byte(bootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "stat"), []byte("cpu 1 2 3 4\nbtime 100000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(home, ".claude")
	record := strictClaudeRecord{PID: 123, SessionID: strictLinuxFixtureThread, CWD: project,
		Tmux: "agentdeck_fixture:@22.%22", ProcStart: "10000", PIDDomain: "linux:" + bootID + ":pid:[4026532219]",
		Version: "2.1.268", Entrypoint: "cli", Kind: "interactive", StartedAt: time.Unix(100100, 500*int64(time.Millisecond)).UnixMilli(),
		NameSince: time.Unix(100101, 0).UnixMilli(), StatusUpdatedAt: time.Unix(100101, 0).UnixMilli(), UpdatedAt: time.Unix(100101, 0).UnixMilli(),
		PeerProtocol: 1, PeerFeatures: []string{"fixture"}, Status: "idle"}
	recordPath := filepath.Join(configDir, "sessions", "123.json")
	rootPath := filepath.Join(configDir, "projects", session.ConvertToClaudeDirName(project), record.SessionID+".jsonl")
	for _, path := range []string{recordPath, rootPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &strictClaudeLinuxFixture{
		home: home, procRoot: procRoot, recordPath: recordPath, rootPath: rootPath,
		packagePath: packagePath, manifestPath: manifestPath, executablePath: executable,
		inst:   &session.Instance{ID: "5e6a8428-1789103929", Tool: "claude", ProjectPath: project, TmuxSocketName: "agent-deck"},
		target: &tmux.Session{Name: "agentdeck_fixture", SocketName: "agent-deck", InstanceID: "5e6a8428-1789103929"},
		id: tmux.StrictPaneIdentity{SessionID: "$22", SessionName: "agentdeck_fixture", WindowID: "@22", PaneID: "%22",
			PID: "123", Command: "claude", CWD: project, TTY: "/dev/null"},
		record: record,
		deps: strictClaudeLinuxDeps{procRoot: procRoot, bind: strictClaudeOwnedBinding, readBounded: strictLinuxReadBounded,
			readlink: os.Readlink, open: os.Open, stat: os.Stat, packageProof: fixturePackageProof, uid: os.Getuid()},
	}
	fixture.writeRecord(t)
	if err := os.WriteFile(rootPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *strictClaudeLinuxFixture) writeRecord(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.recordPath, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func (f *strictClaudeLinuxFixture) proof() (strictClaudeNativeProof, error) {
	return strictClaudeLinuxThreadProofWithDeps(f.inst, f.target, f.id, f.home, f.deps)
}

func TestStrictClaudeLinuxExactNativeProof(t *testing.T) {
	fixture := newStrictClaudeLinuxFixture(t)
	proof, err := fixture.proof()
	if err != nil {
		t.Fatal(err)
	}
	if !proof.DurableStop || proof.Thread != strictLinuxFixtureThread || proof.CWD != fixture.inst.ProjectPath || proof.Signature == "" {
		t.Fatalf("incomplete Linux proof: %+v", proof)
	}
	wantStart := time.Unix(100100, 0).UTC()
	if !proof.ProcessStartedAt.Equal(wantStart) {
		t.Fatalf("process start=%s want=%s", proof.ProcessStartedAt, wantStart)
	}
}

func TestStrictClaudeLinuxRejectsIndependentIdentityFaults(t *testing.T) {
	for _, fault := range []string{"missing_process", "process_churn", "wrong_start", "wrong_wall_start", "pid_reuse", "wrong_pgid", "wrong_sid", "background", "stopped", "zombie", "wrong_uid", "wrong_executable", "wrong_instance", "wrong_socket", "wrong_session", "wrong_pane", "wrong_cwd", "wrong_tty", "wrong_domain", "unsafe_record", "duplicate_record", "unsafe_record_path", "record_churn", "missing_root", "duplicate_root", "root_churn", "package_path", "package_owner", "package_mode", "package_link", "manifest_path", "manifest_mode", "manifest_link", "manifest_content", "manifest_churn", "package_version", "executable_path", "executable_mode", "executable_link", "executable_content"} {
		t.Run(fault, func(t *testing.T) {
			f := newStrictClaudeLinuxFixture(t)
			switch fault {
			case "missing_process":
				if err := os.Remove(filepath.Join(f.procRoot, "123", "stat")); err != nil {
					t.Fatal(err)
				}
			case "wrong_start":
				f.record.ProcStart = "9999"
				f.writeRecord(t)
			case "wrong_wall_start":
				f.record.StartedAt += 10_000
				f.writeRecord(t)
			case "wrong_pgid":
				path := filepath.Join(f.procRoot, "123", "stat")
				data, _ := os.ReadFile(path)
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), " S 12 123 123 ", " S 12 124 123 ", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_sid":
				path := filepath.Join(f.procRoot, "123", "stat")
				data, _ := os.ReadFile(path)
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), " S 12 123 123 ", " S 12 123 124 ", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "background":
				path := filepath.Join(f.procRoot, "123", "stat")
				data, _ := os.ReadFile(path)
				fields := strings.Split(string(data), " ")
				fields[7] = "124"
				if err := os.WriteFile(path, []byte(strings.Join(fields, " ")), 0600); err != nil {
					t.Fatal(err)
				}
			case "stopped", "zombie":
				path := filepath.Join(f.procRoot, "123", "stat")
				data, _ := os.ReadFile(path)
				state := map[string]string{"stopped": "T", "zombie": "Z"}[fault]
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), "(claude) S ", "(claude) "+state+" ", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_uid":
				path := filepath.Join(f.procRoot, "123", "status")
				data, _ := os.ReadFile(path)
				wrong := strings.Repeat(strconv.Itoa(os.Getuid()+1)+"\t", 4)
				good := strings.Repeat(strconv.Itoa(os.Getuid())+"\t", 4)
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), good, wrong, 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_executable":
				path := filepath.Join(f.procRoot, "123", "exe")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/bin/sh", path); err != nil {
					t.Fatal(err)
				}
			case "wrong_instance":
				f.target.InstanceID = "other"
			case "wrong_socket":
				f.target.SocketName = "other"
			case "wrong_session":
				f.target.Name = "other"
			case "wrong_pane":
				f.record.Tmux = "agentdeck_fixture:@22.%23"
				f.writeRecord(t)
			case "wrong_cwd":
				f.record.CWD = filepath.Join(f.home, "other")
				f.writeRecord(t)
			case "wrong_tty":
				f.id.TTY = "/dev/zero"
			case "wrong_domain":
				f.record.PIDDomain = "linux:00000000-0000-0000-0000-000000000000:pid:[1]"
				f.writeRecord(t)
			case "unsafe_record":
				if err := os.Chmod(f.recordPath, 0620); err != nil {
					t.Fatal(err)
				}
			case "duplicate_record":
				if err := os.Link(f.recordPath, f.recordPath+".duplicate"); err != nil {
					t.Fatal(err)
				}
			case "unsafe_record_path":
				if err := os.Rename(f.recordPath, f.recordPath+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.recordPath+".real", f.recordPath); err != nil {
					t.Fatal(err)
				}
			case "record_churn":
				baseBind := f.deps.bind
				reads := 0
				f.deps.bind = func(path string, max int64, content bool) (strictClaudeFileBinding, *os.File, []byte, error) {
					binding, file, data, err := baseBind(path, max, content)
					if err == nil && path == f.recordPath {
						reads++
						if reads == 1 {
							f.record.Version = "2.1.269"
							f.writeRecord(t)
						}
					}
					return binding, file, data, err
				}
			case "missing_root":
				if err := os.Remove(f.rootPath); err != nil {
					t.Fatal(err)
				}
			case "duplicate_root":
				if err := os.Link(f.rootPath, f.rootPath+".duplicate"); err != nil {
					t.Fatal(err)
				}
			case "root_churn":
				baseBind := f.deps.bind
				reads := 0
				f.deps.bind = func(path string, max int64, content bool) (strictClaudeFileBinding, *os.File, []byte, error) {
					binding, file, data, err := baseBind(path, max, content)
					if err == nil && path == f.rootPath {
						reads++
						if reads == 1 {
							if writeErr := os.WriteFile(f.rootPath, []byte("changed\n"), 0600); writeErr != nil {
								t.Fatal(writeErr)
							}
						}
					}
					return binding, file, data, err
				}
			case "package_path":
				if err := os.Rename(f.packagePath, f.packagePath+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.packagePath+".real", f.packagePath); err != nil {
					t.Fatal(err)
				}
			case "package_owner":
				baseProof := f.deps.packageProof
				f.deps.packageProof = func(home, version string, uid int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
					return baseProof(home, version, uid+1)
				}
			case "package_mode":
				if err := os.Chmod(f.packagePath, 0775); err != nil {
					t.Fatal(err)
				}
			case "package_link":
				if err := os.Mkdir(filepath.Join(f.packagePath, "unexpected"), 0755); err != nil {
					t.Fatal(err)
				}
			case "manifest_path":
				if err := os.Rename(f.manifestPath, f.manifestPath+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.manifestPath+".real", f.manifestPath); err != nil {
					t.Fatal(err)
				}
			case "manifest_mode":
				if err := os.Chmod(f.manifestPath, 0664); err != nil {
					t.Fatal(err)
				}
			case "manifest_link":
				if err := os.Link(f.manifestPath, f.manifestPath+".link"); err != nil {
					t.Fatal(err)
				}
			case "manifest_content":
				if err := os.WriteFile(f.manifestPath, []byte(`{"name":"other","version":"2.1.268"}`), 0644); err != nil {
					t.Fatal(err)
				}
			case "manifest_churn":
				baseProof := f.deps.packageProof
				calls := 0
				f.deps.packageProof = func(home, version string, uid int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
					calls++
					proof, handles, err := baseProof(home, version, uid)
					if err == nil && calls == 1 {
						if writeErr := os.WriteFile(f.manifestPath, []byte(`{"name":"@anthropic-ai/claude-code","version":"2.1.268","description":"changed"}`), 0644); writeErr != nil {
							t.Fatal(writeErr)
						}
					}
					return proof, handles, err
				}
			case "package_version":
				f.record.Version = "2.1.269"
				f.writeRecord(t)
			case "executable_path":
				if err := os.Rename(f.executablePath, f.executablePath+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.executablePath+".real", f.executablePath); err != nil {
					t.Fatal(err)
				}
			case "executable_mode":
				if err := os.Chmod(f.executablePath, 0775); err != nil {
					t.Fatal(err)
				}
			case "executable_link":
				if err := os.Link(f.executablePath, f.executablePath+".third-link"); err != nil {
					t.Fatal(err)
				}
			case "executable_content":
				baseProof := f.deps.packageProof
				calls := 0
				f.deps.packageProof = func(home, version string, uid int) (strictClaudeLinuxPackage, *strictClaudeLinuxPackageHandles, error) {
					calls++
					proof, handles, err := baseProof(home, version, uid)
					if err == nil && calls == 1 {
						if writeErr := os.WriteFile(f.executablePath, []byte("changed native Linux Claude fixture"), 0755); writeErr != nil {
							t.Fatal(writeErr)
						}
					}
					return proof, handles, err
				}
			case "process_churn", "pid_reuse":
				baseRead := f.deps.readBounded
				reads := 0
				f.deps.readBounded = func(path string, limit int64) ([]byte, error) {
					data, err := baseRead(path, limit)
					if path == filepath.Join(f.procRoot, "123", "stat") {
						reads++
						if reads == 2 {
							if fault == "process_churn" {
								data = []byte(strings.Replace(string(data), " S 12 ", " S 13 ", 1))
							} else {
								data = []byte(strings.TrimSpace(string(data)))
								data = []byte(strings.TrimSuffix(string(data), "10000") + "10001\n")
							}
						}
					}
					return data, err
				}
			}
			if proof, err := f.proof(); err == nil || proof.DurableStop {
				t.Fatalf("Linux identity fault %s accepted: %+v err=%v", fault, proof, err)
			}
		})
	}
}

func TestStrictClaudeLinuxRecordSchemaIsClosed(t *testing.T) {
	f := newStrictClaudeLinuxFixture(t)
	valid, err := os.ReadFile(f.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, data string }{
		{"missing", strings.Replace(string(valid), `"procStart":"10000",`, "", 1)},
		{"missing_schema_field", strings.Replace(string(valid), `"name":"",`, "", 1)},
		{"unknown", strings.Replace(string(valid), "{", `{"futureField":true,`, 1)},
		{"duplicate", strings.Replace(string(valid), "{", `{"pid":123,`, 1)},
		{"malformed", string(valid[:len(valid)-1])},
		{"wrong_type", strings.Replace(string(valid), `"startedAt":100100500`, `"startedAt":"100100500"`, 1)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if _, err := strictDecodeClaudeLinuxRecord([]byte(fixture.data)); err == nil {
				t.Fatal("unsafe Linux record accepted")
			}
		})
	}
}

func TestStrictClaudeLinuxWrongExpectedThreadRefuses(t *testing.T) {
	f := newStrictClaudeLinuxFixture(t)
	v := newStrictSessionAdmissionVerifier(f.inst, f.target, "00000000-0000-4000-8000-000000000002")
	v.deps.native = func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
		proof, err := f.proof()
		return strictNativeThreadProof{Thread: proof.Thread, Claude: proof}, err
	}
	v.deps.idle = func(string, string, string, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
		return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
	}
	if _, err := v.verify(f.id); err == nil {
		t.Fatal("wrong expected Linux thread accepted")
	}
}

func strictClaudeLinuxFixtureVerifier(f *strictClaudeLinuxFixture) (*strictSessionAdmissionVerifier, *int) {
	idleCalls := 0
	v := newStrictSessionAdmissionVerifier(f.inst, f.target, strictLinuxFixtureThread)
	v.deps.native = func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
		proof, err := f.proof()
		return strictNativeThreadProof{Thread: proof.Thread, Claude: proof}, err
	}
	v.deps.idle = func(string, string, string, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
		idleCalls++
		return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
	}
	return v, &idleCalls
}

func TestStrictClaudeLinuxFinalRecheckRejectsAuthorityMutation(t *testing.T) {
	for _, mutation := range []string{"record", "record_path", "record_mode", "record_link", "root", "manifest", "executable", "executable_link", "package_mode", "package_content"} {
		t.Run(mutation, func(t *testing.T) {
			f := newStrictClaudeLinuxFixture(t)
			v, idleCalls := strictClaudeLinuxFixtureVerifier(f)
			if _, err := v.verify(f.id); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "record":
				f.record.Version = "2.1.269"
				f.writeRecord(t)
			case "record_path":
				data, err := os.ReadFile(f.recordPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(f.recordPath, f.recordPath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.recordPath, data, 0644); err != nil {
					t.Fatal(err)
				}
			case "record_mode":
				if err := os.Chmod(f.recordPath, 0600); err != nil {
					t.Fatal(err)
				}
			case "record_link":
				if err := os.Link(f.recordPath, f.recordPath+".link"); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.WriteFile(f.rootPath, []byte("changed\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				if err := os.WriteFile(f.manifestPath, []byte(`{"name":"@anthropic-ai/claude-code","version":"2.1.268","description":"changed"}`), 0644); err != nil {
					t.Fatal(err)
				}
			case "executable":
				data, err := os.ReadFile(f.executablePath)
				if err != nil {
					t.Fatal(err)
				}
				data[0] ^= 1
				if err := os.WriteFile(f.executablePath, data, 0755); err != nil {
					t.Fatal(err)
				}
			case "executable_link":
				if err := os.Link(f.executablePath, f.executablePath+".third-link"); err != nil {
					t.Fatal(err)
				}
			case "package_mode":
				if err := os.Chmod(f.packagePath, 0775); err != nil {
					t.Fatal(err)
				}
			case "package_content":
				if err := os.WriteFile(filepath.Join(f.packagePath, "unexpected-file"), []byte("changed"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(f.packagePath, time.Unix(200000, 0), time.Unix(200000, 0)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := v.verify(f.id); err == nil {
				t.Fatalf("final %s mutation accepted", mutation)
			}
			if *idleCalls != 1 {
				t.Fatalf("mutation reached final idle admission: calls=%d", *idleCalls)
			}
		})
	}
}

func TestStrictClaudeLinuxPackageFilesRequireExactOwner(t *testing.T) {
	f := newStrictClaudeLinuxFixture(t)
	manifestInfo, _ := os.Stat(f.manifestPath)
	executableInfo, _ := os.Stat(f.executablePath)
	for _, tc := range []struct {
		path    string
		limit   int64
		capture bool
		mode    os.FileMode
		links   uint64
	}{
		{f.manifestPath, 64 * 1024, true, 0644, uint64(manifestInfo.Sys().(*syscall.Stat_t).Nlink)},
		{f.executablePath, 512 * 1024 * 1024, false, 0755, uint64(executableInfo.Sys().(*syscall.Stat_t).Nlink)},
	} {
		if _, file, _, err := strictClaudeLinuxOwnedPackageFile(tc.path, tc.limit, tc.capture, tc.mode, tc.links, os.Getuid()+1); err == nil {
			file.Close()
			t.Fatalf("wrong owner accepted for %s", tc.path)
		}
	}
}

func TestStrictClaudeLinuxTransientStateAndTimestampsDoNotBreakContinuity(t *testing.T) {
	f := newStrictClaudeLinuxFixture(t)
	v, idleCalls := strictClaudeLinuxFixtureVerifier(f)
	if _, err := v.verify(f.id); err != nil {
		t.Fatal(err)
	}
	f.record.NameSince++
	f.record.StatusUpdatedAt++
	f.record.UpdatedAt++
	f.writeRecord(t)
	statPath := filepath.Join(f.procRoot, "123", "stat")
	statData, _ := os.ReadFile(statPath)
	if err := os.WriteFile(statPath, []byte(strings.Replace(string(statData), "(claude) S ", "(claude) R ", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(f.procRoot, "123", "status")
	statusData, _ := os.ReadFile(statusPath)
	if err := os.WriteFile(statusPath, []byte(strings.Replace(string(statusData), "State:\tS (sleeping)", "State:\tR (running)", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.verify(f.id); err != nil {
		t.Fatalf("transient Linux evidence broke stable continuity: %v", err)
	}
	if *idleCalls != 2 {
		t.Fatalf("idle checks=%d want=2", *idleCalls)
	}
}

func TestStrictClaudeLinuxRecordBounds(t *testing.T) {
	f := newStrictClaudeLinuxFixture(t)
	for _, mutate := range []func(*strictClaudeRecord){
		func(r *strictClaudeRecord) { r.StartedAt = 0 },
		func(r *strictClaudeRecord) { r.NameSince = r.StartedAt - 1 },
		func(r *strictClaudeRecord) { r.StatusUpdatedAt = 1 << 53 },
		func(r *strictClaudeRecord) { r.PeerProtocol = 65 },
		func(r *strictClaudeRecord) { r.PeerFeatures = []string{strings.Repeat("x", 129)} },
	} {
		record := f.record
		mutate(&record)
		if strictClaudeLinuxRecordBounds(record) {
			t.Fatalf("out-of-bounds record accepted: %+v", record)
		}
	}
}

func TestStrictClaudeLinuxPackageMetadataIsExact(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"name":"@anthropic-ai/claude-code","version":"2.1.268"}`, true},
		{"duplicate", `{"name":"@anthropic-ai/claude-code","version":"2.1.268","version":"2.1.269"}`, false},
		{"wrong_type", `{"name":"@anthropic-ai/claude-code","version":268}`, false},
		{"missing", `{"name":"@anthropic-ai/claude-code"}`, false},
		{"trailing", `{"name":"@anthropic-ai/claude-code","version":"2.1.268"}x`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := strictClaudeLinuxPackageMetadata([]byte(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestStrictClaudeLinuxProductionPackageLinkContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux directory link semantics are verified in the bundle worktree")
	}
	f := newStrictClaudeLinuxFixture(t)
	proof, handles, err := strictClaudeLinuxPackageProof(f.home, f.record.Version, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer handles.Close()
	if proof.Directory.Nlink != 4 || proof.Executable.Nlink != 2 {
		t.Fatalf("production package link contract drifted: %+v", proof)
	}
}

func TestStrictLinuxExecutableGrammarIsMeasuredAndClosed(t *testing.T) {
	home := "/home/franco"
	good := home + "/.nvm/versions/node/v24.18.0/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe"
	if !strictLinuxClaudeExecutable(good, home) {
		t.Fatalf("stable executable rejected: %s", good)
	}
	for _, value := range []string{"/usr/bin/claude", home + "/.local/bin/claude", strings.Replace(good, "@anthropic-ai", "other", 1),
		strings.Replace(good, "v24.18.0", "v24.19.0", 1), strings.Replace(good, "/claude-code/", "/.claude-code-XnC2OMep/", 1),
		good + " (deleted)", good + ".old", "relative/claude.exe"} {
		if strictLinuxClaudeExecutable(value, home) {
			t.Fatalf("unmeasured executable accepted: %s", value)
		}
	}
}
