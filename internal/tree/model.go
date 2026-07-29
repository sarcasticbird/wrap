// Package tree renders the folders+git pane: the workspace's repos and
// worktrees with branch and dirty info inline, expandable to changed
// files. Selecting a row records it as the workspace's selection;
// selecting a changed file opens it in the diff pager.
package tree

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/pane"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

// Backend is what the tree pane needs from the launcher. launcher.Manager
// satisfies it.
type Backend interface {
	SwitchMiddle(target string) error
	KillEntrySession(name, targetID, targetGeneration, successor string) error
	Sessions() ([]tmux.SessionInfo, error)
	WriteSelection(string, state.Selection) error
	DetachUI() error
	ShutdownWorkspace() error
	ShowDiff(repoRoot, relPath string, staged, untracked bool) error
}

// Options carries the wiring NewModel needs beyond the backend.
type Options struct {
	WS, Root, RootName, Cmd, Note string
	Repos                         []gitx.Discovered
	Status                        func(string) (*gitx.Snapshot, error)
	Take                          func(string) (*gitx.Snapshot, error)
	Worktrees                     func(string) ([]gitx.Worktree, error)
	GitMetadata                   func(string) (bool, error)
}

type rowKind int

const (
	rowRoot rowKind = iota
	rowRepo         // repo or worktree — selectable, expandable
	rowFile         // read-only changed-file line
)

type row struct {
	kind    rowKind
	name    string
	path    string // dir (rowRoot/rowRepo); "" for rowFile
	session string // "" for rowFile
	// file fields
	status         byte
	added, deleted int
	staged         bool // true for rows from snap.Staged; drives ShowDiff's --cached
	untracked      bool
}

type repoGit struct {
	branch string
	dirty  int
	err    string
	snap   *gitx.Snapshot // populated while expanded
}

type gitMsg map[string]repoGit

// sessionsMsg is the 2s-tick payload: the raw session list plus the
// workspace's persisted selection (read alongside it so a row's
// "current" status reflects a selection made anywhere, not just via
// this pane's own selectRow). ok is false when the state read didn't
// find a selection (e.g. brand-new workspace) — current is left as-is
// rather than clobbered with "" in that case.
//
// err applies the same rule to the session list itself: a poll that
// could not run is not a poll that found nothing, and the two are
// otherwise indistinguishable empty slices.
type sessionsMsg struct {
	sessions     []tmux.SessionInfo
	current      string
	ok           bool
	err          error
	selectionErr error
}
type gitTickMsg struct{}
type sessionsTickMsg struct{}

type Model struct {
	pane.Nav
	backend              Backend
	ws                   string
	root                 string
	rootName             string
	defCmd               string
	note                 string
	repos                []gitx.Discovered // walker output + plain-repo worktree kids
	rootKids             []gitx.Discovered // root's own worktree siblings, when root is a repo
	expanded             map[string]bool   // dir path → show files
	git                  map[string]repoGit
	sessions             map[string]tmux.SessionInfo
	lastSeen             map[string]int64
	current              string
	info                 string // dim informational footer message; cleared on any ErrText or a successful switch
	stale                string // why the last session poll failed; "" once one succeeds again
	selectionStale       string // why the last selection read failed; sessions still update
	statusFn             func(string) (*gitx.Snapshot, error)
	takeFn               func(string) (*gitx.Snapshot, error)
	gitMetadataFn        func(string) (bool, error)
	sessionsPolling      bool
	gitPolling           bool
	sessionsTimerPending bool
	gitTimerPending      bool
}

var (
	rootStyle = lipgloss.NewStyle().Bold(true)
	addStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	delStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// NewModel seeds the repo list from o.Repos, inserting the sibling
// worktrees of any plain repo (via o.Worktrees) right after that repo.
func NewModel(b Backend, o Options) Model {
	gitMetadataFn := o.GitMetadata
	if gitMetadataFn == nil {
		gitMetadataFn = gitx.HasMetadata
	}
	m := Model{
		backend:       b,
		ws:            o.WS,
		root:          o.Root,
		rootName:      o.RootName,
		defCmd:        o.Cmd,
		note:          o.Note,
		expanded:      map[string]bool{},
		git:           map[string]repoGit{},
		sessions:      map[string]tmux.SessionInfo{},
		lastSeen:      map[string]int64{},
		statusFn:      o.Status,
		takeFn:        o.Take,
		gitMetadataFn: gitMetadataFn,
	}
	m.repos, m.rootKids, m.ErrText = entryTopology(o.Root, o.RootName, o.Repos, o.Worktrees)
	return m
}

func entryTopology(root, rootName string, repos []gitx.Discovered, worktreesFn func(string) ([]gitx.Worktree, error)) ([]gitx.Discovered, []gitx.Discovered, string) {
	expanded, errText := buildRepos(repos, worktreesFn)
	rootKids, rootErr := rootWorktreeKids(root, expanded, worktreesFn)
	if rootErr != nil {
		if errText != "" {
			errText += "; "
		}
		errText += fmt.Sprintf("worktrees %s: %v", rootName, rootErr)
	}
	expanded, rootKids, collisionErr := rejectNameCollisions(expanded, rootKids)
	if collisionErr != "" {
		if errText != "" {
			errText += "; "
		}
		errText += collisionErr
	}
	return expanded, rootKids, errText
}

// rejectNameCollisions removes every ambiguous entry rather than choosing
// one arbitrarily. An entry name is also its tmux session identity, so two
// different paths with the same name cannot safely coexist: whichever is
// selected second would attach to the first path's existing shell.
func rejectNameCollisions(repos, rootKids []gitx.Discovered) ([]gitx.Discovered, []gitx.Discovered, string) {
	paths := map[string][]string{}
	all := append(append([]gitx.Discovered(nil), repos...), rootKids...)
	for _, d := range all {
		paths[d.Name] = append(paths[d.Name], d.Path)
	}
	ambiguous := map[string]bool{}
	var errs []string
	for name, candidates := range paths {
		distinct := map[string]string{}
		for _, path := range candidates {
			distinct[canonPath(path)] = path
		}
		if len(distinct) < 2 {
			continue
		}
		ambiguous[name] = true
		display := make([]string, 0, len(distinct))
		for _, path := range distinct {
			display = append(display, path)
		}
		sort.Strings(display)
		errs = append(errs, fmt.Sprintf("duplicate entry name %q for paths %s; rename one directory", name, strings.Join(display, ", ")))
	}
	sort.Strings(errs)
	filter := func(in []gitx.Discovered) []gitx.Discovered {
		out := make([]gitx.Discovered, 0, len(in))
		for _, d := range in {
			if !ambiguous[d.Name] {
				out = append(out, d)
			}
		}
		return out
	}
	return filter(repos), filter(rootKids), strings.Join(errs, "; ")
}

func entryNames(root string, repos []gitx.Discovered, worktreesFn func(string) ([]gitx.Worktree, error)) ([]string, string) {
	expanded, rootKids, errText := entryTopology(root, filepath.Base(root), repos, worktreesFn)
	names := make([]string, 0, len(rootKids)+len(expanded))
	for _, d := range rootKids {
		names = append(names, d.Name)
	}
	for _, d := range expanded {
		names = append(names, d.Name)
	}
	return names, errText
}

// rootWorktreeKids discovers the root's own worktree siblings, exactly as
// buildRepos does for a plain repo below it. The discovery call is
// unconditional (mirroring buildRepos, which never checks whether a
// Discovered entry is "really" a repo either) rather than gated on the
// root's async git status: production's discoverWorktrees already
// returns nil for a non-repo root, so an umbrella root's kids come out
// empty for free. Whether to *render* them is a separate, later
// decision made in rows() once the root's git status confirms it's
// actually a repo.
//
// Unlike buildRepos's child-repo kids (named "repo/worktree"), the
// root's kids are named by bare basename, so their session becomes
// "<ws>/<basename>" — the root has no repo-name prefix to compose with.
//
// known lists the paths that already have a row (the root itself and
// everything buildRepos emitted). `git worktree list` reports the root's
// own main worktree, and a root worktree may also sit below the root where
// the walker already found it; either would otherwise produce a second row
// over a directory that already has one — and a second tmux session with
// it.
func rootWorktreeKids(root string, known []gitx.Discovered, worktreesFn func(string) ([]gitx.Worktree, error)) ([]gitx.Discovered, error) {
	if worktreesFn == nil {
		return nil, nil
	}
	wts, err := worktreesFn(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{canonPath(root): true}
	for _, d := range known {
		seen[canonPath(d.Path)] = true
	}
	var out []gitx.Discovered
	for _, w := range wts {
		key := canonPath(w.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, gitx.Discovered{
			Name: filepath.Base(w.Path),
			Path: w.Path,
			Kind: gitx.DiscoveredWorktree,
		})
	}
	return out, nil
}

// canonPath resolves symlinks so that paths from different sources compare
// equal. The walker builds paths with filepath.Abs, which does NOT resolve
// symlinks, while `git worktree list` reports fully resolved ones — so a
// workspace opened through a symlink produced two spellings of one
// directory, no dedupe key ever matched, and the duplicate rows and
// sessions came straight back. gitx.Take guards the same hazard for the
// same reason (macOS /var -> /private/var makes it routine, not exotic).
//
// An unresolvable path is returned as-is: a path that no longer exists
// cannot collide with a live one in a way that matters here.
func canonPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// buildRepos expands each plain repo with the worktrees the walker did not
// already find on its own, inserted immediately after the repo they belong
// to.
//
// One directory must produce exactly one row, because a row's name IS its
// tmux session name. Two things break that if left alone:
//
//   - `git worktree list` always reports the repo's own MAIN worktree, so
//     emitting a child for every entry gave every ordinary checkout a
//     duplicate row over the same directory.
//   - a linked worktree sitting beside its repo is itself discovered as a
//     repo (its .git is a file, not a directory), and `git worktree list`
//     returns BOTH from EITHER — so expanding each produced the full cross
//     product: 2 directories became 6 rows and 6 sessions.
//
// Tracking the paths already emitted collapses both: anything the walker
// surfaced keeps its own top-level row, and only worktrees living outside
// that set (nested, or beyond the workspace root) become children.
func buildRepos(repos []gitx.Discovered, worktreesFn func(string) ([]gitx.Worktree, error)) ([]gitx.Discovered, string) {
	seen := make(map[string]bool, len(repos))
	for _, d := range repos {
		seen[canonPath(d.Path)] = true
	}
	var out []gitx.Discovered
	var errs []string
	for _, d := range repos {
		out = append(out, d)
		if d.Kind != gitx.DiscoveredRepo || worktreesFn == nil {
			continue
		}
		wts, err := worktreesFn(d.Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("worktrees %s: %v", d.Name, err))
			continue
		}
		for _, w := range wts {
			key := canonPath(w.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			base := filepath.Base(w.Path)
			out = append(out, gitx.Discovered{
				Name: d.Name + "/" + base,
				Path: w.Path,
				Kind: gitx.DiscoveredWorktree,
			})
		}
	}
	return out, strings.Join(errs, "; ")
}

// rows renders the current flat row list: root first, then (when the
// root is itself a git repo) its own expanded file rows and worktree
// kids, then each child repo (with its worktree kids already
// interleaved in m.repos), with an expanded repo's changed files
// inserted directly after it.
//
// The root's file rows and worktree kids only ever appear once the
// root's async git status has confirmed it's a repo (rootGit.branch !=
// ""); a plain umbrella root shows neither, no matter what expanded or
// rootKids hold.
func (m Model) rows() []row {
	rows := []row{{kind: rowRoot, name: m.rootName, path: m.root, session: m.ws}}
	rootGit := m.git[m.root]
	rootIsRepo := rootGit.branch != ""
	if rootIsRepo && m.expanded[m.root] && rootGit.snap != nil {
		rows = append(rows, fileRows(rootGit.snap)...)
	}
	if rootIsRepo {
		for _, k := range m.rootKids {
			rows = append(rows, row{kind: rowRepo, name: k.Name, path: k.Path, session: config.SessionName(m.ws, k.Name)})
			if !m.expanded[k.Path] {
				continue
			}
			if g, ok := m.git[k.Path]; ok && g.snap != nil {
				rows = append(rows, fileRows(g.snap)...)
			}
		}
	}
	for _, d := range m.repos {
		session := config.SessionName(m.ws, d.Name)
		rows = append(rows, row{kind: rowRepo, name: d.Name, path: d.Path, session: session})
		if !m.expanded[d.Path] {
			continue
		}
		if g, ok := m.git[d.Path]; ok && g.snap != nil {
			rows = append(rows, fileRows(g.snap)...)
		}
	}
	return rows
}

func fileRows(snap *gitx.Snapshot) []row {
	var out []row
	for _, f := range snap.Staged {
		out = append(out, row{kind: rowFile, name: f.Path, status: f.Status, added: f.Added, deleted: f.Deleted, staged: true})
	}
	for _, f := range snap.Unstaged {
		out = append(out, row{kind: rowFile, name: f.Path, status: f.Status, added: f.Added, deleted: f.Deleted})
	}
	for _, u := range snap.Untracked {
		out = append(out, row{kind: rowFile, name: u, untracked: true})
	}
	return out
}

// parentOwnerIndex walks backward from idx to find the nearest rowRepo
// or rowRoot row — the owner of a file row at idx. A root that's itself
// a git repo can own file rows directly (rendered above any child
// repos), so it's an eligible owner alongside plain repo rows; child
// repos are always nearer to their own files than the root is, so
// existing repo-owned files are unaffected.
func parentOwnerIndex(rows []row, idx int) int {
	for i := idx; i >= 0; i-- {
		if rows[i].kind == rowRepo || rows[i].kind == rowRoot {
			return i
		}
	}
	return -1
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return sessionsTickMsg{} },
		func() tea.Msg { return gitTickMsg{} },
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case sessionsTickMsg:
		m.sessionsTimerPending = false
		if m.sessionsPolling {
			return m, nil
		}
		m.sessionsPolling = true
		return m, m.fetchSessions()
	case gitTickMsg:
		m.gitTimerPending = false
		return m.startGitPoll()
	case sessionsMsg:
		m.sessionsPolling = false
		// Same rule the state read below already follows: only trust a
		// poll that ran. Blanking the list on a transient tmux failure
		// would erase every session's 🔔 and "!" two seconds at a time.
		// m.stale keeps the held-over rows from passing as live ones.
		if msg.err != nil {
			m.stale = msg.err.Error()
			return m.scheduleSessionsTick()
		}
		m.stale = ""
		next := map[string]tmux.SessionInfo{}
		for _, s := range msg.sessions {
			next[s.Name] = s
			// First sighting baselines lastSeen so pre-existing
			// sessions don't all flash "!" on startup.
			if _, seen := m.lastSeen[s.Name]; !seen {
				m.lastSeen[s.Name] = s.Activity
			}
		}
		m.sessions = next
		// A state read can fail independently of this successful tmux
		// poll. Keep the last selection but still publish fresh session,
		// activity, and bell data.
		if msg.selectionErr != nil {
			m.selectionStale = msg.selectionErr.Error()
		} else {
			m.selectionStale = ""
			if msg.ok {
				m.current = msg.current
			}
		}
		if cur, ok := next[m.current]; ok {
			m.lastSeen[m.current] = cur.Activity
		}
		// Clear the "press n" hint once the terminal exists.
		if m.info != "" && m.current != "" {
			if _, ok := next[m.current]; ok {
				m.info = ""
			}
		}
		return m.scheduleSessionsTick()
	case gitMsg:
		m.gitPolling = false
		for path, g := range msg {
			m.git[path] = g
		}
		m.Cursor = m.clampCursor()
		return m.scheduleGitTick()
	case tea.MouseMsg:
		if m.ConfirmKill || m.ConfirmShutdown {
			return m, nil
		}
		return m.handleMouse(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) startGitPoll() (Model, tea.Cmd) {
	if m.gitPolling {
		return m, nil
	}
	m.gitPolling = true
	return m, m.fetchGit()
}

func (m Model) scheduleSessionsTick() (Model, tea.Cmd) {
	if m.sessionsTimerPending {
		return m, nil
	}
	m.sessionsTimerPending = true
	return m, nextSessionsTick()
}

func (m Model) scheduleGitTick() (Model, tea.Cmd) {
	if m.gitTimerPending {
		return m, nil
	}
	m.gitTimerPending = true
	return m, nextGitTick()
}

func nextSessionsTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return sessionsTickMsg{} })
}

func nextGitTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return gitTickMsg{} })
}

// handleMouse handles left-click (select only, no activation) and wheel
// (cursor step) mouse events. A left press never activates the row — only
// Enter does; this fixes click-to-focus accidentally switching terminals.
// Wheel events from bubbletea v1.3.10 report Action as its zero value
// (MouseActionPress) rather than a dedicated wheel action — confirmed in
// the vendored source (parseMouseButton in mouse.go): the motion bit is
// explicitly skipped for wheel buttons ("Motion bit doesn't get reported
// for wheel events") and the release-downgrade in parseSGRMouseEvent
// excludes IsWheel() too, so wheel Button codes are dispatched on here
// without gating on Action at all.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	start, end := m.viewport(rows)
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft &&
		(msg.Y < 0 || msg.Y >= end-start) {
		return m, nil
	}
	msg.Y += start
	m.HandleMouse(msg, len(rows))
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	if m.HandleKey(msg, m.backend, len(rows)) {
		return m, nil
	}
	switch msg.String() {
	case "h", "left":
		if m.Cursor < len(rows) {
			r := rows[m.Cursor]
			switch r.kind {
			case rowRepo:
				m.expanded[r.path] = false
			case rowRoot:
				// A no-op on an umbrella root (nothing was expanded), a
				// real collapse when the root is a git repo.
				m.expanded[r.path] = false
			case rowFile:
				if pi := parentOwnerIndex(rows, m.Cursor); pi >= 0 {
					m.expanded[rows[pi].path] = false
					m.Cursor = pi
				}
			}
			m.Cursor = m.clampCursor()
		}
	case "l", "right":
		if m.Cursor < len(rows) {
			r := rows[m.Cursor]
			switch r.kind {
			case rowRepo:
				m.expanded[r.path] = true
				return m.startGitPoll()
			case rowRoot:
				// Only expandable once git status has confirmed the root
				// is itself a repo; an umbrella root has nothing to show
				// and l stays a no-op.
				if m.git[r.path].branch != "" {
					m.expanded[r.path] = true
					return m.startGitPoll()
				}
			}
		}
	case "enter":
		if m.Cursor < len(rows) {
			return m.activate(rows, m.Cursor)
		}
	case "x":
		if m.Cursor < len(rows) {
			r := rows[m.Cursor]
			if r.session != "" {
				if info, ok := m.sessions[r.session]; ok {
					m.ArmKill(r.session, info.ID, info.Generation, m.successorSession(rows, m.Cursor))
				}
			}
		}
	}
	return m, nil
}

func (m Model) clampCursor() int {
	return m.ClampCursor(len(m.rows()))
}

// successorSession names the live session the terminal pane should fall to
// when rows[idx] is killed while on screen. The tree's rows are a mix of
// repos, worktrees and read-only file rows, so this scans outward for the
// nearest row that actually has a session — below first, then above.
func (m Model) successorSession(rows []row, idx int) string {
	alive := func(i int) string {
		s := rows[i].session
		if s == "" || i == idx {
			return ""
		}
		if _, ok := m.sessions[s]; !ok {
			return ""
		}
		return s
	}
	for i := idx + 1; i < len(rows); i++ {
		if s := alive(i); s != "" {
			return s
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if s := alive(i); s != "" {
			return s
		}
	}
	return ""
}

// activate handles Enter on a row (click never activates — see
// handleMouse). A file row opens its diff in the middle pane via the
// backend; anything else selects it (existing selectRow behavior).
func (m Model) activate(rows []row, idx int) (tea.Model, tea.Cmd) {
	r := rows[idx]
	if r.kind == rowFile {
		if err := m.showDiff(rows, idx, r); err != nil {
			m.ErrText = err.Error()
			m.info = ""
		} else {
			m.ErrText = ""
		}
		return m, nil
	}
	if err := m.selectRow(r); err != nil {
		m.ErrText = err.Error()
		m.info = ""
	} else {
		m.ErrText = ""
	}
	return m, nil
}

// showDiff resolves a file row's owning repo/root (walking back to the
// nearest rowRepo/rowRoot via parentOwnerIndex) and asks the backend to
// open its diff. A missing owner or a not-yet-statused owner (nil snap —
// e.g. git status hasn't come back yet) is a silent no-op, not an error:
// the row can't have been rendered as a file row without an owner whose
// snap produced it, but the guard keeps this safe against future
// refactors.
func (m Model) showDiff(rows []row, idx int, r row) error {
	oi := parentOwnerIndex(rows, idx)
	if oi < 0 {
		return nil
	}
	owner := rows[oi]
	ownerGit, ok := m.git[owner.path]
	if !ok || ownerGit.snap == nil {
		return nil
	}
	return m.backend.ShowDiff(ownerGit.snap.RepoRoot, r.name, r.staged, r.untracked)
}

// selectRow always persists the row as the workspace's selection. It only
// touches the middle pane (and m.current bookkeeping) when the row's
// session already exists: this pane never creates terminal sessions —
// that's the terms pane's job (`n`). A session-less selection is still
// recorded (so `n` in terms knows what to bind), with a dim info message
// telling the user how to get a terminal for it.
func (m *Model) selectRow(r row) error {
	sel := state.Selection{Entry: r.name, Session: r.session, Path: r.path}
	if err := m.backend.WriteSelection(m.ws, sel); err != nil {
		return err
	}
	if _, exists := m.sessions[r.session]; !exists {
		m.info = "no terminal — press n in terminals"
		return nil
	}
	if err := m.backend.SwitchMiddle(r.session); err != nil {
		// A session that existed at the last tick but dies before the
		// switch (e.g. a session command that exits at once) is a human-error case: tmux's
		// "can't find session" is cryptic, so name the real cause.
		if infos, ierr := m.backend.Sessions(); ierr == nil {
			alive := false
			for _, s := range infos {
				if s.Name == r.session {
					alive = true
					break
				}
			}
			if !alive {
				cmd := m.defCmd
				if cmd == "" {
					cmd = "the shell"
				}
				return fmt.Errorf("%s exited immediately — %s failed to start here (try running it manually)", r.session, cmd)
			}
		}
		return err
	}
	m.current = r.session
	m.info = ""
	if info, ok := m.sessions[r.session]; ok {
		m.lastSeen[r.session] = info.Activity
	}
	return nil
}

// fetchSessions snapshots the backend and workspace before returning the
// closure, so a later Update (which replaces m with a new value) can't
// race with this Cmd's goroutine.
func (m Model) fetchSessions() tea.Cmd {
	backend, ws := m.backend, m.ws
	return func() tea.Msg {
		infos, err := backend.Sessions()
		if err != nil {
			return sessionsMsg{err: err}
		}
		sel, ok, err := state.Read(ws)
		if err != nil {
			return sessionsMsg{sessions: infos, selectionErr: err}
		}
		return sessionsMsg{sessions: infos, current: sel.Session, ok: ok}
	}
}

// fetchGit snapshots the repo targets to poll now, then runs git in the
// returned Cmd so it never blocks the UI thread.
func (m Model) fetchGit() tea.Cmd {
	type target struct {
		path     string
		expanded bool
	}
	// The workspace root is always statused alongside the discovered
	// repos below it, plus its own worktree kids (if any) — the common
	// "wrap run inside a repo" case needs the root's own branch, dirty
	// count, and (once expanded) changed files, not just its children's.
	targets := []target{{path: m.root, expanded: m.expanded[m.root]}}
	for _, k := range m.rootKids {
		targets = append(targets, target{path: k.Path, expanded: m.expanded[k.Path]})
	}
	for _, d := range m.repos {
		targets = append(targets, target{path: d.Path, expanded: m.expanded[d.Path]})
	}
	statusFn, takeFn, metadataFn, root := m.statusFn, m.takeFn, m.gitMetadataFn, m.root
	return func() tea.Msg {
		out := gitMsg{}
		for _, t := range targets {
			var g repoGit
			if statusFn != nil {
				if snap, err := statusFn(t.path); err != nil {
					if t.path != root || !errors.Is(err, gitx.ErrNotARepo) {
						g.err = err.Error()
					} else if hasMetadata, metadataErr := metadataFn(t.path); metadataErr != nil {
						g.err = metadataErr.Error()
					} else if hasMetadata {
						g.err = err.Error()
					}
				} else {
					g.branch = snap.Branch
					g.dirty = uniqueDirty(snap)
				}
			}
			if t.expanded && takeFn != nil {
				if full, err := takeFn(t.path); err == nil {
					g.snap = full
				} else if g.err == "" {
					g.err = err.Error()
				}
			}
			out[t.path] = g
		}
		return out
	}
}

// uniqueDirty counts distinct changed paths across staged, unstaged, and
// untracked (a file can be both staged and unstaged at once).
func uniqueDirty(snap *gitx.Snapshot) int {
	seen := map[string]bool{}
	for _, f := range snap.Staged {
		seen[f.Path] = true
	}
	for _, f := range snap.Unstaged {
		seen[f.Path] = true
	}
	for _, u := range snap.Untracked {
		seen[u] = true
	}
	return len(seen)
}

func (m Model) totalDirty() int {
	total := 0
	for _, g := range m.git {
		total += g.dirty
	}
	return total
}

func (m Model) viewport(rows []row) (int, int) {
	capacity := len(rows)
	if m.Height > 0 {
		capacity = m.Height - 2 // rows, then the blank/footer area
		if capacity < 1 {
			capacity = 1
		}
	}
	anchor := m.Cursor
	if m.ConfirmKill {
		for i, r := range rows {
			if r.session == m.ConfirmTarget {
				anchor = i
				break
			}
		}
	}
	return pane.VisibleRange(len(rows), capacity, anchor)
}

func (m Model) View() string {
	var b strings.Builder
	rows := m.rows()
	start, end := m.viewport(rows)
	for i := start; i < end; i++ {
		r := rows[i]
		line := m.renderRow(r)
		// The row about to die outranks the cursor: with a confirmation
		// pending, that is the one thing worth looking at.
		switch {
		case m.IsKillTarget(r.session):
			line = pane.AlertStyle.Render(line)
		case i == m.Cursor:
			line = pane.CursorStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + m.footer())
	return b.String()
}

// rootBell surfaces a workspace-wide 🔔 on the root row when any session
// this workspace owns is ringing. The terminals monitor shows which one;
// the root row means you do not have to be looking at that pane, or have
// it expanded, to know something wants you.
func (m Model) rootBell() string {
	for name, info := range m.sessions {
		if info.Bell && config.SessionOwnedBy(m.ws, name) {
			return " 🔔"
		}
	}
	return ""
}

func (m Model) renderRow(r row) string {
	switch r.kind {
	case rowRoot:
		// A root whose git status came back with a branch is itself a
		// repo — render it like a repo row (arrow, branch, own dirty
		// count) but keep the bold name. A verified no-metadata
		// ErrNotARepo is the common umbrella case; other status failures
		// leave branch empty but remain visible as a warning.
		g := m.git[r.path]
		if g.branch != "" {
			arrow := "  "
			if g.dirty > 0 {
				if m.expanded[r.path] {
					arrow = "▾ "
				} else {
					arrow = "▸ "
				}
			}
			label := arrow + rootStyle.Render(pane.SafeLabel(r.name)) + m.rootBell()
			label += pane.DimStyle.Render(" ⎇" + pane.SafeLabel(g.branch))
			if g.dirty > 0 {
				label += pane.DimStyle.Render(fmt.Sprintf(" [%d]", g.dirty))
			}
			if g.err != "" {
				label += " " + pane.AlertStyle.Render("⚠")
			}
			return label
		}
		label := rootStyle.Render(pane.SafeLabel(r.name)) + m.rootBell()
		if n := m.totalDirty(); n > 0 {
			label += pane.DimStyle.Render(fmt.Sprintf(" [%d]", n))
		}
		if g.err != "" {
			label += " " + pane.AlertStyle.Render("⚠")
		}
		return label
	case rowRepo:
		marker := " "
		if info, ok := m.sessions[r.session]; ok {
			marker = "●"
			if info.Activity > m.lastSeen[r.session] && r.session != m.current {
				marker = pane.AlertStyle.Render("!")
			}
		}
		g := m.git[r.path]
		// The expand/collapse affordance only appears once there's
		// something to expand; a clean repo (or one git hasn't reported
		// on yet) gets a same-width blank prefix so columns still align.
		arrow := "  "
		if g.dirty > 0 {
			if m.expanded[r.path] {
				arrow = "▾ "
			} else {
				arrow = "▸ "
			}
		}
		label := arrow + marker + " " + pane.SafeLabel(r.name)
		if g.branch != "" {
			label += pane.DimStyle.Render(" ⎇" + pane.SafeLabel(g.branch))
		}
		if g.dirty > 0 {
			label += pane.DimStyle.Render(fmt.Sprintf(" [%d]", g.dirty))
		}
		if g.err != "" {
			label += " " + pane.AlertStyle.Render("⚠")
		}
		return label
	default: // rowFile
		width := m.Width
		if width == 0 {
			width = 80
		}
		status := r.status
		if r.untracked {
			status = '?'
		}
		line := fmt.Sprintf("   %c %s", status, truncate(pane.SafeLabel(r.name), width-12))
		if !r.untracked && (r.added > 0 || r.deleted > 0) {
			line += " " + addStyle.Render(fmt.Sprintf("+%d", r.added)) + " " + delStyle.Render(fmt.Sprintf("-%d", r.deleted))
		}
		return line
	}
}

func (m Model) footer() string {
	if s := m.ConfirmFooter(m.current); s != "" {
		return s
	}
	if m.stale != "" {
		return pane.AlertStyle.Render(" rows stale: " + pane.SafeLabel(m.stale) + " ")
	}
	if m.selectionStale != "" {
		return pane.AlertStyle.Render(" selection stale: " + pane.SafeLabel(m.selectionStale) + " ")
	}
	if m.info != "" {
		return pane.DimStyle.Render(" " + m.info + " ")
	}
	if m.note != "" {
		return pane.DimStyle.Render(" " + pane.SafeLabel(m.note) + " ")
	}
	return ""
}

// truncate shortens s to at most max runes, keeping the tail (the
// distinguishing part of a path) and prefixing an ellipsis. Rune-based
// (not byte-based) so multi-byte UTF-8 paths aren't sliced mid-rune.
func truncate(s string, max int) string {
	r := []rune(s)
	if max < 4 || len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max+1:])
}
