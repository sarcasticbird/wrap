// Package terms renders the terminals-monitor pane: every tmux session
// owned by the current workspace (the root session, its named entries,
// and any free `<ws>·term·N` terminals), kept in creation order.
package terms

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/launcher"
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

type Model struct {
	pane.Nav
	backend         Backend
	ws              string
	root            string
	cmd             string
	sessions        map[string]tmux.SessionInfo
	lastSeen        map[string]int64
	expanded        map[sessionKey]bool
	paths           map[sessionKey]pathState
	current         string // last-read state-selection (the tree's pick)
	display         string // local Enter/`n` switch override; cleared when state picks anew
	renaming        bool
	renameBuf       []rune
	renameTarget    string // session identity captured at "r" time; survives row churn before enter
	renameTargetID  string
	renameTargetGen string
	alerted         bool   // last workspace-level alert state pushed to the title
	alertRung       bool   // one-shot BEL attempted for the current alert episode
	stale           string // why the last session poll failed; "" once one succeeds again
	selectionStale  string // why the last selection read failed; rows still update
	alertErr        string // why the last title transition failed; "" once one lands
	ringErr         string // why this episode's one-shot BEL failed; held until clear
	displayStale    string // why the displayed-session read failed; last display remains
	polling         bool
	timerPending    bool
}

// NewModel builds an empty terms Model; the first tick populates rows.
func NewModel(b Backend, o Options) Model {
	return Model{
		backend:  b,
		ws:       o.WS,
		root:     o.Root,
		cmd:      o.Cmd,
		sessions: map[string]tmux.SessionInfo{},
		lastSeen: map[string]int64{},
		expanded: map[sessionKey]bool{},
		paths:    map[sessionKey]pathState{},
	}
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return tickMsg{} }
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
		return m.scheduleTick()
	case tea.MouseMsg:
		if m.ConfirmKill || m.ConfirmShutdown || m.renaming {
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

func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		lines := m.visibleLines(rows)
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
	if m.renaming {
		return m.handleRenameKey(msg)
	}
	if m.HandleKey(msg, scratchPaneBackend{m.backend}, len(rows)) {
		return m, nil
	}
	switch msg.String() {
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
		helpLines := strings.Count(helpBlock(m.Width), "\n") + 1
		capacity = m.Height - helpLines - 1
		if capacity < 1 {
			capacity = 1
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

// View renders the rows, an optional footer, and the help block. When
// m.Height is known (a WindowSizeMsg has arrived) and everything fits, the
// help block is pinned so its LAST line lands on the pane's last line:
// blank padding lines soak up the gap between the rows and the
// footer+help group, which stays glued together at the bottom (the footer
// sits directly above help, with no separating blank — unlike the
// fallback flow below, which always has one). When height is 0/unknown,
// or rows+footer+help wouldn't fit in it (padding would go negative), it
// falls back to the original top-down flow: rows, footer, one blank line,
// help.
func (m Model) View() string {
	rows := m.rows()
	visible := m.visibleLines(rows)
	rowLines := make([]string, 0, len(visible))
	for _, visibleLine := range visible {
		r := rows[visibleLine.row]
		if visibleLine.detail {
			rowLines = append(rowLines, m.renderPath(r.path))
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
		rowLines = append(rowLines, line)
	}
	footer := m.footer()
	help := helpBlock(m.Width)
	helpLines := strings.Count(help, "\n") + 1

	var b strings.Builder
	for _, l := range rowLines {
		b.WriteString(l + "\n")
	}

	contentLines := len(rowLines)
	if footer != "" {
		contentLines++
	}
	pad := m.Height - contentLines - helpLines
	if m.Height > 0 && pad >= 0 {
		for i := 0; i < pad; i++ {
			b.WriteString("\n")
		}
		if footer != "" {
			b.WriteString(footer + "\n")
		}
		b.WriteString(help)
		return b.String()
	}

	if footer != "" {
		b.WriteString(footer + "\n")
	}
	b.WriteString("\n" + help)
	return b.String()
}

func renderRow(r row, width int) string {
	prefix := "  "
	if r.current {
		prefix = "▸ "
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
	value := formatPWD(m.root, path.value)
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

func formatPWD(root, path string) string {
	display := path
	if rel, err := filepath.Rel(root, path); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			display = "."
		} else {
			display = "." + string(filepath.Separator) + rel
		}
	}
	quoted := strconv.QuoteToGraphic(display)
	return quoted[1 : len(quoted)-1]
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
	return ""
}

func helpBlock(width int) string {
	lines := []string{
		" terms: ↵ open · ←/→ details · n new · r rename · x kill",
		" tree:  ↵ open · l files · h close · x kill",
		" focus: ⌥1 tree · ⌥2 terminal · ⌥3 terms",
		" q detach · Q shutdown",
	}
	if width > 0 {
		for i, line := range lines {
			lines[i] = runewidth.Truncate(line, width, "")
		}
	}
	return pane.DimStyle.Render(strings.Join(lines, "\n"))
}
