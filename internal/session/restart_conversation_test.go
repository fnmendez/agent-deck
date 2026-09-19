package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Restart resolves the Claude conversation from project path + title (see
// restart_conversation.go). These tests pin the contract with real on-disk
// transcripts shaped like Claude 2.1.27x writes them.

const (
	convOld     = "11111111-1111-4111-8111-111111111111"
	convNew     = "22222222-2222-4222-8222-222222222222"
	convSibling = "33333333-3333-4333-8333-333333333333"
	convOther   = "44444444-4444-4444-8444-444444444444"
	convForced  = "55555555-5555-4555-8555-555555555555"
)

// restartTestEnv isolates HOME and CLAUDE_CONFIG_DIR in a directory on disk
// inside the package dir (not /tmp) and stubs every live-state seam.
type restartTestEnv struct {
	home, config, project string
	live                  []claudeProcessRecord
	alive                 map[string]bool // tmux session name -> alive
	dbPeers               []restartPeer
	dbPeersKnown          bool
}

func newRestartTestEnv(t *testing.T) *restartTestEnv {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(wd, ".restart-conv-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	env := &restartTestEnv{
		home:    filepath.Join(base, "home"),
		config:  filepath.Join(base, "home", ".claude"),
		project: filepath.Join(base, "work", "ops"),
		alive:   map[string]bool{},
	}
	for _, d := range []string{env.config, env.project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", env.home)
	t.Setenv("CLAUDE_CONFIG_DIR", env.config)
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	prevLive, prevAlive, prevDB := restartClaudeRecordsFn, restartTmuxSessionAliveFn, restartDBPeersFn
	restartClaudeRecordsFn = func([]string) []claudeProcessRecord { return env.live }
	restartTmuxSessionAliveFn = func(_ string, name string) bool { return env.alive[name] }
	restartDBPeersFn = func(*Instance) ([]restartPeer, bool) { return env.dbPeers, env.dbPeersKnown }
	t.Cleanup(func() {
		restartClaudeRecordsFn, restartTmuxSessionAliveFn, restartDBPeersFn = prevLive, prevAlive, prevDB
	})
	return env
}

// transcript writes <config>/projects/<slug>/<id>.jsonl in Claude's format:
// a user message carrying cwd/entrypoint, and (when title != "") the
// `custom-title` + `agent-name` entries `/rename <title>` appends.
func (e *restartTestEnv) transcript(t *testing.T, id, title, entrypoint string, age time.Duration) {
	t.Helper()
	dir := claudeProjectDirFor(e.config, e.project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"` + id + `"}` + "\n")
	msg, _ := json.Marshal(map[string]any{
		"parentUuid": nil, "isSidechain": false, "type": "user", "cwd": e.project,
		"entrypoint": entrypoint, "sessionId": id, "message": map[string]any{"role": "user", "content": "hi"},
	})
	b.Write(msg)
	b.WriteString("\n")
	if title != "" {
		b.WriteString(`{"type":"custom-title","customTitle":"` + title + `","sessionId":"` + id + `"}` + "\n")
		b.WriteString(`{"type":"agent-name","agentName":"` + title + `","sessionId":"` + id + `"}` + "\n")
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func (e *restartTestEnv) instance(id, title, persisted string) *Instance {
	inst := &Instance{
		ID:          id,
		Title:       title,
		Tool:        "claude",
		ProjectPath: e.project,
		tmuxSession: &tmux.Session{Name: "agentdeck_" + title + "_0000"},
	}
	inst.ClaudeSessionID = persisted
	inst.markClaudeSessionIDVerified()
	return inst
}

func (e *restartTestEnv) peer(title, persisted string, live bool) *Instance {
	p := e.instance("peer-"+title, title, persisted)
	e.alive[p.tmuxSession.Name] = live
	return p
}

func TestRestartConversationResolution(t *testing.T) {
	type setup func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance)
	cases := []struct {
		name      string
		setup     setup
		override  string
		wantID    string
		wantErr   bool
		wantInErr string
	}{
		{
			// The incident: a fleet cycle started convNew (renamed), the row
			// still says convOld.
			name: "stale row id resumes the current titled conversation",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				return e.instance("row-1", "director-localserver", convOld), nil
			},
			wantID: convNew,
		},
		{
			// A manual /clear: the new conversation is untitled and the row id
			// is stale. Sole session in its dir: the untitled one is current.
			name: "stale row id after manual clear resumes the newer untitled conversation",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "causalert-main", "cli", 3*time.Hour)
				e.transcript(t, convNew, "", "cli", time.Minute)
				self := e.instance("row-1", "causalert-main", convOld)
				return self, []*Instance{self}
			},
			wantID: convNew,
		},
		{
			// Same /clear in a shared dir: the dead pane's registry record
			// (rewritten by /clear) names the current conversation.
			name: "shared dir after clear resolves from this pane's last registry record",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
				e.transcript(t, convNew, "", "cli", 10*time.Minute)
				e.transcript(t, convSibling, "", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOld)
				e.live = []claudeProcessRecord{
					{PID: 9, SessionID: convOld, TmuxSession: self.tmuxSession.Name, Kind: "interactive", UpdatedAt: time.Now().Add(-3 * time.Hour)},
					{PID: 10, SessionID: convNew, TmuxSession: self.tmuxSession.Name, Kind: "interactive", UpdatedAt: time.Now().Add(-10 * time.Minute)},
					{PID: 11, SessionID: convSibling, TmuxSession: "agentdeck_ops-heartbeat_0000", Kind: "interactive", UpdatedAt: time.Now()},
				}
				return self, []*Instance{self, e.peer("ops-heartbeat", "", false)}
			},
			wantID: convNew,
		},
		{
			name: "shared dir after clear without pane evidence is refused",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
				e.transcript(t, convNew, "", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self, e.peer("ops-heartbeat", "", false)}
			},
			wantErr:   true,
			wantInErr: "untitled",
		},
		{
			name: "newest titled conversation held elsewhere is refused, never an older one",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				e.live = []claudeProcessRecord{{PID: 5, SessionID: convNew, TmuxSession: "other-pane", Kind: "interactive", Alive: true, UpdatedAt: time.Now()}}
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self}
			},
			wantErr: true,
		},
		{
			name: "untitled conversation last run by another pane is not a candidate",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convOther, "", "cli", time.Minute)
				e.live = []claudeProcessRecord{{PID: 5, SessionID: convOther, Kind: "interactive", UpdatedAt: time.Now()}}
				self := e.instance("row-1", "solo", "")
				return self, []*Instance{self}
			},
			wantID: convOld,
		},
		{
			name: "untitled fallback refused when another session's titled conversation is newer",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convOther, "renamed-elsewhere", "cli", time.Minute)
				self := e.instance("row-1", "solo", "")
				return self, []*Instance{self}
			},
			wantErr: true,
		},
		{
			name: "archived row in the same dir does not make it shared",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convNew, "", "cli", time.Minute)
				self := e.instance("row-1", "solo", "")
				archived := e.peer("old-row", "", false)
				archived.ArchivedAt = time.Now()
				return self, []*Instance{self, archived}
			},
			wantID: convNew,
		},
		{
			name:     "override held by another live session is refused",
			override: convSibling,
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self, e.peer("ops-heartbeat", convSibling, true)}
			},
			wantErr: true,
		},
		{
			// Two sessions in the same dir; the sibling wrote last.
			name: "rename title picks the right conversation among two sessions in one dir",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
				e.transcript(t, convNew, "director-localserver", "cli", 30*time.Minute)
				e.transcript(t, convSibling, "ops-heartbeat", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self, e.peer("ops-heartbeat", convSibling, true)}
			},
			wantID: convNew,
		},
		{
			name: "sibling restart picks its own titled conversation, not the newest",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convSibling, "ops-heartbeat", "cli", 2*time.Hour)
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				self := e.instance("row-2", "ops-heartbeat", "")
				return self, []*Instance{self, e.peer("director-localserver", convNew, true)}
			},
			wantID: convSibling,
		},
		{
			name: "untitled conversations in a shared dir are refused",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", 2*time.Hour)
				e.transcript(t, convSibling, "", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOther)
				return self, []*Instance{self, e.peer("ops-heartbeat", "", false)}
			},
			wantErr:   true,
			wantInErr: "shares this project directory",
		},
		{
			name: "only candidate is held by another live session: refused",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convSibling, "", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", "")
				return self, []*Instance{self, e.peer("ops-heartbeat", convSibling, true)}
			},
			wantErr: true,
		},
		{
			name: "live claude registry claim removes a candidate even with no peer row",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convSibling, "", "cli", time.Minute)
				e.live = []claudeProcessRecord{{PID: 42, SessionID: convSibling, TmuxSession: "someone-else", Kind: "interactive", Alive: true, UpdatedAt: time.Now()}}
				self := e.instance("row-1", "solo", "")
				return self, []*Instance{self}
			},
			wantID: convOld,
		},
		{
			name: "untitled sole session in its dir takes the newest (claude --continue)",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convNew, "", "cli", time.Minute)
				self := e.instance("row-1", "solo", convOld)
				return self, []*Instance{self}
			},
			wantID: convNew,
		},
		{
			name: "print-mode one-shot transcripts are never candidates",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Hour)
				e.transcript(t, convOther, "", "sdk-cli", time.Minute)
				self := e.instance("row-1", "solo", "")
				return self, []*Instance{self}
			},
			wantID: convOld,
		},
		{
			name: "own pane's live process wins over transcripts",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "director-localserver", "cli", time.Hour)
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convNew)
				e.live = []claudeProcessRecord{{PID: 7, SessionID: convOld, TmuxSession: self.tmuxSession.Name, Kind: "interactive", Alive: true, UpdatedAt: time.Now()}}
				return self, []*Instance{self}
			},
			wantID: convOld,
		},
		{
			name:     "explicit override wins over title resolution",
			override: convForced,
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self}
			},
			wantID: convForced,
		},
		{
			name:     "malformed override is rejected",
			override: "not-a-uuid; rm -rf /",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				return e.instance("row-1", "director-localserver", convOld), nil
			},
			wantErr: true,
		},
		{
			name: "explicit --session-id in the row command wins",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convNew, "director-localserver", "cli", time.Minute)
				self := e.instance("row-1", "director-localserver", convOld)
				self.Command = "claude --session-id " + convForced
				return self, []*Instance{self}
			},
			wantID: convForced,
		},
		{
			name: "no transcripts keeps the persisted id (legacy path)",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				self := e.instance("row-1", "director-localserver", convOld)
				return self, []*Instance{self}
			},
			wantID: convOld,
		},
		{
			name: "unknown peers make the untitled fallback strict",
			setup: func(t *testing.T, e *restartTestEnv) (*Instance, []*Instance) {
				e.transcript(t, convOld, "", "cli", time.Minute)
				return e.instance("row-1", "solo", ""), nil
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRestartTestEnv(t)
			self, all := tc.setup(t, e)
			if all != nil {
				self.SetRestartPeers(all)
			}
			if tc.override != "" {
				self.SetRestartClaudeSessionOverride(tc.override)
			}
			before := self.ClaudeSessionID
			err := self.resolveRestartClaudeConversation()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (resolved %q)", self.ClaudeSessionID)
				}
				if tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantInErr)
				}
				if self.ClaudeSessionID != before {
					t.Fatalf("refused resolution mutated the row id: %q -> %q", before, self.ClaudeSessionID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if self.ClaudeSessionID != tc.wantID {
				t.Fatalf("resolved %q, want %q", self.ClaudeSessionID, tc.wantID)
			}
			if self.restartClaudeSessionOverride != "" || self.restartPeersSet {
				t.Fatalf("one-shot restart inputs were not consumed")
			}
		})
	}
}

func TestRestartConversationAmbiguityIsTyped(t *testing.T) {
	e := newRestartTestEnv(t)
	e.transcript(t, convOld, "", "cli", time.Hour)
	e.transcript(t, convSibling, "", "cli", time.Minute)
	self := e.instance("row-1", "director-localserver", "")
	self.SetRestartPeers([]*Instance{self, e.peer("ops-heartbeat", "", false)})
	err := self.resolveRestartClaudeConversation()
	if !errors.Is(err, ErrRestartConversationAmbiguous) {
		t.Fatalf("want ErrRestartConversationAmbiguous, got %v", err)
	}
	if !strings.Contains(err.Error(), "--session-id") {
		t.Fatalf("error must name the escape hatch: %v", err)
	}
}

// The resolved id must replace the stale one in the persisted row and be
// resumable (the #1815 identity guard must accept it).
func TestRestartConversationUpdatesPersistedRow(t *testing.T) {
	e := newRestartTestEnv(t)
	db := withTempGlobalStateDB(t)
	e.transcript(t, convOld, "director-localserver", "cli", 3*time.Hour)
	e.transcript(t, convNew, "director-localserver", "cli", time.Minute)

	self := e.instance("row-persist", "director-localserver", convOld)
	if err := db.SaveInstance(&statedb.InstanceRow{
		ID: self.ID, Title: self.Title, ProjectPath: self.ProjectPath, Tool: "claude",
		Status: "idle", CreatedAt: time.Now(),
		ToolData: json.RawMessage(`{"claude_session_id":"` + convOld + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	self.SetRestartPeers([]*Instance{self})
	if err := self.resolveRestartClaudeConversation(); err != nil {
		t.Fatal(err)
	}
	if got := readClaudeSessionIDFromDB(t, db, self.ID); got != convNew {
		t.Fatalf("persisted claude_session_id = %q, want %q", got, convNew)
	}
	if self.ClaudeDetectedAt.IsZero() {
		t.Fatalf("ClaudeDetectedAt not stamped")
	}
	cmd := self.buildClaudeResumeCommand()
	if !strings.Contains(cmd, "--resume "+convNew) {
		t.Fatalf("resume command does not resume the resolved conversation: %s", cmd)
	}
}

func TestReadClaudeTranscriptMetaLastTitleWins(t *testing.T) {
	e := newRestartTestEnv(t)
	dir := claudeProjectDirFor(e.config, e.project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, convNew+".jsonl")
	body := `{"type":"user","cwd":"` + e.project + `","entrypoint":"cli","sessionId":"` + convNew + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"first","sessionId":"` + convNew + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"foreign","sessionId":"` + convOther + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"second","sessionId":"` + convNew + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"trunc` // mid-write, no newline
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := readClaudeTranscriptMeta(path, convNew, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "second" || meta.CWD != e.project || meta.Entrypoint != "cli" {
		t.Fatalf("meta = %+v", meta)
	}
}

// Restart() must run the resolution before touching the pane: an ambiguous
// resolution aborts with the error and nothing is spawned or killed.
func TestRestartRefusesAmbiguousConversationBeforeTouchingPane(t *testing.T) {
	e := newRestartTestEnv(t)
	e.transcript(t, convOld, "", "cli", time.Hour)
	e.transcript(t, convSibling, "", "cli", time.Minute)
	self := e.instance("row-restart-"+time.Now().Format("150405.000000"), "director-localserver", convOther)
	self.SetRestartPeers([]*Instance{self, e.peer("ops-heartbeat", "", false)})
	err := self.Restart()
	if !errors.Is(err, ErrRestartConversationAmbiguous) {
		t.Fatalf("Restart() = %v, want ErrRestartConversationAmbiguous", err)
	}
	if self.ClaudeSessionID != convOther {
		t.Fatalf("refused restart changed the row id to %q", self.ClaudeSessionID)
	}
}
