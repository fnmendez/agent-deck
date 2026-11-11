// [ui] flat_remotes: every remote session listed under one "remotes" header,
// at the root and sorted by name, instead of one "remotes/<name>" header per
// remote with that remote's groups below it. Children still nest under their
// parent (#2450). The flag is opt-in; off, the per-remote tree is unchanged.

package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func flatRemotesHome(flat bool) *Home {
	h := seamBNewHome()
	h.activeFilterExcludes = (session.DisplaySettings{}).GetActiveFilterExcludes()
	h.flatRemotes = flat
	h.remoteSessions = map[string][]session.RemoteSessionInfo{
		"causalert": {
			{ID: "c1", Title: "causalert-main", Group: "products", Status: "running"},
			{ID: "c2", Title: "causalert-wo1736-bot", Group: "products", Status: "waiting", ParentSessionID: "c1"},
		},
		"palapt": {
			{ID: "p1", Title: "palapt-main", Group: "projects", Status: "idle"},
			{ID: "p2", Title: "Alpha", Group: "projects/deep", Status: "stopped"},
		},
		"idis-genetec": {
			{ID: "i1", Title: "beta", Group: "", Status: "idle"},
			// Parent is not listed on this remote: falls back to the root.
			{ID: "i2", Title: "orphan", Group: "products", Status: "idle", ParentSessionID: "gone"},
		},
	}
	h.remoteGroups = map[string][]string{
		"causalert": {"products"},
		"palapt":    {"projects", "projects/deep"},
	}
	return h
}

type flatRemoteRow struct {
	typ    session.ItemType
	remote string
	title  string
	path   string
	level  int
	sub    bool
}

func flatRemoteRows(items []session.Item) []flatRemoteRow {
	var rows []flatRemoteRow
	for _, it := range items {
		switch it.Type {
		case session.ItemTypeRemoteGroup, session.ItemTypeRemoteSession:
		default:
			continue
		}
		r := flatRemoteRow{typ: it.Type, remote: it.RemoteName, path: it.Path, level: it.Level, sub: it.IsSubSession}
		if it.RemoteSession != nil {
			r.title = it.RemoteSession.Title
		}
		rows = append(rows, r)
	}
	return rows
}

// Flag off: the per-remote tree, pinned as literal rows (not rebuilt with the
// production builder) so a change to the default layout fails here.
func TestFlatRemotes_OffRendersPerRemoteTree(t *testing.T) {
	h := flatRemotesHome(false)
	h.rebuildFlatItems()

	rg, rs := session.ItemTypeRemoteGroup, session.ItemTypeRemoteSession
	want := []flatRemoteRow{
		{typ: rg, remote: "causalert", path: "remotes/causalert", level: 0},
		{typ: rg, remote: "causalert", path: "remotes/causalert/products", level: 1},
		{typ: rs, remote: "causalert", title: "causalert-main", path: "remotes/causalert/products", level: 2},
		{typ: rs, remote: "causalert", title: "causalert-wo1736-bot", path: "remotes/causalert/products", level: 3, sub: true},
		{typ: rg, remote: "idis-genetec", path: "remotes/idis-genetec", level: 0},
		{typ: rg, remote: "idis-genetec", path: "remotes/idis-genetec/my-sessions", level: 1},
		{typ: rs, remote: "idis-genetec", title: "beta", path: "remotes/idis-genetec/my-sessions", level: 2},
		{typ: rg, remote: "idis-genetec", path: "remotes/idis-genetec/products", level: 1},
		{typ: rs, remote: "idis-genetec", title: "orphan", path: "remotes/idis-genetec/products", level: 2},
		{typ: rg, remote: "palapt", path: "remotes/palapt", level: 0},
		{typ: rg, remote: "palapt", path: "remotes/palapt/projects", level: 1},
		{typ: rs, remote: "palapt", title: "palapt-main", path: "remotes/palapt/projects", level: 2},
		{typ: rg, remote: "palapt", path: "remotes/palapt/projects/deep", level: 2},
		{typ: rs, remote: "palapt", title: "Alpha", path: "remotes/palapt/projects/deep", level: 3},
	}
	if got := flatRemoteRows(h.flatItems); !reflect.DeepEqual(got, want) {
		t.Fatalf("flag off changed the remote rows:\n got  %+v\n want %+v", got, want)
	}
	var b strings.Builder
	h.renderRemoteGroupItem(&b, h.flatItems[0], false, 0)
	if got := plain(b.String()); !strings.HasPrefix(got, "  ▾ remotes/causalert (2) ● 1 ◐ 1") {
		t.Fatalf("flag off host header = %q", got)
	}
	if len(h.flatRemoteNames) != 0 || h.flatRemoteDupTitles != nil {
		t.Fatalf("flag off recorded flat state: names %v, shared titles %v", h.flatRemoteNames, h.flatRemoteDupTitles)
	}
}

// Flag on: one header, every remote's sessions merged and sorted by name,
// children under their parent, each row on its own remote.
func TestFlatRemotes_OnSingleHeaderSortedByName(t *testing.T) {
	h := flatRemotesHome(true)
	h.rebuildFlatItems()

	want := []flatRemoteRow{
		{typ: session.ItemTypeRemoteGroup, path: flatRemotesRootPath, level: 0},
		{typ: session.ItemTypeRemoteSession, remote: "palapt", title: "Alpha", path: flatRemotesRootPath, level: 1},
		{typ: session.ItemTypeRemoteSession, remote: "idis-genetec", title: "beta", path: flatRemotesRootPath, level: 1},
		{typ: session.ItemTypeRemoteSession, remote: "causalert", title: "causalert-main", path: flatRemotesRootPath, level: 1},
		{typ: session.ItemTypeRemoteSession, remote: "causalert", title: "causalert-wo1736-bot", path: flatRemotesRootPath, level: 2, sub: true},
		{typ: session.ItemTypeRemoteSession, remote: "idis-genetec", title: "orphan", path: flatRemotesRootPath, level: 1},
		{typ: session.ItemTypeRemoteSession, remote: "palapt", title: "palapt-main", path: flatRemotesRootPath, level: 1},
	}
	if got := flatRemoteRows(h.flatItems); !reflect.DeepEqual(got, want) {
		t.Fatalf("flat rows:\n got  %+v\n want %+v", got, want)
	}

	// Each row still points at its own remote's session, so actions resolve
	// the right remote and id.
	for _, it := range h.flatItems {
		if it.Type != session.ItemTypeRemoteSession {
			continue
		}
		found := false
		for _, rs := range h.remoteSessions[it.RemoteName] {
			if rs.ID == it.RemoteSession.ID && rs.Title == it.RemoteSession.Title {
				found = true
			}
		}
		if !found {
			t.Fatalf("row %q carries remote %q, which does not list it", it.RemoteSession.Title, it.RemoteName)
		}
	}

	// Tree connectors: the last root row closes the list, the child closes
	// its parent's subtree.
	last := h.flatItems[len(h.flatItems)-1]
	if !last.IsLastInGroup {
		t.Fatalf("last row %q not marked IsLastInGroup", last.RemoteSession.Title)
	}
	for _, it := range h.flatItems {
		if it.IsSubSession && !it.IsLastSubSession {
			t.Fatalf("only child %q not marked IsLastSubSession", it.RemoteSession.Title)
		}
	}

	counts := h.remoteHeaderCounts[flatRemotesRootPath]
	if counts.total != 6 || counts.running != 1 || counts.waiting != 1 {
		t.Fatalf("header counts = %+v, want total 6, running 1, waiting 1", counts)
	}
	var b strings.Builder
	h.renderRemoteGroupItem(&b, h.flatItems[0], false, 0)
	if line := plain(b.String()); !strings.Contains(line, "remotes (6)") || strings.Contains(line, "remotes/") {
		t.Fatalf("header line = %q, want a single \"remotes (6)\" header", line)
	}
	if got := len(h.flatRemoteNames); got != 3 {
		t.Fatalf("flatRemoteNames = %v, want all three remotes", h.flatRemoteNames)
	}
}

// A collapsed header is still emitted; its rows are withheld. Folding from a
// session row folds the header and parks the cursor on it.
func TestFlatRemotes_CollapsedHeaderWithholdsRows(t *testing.T) {
	h := flatRemotesHome(true)
	h.rebuildFlatItems()

	idx := -1
	for i, it := range h.flatItems {
		if it.Type == session.ItemTypeRemoteSession && it.RemoteSession.ID == "c2" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("child row missing before folding")
	}
	row := h.flatItems[idx]
	if !h.setRemoteGroupCollapsed(row.RemoteName, row.Path, true) {
		t.Fatalf("folding from a session row changed nothing")
	}

	rows := flatRemoteRows(h.flatItems)
	if len(rows) != 1 || rows[0].typ != session.ItemTypeRemoteGroup || rows[0].path != flatRemotesRootPath {
		t.Fatalf("collapsed rows = %+v, want the header alone", rows)
	}
	if !isFlatRemotesHeader(h.flatItems[h.cursor]) {
		t.Fatalf("cursor on %+v after folding, want the header", h.flatItems[h.cursor])
	}

	// Cursor identity survives a refresh.
	h.rebuildFlatItemsPreservingSelection(h.captureSelectedItemIdentity())
	if !isFlatRemotesHeader(h.flatItems[h.cursor]) {
		t.Fatalf("cursor left the header across a rebuild")
	}
}

// A filter that matches no remote session emits no header; one that matches
// some emits the header with only those rows.
func TestFlatRemotes_FilteredViewEmitsNoEmptyHeader(t *testing.T) {
	h := flatRemotesHome(true)
	h.statusFilter = FilterModeArchived
	h.rebuildFlatItems()
	if rows := flatRemoteRows(h.flatItems); len(rows) != 0 {
		t.Fatalf("archived view with no archived remote session emitted %+v", rows)
	}

	h = flatRemotesHome(true)
	for name, sessions := range h.remoteSessions {
		for i := range sessions {
			sessions[i].Status = "stopped"
		}
		h.remoteSessions[name] = sessions
	}
	h.statusFilter = FilterModeActive
	h.rebuildFlatItems()
	if rows := flatRemoteRows(h.flatItems); len(rows) != 0 {
		t.Fatalf("active filter with only stopped remote sessions emitted %+v", rows)
	}

	h = flatRemotesHome(true)
	h.statusFilter = session.StatusWaiting
	h.rebuildFlatItems()
	want := []flatRemoteRow{
		{typ: session.ItemTypeRemoteGroup, path: flatRemotesRootPath, level: 0},
		// The waiting child's parent is filtered out: it falls back to the root.
		{typ: session.ItemTypeRemoteSession, remote: "causalert", title: "causalert-wo1736-bot", path: flatRemotesRootPath, level: 1},
	}
	if got := flatRemoteRows(h.flatItems); !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting filter rows:\n got  %+v\n want %+v", got, want)
	}
}

// Manual reorder has no meaning in a name-sorted list: it reports so instead
// of storing an order that changes nothing.
func TestFlatRemotes_MoveIsRefused(t *testing.T) {
	h := flatRemotesHome(true)
	h.rebuildFlatItems()
	for _, it := range h.flatItems {
		if it.Type != session.ItemTypeRemoteSession {
			continue
		}
		if cmd := h.moveRemoteItem(it, 1); cmd != nil {
			t.Fatalf("move returned a command")
		}
		if len(h.remoteSessionOrder) != 0 {
			t.Fatalf("move stored an order overlay: %v", h.remoteSessionOrder)
		}
		return
	}
	t.Fatalf("no remote session row")
}

// The header's preview lists every remote instead of one host's panel.
func TestFlatRemotes_HeaderPreviewListsRemotes(t *testing.T) {
	h := flatRemotesHome(true)
	h.rebuildFlatItems()
	out := plain(h.renderRemotePreview(h.flatItems[0], 80, 30))
	for _, want := range []string{"Remotes", "causalert", "idis-genetec", "palapt"} {
		if !strings.Contains(out, want) {
			t.Fatalf("header preview missing %q:\n%s", want, out)
		}
	}
}

func flatRemoteKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func flatRemoteRowIndex(t *testing.T, h *Home, id string) int {
	t.Helper()
	for i, it := range h.flatItems {
		if it.Type == session.ItemTypeRemoteSession && it.RemoteSession != nil && it.RemoteSession.ID == id {
			return i
		}
	}
	t.Fatalf("no flat row %q", id)
	return -1
}

func flatRemoteRowText(h *Home, item session.Item, width int) string {
	var b strings.Builder
	h.renderRemoteSessionItemAtWidth(&b, item, false, width)
	return strings.SplitN(plain(b.String()), "\n", 2)[0]
}

// Every key on the header either acts on all remotes (R), folds (Enter,
// left), or says why a per-remote action does not apply; none is silent.
func TestFlatRemotes_HeaderKeys(t *testing.T) {
	for _, key := range []string{"n", "N", "g", "d", "u"} {
		t.Run(key, func(t *testing.T) {
			h := flatRemotesHome(true)
			h.rebuildFlatItems()
			h.cursor = 0
			_, cmd := h.handleMainKey(flatRemoteKey(key))
			if cmd != nil {
				t.Fatalf("%s on the header returned a command", key)
			}
			if h.err == nil || h.err.Error() != errFlatRemotesNoRemote.Error() {
				t.Fatalf("%s on the header: err = %v, want %v", key, h.err, errFlatRemotesNoRemote)
			}
			if h.pendingRemoteName != "" {
				t.Fatalf("%s on the header picked remote %q", key, h.pendingRemoteName)
			}
		})
	}

	t.Run("R skips remotes already polling", func(t *testing.T) {
		h := flatRemotesHome(true)
		h.rebuildFlatItems()
		h.cursor = 0
		h.remotePollActive = map[string]bool{"causalert": true, "palapt": true}
		_, cmd := h.handleMainKey(flatRemoteKey("R"))
		if cmd == nil {
			t.Fatalf("R with one idle remote returned no command")
		}
		if h.err != nil {
			t.Fatalf("R with one idle remote set %v", h.err)
		}
	})

	t.Run("R with every remote polling", func(t *testing.T) {
		h := flatRemotesHome(true)
		h.rebuildFlatItems()
		h.cursor = 0
		h.remotePollActive = map[string]bool{"causalert": true, "palapt": true, "idis-genetec": true}
		_, cmd := h.handleMainKey(flatRemoteKey("R"))
		if cmd != nil {
			t.Fatalf("R with every remote polling returned a command")
		}
		if h.err == nil || !strings.Contains(h.err.Error(), "already in progress") {
			t.Fatalf("R with every remote polling: err = %v", h.err)
		}
	})

	for _, key := range []string{"enter", "left"} {
		t.Run(key, func(t *testing.T) {
			h := flatRemotesHome(true)
			h.rebuildFlatItems()
			h.cursor = 0
			h.handleMainKey(flatRemoteKey(key))
			if !h.remoteGroupsCollapsed[flatRemotesRootPath] {
				t.Fatalf("%s did not fold the header", key)
			}
			if len(flatRemoteRows(h.flatItems)) != 1 || !isFlatRemotesHeader(h.flatItems[h.cursor]) {
				t.Fatalf("%s: rows %+v, cursor %d", key, flatRemoteRows(h.flatItems), h.cursor)
			}
		})
	}

	t.Run("left on a session row", func(t *testing.T) {
		h := flatRemotesHome(true)
		h.rebuildFlatItems()
		h.cursor = flatRemoteRowIndex(t, h, "p1")
		h.handleMainKey(flatRemoteKey("left"))
		if !h.remoteGroupsCollapsed[flatRemotesRootPath] || !isFlatRemotesHeader(h.flatItems[h.cursor]) {
			t.Fatalf("left on a row did not fold the header onto the cursor")
		}
	})
}

// A title two remotes share names the remote on both rows, in either case;
// a unique title shows the title alone.
func TestFlatRemotes_SharedTitleNamesRemote(t *testing.T) {
	h := flatRemotesHome(true)
	h.remoteSessions["idis-genetec"] = append(h.remoteSessions["idis-genetec"],
		session.RemoteSessionInfo{ID: "i3", Title: "Causalert-Main", Status: "idle"})
	h.rebuildFlatItems()

	main := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "c1")], 0)
	twin := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "i3")], 0)
	alone := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "p1")], 0)
	if !strings.Contains(main, "causalert-main · causalert") {
		t.Fatalf("shared title row = %q, want the remote named", main)
	}
	if !strings.Contains(twin, "Causalert-Main · idis-genetec") {
		t.Fatalf("shared title row = %q, want the remote named", twin)
	}
	if strings.Contains(alone, "·") || strings.Contains(alone, "palapt ") {
		t.Fatalf("unique title row = %q, want the title alone", alone)
	}

	// Embedded compact rail: the metadata line that names the remote is
	// hidden there, so the identity line names it for twins only.
	h.embeddedLayout, h.compactSidebar = true, true
	width := embeddedCardMinWidth + 20
	if got := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "i3")], width); !strings.Contains(got, "· idis-genetec") {
		t.Fatalf("embedded shared title row = %q, want the remote named", got)
	}
	if got := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "p1")], width); strings.Contains(got, "·") {
		t.Fatalf("embedded unique title row = %q, want the title alone", got)
	}

	// Flag off: no suffix, whatever the titles.
	h = flatRemotesHome(false)
	h.remoteSessions["idis-genetec"] = append(h.remoteSessions["idis-genetec"],
		session.RemoteSessionInfo{ID: "i3", Title: "Causalert-Main", Status: "idle"})
	h.rebuildFlatItems()
	if got := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "c1")], 0); strings.Contains(got, "·") {
		t.Fatalf("flag off row = %q, want no remote suffix", got)
	}
}

// "Active on top": live blocks (a parent moves with its children), a
// divider, then the rest, each part in name order, under the one header.
func TestFlatRemotes_ActiveOnTop(t *testing.T) {
	h := flatRemotesHome(true)
	h.groupViewMode = session.GroupViewActiveTop
	h.rebuildFlatItems()

	var got []string
	for _, it := range h.flatItems {
		switch {
		case it.Type == session.ItemTypeDivider:
			got = append(got, "---")
		case isFlatRemotesHeader(it):
			got = append(got, "[remotes]")
		case it.Type == session.ItemTypeRemoteGroup:
			got = append(got, "[other header "+it.Path+"]")
		case it.Type == session.ItemTypeRemoteSession:
			got = append(got, it.RemoteSession.Title)
		}
	}
	want := []string{"[remotes]", "causalert-main", "causalert-wo1736-bot", "---", "Alpha", "beta", "orphan", "palapt-main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active on top:\n got  %v\n want %v", got, want)
	}
}

// The time filter keeps only matching rows under the header, and no header
// when nothing matches.
func TestFlatRemotes_TimeFilter(t *testing.T) {
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	old := time.Now().AddDate(-1, 0, 0).UTC().Format(time.RFC3339Nano)
	stamp := func(h *Home, recentID string) {
		for name, sessions := range h.remoteSessions {
			for i := range sessions {
				sessions[i].LastActivityAt = old
				if sessions[i].ID == recentID {
					sessions[i].LastActivityAt = recent
				}
			}
			h.remoteSessions[name] = sessions
		}
	}

	h := flatRemotesHome(true)
	stamp(h, "p1")
	h.timeFilter = session.TimeFilterToday
	h.rebuildFlatItems()
	want := []flatRemoteRow{
		{typ: session.ItemTypeRemoteGroup, path: flatRemotesRootPath, level: 0},
		{typ: session.ItemTypeRemoteSession, remote: "palapt", title: "palapt-main", path: flatRemotesRootPath, level: 1},
	}
	if got := flatRemoteRows(h.flatItems); !reflect.DeepEqual(got, want) {
		t.Fatalf("time filter rows:\n got  %+v\n want %+v", got, want)
	}

	h = flatRemotesHome(true)
	stamp(h, "")
	h.timeFilter = session.TimeFilterToday
	h.keepEmptyFilter = true
	h.rebuildFlatItems()
	if rows := flatRemoteRows(h.flatItems); len(rows) != 0 {
		t.Fatalf("time filter with no match emitted %+v", rows)
	}
}

// An unavailable remote is counted on the header with the predicate that
// dims its rows, and drops out of the running/waiting glyphs; an outdated
// remote is counted too.
func TestFlatRemotes_HeaderCountsUnavailableAndOutdated(t *testing.T) {
	prev := Version
	Version = "1.16.38"
	defer func() { Version = prev }()

	h := flatRemotesHome(true)
	h.remotePolls = map[string]session.RemotePollState{
		"causalert":    {LastPollStatus: "error", LastPollError: "ssh: connect timeout"},
		"palapt":       {LastPollStatus: "ok"},
		"idis-genetec": {LastPollStatus: "ok"},
	}
	h.remoteVersions = map[string]session.RemoteVersionState{
		"palapt": {Version: "1.16.10", Found: true},
	}
	h.rebuildFlatItems()

	var b strings.Builder
	h.renderRemoteGroupItem(&b, h.flatItems[0], false, 0)
	line := plain(b.String())
	for _, want := range []string{"remotes (6)", "· 1 unavailable", "· 1 outdated"} {
		if !strings.Contains(line, want) {
			t.Fatalf("header = %q, want %q", line, want)
		}
	}
	if strings.Contains(line, "●") || strings.Contains(line, "◐") {
		t.Fatalf("header = %q, the unavailable remote's running/waiting still counted", line)
	}
	if row := flatRemoteRowText(h, h.flatItems[flatRemoteRowIndex(t, h, "c1")], 0); !strings.Contains(row, "last known") {
		t.Fatalf("row of the unavailable remote = %q, want it dimmed as last known", row)
	}

	// A startup cache snapshot is unavailable by the same predicate.
	h = flatRemotesHome(true)
	h.remoteFromCache = map[string]bool{"palapt": true}
	h.rebuildFlatItems()
	b.Reset()
	h.renderRemoteGroupItem(&b, h.flatItems[0], false, 0)
	if line := plain(b.String()); !strings.Contains(line, "· 1 unavailable") {
		t.Fatalf("header with a cached remote = %q", line)
	}
}

// A session row keeps the cursor across a refresh that inserts rows above it.
func TestFlatRemotes_SessionCursorSurvivesRefresh(t *testing.T) {
	h := flatRemotesHome(true)
	h.rebuildFlatItems()
	h.cursor = flatRemoteRowIndex(t, h, "p1")
	identity := h.captureSelectedItemIdentity()

	h.remoteSessions["idis-genetec"] = append(h.remoteSessions["idis-genetec"],
		session.RemoteSessionInfo{ID: "i9", Title: "aaa-first", Status: "idle"})
	h.rebuildFlatItemsPreservingSelection(identity)

	it := h.flatItems[h.cursor]
	if it.Type != session.ItemTypeRemoteSession || it.RemoteSession.ID != "p1" || it.RemoteName != "palapt" {
		t.Fatalf("cursor on %+v after refresh, want palapt-main", flatRemoteRows([]session.Item{it}))
	}
}

// SelectRemoteSession (session focus --remote) opens a folded flat header.
func TestFlatRemotes_SelectRemoteSessionUnfoldsHeader(t *testing.T) {
	h := flatRemotesHome(true)
	h.remoteGroupsCollapsed = map[string]bool{flatRemotesRootPath: true}
	h.rebuildFlatItems()
	if !h.SelectRemoteSession("causalert", "c2") {
		t.Fatalf("SelectRemoteSession did not select the row")
	}
	if h.remoteGroupsCollapsed[flatRemotesRootPath] {
		t.Fatalf("header still folded")
	}
	it := h.flatItems[h.cursor]
	if it.RemoteSession == nil || it.RemoteSession.ID != "c2" || it.RemoteName != "causalert" {
		t.Fatalf("cursor on %+v, want causalert-wo1736-bot", flatRemoteRows([]session.Item{it}))
	}
}
