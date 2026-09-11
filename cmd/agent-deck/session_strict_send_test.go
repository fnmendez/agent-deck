package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type strictRegistryFingerprint struct {
	Mode          os.FileMode
	Size, ModTime int64
	SHA           [32]byte
}

func strictVerifiedPaneIdentity() tmux.StrictPaneIdentity {
	return tmux.StrictPaneIdentity{ServerVersion: "3.7b", BracketPaste: "1"}
}

func strictFixedProbeObservation(id tmux.StrictPaneIdentity) func() (tmux.StrictPaneIdentity, error) {
	return func() (tmux.StrictPaneIdentity, error) { return id, nil }
}

func fingerprintStrictRegistry(t *testing.T, directory string) map[string]strictRegistryFingerprint {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]strictRegistryFingerprint, len(entries))
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("unsafe registry fixture entry %s: %v", entry.Name(), err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = strictRegistryFingerprint{Mode: info.Mode(), Size: info.Size(),
			ModTime: info.ModTime().UnixNano(), SHA: sha256.Sum256(data)}
	}
	return result
}

func TestStrictNativeClaudeIdentityRequiresAllEvidence(t *testing.T) {
	id := tmux.StrictPaneIdentity{PID: "123", PaneID: "%2", SessionName: "target", WindowID: "@3", CWD: "/project", Command: "claude"}
	started := "Fri Sep  4 18:43:39 2026"
	good := strictClaudeRecord{PID: 123, SessionID: "expected", CWD: "/project", Tmux: "target:@3.%2", ProcStart: started, PIDDomain: "darwin"}
	if !strictClaudeIdentityMatches(good, id, "/project", started, "darwin") {
		t.Fatal("valid native binding rejected")
	}
	for name, mutate := range map[string]func(*strictClaudeRecord){
		"pid":    func(r *strictClaudeRecord) { r.PID = 124 },
		"thread": func(r *strictClaudeRecord) { r.SessionID = "" },
		"cwd":    func(r *strictClaudeRecord) { r.CWD = "/other" },
		"pane":   func(r *strictClaudeRecord) { r.Tmux = "target:@3.%4" },
		"start":  func(r *strictClaudeRecord) { r.ProcStart = "Fri Sep 4 19:43:39 2026" },
		"domain": func(r *strictClaudeRecord) { r.PIDDomain = "linux" },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			if strictClaudeIdentityMatches(r, id, "/project", started, "darwin") {
				t.Fatal("mismatched identity accepted")
			}
		})
	}
}

func TestStrictIdleRequiresFreshMatchingHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := session.GetHooksDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := now.Add(-24 * time.Hour).Unix()
	strong := session.StrictSendIdleEvidence{DurableClaudeStop: true,
		ProcessStartedAt: now.Add(-48 * time.Hour), CWD: "/project"}
	for _, tc := range []struct {
		name, tool, status, thread, event, cwd string
		ts                                     int64
		evidence                               session.StrictSendIdleEvidence
		want                                   bool
	}{
		{"fresh_without_event", "claude", "waiting", "expected", "", "", now.Unix(), session.StrictSendIdleEvidence{}, true},
		{"busy", "claude", "running", "expected", "UserPromptSubmit", "/project", now.Unix(), strong, false},
		{"wrong_thread", "claude", "waiting", "other", "Stop", "/project", now.Unix(), strong, false},
		{"no_timestamp", "claude", "waiting", "expected", "Stop", "/project", 0, strong, false},
		{"future", "claude", "waiting", "expected", "Stop", "/project", now.Add(time.Hour).Unix(), strong, false},
		{"stale_weak", "claude", "waiting", "expected", "Stop", "/project", stale, session.StrictSendIdleEvidence{}, false},
		{"stale_durable", "claude", "waiting", "expected", "Stop", "/project", stale, strong, true},
		{"stale_equal_start", "claude", "waiting", "expected", "Stop", "/project", stale,
			session.StrictSendIdleEvidence{DurableClaudeStop: true, ProcessStartedAt: time.Unix(stale, 0), CWD: "/project"}, true},
		{"stale_before_process", "claude", "waiting", "expected", "Stop", "/project", stale,
			session.StrictSendIdleEvidence{DurableClaudeStop: true, ProcessStartedAt: time.Unix(stale+1, 0), CWD: "/project"}, false},
		{"stale_event_case", "claude", "waiting", "expected", "stop", "/project", stale, strong, false},
		{"stale_other_event", "claude", "waiting", "expected", "PermissionRequest", "/project", stale, strong, false},
		{"stale_idle", "claude", "idle", "expected", "Stop", "/project", stale, strong, false},
		{"stale_codex", "codex", "waiting", "expected", "Stop", "/project", stale, strong, false},
		{"stale_missing_cwd", "claude", "waiting", "expected", "Stop", "", stale, strong, false},
		{"stale_wrong_cwd", "claude", "waiting", "expected", "Stop", "/other", stale, strong, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"status": tc.status, "session_id": tc.thread,
				"event": tc.event, "cwd": tc.cwd, "ts": tc.ts})
			if err := os.WriteFile(filepath.Join(dir, "strict-test.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			if got := session.StrictSendIdle("strict-test", tc.tool, "expected", tc.evidence); got.Admitted != tc.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if session.StrictSendIdle("missing", "claude", "expected", strong).Admitted {
		t.Fatal("missing hook accepted")
	}

	control, _ := json.Marshal(map[string]any{"generation": "g1"})
	if err := os.WriteFile(filepath.Join(dir, "strict-test.generation.json"), control, 0600); err != nil {
		t.Fatal(err)
	}
	writeGenerated := func(generation string, sequence uint64) session.StrictSendIdleDecision {
		body, _ := json.Marshal(map[string]any{"status": "waiting", "session_id": "expected",
			"event": "Stop", "cwd": "/project", "ts": stale,
			"hook_generation": generation, "sequence": sequence})
		if err := os.WriteFile(filepath.Join(dir, "strict-test.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
		return session.StrictSendIdle("strict-test", "claude", "expected", strong)
	}
	if got := writeGenerated("g1", 1); !got.Admitted || !got.DurableStop {
		t.Fatalf("matching generated Stop rejected: %+v", got)
	}
	if got := writeGenerated("g1", 0); got.Admitted {
		t.Fatalf("zero sequence accepted: %+v", got)
	}
	if got := writeGenerated("other", 1); got.Admitted {
		t.Fatalf("wrong generation accepted: %+v", got)
	}
}

func TestStrictStdinPreservesPrompt(t *testing.T) {
	want := "A literal prompt\nwith two lines and $() `characters`.\n\n"
	got, err := resolveStrictMessageInput("", "-", strings.NewReader(want))
	if err != nil || got != want {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestStrictSessionMetaReaderNeverConsumesSecondRecord(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	first := `{"type":"session_meta","payload":{"id":"expected","session_id":"expected","source":"cli"}}`
	if err := os.WriteFile(path, []byte(first+"\nINVALID SECOND RECORD"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := strictReadSessionMeta(path)
	if err != nil || record.Payload.ID != "expected" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := strictReadSessionMeta(link); err == nil {
		t.Fatal("symlink followed")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 65536)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := strictReadSessionMeta(path); err == nil {
		t.Fatal("unbounded record accepted")
	}
}

func TestStrictInputBoundedBeforeTargetLookup(t *testing.T) {
	if _, err := resolveStrictMessageInput("", "-", strings.NewReader(strings.Repeat("x", 65537))); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := resolveStrictMessageInput("inline", "-", strings.NewReader("stdin")); err == nil {
		t.Fatal("conflicting input accepted")
	}
}

func strictVerifierFixture(native []strictNativeThreadProof, idle func(int, session.StrictSendIdleEvidence) session.StrictSendIdleDecision) *strictSessionAdmissionVerifier {
	calls := 0
	return &strictSessionAdmissionVerifier{
		inst:   &session.Instance{ID: "instance", Tool: "claude", ProjectPath: "/project"},
		target: &tmux.Session{},
		deps: strictAdmissionDeps{
			native: func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
				if calls >= len(native) {
					return strictNativeThreadProof{}, fmt.Errorf("missing native pass")
				}
				value := native[calls]
				calls++
				return value, nil
			},
			codex: func(tmux.StrictPaneIdentity, string, string) (string, error) {
				return "", fmt.Errorf("unexpected Codex proof")
			},
			idle: func(_, _, _ string, evidence session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
				return idle(calls, evidence)
			},
		},
	}
}

func TestStrictAdmissionFreshThenStaleRequiresStrongBothPasses(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	start := time.Date(2026, time.September, 8, 18, 28, 28, 0, time.UTC)
	strong := strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{
		Thread: thread, ProcessStartedAt: start, CWD: "/project", Signature: "same", DurableStop: true}}
	v := strictVerifierFixture([]strictNativeThreadProof{strong, strong},
		func(pass int, evidence session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
			if !evidence.DurableClaudeStop || !evidence.ProcessStartedAt.Equal(start) || evidence.CWD != "/project" {
				t.Fatalf("pass %d lost strong evidence: %+v", pass, evidence)
			}
			return session.StrictSendIdleDecision{Admitted: true, DurableStop: pass == 2,
				Reason: map[bool]string{true: "durable_stop", false: "fresh_hook"}[pass == 2]}
		})
	observations, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity()))
	if err != nil || v.passes != 2 || observations[0].NativeProof != "same" || !observations[1].IdleDecision.DurableStop {
		t.Fatalf("observations=%+v passes=%d err=%v", observations, v.passes, err)
	}
}

func strictHooklessLinuxProof(status string, statusUpdatedAt int64, stable bool) strictNativeThreadProof {
	thread := "00000000-0000-4000-8000-000000000001"
	return strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{
		Thread: thread, ProcessStartedAt: time.Unix(100100, 0).UTC(), CWD: "/project", Signature: "same", DurableStop: true,
		NativeStatus: status, NativeStatusUpdatedAt: statusUpdatedAt, NativeStartedAt: 100100500, NativeStatusStable: stable,
	}}
}

func TestStrictLinuxHooklessNativeIdleAdmission(t *testing.T) {
	proof := strictHooklessLinuxProof("idle", 100101000, true)
	v := strictVerifierFixture([]strictNativeThreadProof{proof, proof},
		func(int, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
			return session.StrictSendIdleDecision{Reason: "hook_unavailable"}
		})
	v.platform, v.architecture = "linux", "amd64"
	observedTmux := 0
	observations, err := runStrictAdmissionProbe(v, func() (tmux.StrictPaneIdentity, error) {
		observedTmux++
		return strictVerifiedPaneIdentity(), nil
	})
	if err != nil || !observations[0].IdleDecision.Admitted || !observations[1].IdleDecision.Admitted ||
		observations[1].IdleDecision.Reason != "native_idle_without_hook" || observations[1].IdleDecision.DurableStop || observedTmux != 2 {
		t.Fatalf("hookless native idle result=%+v err=%v", observations, err)
	}
}

func TestStrictLinuxHooklessAdmissionRejectsMissingInvalidBusyAndWeakEvidence(t *testing.T) {
	now := time.Now()
	valid := strictHooklessLinuxProof("idle", 100101000, true).Claude
	for _, tc := range []struct {
		name, platform, architecture, tool, hookReason string
		proof                                          strictClaudeNativeProof
	}{
		{"missing_status", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof { p := valid; p.NativeStatus = ""; return p }()},
		{"busy", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof { p := valid; p.NativeStatus = "running"; return p }()},
		{"unstable", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof { p := valid; p.NativeStatusStable = false; return p }()},
		{"missing_timestamp", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof { p := valid; p.NativeStatusUpdatedAt = 0; return p }()},
		{"before_start", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof {
			p := valid
			p.NativeStatusUpdatedAt = 100099000
			return p
		}()},
		{"future", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof {
			p := valid
			p.NativeStatusUpdatedAt = now.Add(time.Minute).UnixMilli()
			return p
		}()},
		{"weak", "linux", "amd64", "claude", "hook_unavailable", func() strictClaudeNativeProof { p := valid; p.DurableStop = false; return p }()},
		{"darwin", "darwin", "arm64", "claude", "hook_unavailable", valid},
		{"wrong_arch", "linux", "arm64", "claude", "hook_unavailable", valid},
		{"non_claude", "linux", "amd64", "codex", "hook_unavailable", valid},
		{"invalid_hook_not_bypassed", "linux", "amd64", "claude", "hook_invalid", valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strictLinuxNativeIdleAdmission(tc.platform, tc.architecture, tc.tool, tc.proof, now) && tc.hookReason != "hook_invalid" {
				t.Fatal("unsafe hookless evidence admitted")
			}
			threadProof := strictNativeThreadProof{Thread: tc.proof.Thread, Claude: tc.proof}
			v := strictVerifierFixture([]strictNativeThreadProof{threadProof},
				func(int, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
					return session.StrictSendIdleDecision{Reason: tc.hookReason}
				})
			v.inst.Tool = tc.tool
			v.platform, v.architecture = tc.platform, tc.architecture
			v.expected = tc.proof.Thread
			if _, err := v.verify(strictVerifiedPaneIdentity()); err == nil {
				t.Fatal("hookless refusal admitted by verifier")
			}
		})
	}
}

func TestStrictLinuxHooklessAdmissionRejectsStatusTimestampAndAuthorityChange(t *testing.T) {
	base := strictHooklessLinuxProof("idle", 100101000, true)
	for _, mutation := range []string{"status", "timestamp", "authority"} {
		t.Run(mutation, func(t *testing.T) {
			second := base
			if mutation == "status" {
				second.Claude.NativeStatus = "running"
			}
			if mutation == "timestamp" {
				second.Claude.NativeStatusUpdatedAt++
			}
			idleCalls := 0
			v := strictVerifierFixture([]strictNativeThreadProof{base, second},
				func(pass int, _ session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
					idleCalls++
					if mutation == "authority" && pass == 2 {
						return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
					}
					return session.StrictSendIdleDecision{Reason: "hook_unavailable"}
				})
			v.platform, v.architecture = "linux", "amd64"
			if _, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity())); err == nil || idleCalls != 2 {
				t.Fatalf("hookless %s change admitted: err=%v idle_calls=%d", mutation, err, idleCalls)
			}
		})
	}
}

func TestStrictAdmissionRejectsLegacyAtStaleSecondPass(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	weak := strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{Thread: thread, CWD: "/project"}}
	v := strictVerifierFixture([]strictNativeThreadProof{weak, weak},
		func(pass int, evidence session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
			if evidence.DurableClaudeStop {
				t.Fatal("legacy proof gained durable authority")
			}
			return session.StrictSendIdleDecision{Admitted: pass == 1, Reason: "stale_hook"}
		})
	if _, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity())); err == nil || v.passes != 2 {
		t.Fatalf("legacy fresh-to-stale accepted: passes=%d err=%v", v.passes, err)
	}
}

func TestStrictAdmissionRejectsStrongDowngradeAndSignatureChange(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	base := strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{
		Thread: thread, ProcessStartedAt: time.Unix(1, 0), CWD: "/project", Signature: "first", DurableStop: true}}
	for name, second := range map[string]strictNativeThreadProof{
		"legacy": {Thread: thread, Claude: strictClaudeNativeProof{Thread: thread, CWD: "/project"}},
		"changed": {Thread: thread, Claude: strictClaudeNativeProof{Thread: thread,
			ProcessStartedAt: time.Unix(1, 0), CWD: "/project", Signature: "second", DurableStop: true}},
	} {
		t.Run(name, func(t *testing.T) {
			idleCalls := 0
			v := strictVerifierFixture([]strictNativeThreadProof{base, second},
				func(_ int, _ session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
					idleCalls++
					return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
				})
			if _, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity())); err == nil || idleCalls != 1 {
				t.Fatalf("identity change accepted: idle_calls=%d err=%v", idleCalls, err)
			}
		})
	}
}

func TestStrictAdmissionBindsExpectedThreadOnBothPasses(t *testing.T) {
	expected := "00000000-0000-4000-8000-000000000001"
	other := "00000000-0000-4000-8000-000000000002"
	start := time.Unix(1, 0)
	for _, strength := range []string{"strong", "legacy"} {
		for _, wrongPass := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s_pass%d", strength, wrongPass), func(t *testing.T) {
				makeProof := func(thread string) strictNativeThreadProof {
					proof := strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{Thread: thread, CWD: "/project"}}
					if strength == "strong" {
						proof.Claude.ProcessStartedAt = start
						proof.Claude.Signature = "same"
						proof.Claude.DurableStop = true
					}
					return proof
				}
				threads := []string{expected, expected}
				threads[wrongPass-1] = other
				idleCalls := 0
				v := strictVerifierFixture([]strictNativeThreadProof{makeProof(threads[0]), makeProof(threads[1])},
					func(_ int, _ session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
						idleCalls++
						return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
					})
				v.expected = expected
				if _, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity())); err == nil {
					t.Fatal("wrong expected thread accepted")
				}
				if want := wrongPass - 1; idleCalls != want {
					t.Fatalf("idle calls=%d want=%d", idleCalls, want)
				}
			})
		}
	}
}

func TestStrictAdmissionPreservesCodexProofContinuity(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name, expected string
		proofs         []string
		want           bool
	}{
		{name: "same", expected: thread, proofs: []string{"a", "a"}, want: true},
		{name: "changed", expected: thread, proofs: []string{"a", "b"}},
		{name: "empty_expected", expected: "", proofs: []string{"a", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proofCall := 0
			v := &strictSessionAdmissionVerifier{
				inst:   &session.Instance{ID: "instance", Tool: "codex", ProjectPath: "/project"},
				target: &tmux.Session{}, expected: tc.expected,
				deps: strictAdmissionDeps{
					native: func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
						return strictNativeThreadProof{Thread: thread}, nil
					},
					codex: func(tmux.StrictPaneIdentity, string, string) (string, error) {
						value := tc.proofs[proofCall]
						proofCall++
						return value, nil
					},
					idle: func(string, string, string, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
						return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
					},
				},
			}
			_, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity()))
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v err=%v", err == nil, err)
			}
		})
	}
}

func TestStrictProbeIsRegisteredThroughSessionDispatcher(t *testing.T) {
	dispatch := mustExtractFuncBody(t, "session_cmd.go", "handleSession")
	if !strings.Contains(dispatch, `case "strict-probe":`) || !strings.Contains(dispatch, "handleStrictSessionProbe(profile, args[1:])") {
		t.Fatal("session strict-probe is not registered through the exact session dispatcher")
	}
}

func TestStrictProbeArgumentsHaveNoMessageSurface(t *testing.T) {
	identifier := "caa5b90c-1788154378"
	for _, args := range [][]string{{identifier, "--json"}, {"--json", identifier}} {
		got, jsonOutput, err := parseStrictProbeArguments(args)
		if err != nil || got != identifier || !jsonOutput {
			t.Fatalf("args=%v got=%q json=%v err=%v", args, got, jsonOutput, err)
		}
	}
	for _, args := range [][]string{{}, {identifier, "message"}, {identifier, "--message", "text"},
		{identifier, "--message-file", "prompt"}, {identifier, "--expected-thread", "thread"}} {
		if _, _, err := parseStrictProbeArguments(args); err == nil {
			t.Fatalf("effect-capable or ambiguous args accepted: %v", args)
		}
	}
	if _, _, err := parseStrictProbeArguments([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help result=%v", err)
	}
}

func TestStrictProbeTargetRemainsClaudeOnlyOnDarwin(t *testing.T) {
	if !strictProbeTargetSupported("darwin", "arm64", "claude") {
		t.Fatal("Darwin Claude probe was disabled")
	}
	for _, tool := range []string{"codex", "shell", "gemini"} {
		if strictProbeTargetSupported("darwin", "arm64", tool) {
			t.Fatalf("Darwin %s row passed the Claude-only probe gate", tool)
		}
	}
}

func TestStrictTmux36BracketCompatibilityRequiresExactLinuxClaudeProof(t *testing.T) {
	live := tmux.StrictPaneIdentity{ServerVersion: "3.6"}
	if !strictBracketPasteAdmission("linux", "amd64", "claude", live, true) {
		t.Fatal("exact Linux Claude tmux 3.6 compatibility rejected")
	}
	for _, tc := range []struct {
		name, platform, architecture, tool, version, flag string
		durable                                           bool
	}{
		{"wrong_version", "linux", "amd64", "claude", "3.5", "", true},
		{"future_version", "linux", "amd64", "claude", "3.7", "", true},
		{"non_claude", "linux", "amd64", "shell", "3.6", "", true},
		{"linux_codex", "linux", "amd64", "codex", "3.6", "", true},
		{"wrong_arch", "linux", "arm64", "claude", "3.6", "", true},
		{"darwin", "darwin", "arm64", "claude", "3.6", "", true},
		{"weak_native", "linux", "amd64", "claude", "3.6", "", false},
		{"known_false", "linux", "amd64", "claude", "3.6", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tmux.StrictPaneIdentity{ServerVersion: tc.version, BracketPaste: tc.flag}
			if strictBracketPasteAdmission(tc.platform, tc.architecture, tc.tool, id, tc.durable) {
				t.Fatal("unverified bracket compatibility admitted")
			}
		})
	}
	known := tmux.StrictPaneIdentity{ServerVersion: "3.7b", BracketPaste: "1"}
	if !strictBracketPasteAdmission("darwin", "arm64", "codex", known, false) {
		t.Fatal("known Darwin bracket-paste evidence regressed")
	}
}

func TestStrictProbeDiagnosticsAreBoundedAndZeroTimeIsEmpty(t *testing.T) {
	if strictProbeTime(time.Time{}) != "" {
		t.Fatal("zero process start was exposed as a year-one timestamp")
	}
	want := "2026-09-08T18:28:28Z"
	if got := strictProbeTime(time.Date(2026, time.September, 8, 18, 28, 28, 0, time.UTC)); got != want {
		t.Fatalf("time=%q want=%q", got, want)
	}
	for value, wantSafe := range map[string]bool{
		"native_root_changed": true, "a": true, "": false, "Upper": false,
		"path/leak": false, "two words": false, strings.Repeat("a", 81): false,
	} {
		if got := safeStrictProbeReason(value); got != wantSafe {
			t.Fatalf("reason %q safe=%v want=%v", value, got, wantSafe)
		}
	}
	v := strictVerifierFixture(nil, func(int, session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
		t.Fatal("idle reader called after native root churn")
		return session.StrictSendIdleDecision{}
	})
	v.deps.native = func(*session.Instance, *tmux.Session, tmux.StrictPaneIdentity) (strictNativeThreadProof, error) {
		return strictNativeThreadProof{}, fmt.Errorf("native root changed")
	}
	if _, err := v.verify(strictVerifiedPaneIdentity()); err == nil || err.Error() != "native_root_changed" {
		t.Fatalf("root churn diagnostic=%v", err)
	}
}

func TestStrictAdmissionRefusesThirdVerifierPass(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	weak := strictNativeThreadProof{Thread: thread, Claude: strictClaudeNativeProof{Thread: thread, CWD: "/project"}}
	v := strictVerifierFixture([]strictNativeThreadProof{weak, weak, weak},
		func(_ int, _ session.StrictSendIdleEvidence) session.StrictSendIdleDecision {
			return session.StrictSendIdleDecision{Admitted: true, Reason: "fresh_hook"}
		})
	if _, err := runStrictAdmissionProbe(v, strictFixedProbeObservation(strictVerifiedPaneIdentity())); err != nil {
		t.Fatal(err)
	}
	if _, err := v.verify(strictVerifiedPaneIdentity()); err == nil {
		t.Fatal("third verifier pass accepted")
	}
}

func TestStrictProbeRegistryReadsNewestWALWithoutWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(name, "")
	}
	writer, err := session.NewStorageWithProfile("operator")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.GetDB().DB().Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.GetDB().DB().Exec(`INSERT INTO instances (id,title,project_path,tool,status,tmux_session,created_at) VALUES ('older-1','older','/older','claude','waiting','older-pane',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.GetDB().DB().Exec(`INSERT INTO instances (id,title,project_path,tool,status,tmux_session,created_at) VALUES ('caa5b90c-1788154378','director-claude','/project','claude','waiting','target-pane',2)`); err != nil {
		t.Fatal(err)
	}
	profileDir, err := session.GetProfileDir("operator")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(profileDir, "state.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("fixture lacks live WAL: %v", err)
	}
	before := fingerprintStrictRegistry(t, profileDir)
	inst, err := loadStrictProbeRegistryTarget("operator", "caa5b90c-1788154378")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != "caa5b90c-1788154378" || inst.Title != "director-claude" || inst.ProjectPath != "/project" {
		t.Fatalf("newest committed WAL row not loaded: %+v", inst)
	}
	after := fingerprintStrictRegistry(t, profileDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("probe registry read changed files:\nbefore=%+v\nafter=%+v", before, after)
	}
	for _, ambiguous := range []string{"director-claude", "caa5b90c"} {
		if _, err := loadStrictProbeRegistryTarget("operator", ambiguous); err == nil {
			t.Fatalf("non-exact registry selector accepted: %q", ambiguous)
		}
	}
	if _, err := loadStrictProbeRegistryTarget("missing-profile", "anything"); err == nil {
		t.Fatal("missing registry accepted")
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "agent-deck", "profiles", "missing-profile")); !os.IsNotExist(err) {
		t.Fatal("read-only registry lookup created missing profile")
	}
	if final := fingerprintStrictRegistry(t, profileDir); !reflect.DeepEqual(final, before) {
		t.Fatalf("refused probe registry reads changed files:\nbefore=%+v\nafter=%+v", before, final)
	}
}
