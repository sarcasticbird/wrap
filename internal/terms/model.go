// Package terms renders the terminals-monitor pane: every tmux session
// owned by the current workspace (the root session, its named entries,
// and any free `<ws>·term·N` terminals), kept in creation order.
package terms

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/launcher"
	mirrorapi "github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/pane"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

// Backend is what the terms pane needs from the launcher. launcher.Manager
// satisfies it.
type Backend interface {
	Sessions() ([]tmux.SessionInfo, error)
	DisplayedSession() (string, error)
	SessionCurrentPath(id, generation string) (string, error)
	ShowInMiddle(target string) error
	KillScratchSession(name, targetID, targetGeneration, successor string) error
	NewTerm(dir, cmd string) (string, error)
	RenameTerm(oldName, targetID, targetGeneration, label string) (string, error)
	EnsureEntrySession(name, dir, cmd string) error
	DetachUI() error
	ShutdownWorkspace() error
	SetWorkspaceAlert(alert bool) error
	RingWorkspaceAlert() error
}

var _ Backend = (*launcher.Manager)(nil)

// scratchPaneBackend adapts pane.Nav's shared confirmation handler to the
// terminals pane's stricter lifecycle boundary. The tree passes its backend
// directly and retains KillEntrySession; pane 3 can only confirm scratch kills.
type scratchPaneBackend struct{ Backend }

func (b scratchPaneBackend) KillEntrySession(name, targetID, targetGeneration, successor string) error {
	return b.KillScratchSession(name, targetID, targetGeneration, successor)
}

// Options carries the wiring NewModel needs beyond the backend.
type Options struct {
	WS, Root, Cmd string
	Keys          config.Keys
	Mirrors       Mirror
}

type Mirror interface {
	Events() <-chan mirrorapi.Event
	Snapshot() mirrorapi.Snapshot
	Mirror(context.Context, mirrorapi.HostSession) error
	Revoke(context.Context, mirrorapi.Identity) error
	Rotate(context.Context) error
	Reconcile(context.Context, []mirrorapi.HostSession) error
}

// row is a single ws-owned session line.
type row struct {
	id         string
	generation string
	name       string
	kind       string
	created    int64
	bell       bool
	activity   bool
	mirrored   bool
	current    bool
	key        sessionKey
	expanded   bool
	path       pathState
}

type sessionKey struct {
	generation string
	id         string
}

type pathResult struct {
	path string
	err  error
}

type pathState struct {
	value string
	valid bool
	stale bool
}

// rowsMsg is the 2s-tick payload: the raw session list plus the
// workspace's current selection, fetched together so a row's "current"
// flag and its activity baseline are always consistent with each other.
//
// err distinguishes "the poll could not run" from "there are no
// sessions", which are otherwise the same empty slice. They must not be
// treated alike: see the handler in Update.
type rowsMsg struct {
	sessions     []tmux.SessionInfo
	current      string
	display      string
	err          error
	selectionErr error
	displayErr   error
	paths        map[sessionKey]pathResult
}

type tickMsg struct{}

type mirrorEventMsg struct {
	event mirrorapi.Event
	ok    bool
}

type mirrorOperationMsg struct {
	operation string
	token     uint64
	err       error
}

type mirrorReconcileMsg struct {
	err error
}

type Model struct {
	pane.Nav
	backend           Backend
	ws                string
	root              string
	cmd               string
	sessions          map[string]tmux.SessionInfo
	lastSeen          map[string]int64
	expanded          map[sessionKey]bool
	paths             map[sessionKey]pathState
	current           string // last-read state-selection (the tree's pick)
	display           string // local Enter/`n` switch override; cleared when state picks anew
	renaming          bool
	renameBuf         []rune
	renameTarget      string // session identity captured at "r" time; survives row churn before enter
	renameTargetID    string
	renameTargetGen   string
	alerted           bool   // last workspace-level alert state pushed to the title
	alertRung         bool   // one-shot BEL attempted for the current alert episode
	stale             string // why the last session poll failed; "" once one succeeds again
	selectionStale    string // why the last selection read failed; rows still update
	alertErr          string // why the last title transition failed; "" once one lands
	ringErr           string // why this episode's one-shot BEL failed; held until clear
	displayStale      string // why the displayed-session read failed; last display remains
	polling           bool
	timerPending      bool
	keys              config.Keys
	helpOpen          bool
	mirrors           Mirror
	mirrorSnapshot    mirrorapi.Snapshot
	mirrorOpen        bool
	mirrorTarget      mirrorapi.Identity
	mirrorTargetName  string
	mirrorCancel      context.CancelFunc
	mirrorStarting    bool
	mirrorOperationID uint64
	mirrorScroll      int
	mirrorReconciling bool
	mirrorSyncErr     string
}

// NewModel builds an empty terms Model; the first tick populates rows.
func NewModel(b Backend, o Options) Model {
	model := Model{
		backend:  b,
		ws:       o.WS,
		root:     o.Root,
		cmd:      o.Cmd,
		sessions: map[string]tmux.SessionInfo{},
		lastSeen: map[string]int64{},
		expanded: map[sessionKey]bool{},
		paths:    map[sessionKey]pathState{},
		keys:     o.Keys.WithDefaults(),
		mirrors:  o.Mirrors,
	}
	if o.Mirrors != nil {
		model.mirrorSnapshot = o.Mirrors.Snapshot()
	}
	return model
}

func (m Model) Init() tea.Cmd {
	if m.mirrors == nil {
		return func() tea.Msg { return tickMsg{} }
	}
	return tea.Batch(
		func() tea.Msg { return tickMsg{} },
		waitMirrorEvent(m.mirrors.Events()),
	)
}

// fetch snapshots the backend and workspace before returning the
// closure, so a later Update (which replaces m with a new value) can't
// race with this Cmd's goroutine.
func (m Model) fetch() tea.Cmd {
	backend, ws := m.backend, m.ws
	expanded := make(map[sessionKey]bool, len(m.expanded))
	for key, value := range m.expanded {
		expanded[key] = value
	}
	return func() tea.Msg {
		infos, err := backend.Sessions()
		if err != nil {
			return rowsMsg{err: err}
		}
		paths := make(map[sessionKey]pathResult)
		for _, info := range infos {
			key := sessionKey{generation: info.Generation, id: info.ID}
			if !expanded[key] {
				continue
			}
			path, err := backend.SessionCurrentPath(info.ID, info.Generation)
			paths[key] = pathResult{path: path, err: err}
		}
		display, displayErr := backend.DisplayedSession()
		sel, _, err := state.Read(ws)
		if err != nil {
			return rowsMsg{
				sessions: infos, display: display, displayErr: displayErr,
				selectionErr: err, paths: paths,
			}
		}
		return rowsMsg{
			sessions: infos, current: sel.Session, display: display,
			displayErr: displayErr, paths: paths,
		}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.timerPending = false
		return m.startPoll()
	case rowsMsg:
		m.polling = false
		// A poll that could not run tells us nothing, so keep showing what
		// the last good one said. Clearing here would blank every row, 🔔
		// and "!" for a tmux hiccup — and worse, m.anyBell() below would
		// go false, withdraw the workspace alert, and reset m.alerted, so
		// the next good poll would see the same bell as newly raised and
		// ring the terminal again for something already acknowledged.
		//
		// Holding the last good rows silently would be its own lie
		// though: they look live and are not. m.stale says so in the
		// footer until a poll succeeds again.
		if msg.err != nil {
			m.stale = msg.err.Error()
			for key, path := range m.paths {
				if m.expanded[key] {
					path.stale = true
					m.paths[key] = path
				}
			}
			return m.scheduleTick()
		}
		m.stale = ""
		next := map[string]tmux.SessionInfo{}
		live := map[sessionKey]bool{}
		for _, s := range msg.sessions {
			next[s.Name] = s
			live[sessionKey{generation: s.Generation, id: s.ID}] = true
			// First sighting baselines lastSeen so pre-existing
			// sessions don't all flash "!" on startup.
			if _, seen := m.lastSeen[s.Name]; !seen {
				m.lastSeen[s.Name] = s.Activity
			}
		}
		m.sessions = next
		for key := range m.expanded {
			if !live[key] {
				delete(m.expanded, key)
				delete(m.paths, key)
			}
		}
		for key := range m.paths {
			if !live[key] {
				delete(m.paths, key)
			}
		}
		for key, result := range msg.paths {
			if !live[key] || !m.expanded[key] {
				continue
			}
			if result.err != nil {
				path := m.paths[key]
				path.stale = path.valid
				m.paths[key] = path
				continue
			}
			m.paths[key] = pathState{value: result.path, valid: true}
		}
		if msg.selectionErr != nil {
			m.selectionStale = msg.selectionErr.Error()
		} else {
			m.selectionStale = ""
			m.current = msg.current
		}
		if msg.displayErr != nil {
			m.displayStale = msg.displayErr.Error()
		} else {
			m.displayStale = ""
			m.display = msg.display
		}
		// Viewing a session (it's the effective current one) re-baselines
		// its activity — output that arrived while it was on-screen
		// isn't "unseen".
		effective := m.effectiveCurrent()
		if cur, ok := next[effective]; ok {
			m.lastSeen[effective] = cur.Activity
		}
		m.Cursor = m.clampCursor()
		// Escalate to the outer terminal's title. A per-row 🔔 only helps
		// while you are looking at THIS workspace; with several wraps open
		// across tabs, the one that wants you is the one you are not
		// looking at. Written only on change — this runs every 2s.
		//
		// The persistent title and one-shot BEL have separate delivery
		// state. A failed title is safe and necessary to retry; repeating
		// the BEL would beep every two seconds until the user looks.
		alert := m.anyBell()
		if alert && !m.alertRung {
			m.alertRung = true
			m.ringErr = ""
			if err := m.backend.RingWorkspaceAlert(); err != nil {
				m.ringErr = err.Error()
			}
		}
		if alert != m.alerted {
			m.alertErr = ""
			if err := m.backend.SetWorkspaceAlert(alert); err != nil {
				m.alertErr = err.Error()
			} else {
				m.alerted = alert
			}
		}
		if !alert {
			// A failed raise never changed the persistent title. Once the
			// bell disappears there is therefore nothing to clear or retry;
			// do not leave its obsolete failure pinned in the footer.
			if !m.alerted {
				m.alertErr = ""
			}
			m.alertRung = false
			m.ringErr = ""
		}
		m, tick := m.scheduleTick()
		if m.mirrors == nil || m.mirrorReconciling {
			return m, tick
		}
		m.mirrorReconciling = true
		sessions := m.hostMirrorSessions()
		reconcile := func() tea.Msg {
			return mirrorReconcileMsg{err: m.mirrors.Reconcile(context.Background(), sessions)}
		}
		return m, tea.Batch(tick, reconcile)
	case mirrorEventMsg:
		if !msg.ok {
			return m, nil
		}
		if msg.event.Snapshot != nil {
			if msg.event.Snapshot.PairingURL != m.mirrorSnapshot.PairingURL {
				m.mirrorScroll = 0
			}
			m.mirrorSnapshot = *msg.event.Snapshot
		}
		if msg.event.Viewed != nil {
			for name, session := range m.sessions {
				if session.ID == msg.event.Viewed.ID &&
					session.Generation == msg.event.Viewed.Generation {
					m.lastSeen[name] = msg.event.Viewed.Activity
					break
				}
			}
		}
		return m, waitMirrorEvent(m.mirrors.Events())
	case mirrorOperationMsg:
		if msg.token != m.mirrorOperationID {
			return m, nil
		}
		m.mirrorStarting = false
		m.clearMirrorCancel()
		if errors.Is(msg.err, context.Canceled) {
			m.mirrorSyncErr = ""
		} else if msg.err != nil {
			m.mirrorSyncErr = "encrypted mirror operation failed; retry"
		} else {
			m.mirrorSyncErr = ""
		}
		if msg.operation == "revoke" {
			m.mirrorOpen = false
		}
		return m, nil
	case mirrorReconcileMsg:
		m.mirrorReconciling = false
		if msg.err != nil {
			m.mirrorSyncErr = "encrypted mirror session sync failed"
		} else {
			m.mirrorSyncErr = ""
		}
		return m, nil
	case tea.MouseMsg:
		if m.mirrorOpen || m.helpOpen || m.ConfirmKill || m.ConfirmShutdown || m.renaming {
			return m, nil
		}
		return m.handleMouse(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) startPoll() (Model, tea.Cmd) {
	if m.polling {
		return m, nil
	}
	m.polling = true
	return m, m.fetch()
}

func (m Model) scheduleTick() (Model, tea.Cmd) {
	if m.timerPending {
		return m, nil
	}
	m.timerPending = true
	return m, nextTick()
}

func nextTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func waitMirrorEvent(events <-chan mirrorapi.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return mirrorEventMsg{event: event, ok: ok}
	}
}

func (m Model) hostMirrorSessions() []mirrorapi.HostSession {
	sessions := make([]mirrorapi.HostSession, 0, len(m.sessions))
	for name, session := range m.sessions {
		if !config.SessionOwnedBy(m.ws, name) {
			continue
		}
		sessions = append(sessions, mirrorapi.HostSession{
			ID:           session.ID,
			Generation:   session.Generation,
			Name:         name,
			Kind:         session.Kind,
			Bell:         session.Bell,
			Activity:     session.Activity,
			SeenActivity: m.lastSeen[name],
		})
	}
	return sessions
}

func (m *Model) clearMirrorCancel() {
	if m.mirrorCancel != nil {
		m.mirrorCancel()
		m.mirrorCancel = nil
	}
}

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		lines := m.visibleLines(rows)
		msg.Y-- // the heading occupies physical pane row 0
		if msg.Y < 0 || msg.Y >= len(lines) {
			return m, nil
		}
		m.Cursor = lines[msg.Y].row
		return m, nil
	}
	m.HandleMouse(msg, len(rows))
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	if m.mirrorOpen {
		return m.handleMirrorKey(msg)
	}
	if m.renaming {
		return m.handleRenameKey(msg)
	}
	if m.ConfirmKill || m.ConfirmShutdown {
		m.HandleKey(msg, scratchPaneBackend{m.backend}, len(rows))
		return m, nil
	}
	if m.helpOpen {
		switch msg.String() {
		case "h", "esc", "q":
			m.helpOpen = false
		}
		return m, nil
	}
	if msg.String() == "h" {
		m.helpOpen = true
		return m, nil
	}
	if m.HandleKey(msg, scratchPaneBackend{m.backend}, len(rows)) {
		return m, nil
	}
	switch msg.String() {
	case "m":
		if m.mirrors == nil {
			m.ErrText = "encrypted mirror is unavailable"
			return m, nil
		}
		if m.Cursor >= len(rows) {
			return m, nil
		}
		target := rows[m.Cursor]
		if !mirrorEligible(m.ws, target) {
			m.ErrText = "diff terminals cannot be mirrored"
			return m, nil
		}
		m.ErrText = ""
		m.mirrorOpen = true
		m.mirrorScroll = 0
		m.mirrorTarget = mirrorapi.Identity{ID: target.id, Generation: target.generation}
		m.mirrorTargetName = target.name
		if target.mirrored {
			return m, nil
		}
		return m.startMirror(target)
	case "right":
		if m.Cursor < len(rows) {
			m.expanded[rows[m.Cursor].key] = true
			return m.startPoll()
		}
	case "left":
		if m.Cursor < len(rows) {
			delete(m.expanded, rows[m.Cursor].key)
			delete(m.paths, rows[m.Cursor].key)
		}
	case "enter":
		if m.Cursor < len(rows) {
			// Switches the DISPLAY only: this does not write
			// state.Selection (the tree pane owns selection
			// semantics), so the tree's own highlight can end up
			// pointing at a different session than the middle pane
			// shows. That's tracked locally (m.display) so THIS
			// pane's ▸ still follows it — until the tree makes a new
			// selection, which supersedes it. ShowInMiddle (unlike the
			// tree's SwitchMiddle) never moves focus, so this pane
			// keeps it.
			target := rows[m.Cursor].name
			if err := m.backend.ShowInMiddle(target); err != nil {
				m.ErrText = err.Error()
			} else {
				m.ErrText = ""
				m.display = target
			}
		}
	case "n":
		// n is selection-aware: if the tree's current selection names a
		// session that isn't alive (a repo/worktree row picked while
		// session-less), bind that session first instead of opening an
		// unrelated scratch terminal at the root. state.Read is a fresh
		// snapshot (not just the tick-derived m.current) so this is
		// accurate even between ticks.
		ws := m.ws
		sel, ok, err := state.Read(ws)
		if err != nil {
			m.ErrText = err.Error()
			return m, nil
		}
		dir := m.root
		if sel.Path != "" {
			dir = sel.Path
		}
		if ok && sel.Session != "" {
			if _, exists := m.sessions[sel.Session]; !exists {
				if err := m.backend.EnsureEntrySession(sel.Session, dir, m.cmd); err != nil {
					m.ErrText = err.Error()
					return m, nil
				}
				if err := m.backend.ShowInMiddle(sel.Session); err != nil {
					m.ErrText = err.Error()
					return m, nil
				}
				m.ErrText = ""
				m.display = sel.Session
				return m.startPoll()
			}
		}
		name, err := m.backend.NewTerm(dir, m.cmd)
		if err != nil {
			m.ErrText = err.Error()
			return m, nil
		}
		if err := m.backend.ShowInMiddle(name); err != nil {
			m.ErrText = err.Error()
			return m, nil
		}
		m.ErrText = ""
		m.display = name
		return m.startPoll()
	case "x":
		if m.Cursor < len(rows) {
			target := rows[m.Cursor]
			if !config.IsTermSession(m.ws, target.name) || target.kind != tmux.SessionKindScratch {
				m.ErrText = "only scratch terminals can be killed"
				return m, nil
			}
			m.ErrText = ""
			m.ArmKill(target.name, target.id, target.generation, successorRow(rows, m.Cursor))
		}
	case "r":
		if m.Cursor < len(rows) {
			target := rows[m.Cursor]
			if !config.IsTermSession(m.ws, target.name) || target.kind != tmux.SessionKindScratch {
				m.ErrText = "only scratch terminals can be renamed"
				return m, nil
			}
			m.renaming = true
			m.renameBuf = nil
			m.renameTarget = target.name
			m.renameTargetID = target.id
			m.renameTargetGen = target.generation
			m.ErrText = ""
		}
	}
	return m, nil
}

func (m Model) startMirror(target row) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.clearMirrorCancel()
	m.mirrorCancel = cancel
	m.mirrorStarting = true
	token := m.beginMirrorOperation()
	session := mirrorapi.HostSession{
		ID:           target.id,
		Generation:   target.generation,
		Name:         target.name,
		Kind:         target.kind,
		Bell:         target.bell,
		Activity:     m.sessions[target.name].Activity,
		SeenActivity: m.lastSeen[target.name],
	}
	return m, func() tea.Msg {
		return mirrorOperationMsg{
			operation: "start",
			token:     token,
			err:       m.mirrors.Mirror(ctx, session),
		}
	}
}

func (m Model) handleMirrorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.mirrorScroll = max(0, m.mirrorScroll-1)
	case "down", "j":
		m.mirrorScroll = min(m.mirrorMaxScroll(), m.mirrorScroll+1)
	case "esc":
		m.clearMirrorCancel()
		m.mirrorStarting = false
		m.mirrorOpen = false
	case "x":
		if m.mirrors == nil || !m.targetIsMirrored() {
			return m, nil
		}
		identity := m.mirrorTarget
		token := m.beginMirrorOperation()
		return m, func() tea.Msg {
			return mirrorOperationMsg{
				operation: "revoke",
				token:     token,
				err:       m.mirrors.Revoke(context.Background(), identity),
			}
		}
	case "R":
		if m.mirrors == nil || m.mirrorSnapshot.State != mirrorapi.StateReady {
			return m, nil
		}
		token := m.beginMirrorOperation()
		return m, func() tea.Msg {
			return mirrorOperationMsg{
				operation: "rotate",
				token:     token,
				err:       m.mirrors.Rotate(context.Background()),
			}
		}
	case "m":
		if m.mirrors == nil || m.mirrorStarting || m.targetIsMirrored() {
			return m, nil
		}
		for _, target := range m.rows() {
			if target.id == m.mirrorTarget.ID && target.generation == m.mirrorTarget.Generation {
				return m.startMirror(target)
			}
		}
	}
	return m, nil
}

func (m *Model) beginMirrorOperation() uint64 {
	m.mirrorOperationID++
	return m.mirrorOperationID
}

func mirrorEligible(workspace string, target row) bool {
	return config.SessionOwnedBy(workspace, target.name) && target.kind != tmux.SessionKindDiff
}

func (m Model) targetIsMirrored() bool {
	for _, session := range m.mirrorSnapshot.Sessions {
		if session.ID == m.mirrorTarget.ID && session.Generation == m.mirrorTarget.Generation {
			return true
		}
	}
	return false
}

// handleRenameKey handles all key input while renaming: printable runes
// append to the buffer, backspace removes the last rune, esc cancels,
// enter commits via backend.RenameTerm against the target captured at
// "r" time. Every other key (j/k/n/x/enter navigation) is suspended
// while renaming — a rune-typed "j", "n", or "x" is buffer input here,
// not a navigation command.
func (m Model) handleRenameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.renaming = false
		m.renameBuf = nil
		m.renameTarget = ""
		m.renameTargetID = ""
		m.renameTargetGen = ""
	case tea.KeyEnter:
		target := m.renameTarget
		targetID := m.renameTargetID
		targetGeneration := m.renameTargetGen
		label := string(m.renameBuf)
		m.renaming = false
		m.renameBuf = nil
		m.renameTarget = ""
		m.renameTargetID = ""
		m.renameTargetGen = ""
		newName, err := m.backend.RenameTerm(target, targetID, targetGeneration, label)
		if err != nil {
			m.ErrText = err.Error()
			return m, nil
		}
		m.ErrText = ""
		if m.display == target {
			m.display = newName
		}
		return m.startPoll()
	case tea.KeyBackspace:
		if len(m.renameBuf) > 0 {
			m.renameBuf = m.renameBuf[:len(m.renameBuf)-1]
		}
	case tea.KeyRunes:
		m.renameBuf = append(m.renameBuf, msg.Runes...)
	}
	return m, nil
}

// effectiveCurrent returns the session this pane highlights ▸: a local
// Enter/`n` switch (display) takes priority over the tree's
// state-selection (current), since it's the more recent signal of what
// the middle pane is actually showing — until the tree picks anew.
func (m Model) effectiveCurrent() string {
	if m.display != "" {
		return m.display
	}
	return m.current
}

func (m Model) clampCursor() int {
	return m.ClampCursor(len(m.rows()))
}

// rows assembles the ws-owned session rows in tmux creation order.
// anyBell reports whether any session this workspace owns is ringing.
func (m Model) anyBell() bool {
	for name, info := range m.sessions {
		if info.Bell && config.SessionOwnedBy(m.ws, name) {
			return true
		}
	}
	return false
}

// successorRow names the session the terminal pane should fall to when
// rows[idx] is killed while on screen: the row below it, or the row above
// when killing the last one. Empty when nothing would be left.
func successorRow(rows []row, idx int) string {
	if idx+1 < len(rows) {
		return rows[idx+1].name
	}
	if idx-1 >= 0 {
		return rows[idx-1].name
	}
	return ""
}

func (m Model) rows() []row {
	var rows []row
	effective := m.effectiveCurrent()
	mirrored := make(map[sessionKey]bool, len(m.mirrorSnapshot.Sessions))
	if m.mirrorSnapshot.State == mirrorapi.StateReady {
		for _, session := range m.mirrorSnapshot.Sessions {
			mirrored[sessionKey{generation: session.Generation, id: session.ID}] = true
		}
	}
	for name, info := range m.sessions {
		if !config.SessionOwnedBy(m.ws, name) {
			continue
		}
		rows = append(rows, row{
			id:         info.ID,
			generation: info.Generation,
			name:       name,
			kind:       info.Kind,
			created:    info.Created,
			bell:       info.Bell,
			activity:   info.Activity > m.lastSeen[name],
			mirrored:   mirrored[sessionKey{generation: info.Generation, id: info.ID}],
			current:    name == effective,
			key:        sessionKey{generation: info.Generation, id: info.ID},
			expanded:   m.expanded[sessionKey{generation: info.Generation, id: info.ID}],
			path:       m.paths[sessionKey{generation: info.Generation, id: info.ID}],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rowCreatedLess(rows[i], rows[j])
	})
	return rows
}

func rowCreatedLess(a, b row) bool {
	aHasCreated := a.created > 0
	bHasCreated := b.created > 0
	if aHasCreated != bHasCreated {
		return aHasCreated
	}
	if aHasCreated && a.created != b.created {
		return a.created < b.created
	}
	aID, aHasID := numericSessionID(a.id)
	bID, bHasID := numericSessionID(b.id)
	if aHasID != bHasID {
		return aHasID
	}
	if aHasID && aID != bID {
		return aID < bID
	}
	return a.name < b.name
}

func numericSessionID(id string) (uint64, bool) {
	if len(id) < 2 || id[0] != '$' {
		return 0, false
	}
	n, err := strconv.ParseUint(id[1:], 10, 64)
	return n, err == nil
}

type physicalLine struct {
	row    int
	detail bool
}

func (m Model) visibleLines(rows []row) []physicalLine {
	if len(rows) == 0 {
		return nil
	}
	anchor := m.Cursor
	if m.ConfirmKill {
		for i, r := range rows {
			if r.name == m.ConfirmTarget {
				anchor = i
				break
			}
		}
	}
	if anchor < 0 {
		anchor = 0
	}
	if anchor >= len(rows) {
		anchor = len(rows) - 1
	}

	capacity := len(rows) * 2
	if m.Height > 0 {
		capacity = m.Height - 2 // heading and compact action footer
		if m.footer() != "" {
			capacity--
		}
		if capacity <= 0 {
			return nil
		}
	}
	rowHeight := func(index int) int {
		if rows[index].expanded {
			return 2
		}
		return 1
	}
	start, end := anchor, anchor+1
	used := rowHeight(anchor)
	if used > capacity {
		used = capacity
	}
	for start > 0 {
		height := rowHeight(start - 1)
		if used+height > capacity {
			break
		}
		start--
		used += height
	}
	for end < len(rows) {
		height := rowHeight(end)
		if used+height > capacity {
			break
		}
		end++
		used += height
	}

	lines := make([]physicalLine, 0, capacity)
	for i := start; i < end && len(lines) < capacity; i++ {
		lines = append(lines, physicalLine{row: i})
		if rows[i].expanded && len(lines) < capacity {
			lines = append(lines, physicalLine{row: i, detail: true})
		}
	}
	return lines
}

// View renders a pane heading, rows, an optional status line, and the compact
// workspace-action footer. Help replaces the ordinary content without
// stopping background polling.
func (m Model) View() string {
	heading := pane.Heading("Terminals", m.keys.FocusTerms, m.Width)
	if m.helpOpen {
		return m.helpView(heading)
	}
	if m.mirrorOpen {
		return m.mirrorView(heading)
	}

	rows := m.rows()
	visible := m.visibleLines(rows)
	lines := []string{heading}
	for _, visibleLine := range visible {
		r := rows[visibleLine.row]
		if visibleLine.detail {
			lines = append(lines, m.renderPath(r.path))
			continue
		}
		line := renderRow(r, m.Width)
		// The row about to die outranks the cursor: with a confirmation
		// pending, that is the one thing worth looking at.
		switch {
		case m.IsKillTarget(r.name):
			line = pane.AlertStyle.Render(line)
		case visibleLine.row == m.Cursor:
			line = pane.CursorStyle.Render(line)
		}
		lines = append(lines, line)
	}
	footer := m.footer()
	reserved := len(lines) + 1 // compact action footer
	if footer != "" {
		reserved++
	}
	if m.Height > 0 {
		for pad := m.Height - reserved; pad > 0; pad-- {
			lines = append(lines, "")
		}
	}
	if footer != "" {
		lines = append(lines, footer)
	}
	lines = append(lines, pane.ActionFooter(m.Width))
	return strings.Join(lines, "\n")
}

func (m Model) helpView(heading string) string {
	bodyHeight := -1 // unknown pane height: render the complete Help body
	if m.Height > 0 {
		if m.Height == 1 {
			return heading
		}
		bodyHeight = m.Height - 2 // heading and close footer
	}
	lines := []string{heading}
	if body := pane.HelpBody(m.keys, m.Width, bodyHeight); body != "" {
		lines = append(lines, strings.Split(body, "\n")...)
	}
	if m.Height > 0 {
		for pad := m.Height - len(lines) - 1; pad > 0; pad-- {
			lines = append(lines, "")
		}
	}
	lines = append(lines, pane.HelpFooter(m.Width))
	return strings.Join(lines, "\n")
}

func renderRow(r row, width int) string {
	prefix := "  "
	if r.current {
		prefix = "▸ "
	}
	if r.mirrored {
		prefix += "📡 "
	}
	switch {
	case r.bell:
		prefix += "🔔 "
	case r.activity:
		prefix += "! "
	}
	disclosure := "›"
	if r.expanded {
		disclosure = "⌄"
	}
	prefix += disclosure + " "

	name := r.name
	if width > 0 {
		available := width - runewidth.StringWidth(prefix)
		if available <= 0 {
			return runewidth.Truncate(prefix, width, "")
		}
		name = runewidth.Truncate(name, available, "…")
	}
	return prefix + name
}

func (m Model) renderPath(path pathState) string {
	line := "    ⚠ unavailable"
	if !path.valid {
		if m.Width > 0 {
			line = runewidth.Truncate(line, m.Width, "")
		}
		return pane.DimStyle.Render(line)
	}
	value := formatPWD(path.value)
	suffix := ""
	if path.stale {
		suffix = " ?"
	}
	const prefix = "    "
	if m.Width > 0 {
		value = truncateLeft(value, m.Width-runewidth.StringWidth(prefix)-runewidth.StringWidth(suffix))
	}
	line = prefix + value + suffix
	if m.Width > 0 {
		line = runewidth.Truncate(line, m.Width, "")
	}
	return pane.DimStyle.Render(line)
}

func formatPWD(path string) string {
	return pane.SafeLabel(path)
}

func truncateLeft(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(value) <= width {
		return value
	}
	// runewidth.TruncateLeft's width is the number of cells to remove,
	// not the desired output width.
	cut := runewidth.StringWidth(value) - width + runewidth.StringWidth("…")
	return runewidth.TruncateLeft(value, cut, "…")
}

func (m Model) footer() string {
	if m.renaming {
		return pane.DimStyle.Render(" rename: " + string(m.renameBuf) + "▌ (enter/esc) ")
	}
	if s := m.ConfirmFooter(m.effectiveCurrent()); s != "" {
		return s
	}
	// Both of these are background failures, kept out of ErrText so they
	// cannot squat on the footer that the user's own keypresses report
	// through — nor be wiped by one. Stale rows outrank a missed alert:
	// they change how every line above should be read.
	if m.stale != "" {
		return pane.AlertStyle.Render(" rows stale: " + m.stale + " ")
	}
	if m.selectionStale != "" {
		return pane.AlertStyle.Render(" selection stale: " + m.selectionStale + " ")
	}
	if m.displayStale != "" {
		return pane.AlertStyle.Render(" display stale: " + m.displayStale + " ")
	}
	if m.alertErr != "" {
		return pane.AlertStyle.Render(" alert failed: " + m.alertErr + " ")
	}
	if m.ringErr != "" {
		return pane.AlertStyle.Render(" alert failed: " + m.ringErr + " ")
	}
	if m.mirrorSyncErr != "" {
		return pane.AlertStyle.Render(" " + m.mirrorSyncErr + " ")
	}
	return ""
}

func (m Model) mirrorView(heading string) string {
	content := m.mirrorContentLines()
	scroll := min(max(0, m.mirrorScroll), m.mirrorMaxScroll())
	lines := []string{heading}
	if m.Height > 0 {
		bodyHeight := max(0, m.Height-2)
		end := min(len(content), scroll+bodyHeight)
		lines = append(lines, content[scroll:end]...)
	} else {
		lines = append(lines, content...)
	}
	footer := "esc close"
	if m.mirrorSnapshot.State == mirrorapi.StateReady {
		footer = "x revoke · R rotate · esc close"
	} else if !m.mirrorStarting {
		footer = "m retry · esc close"
	}
	if m.mirrorMaxScroll() > 0 {
		footer = "↑/↓ scroll · " + footer
	}
	for i := range lines {
		if m.Width > 0 {
			lines[i] = runewidth.Truncate(lines[i], m.Width, "")
		}
	}
	if m.Height > 0 {
		for len(lines) < m.Height-1 {
			lines = append(lines, "")
		}
	}
	if m.Width > 0 {
		footer = runewidth.Truncate(footer, m.Width, "")
	}
	lines = append(lines, pane.DimStyle.Render(footer))
	return strings.Join(lines, "\n")
}

func (m Model) mirrorContentLines() []string {
	var lines []string
	switch {
	case m.mirrorStarting || m.mirrorSnapshot.State == mirrorapi.StateStarting:
		lines = append(lines, "", "Starting encrypted mirror…", "", m.mirrorTargetName)
	case m.mirrorSnapshot.State == mirrorapi.StateReady:
		lines = append(lines, "")
		lines = append(lines, mirrorWrapLine(m.mirrorSnapshot.PairingURL, m.Width)...)
		lines = append(lines,
			"",
			"Anyone with this URL can control mirrored terminals.",
			"",
			m.mirrorTargetName+" · "+strconv.Itoa(len(m.mirrorSnapshot.Sessions))+" mirrored",
		)
		if m.mirrorSnapshot.QR != "" {
			qrLines := strings.Split(strings.Trim(m.mirrorSnapshot.QR, "\n"), "\n")
			qrFitsWidth := m.Width <= 0
			if !qrFitsWidth {
				qrFitsWidth = true
				for _, line := range qrLines {
					if runewidth.StringWidth(line) > m.Width {
						qrFitsWidth = false
						break
					}
				}
			}
			if qrFitsWidth {
				lines = append(lines, "")
				lines = append(lines, qrLines...)
			}
		}
	default:
		lines = append(lines,
			"",
			"Encrypted mirror unavailable.",
			"Check cloudflared is installed and retry.",
		)
	}
	return lines
}

func (m Model) mirrorMaxScroll() int {
	if m.Height <= 0 {
		return 0
	}
	return max(0, len(m.mirrorContentLines())-max(0, m.Height-2))
}

func mirrorWrapLine(line string, width int) []string {
	if width <= 0 || runewidth.StringWidth(line) <= width {
		return []string{line}
	}
	var lines []string
	var chunk []rune
	chunkWidth := 0
	for _, r := range []rune(line) {
		runeWidth := runewidth.RuneWidth(r)
		if chunkWidth+runeWidth > width && len(chunk) != 0 {
			lines = append(lines, string(chunk))
			chunk = nil
			chunkWidth = 0
		}
		chunk = append(chunk, r)
		chunkWidth += runeWidth
	}
	if len(chunk) != 0 {
		lines = append(lines, string(chunk))
	}
	return lines
}
