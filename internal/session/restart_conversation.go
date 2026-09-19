package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Restart resolves the conversation by project path + logical title, not by
// the persisted row id.
//
// Incident (2026-09-19): a fleet cycle (/clear + resume kickoff) or a manual
// /clear starts a NEW Claude conversation in the same pane. The row's
// persisted claude_session_id is not always rebound, so a later restart
// `--resume`d the OLD conversation — the session came back with context that
// was hours stale. Several sessions had to be repaired by hand with /resume
// during a tmux server switch.
//
// The persisted id is therefore only a hint. On every restart of a Claude row
// the conversation is resolved from what Claude itself records on disk:
//
//  1. An explicit override (`session restart --session-id <uuid>`, or a
//     `--session-id` / `--resume <uuid>` baked into the row's own command)
//     wins and nothing is resolved.
//  2. Claude's process registry (<config>/sessions/<pid>.json, kept after the
//     process exits and updated on /clear) names what THIS row's own tmux
//     session was last running: the live process, else the newest record.
//  3. Otherwise the transcripts under <config>/projects/<slug(project path)>/
//     are the candidates, restricted to interactive (`entrypoint: cli`)
//     transcripts whose first recorded `cwd` is this project path. The newest
//     candidate whose `/rename` title (the `custom-title` entry Claude appends
//     to the JSONL) equals the row's title wins — unless it is held by
//     another live session (refused, never an older one), or a newer
//     untitled conversation exists (/clear starts untitled): that one is
//     taken only when it is the persisted id or no other row shares the dir.
//  4. With no title match, only untitled candidates not held or last run by
//     another pane are eligible, and the newest must be newer than every
//     conversation titled for another session. It is taken only when no
//     other non-archived AgentDeck row shares the project directory, or when
//     it is the row's own persisted id, or when it is the only eligible
//     candidate and no sharing row has it persisted.
//  5. Anything else is ambiguous: the restart fails closed, before anything is
//     killed, and names the escape hatch. Guessing across sessions is worse
//     than not restarting, because the duplicate sweeper that runs after a
//     restart kills whichever other tmux session holds the same id.
//
// The resolved id is recorded as this row's own (vouched) conversation and
// persisted, so status and verification read the same id the pane resumes.

// restartTranscriptTailBytes bounds the tail read used to find the latest
// `custom-title` entry. Claude re-appends the entry every few turns, so the
// tail almost always holds it; the full file is scanned only when it does not.
const restartTranscriptTailBytes = 2 << 20

// restartTranscriptHeadLines bounds how far into a transcript the first
// `cwd` / `entrypoint` fields are searched for.
const restartTranscriptHeadLines = 400

// ErrRestartConversationAmbiguous is returned (wrapped) when restart cannot
// tell which conversation belongs to the row.
var ErrRestartConversationAmbiguous = errors.New("restart conversation is ambiguous")

// claudeTranscriptMeta is what restart needs to know about one transcript.
type claudeTranscriptMeta struct {
	ID         string
	ModTime    time.Time
	Title      string // last /rename title (custom-title), "" when never renamed
	CWD        string // first recorded cwd
	Entrypoint string // first recorded entrypoint ("cli" for interactive)
}

// claudeProcessRecord is one <config>/sessions/<pid>.json. Claude keeps the
// file after the process exits, and rewrites sessionId when /clear starts a
// new conversation, so the newest record of a pane names the conversation
// that pane was last running.
type claudeProcessRecord struct {
	PID         int
	SessionID   string
	TmuxSession string // tmux session name the process runs in, "" if none
	Kind        string // "interactive", "bg", ...
	CWD         string
	UpdatedAt   time.Time
	Alive       bool
}

// restartPaneRecord is a registry record of this row's own tmux session.
type restartPaneRecord struct {
	ID        string
	Alive     bool
	UpdatedAt time.Time
}

// restartPeer is another AgentDeck row, as far as restart resolution cares.
type restartPeer struct {
	ID              string
	Title           string
	WorkDir         string
	ClaudeSessionID string
	TmuxSession     string
	TmuxSocket      string
	Archived        bool
}

// restartConversationInput is everything the pure decision needs.
type restartConversationInput struct {
	Title       string
	PersistedID string
	Pane        []restartPaneRecord // registry records of this row's own tmux session
	Candidates  []claudeTranscriptMeta
	Claimed     map[string]string // conversation id -> live claimant description
	Foreign     map[string]bool   // last run by another (now dead) pane
	PeersShare  bool              // another row shares this project dir (or peers are unknown)
	PeersKnown  bool              // the peer list is authoritative
	PeerRefs    map[string]bool   // ids persisted on sharing rows, live or not
}

type restartConversationDecision struct {
	ID     string // "" means: nothing to resolve, keep the legacy path
	Reason string
}

// Test seams. Production reads the real registry, DB and tmux.
var (
	restartClaudeRecordsFn    = readClaudeProcessRecords
	restartTmuxSessionAliveFn = tmux.HasSessionOnSocket
	restartDBPeersFn          = loadRestartPeersFromDB
)

// SetRestartClaudeSessionOverride forces the next restart of this row to
// resume exactly this conversation id, bypassing path+title resolution. It is
// consumed by that restart.
func (i *Instance) SetRestartClaudeSessionOverride(id string) {
	i.restartClaudeSessionOverride = strings.TrimSpace(id)
}

// SetRestartPeers hands the next restart the caller's view of every AgentDeck
// row, used to tell whether another row shares this project directory and
// which conversations live rows own. Without it, restart falls back to the
// process-wide state DB, and without that it treats peers as unknown (which
// only makes untitled resolution stricter). Consumed by that restart.
func (i *Instance) SetRestartPeers(all []*Instance) {
	peers := make([]restartPeer, 0, len(all))
	for _, p := range all {
		if p == nil || p == i || p.ID == i.ID || !IsClaudeCompatible(p.Tool) {
			continue
		}
		name := ""
		if ts := p.GetTmuxSession(); ts != nil {
			name = ts.Name
		}
		peers = append(peers, restartPeer{
			ID:              p.ID,
			Title:           p.Title,
			WorkDir:         p.EffectiveWorkingDir(),
			ClaudeSessionID: p.ClaudeSessionID,
			TmuxSession:     name,
			TmuxSocket:      p.TmuxSocketName,
			Archived:        !p.ArchivedAt.IsZero(),
		})
	}
	i.restartPeers = peers
	i.restartPeersSet = true
}

// resolveRestartClaudeConversation runs before restart touches the pane. A
// non-nil error aborts the restart with nothing killed.
func (i *Instance) resolveRestartClaudeConversation() error {
	override := i.restartClaudeSessionOverride
	peers, peersSet := i.restartPeers, i.restartPeersSet
	i.clearRestartOneShots()

	if !IsClaudeCompatible(i.Tool) {
		if override != "" {
			return fmt.Errorf("--session-id applies to Claude sessions only (tool %q)", i.Tool)
		}
		return nil
	}
	if override != "" && !IsBareClaudeSessionUUID(override) {
		return fmt.Errorf("invalid Claude session id %q: expected a bare lowercase UUID", override)
	}
	if !i.TranscriptIsResolvableLocally() {
		if override != "" {
			i.applyResolvedClaudeSessionID(override, "restart_session_id_override")
		}
		return nil
	}

	workDir := i.EffectiveWorkingDir()
	configDir := GetClaudeConfigDirForInstance(i)
	if configDir == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	ownTmux := ""
	if i.tmuxSession != nil {
		ownTmux = i.tmuxSession.Name
	}

	// Evidence from Claude's own process registry: live processes elsewhere
	// claim their conversation; records of THIS pane (live or dead) name the
	// conversation it was last running (the record follows /clear); dead
	// records of other panes mark their conversation as foreign.
	claimed := map[string]string{}
	foreign := map[string]bool{}
	var pane []restartPaneRecord
	for _, rec := range restartClaudeRecordsFn(restartRegistryDirs(configDir)) {
		if rec.SessionID == "" {
			continue
		}
		if ownTmux != "" && rec.TmuxSession == ownTmux {
			if (rec.Kind == "" || rec.Kind == "interactive") && (rec.CWD == "" || samePath(rec.CWD, workDir)) {
				pane = append(pane, restartPaneRecord{ID: rec.SessionID, Alive: rec.Alive, UpdatedAt: rec.UpdatedAt})
			}
			continue
		}
		if !rec.Alive {
			foreign[rec.SessionID] = true
			continue
		}
		label := fmt.Sprintf("live claude pid %d", rec.PID)
		if rec.TmuxSession != "" {
			label += " in tmux session " + rec.TmuxSession
		}
		claimed[rec.SessionID] = label
	}

	if !peersSet {
		peers, peersSet = restartDBPeersFn(i)
	}
	peersShare := !peersSet // unknown peers: be strict
	peerRefs := map[string]bool{}
	slug := ConvertToClaudeDirName(resolvePathForSlug(workDir))
	for _, p := range peers {
		if p.ID == i.ID || p.Archived {
			continue
		}
		alive := p.TmuxSession != "" && restartTmuxSessionAliveFn(p.TmuxSocket, p.TmuxSession)
		// Any live row holding an id claims it, wherever it runs.
		if alive && p.ClaudeSessionID != "" {
			if _, ok := claimed[p.ClaudeSessionID]; !ok {
				claimed[p.ClaudeSessionID] = fmt.Sprintf("live AgentDeck session %q", p.Title)
			}
		}
		if strings.TrimSpace(p.WorkDir) == "" || ConvertToClaudeDirName(resolvePathForSlug(p.WorkDir)) != slug {
			continue
		}
		peersShare = true
		if p.ClaudeSessionID != "" {
			peerRefs[p.ClaudeSessionID] = true
		}
	}

	if override != "" {
		// The operator's choice wins over resolution, but never over another
		// live session: the post-restart duplicate sweep would kill it.
		if who, ok := claimed[override]; ok {
			return fmt.Errorf("restart %q: --session-id %s is held by %s", i.Title, override, who)
		}
		i.applyResolvedClaudeSessionID(override, "restart_session_id_override")
		return nil
	}
	// An explicit id baked into this row's own command is the operator's
	// declaration of ownership (#1147); it beats any disk resolution.
	if i.adoptExplicitClaudeSessionID("session_id_flag_explicit_restart") {
		return nil
	}
	if strings.TrimSpace(workDir) == "" {
		return nil
	}

	in := restartConversationInput{
		Title:       i.Title,
		PersistedID: i.ClaudeSessionID,
		Pane:        pane,
		Claimed:     claimed,
		Foreign:     foreign,
		PeersShare:  peersShare,
		PeersKnown:  peersSet,
		PeerRefs:    peerRefs,
	}
	// The pane evidence usually decides; scan transcripts only when needed.
	decision, decided, err := decideFromPane(in)
	if !decided {
		projectDir := claudeProjectDirFor(configDir, workDir)
		in.Candidates = scanClaudeProjectTranscripts(projectDir, workDir)
		decision, err = decideRestartConversation(in)
	}
	if err != nil {
		sessionLog.Warn("restart_conversation_refused",
			slog.String("instance_id", logging.SanitizeValue(i.ID)),
			slog.String("title", logging.SanitizeValue(i.Title)),
			slog.String("work_dir", logging.SanitizeValue(workDir)),
			slog.String("error", logging.SanitizeValue(err.Error())))
		return fmt.Errorf("restart %q: %w (choose one explicitly with: agent-deck session restart %q --session-id <uuid>)", i.Title, err, i.Title)
	}
	if decision.ID == "" {
		return nil
	}
	i.applyResolvedClaudeSessionID(decision.ID, decision.Reason)
	return nil
}

func (i *Instance) clearRestartOneShots() {
	i.restartClaudeSessionOverride = ""
	i.restartPeers, i.restartPeersSet = nil, false
}

// decideFromPane applies rule 2: what this row's own pane was last running.
// decided=false means the pane has no usable record.
func decideFromPane(in restartConversationInput) (restartConversationDecision, bool, error) {
	var live []string
	var newestDead *restartPaneRecord
	for k := range in.Pane {
		r := in.Pane[k]
		if r.Alive {
			live = appendUnique(live, r.ID)
			continue
		}
		if newestDead == nil || r.UpdatedAt.After(newestDead.UpdatedAt) {
			newestDead = &r
		}
	}
	pick, reason := "", ""
	switch {
	case len(live) > 1:
		return restartConversationDecision{}, true, fmt.Errorf("%w: %d live Claude conversations in this session's pane (%s)", ErrRestartConversationAmbiguous, len(live), strings.Join(live, ", "))
	case len(live) == 1:
		pick, reason = live[0], "restart_own_pane_live"
	case newestDead != nil:
		pick, reason = newestDead.ID, "restart_own_pane_last_record"
	default:
		return restartConversationDecision{}, false, nil
	}
	if who, ok := in.Claimed[pick]; ok {
		return restartConversationDecision{}, true, fmt.Errorf("%w: conversation %s last run in this session's pane is now held by %s", ErrRestartConversationAmbiguous, pick, who)
	}
	return restartConversationDecision{ID: pick, Reason: reason}, true, nil
}

// decideRestartConversation applies rules 3-5 (see file doc) over the
// transcripts of the project directory. The pane rule is decideFromPane.
func decideRestartConversation(in restartConversationInput) (restartConversationDecision, error) {
	if d, ok, err := decideFromPane(in); ok {
		return d, err
	}
	if len(in.Candidates) == 0 {
		return restartConversationDecision{}, nil
	}
	cands := append([]claudeTranscriptMeta(nil), in.Candidates...)
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].ModTime.After(cands[b].ModTime) })

	title := strings.TrimSpace(in.Title)
	var ours, untitled, others []claudeTranscriptMeta
	for _, c := range cands {
		switch {
		case title != "" && c.Title == title:
			ours = append(ours, c)
		case c.Title == "":
			// Held live elsewhere, or last run in another pane: not ours.
			if _, ok := in.Claimed[c.ID]; ok || in.Foreign[c.ID] {
				continue
			}
			untitled = append(untitled, c)
		default:
			others = append(others, c)
		}
	}
	ambiguous := func(format string, args ...any) (restartConversationDecision, error) {
		return restartConversationDecision{}, fmt.Errorf("%w: "+format, append([]any{ErrRestartConversationAmbiguous}, args...)...)
	}
	persistedIs := func(id string) bool {
		return in.PersistedID != "" && claudeSessionIDsMatch(in.PersistedID, id)
	}

	// Rule 3: the newest conversation carrying this session's /rename title.
	if len(ours) > 0 {
		best := ours[0]
		// Never fall back to an OLDER titled conversation: if the newest one
		// is open elsewhere, a human has to decide.
		if who, ok := in.Claimed[best.ID]; ok {
			return ambiguous("the newest conversation titled %q (%s) is held by %s", title, best.ID, who)
		}
		// /clear starts an untitled conversation; until /rename runs, the
		// session's CURRENT conversation is newer than its titled one.
		var newer []claudeTranscriptMeta
		for _, u := range untitled {
			if u.ModTime.After(best.ModTime) {
				newer = append(newer, u)
			}
		}
		switch {
		case len(newer) == 0:
			return restartConversationDecision{ID: best.ID, Reason: "restart_title_match"}, nil
		case persistedIs(newer[0].ID):
			return restartConversationDecision{ID: newer[0].ID, Reason: "restart_persisted_newer_untitled"}, nil
		case !in.PeersShare && newerThanAll(newer[0], others):
			return restartConversationDecision{ID: newer[0].ID, Reason: "restart_newest_untitled_after_title"}, nil
		}
		return ambiguous("the newest conversation titled %q (%s) is older than %d untitled conversation(s) (newest %s) that another session in this project directory could own", title, best.ID, len(newer), newer[0].ID)
	}

	// Rule 4: nothing carries this title.
	if len(untitled) == 0 {
		return ambiguous("no conversation in this project directory is titled %q, and every other one belongs to another session", title)
	}
	newest := untitled[0]
	if !newerThanAll(newest, others) {
		return ambiguous("no conversation is titled %q and the newest one belongs to another session (titled %q)", title, others[0].Title)
	}
	switch {
	case persistedIs(newest.ID):
		return restartConversationDecision{ID: newest.ID, Reason: "restart_persisted_is_newest_untitled"}, nil
	case !in.PeersShare:
		return restartConversationDecision{ID: newest.ID, Reason: "restart_newest_untitled_sole_session"}, nil
	case in.PeersKnown && len(untitled) == 1 && !in.PeerRefs[newest.ID]:
		// The only eligible candidate, and no sharing row has it persisted.
		return restartConversationDecision{ID: newest.ID, Reason: "restart_only_untitled_candidate"}, nil
	}
	ids := make([]string, 0, 4)
	for k, u := range untitled {
		if k == 3 {
			ids = append(ids, "...")
			break
		}
		ids = append(ids, u.ID)
	}
	return ambiguous("no conversation is titled %q, another AgentDeck session shares this project directory, and %d untitled conversations could be this session's (%s)", title, len(untitled), strings.Join(ids, ", "))
}

// newerThanAll reports whether c is newer than every conversation in others.
func newerThanAll(c claudeTranscriptMeta, others []claudeTranscriptMeta) bool {
	for _, o := range others {
		if !c.ModTime.After(o.ModTime) {
			return false
		}
	}
	return true
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return filepath.Clean(resolvePathForSlug(a)) == filepath.Clean(resolvePathForSlug(b))
}

// applyResolvedClaudeSessionID records id as this row's own conversation and
// persists it when it changed.
func (i *Instance) applyResolvedClaudeSessionID(id, reason string) {
	old := i.ClaudeSessionID
	i.ClaudeSessionID = id
	// The id was resolved from this row's own identity (title, own pane, or
	// operator override), so it is ownership, not a disk-scan hint (#1815).
	i.markClaudeSessionIDVerified()
	changed := old != id
	if changed || i.ClaudeDetectedAt.IsZero() {
		i.ClaudeDetectedAt = time.Now()
	}
	safeID := logging.SanitizeValue(id)
	sessionLog.Info("resume: id="+safeID+" reason="+reason,
		slog.String("instance_id", logging.SanitizeValue(i.ID)),
		slog.String("title", logging.SanitizeValue(i.Title)),
		slog.String("claude_session_id", safeID),
		slog.String("previous_session_id", logging.SanitizeValue(old)),
		slog.String("reason", reason))
	if !changed {
		return
	}
	if db := statedb.GetGlobal(); db != nil {
		if err := db.WriteClaudeSessionBinding(i.ID, id, i.ClaudeDetectedAt); err != nil {
			sessionLog.Warn("restart_conversation_persist_failed",
				slog.String("instance_id", logging.SanitizeValue(i.ID)),
				slog.String("error", logging.SanitizeValue(err.Error())))
		}
	}
}

func resolvePathForSlug(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func claudeProjectDirFor(configDir, workDir string) string {
	encoded := ConvertToClaudeDirName(resolvePathForSlug(workDir))
	if encoded == "" {
		encoded = "-"
	}
	return filepath.Join(configDir, "projects", encoded)
}

// scanClaudeProjectTranscripts lists the resumable interactive transcripts of
// projectDir that were started in workDir. Unreadable files are skipped.
func scanClaudeProjectTranscripts(projectDir, workDir string) []claudeTranscriptMeta {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil
	}
	want := map[string]bool{filepath.Clean(workDir): true}
	want[filepath.Clean(resolvePathForSlug(workDir))] = true

	var out []claudeTranscriptMeta
	for _, e := range entries {
		if e.IsDir() || !uuidSessionFileRegex.MatchString(e.Name()) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		meta, merr := readClaudeTranscriptMeta(filepath.Join(projectDir, e.Name()), id, info.Size())
		if merr != nil {
			continue
		}
		meta.ModTime = info.ModTime()
		// No cwd means no message was ever exchanged: nothing to resume.
		if meta.CWD == "" {
			continue
		}
		// The slug encoding is lossy ("/", "." and "%" all become "-"), so
		// the transcript's own cwd is what proves it belongs to workDir.
		if !want[filepath.Clean(meta.CWD)] && !want[filepath.Clean(resolvePathForSlug(meta.CWD))] {
			continue
		}
		// `claude -p` one-shots record entrypoint "sdk-cli"; only an
		// interactive conversation can be the session's.
		if meta.Entrypoint != "" && meta.Entrypoint != "cli" {
			continue
		}
		out = append(out, meta)
	}
	return out
}

type transcriptLine struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	SessionID   string `json:"sessionId"`
	CWD         string `json:"cwd"`
	Entrypoint  string `json:"entrypoint"`
}

var (
	customTitleMarker = []byte(`"type":"custom-title"`)
	cwdMarker         = []byte(`"cwd":"`)
)

// readClaudeTranscriptMeta extracts the first cwd/entrypoint and the last
// /rename title of one transcript.
func readClaudeTranscriptMeta(path, id string, size int64) (claudeTranscriptMeta, error) {
	meta := claudeTranscriptMeta{ID: id}
	f, err := os.Open(path) // #nosec G304 -- path is a UUID-named file under the Claude projects dir
	if err != nil {
		return meta, err
	}
	defer f.Close()

	// Head: first cwd + entrypoint.
	r := bufio.NewReaderSize(f, 64<<10)
	for n := 0; n < restartTranscriptHeadLines && meta.CWD == ""; n++ {
		line, rerr := r.ReadBytes('\n')
		if bytes.Contains(line, cwdMarker) {
			var tl transcriptLine
			if json.Unmarshal(bytes.TrimSpace(line), &tl) == nil && tl.CWD != "" {
				meta.CWD = tl.CWD
				meta.Entrypoint = tl.Entrypoint
			}
		}
		if rerr != nil {
			break
		}
	}

	// Title: the last custom-title entry for this conversation. A truncated
	// last line (the session is mid-write) simply fails to parse.
	lastTitle := func(rd io.Reader, skipFirst bool) (string, bool) {
		br := bufio.NewReaderSize(rd, 64<<10)
		title, found := "", false
		first := true
		for {
			line, rerr := br.ReadBytes('\n')
			// After a mid-file seek the first line is partial; skip it.
			skip := first && skipFirst
			first = false
			if !skip && bytes.Contains(line, customTitleMarker) {
				var tl transcriptLine
				if json.Unmarshal(bytes.TrimSpace(line), &tl) == nil && tl.Type == "custom-title" &&
					(tl.SessionID == "" || tl.SessionID == id) {
					title, found = strings.TrimSpace(tl.CustomTitle), true
				}
			}
			if rerr != nil {
				return title, found
			}
		}
	}
	if size > restartTranscriptTailBytes {
		if _, err := f.Seek(size-restartTranscriptTailBytes, io.SeekStart); err == nil {
			if t, ok := lastTitle(io.LimitReader(f, restartTranscriptTailBytes), true); ok {
				meta.Title = t
				return meta, nil
			}
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return meta, err
	}
	meta.Title, _ = lastTitle(io.LimitReader(f, size), false)
	return meta, nil
}

// restartRegistryDirs lists the config dirs whose sessions/ registry may hold
// a live process for this project: the row's resolved dir plus the global
// and default ones (a worker scratch dir or a shell-level CLAUDE_CONFIG_DIR
// can differ from the resolved one). More registries only add claims.
func restartRegistryDirs(configDir string) []string {
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		d = filepath.Clean(d)
		for _, x := range dirs {
			if x == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	add(configDir)
	add(GetClaudeConfigDir())
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude"))
	}
	return dirs
}

type claudeProcessRecordFile struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Tmux      string `json:"tmux"`
	Kind      string `json:"kind"`
	CWD       string `json:"cwd"`
	ProcStart string `json:"procStart"`
	UpdatedAt int64  `json:"updatedAt"`
}

// readClaudeProcessRecords reads <dir>/sessions/*.json for every dir. A record
// is Alive only when its pid runs AND (on Linux) has the recorded start time,
// so a reused pid does not count.
func readClaudeProcessRecords(dirs []string) []claudeProcessRecord {
	var out []claudeProcessRecord
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
		for _, m := range matches {
			data, err := os.ReadFile(m) // #nosec G304 -- Claude's own session registry
			if err != nil {
				continue
			}
			var rec claudeProcessRecordFile
			if json.Unmarshal(data, &rec) != nil || rec.PID <= 0 || rec.SessionID == "" {
				continue
			}
			tmuxName := rec.Tmux
			if k := strings.IndexByte(tmuxName, ':'); k >= 0 {
				tmuxName = tmuxName[:k]
			}
			updated := time.UnixMilli(rec.UpdatedAt)
			if rec.UpdatedAt == 0 {
				if info, ierr := os.Stat(m); ierr == nil {
					updated = info.ModTime()
				}
			}
			out = append(out, claudeProcessRecord{
				PID: rec.PID, SessionID: rec.SessionID, TmuxSession: tmuxName, Kind: rec.Kind,
				CWD: rec.CWD, UpdatedAt: updated, Alive: processAliveWithStart(rec.PID, rec.ProcStart),
			})
		}
	}
	return out
}

func processAliveWithStart(pid int, procStart string) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		if procStart == "" {
			return true
		}
		// Field 22 (starttime) counted after the ")" that ends comm.
		s := string(data)
		k := strings.LastIndexByte(s, ')')
		if k < 0 {
			return true
		}
		fields := strings.Fields(s[k+1:])
		const startIdx = 22 - 3
		if len(fields) <= startIdx {
			return true
		}
		return fields[startIdx] == procStart
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// loadRestartPeersFromDB builds the peer list from the process-wide state DB.
// The second result is false when no DB is available (peers unknown).
func loadRestartPeersFromDB(self *Instance) ([]restartPeer, bool) {
	db := statedb.GetGlobal()
	if db == nil {
		return nil, false
	}
	rows, err := db.LoadInstances()
	if err != nil {
		return nil, false
	}
	peers := make([]restartPeer, 0, len(rows))
	for _, r := range rows {
		if r == nil || r.ID == self.ID || !IsClaudeCompatible(r.Tool) {
			continue
		}
		var td struct {
			ClaudeSessionID string `json:"claude_session_id"`
		}
		if len(r.ToolData) > 0 {
			_ = json.Unmarshal(r.ToolData, &td)
		}
		peers = append(peers, restartPeer{
			ID:              r.ID,
			Title:           r.Title,
			WorkDir:         r.ProjectPath,
			ClaudeSessionID: td.ClaudeSessionID,
			TmuxSession:     r.TmuxSession,
			TmuxSocket:      r.TmuxSocketName,
			Archived:        !r.ArchivedAt.IsZero(),
		})
	}
	return peers, true
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
