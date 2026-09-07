package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

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
	for _, tc := range []struct {
		name, status, thread string
		ts                   int64
		want                 bool
	}{
		{"idle", "waiting", "expected", time.Now().Unix(), true},
		{"busy", "running", "expected", time.Now().Unix(), false},
		{"wrong_thread", "waiting", "other", time.Now().Unix(), false},
		{"no_timestamp", "waiting", "expected", 0, false},
		{"future", "waiting", "expected", time.Now().Add(time.Hour).Unix(), false},
		{"stale", "waiting", "expected", time.Now().Add(-24 * time.Hour).Unix(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"status": tc.status, "session_id": tc.thread, "ts": tc.ts})
			if err := os.WriteFile(filepath.Join(dir, "strict-test.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			if got := session.StrictSendIdle("strict-test", "claude", "expected"); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
	if session.StrictSendIdle("missing", "claude", "expected") {
		t.Fatal("missing hook accepted")
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
