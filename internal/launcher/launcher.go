// Package launcher builds wrap's tmux chrome and manages entry sessions.
package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

const (
	// HomeSession always exists on the session server so the middle
	// pane's nested client has somewhere to land.
	HomeSession = config.HomeSession

	focusTreeOption     = "@wrap_focus_tree_key"
	focusTerminalOption = "@wrap_focus_terminal_key"
	focusTermsOption    = "@wrap_focus_terms_key"
)

type Manager struct {
	UI   *tmux.Server
	Sess *tmux.Server
	Exe  string // absolute path to the wrap binary, for pane commands
	WS   string // workspace name

	workspaceLockHeld bool
}

func New(ws string) (*Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate wrap binary: %w", err)
	}
	ui := tmux.NewServer(tmux.SocketUI)
	// Hermetic chrome: without this, the user's tmux.conf (base-index,
	// pane-base-index, ...) applies to the UI server and breaks the
	// fixed wrap:0.N pane targets.
	ui.ConfigFile = os.DevNull
	return &Manager{
		UI:   ui,
		Sess: tmux.NewServer(tmux.SocketSessions),
		Exe:  exe,
		WS:   ws,
	}, nil
}

// SessionCurrentPath reads the active pane path only while the captured
// session identity still belongs to the same tmux server generation.
func (m *Manager) SessionCurrentPath(id, generation string) (string, error) {
	path, err := m.Sess.SessionCurrentPathIfGeneration(id, generation)
	if err != nil {
		return "", fmt.Errorf("read session current path: %w", err)
	}
	return path, nil
}

// UISession names this workspace's chrome session: "wrap-<ws>".
func (m *Manager) UISession() string {
	return "wrap-" + m.WS
}

// GuardWorkspaceMeta records this workspace's meta, refusing to clobber
// a DIFFERENT live workspace that already owns the name: same-basename
// folders (~/a/api and ~/b/api both become "api") would otherwise
// silently share a chrome, meta, and selection state.
func (m *Manager) GuardWorkspaceMeta(meta state.Meta) (func() error, error) {
	release, err := state.LockWorkspace(m.WS)
	if err != nil {
		return nil, fmt.Errorf("lock workspace %q ownership: %w", m.WS, err)
	}
	m.workspaceLockHeld = true
	releaseLock := func() error {
		m.workspaceLockHeld = false
		return release()
	}
	fail := func(err error) (func() error, error) {
		return nil, errors.Join(err, releaseLock())
	}
	if existing, ok, err := state.ReadMeta(m.WS); err != nil {
		return fail(err)
	} else if ok && existing != meta {
		uiAlive, err := m.UI.HasSession(m.UISession())
		if err != nil {
			return fail(fmt.Errorf("check existing UI for workspace %q: %w", m.WS, err))
		}
		active := uiAlive
		if !active {
			infos, err := m.Sess.Sessions()
			if err != nil && !errors.Is(err, tmux.ErrNoServer) {
				return fail(fmt.Errorf("check existing sessions for workspace %q: %w", m.WS, err))
			}
			for _, info := range infos {
				if config.SessionOwnedBy(m.WS, info.Name) {
					active = true
					break
				}
			}
		}
		if active {
			return fail(fmt.Errorf("workspace %q is already running for %s — shut down that workspace first (or kill its remaining sessions), then retry; alternatively open both folders via a common parent", m.WS, existing.Root))
		}
	}
	if err := state.WriteMeta(m.WS, meta); err != nil {
		return fail(err)
	}
	if err := state.ClearShutdown(m.WS); err != nil {
		return fail(err)
	}
	return releaseLock, nil
}

// GuardActiveWorkspaceMeta locks an existing workspace for selector-driven
// attach/recovery. Unlike GuardWorkspaceMeta, it never creates or takes over
// identity: both persisted metadata and live tmux ownership must still match
// what the selector displayed.
func (m *Manager) GuardActiveWorkspaceMeta(meta state.Meta) (func() error, error) {
	release, err := state.LockWorkspace(m.WS)
	if err != nil {
		return nil, fmt.Errorf("lock active workspace %q: %w", m.WS, err)
	}
	m.workspaceLockHeld = true
	releaseLock := func() error {
		m.workspaceLockHeld = false
		return release()
	}
	fail := func(err error) (func() error, error) {
		return nil, errors.Join(err, releaseLock())
	}

	existing, ok, err := state.ReadMeta(m.WS)
	if err != nil {
		return fail(err)
	}
	if !ok || existing != meta {
		return fail(fmt.Errorf("workspace %q metadata changed since selector refresh", m.WS))
	}
	active, err := m.UI.HasSession(m.UISession())
	if err != nil {
		return fail(fmt.Errorf("check active UI for workspace %q: %w", m.WS, err))
	}
	if !active {
		infos, err := m.Sess.Sessions()
		if err != nil && !errors.Is(err, tmux.ErrNoServer) {
			return fail(fmt.Errorf("check active sessions for workspace %q: %w", m.WS, err))
		}
		for _, info := range infos {
			if config.SessionOwnedBy(m.WS, info.Name) {
				active = true
				break
			}
		}
	}
	if !active {
		return fail(fmt.Errorf("workspace %q is no longer active; refresh the selector and retry", m.WS))
	}
	if err := state.ClearShutdown(m.WS); err != nil {
		return fail(err)
	}
	return releaseLock, nil
}

func (m *Manager) beginWorkspaceMutation() (func() error, error) {
	if m.workspaceLockHeld {
		shuttingDown, err := state.IsShuttingDown(m.WS)
		if err != nil {
			return nil, err
		}
		if shuttingDown {
			return nil, fmt.Errorf("workspace %q is shutting down", m.WS)
		}
		return func() error { return nil }, nil
	}
	release, err := state.LockWorkspace(m.WS)
	if err != nil {
		return nil, fmt.Errorf("lock workspace %q mutation: %w", m.WS, err)
	}
	shuttingDown, err := state.IsShuttingDown(m.WS)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	if shuttingDown {
		return nil, errors.Join(fmt.Errorf("workspace %q is shutting down", m.WS), release())
	}
	return release, nil
}

// uiWindow anchors the session name with "=" so tmux matches this
// workspace's chrome exactly. Without it, an unanchored target falls back
// to prefix/fnmatch and can resolve to another workspace (wrap-vb vs
// wrap-vb2) whenever this one is transiently absent during teardown.
func (m *Manager) uiWindow() string        { return "=" + m.UISession() + ":0" }
func (m *Manager) paneTarget(i int) string { return m.uiWindow() + "." + strconv.Itoa(i) }

// EnsureSessionServer guarantees the home session exists and that killing
// a session hops attached clients to another instead of detaching them.
// EnsureSessionServer guarantees the home session exists and that
// killing a session hops attached clients elsewhere. homeDir sets the
// home session's directory when it is first created; "" means $HOME.
// An existing home session keeps the directory it was born with.
func (m *Manager) EnsureSessionServer(homeDir string) error {
	_, err := m.ensureSessionServer(homeDir)
	return err
}

func (m *Manager) ensureSessionServer(homeDir string) (string, error) {
	if _, err := m.ensureHomeSession(homeDir); err != nil {
		return "", err
	}
	return m.configureSessionServer()
}

func (m *Manager) ensureHomeSession(homeDir string) (bool, error) {
	hasHome, err := m.Sess.HasSession(HomeSession)
	if err != nil {
		return false, fmt.Errorf("check fallback session %s: %w", HomeSession, err)
	}
	if !hasHome {
		if homeDir == "" {
			var err error
			homeDir, err = os.UserHomeDir()
			if err != nil {
				return false, fmt.Errorf("home dir: %w", err)
			}
		}
		if err := m.Sess.NewSession(HomeSession, homeDir, ""); err != nil {
			// Check-then-act race: another pane on the session server can
			// create the home session between our HasSession probe and this
			// new-session, which tmux then rejects as a duplicate. Re-check
			// before surfacing the error — if the session now exists, the
			// race resolved in our favor and this is not a failure.
			hasHome, recheckErr := m.Sess.HasSession(HomeSession)
			if recheckErr != nil {
				return false, errors.Join(
					fmt.Errorf("create home session: %w", err),
					fmt.Errorf("recheck fallback session %s: %w", HomeSession, recheckErr),
				)
			}
			if !hasHome {
				return false, fmt.Errorf("create home session: %w", err)
			}
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func (m *Manager) configureSessionServer() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate session-server identity: %w", err)
	}
	generation, err := m.Sess.EnsureServerGeneration(hex.EncodeToString(nonce[:]))
	if err != nil {
		return "", fmt.Errorf("configure session-server identity: %w", err)
	}
	if err := m.Sess.Set("detach-on-destroy", "off"); err != nil {
		return "", fmt.Errorf("configure detach-on-destroy: %w", err)
	}
	if err := m.Sess.SetW("monitor-bell", "on"); err != nil {
		return "", fmt.Errorf("configure monitor-bell: %w", err)
	}
	// Record bells in a wrap-owned session option. tmux's window_bell_flag
	// is viewer-relative and is never raised on a session with a client
	// attached — and the terminal pane keeps one attached to whatever it
	// last displayed, so the session running your agent was precisely the
	// one that could never report a bell. This hook fires regardless of
	// attachment; the command runs in the belling session's context, so it
	// needs no target.
	//
	// It goes in a reserved slot of the hook array rather than replacing
	// "alert-bell" wholesale. The session server deliberately reads the
	// user's tmux.conf, so an unindexed set-hook would silently delete
	// their own alert-bell handler; writing one fixed index leaves theirs
	// intact and stays idempotent across launches.
	if err := m.Sess.SetHook(tmux.BellHook, "set-option "+tmux.BellOption+" 1"); err != nil {
		return "", fmt.Errorf("configure alert-bell hook: %w", err)
	}
	// Copy-mode selections propagate to the system clipboard via OSC 52
	// through the nested-client chain.
	if err := m.Sess.Set("set-clipboard", "on"); err != nil {
		return "", fmt.Errorf("configure clipboard forwarding: %w", err)
	}
	return generation, nil
}

func (m *Manager) EnsureEntrySession(name, dir, cmd string) error {
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return err
	}
	return errors.Join(m.ensureEntrySession(name, dir, cmd), release())
}

func (m *Manager) ensureEntrySession(name, dir, cmd string) error {
	isEntry := name == m.WS || strings.HasPrefix(name, m.WS+"/")
	entryPath := ""
	if isEntry {
		var err error
		entryPath, err = canonicalEntryPath(dir)
		if err != nil {
			return fmt.Errorf("resolve path for entry session %s: %w", name, err)
		}
		if err := m.validateMappedEntry(name, entryPath); err != nil {
			return err
		}
	}
	generation, err := m.ensureSessionServer("")
	if err != nil {
		return err
	}
	exists, err := m.Sess.HasSession(name)
	if err != nil {
		return fmt.Errorf("check session %s: %w", name, err)
	}
	if exists {
		if !isEntry {
			return nil
		}
		infos, err := m.Sess.Sessions()
		if err != nil {
			return fmt.Errorf("list sessions while marking %s: %w", name, err)
		}
		for _, info := range infos {
			if info.Name == name {
				return m.validateEntryInfo(name, entryPath, info)
			}
		}
		return fmt.Errorf("session %s exited before its identity marker could be checked", name)
	}
	if !isEntry {
		if _, _, err := m.Sess.NewSessionIdentity(name, dir, cmd, generation); err != nil {
			return fmt.Errorf("create session %s: %w", name, err)
		}
		return nil
	}
	id, generation, err := m.Sess.NewSessionIdentity(name, dir, cmd, generation)
	if err != nil {
		return fmt.Errorf("create session %s: %w", name, err)
	}
	if err := m.markEntrySession(name, entryPath, id, generation, ""); err != nil {
		return errors.Join(err, m.Sess.KillSessionIDIfGeneration(id, generation))
	}
	return nil
}

func (m *Manager) validateMappedEntry(name, entryPath string) error {
	paths, ok, err := state.ReadEntryPaths(m.WS)
	if err != nil {
		return fmt.Errorf("read validated entry paths before using %s: %w", name, err)
	}
	if !ok {
		return fmt.Errorf("entry path map for workspace %q is missing; relaunch before creating or restoring %s", m.WS, name)
	}
	expected, ok := paths[name]
	if !ok {
		return fmt.Errorf("entry session %s is absent from current workspace discovery; select a current row before creating it", name)
	}
	if expected != entryPath {
		return fmt.Errorf("entry session %s maps to path %s, not requested path %s; relaunch before creating it", name, expected, entryPath)
	}
	return nil
}

func (m *Manager) validateEntryInfo(name, entryPath string, info tmux.SessionInfo) error {
	if info.Kind != "" && info.Kind != tmux.SessionKindEntry {
		return fmt.Errorf("session %s carries kind %q; refusing to claim it as an entry session", name, info.Kind)
	}
	if info.EntryName != "" && info.EntryName != name {
		return fmt.Errorf("session %s carries entry marker %q; refusing to overwrite pending or conflicting identity", name, info.EntryName)
	}
	if err := m.checkEntryPath(name, entryPath, info); err != nil {
		return err
	}
	if info.Kind == tmux.SessionKindEntry &&
		info.EntryName == name &&
		info.EntryPathToken == tmux.EncodeEntryPath(entryPath) {
		return nil
	}
	return m.markEntrySession(name, entryPath, info.ID, info.Generation, info.Kind)
}

func (m *Manager) checkEntryPath(name, entryPath string, info tmux.SessionInfo) error {
	if info.EntryPathToken != "" {
		recorded, err := tmux.DecodeEntryPath(info.EntryPathToken)
		if err != nil {
			return fmt.Errorf("session %s carries an invalid entry path marker: %w", name, err)
		}
		if recorded != entryPath {
			return fmt.Errorf("session %s belongs to entry path %s, not requested path %s; shut down the stale session before reusing this entry name", name, recorded, entryPath)
		}
	} else {
		if info.ID == "" {
			return fmt.Errorf("verify unmarked session %s current path: tmux returned no stable session id", name)
		}
		currentPath, err := m.Sess.SessionCurrentPath(info.ID)
		if errors.Is(err, tmux.ErrMissingTarget) {
			return fmt.Errorf("session %s exited before its path identity could be validated", name)
		}
		if err != nil {
			return fmt.Errorf("read unmarked session %s current path: %w", name, err)
		}
		current, err := canonicalEntryPath(currentPath)
		if err != nil {
			return fmt.Errorf("verify unmarked session %s current path: %w", name, err)
		}
		rel, err := filepath.Rel(entryPath, current)
		if err != nil {
			return fmt.Errorf("compare unmarked session %s current path %s with entry path %s: %w", name, current, entryPath, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("unmarked session %s has current path %s outside requested entry path %s; shut it down before reusing this entry name", name, current, entryPath)
		}
	}
	return nil
}

// ValidateLiveEntrySessions checks every live root/repository session against
// the topology discovered for this launch, backfills safe legacy sessions,
// and only then publishes the mapping used by pane subprocesses.
func (m *Manager) ValidateLiveEntrySessions(paths map[string]string, topologyComplete bool) error {
	if !topologyComplete {
		return fmt.Errorf("entry discovery for workspace %q is incomplete; refusing to validate or publish a partial entry-path map", m.WS)
	}
	canonical := make(map[string]string, len(paths))
	for name, path := range paths {
		resolved, err := canonicalEntryPath(path)
		if err != nil {
			return fmt.Errorf("resolve discovered entry path for %s: %w", name, err)
		}
		canonical[name] = resolved
	}
	infos, err := m.Sess.Sessions()
	if err != nil {
		return fmt.Errorf("list sessions while validating workspace %q entry paths: %w", m.WS, err)
	}
	for _, info := range infos {
		if info.Name != m.WS && !strings.HasPrefix(info.Name, m.WS+"/") {
			continue
		}
		path, ok := canonical[info.Name]
		if !ok {
			return fmt.Errorf("live entry session %s has no matching path in current workspace discovery; shut it down before launching", info.Name)
		}
		if err := m.validateEntryInfo(info.Name, path, info); err != nil {
			return err
		}
	}
	for _, info := range infos {
		kind := ""
		switch {
		case config.IsTermSession(m.WS, info.Name):
			kind = tmux.SessionKindScratch
		case info.Name == config.DiffSession(m.WS):
			kind = tmux.SessionKindDiff
		default:
			continue
		}
		if err := m.migrateLegacySessionKind(info, kind); err != nil {
			return err
		}
	}
	if err := state.WriteEntryPaths(m.WS, canonical); err != nil {
		return fmt.Errorf("persist validated entry paths for workspace %q: %w", m.WS, err)
	}
	return nil
}

func (m *Manager) migrateLegacySessionKind(info tmux.SessionInfo, kind string) error {
	if info.Kind == kind {
		return nil
	}
	if info.Kind != "" {
		return fmt.Errorf("session %s carries kind %q, want %q", info.Name, info.Kind, kind)
	}
	if info.EntryName != "" || info.EntryPathToken != "" {
		return fmt.Errorf("unmarked session %s carries entry identity markers; refusing legacy %s migration", info.Name, kind)
	}
	if info.ID == "" {
		return fmt.Errorf("legacy %s session %s has no stable tmux id", kind, info.Name)
	}
	if info.Generation == "" {
		return fmt.Errorf("legacy %s session %s has no tmux server generation", kind, info.Name)
	}
	if err := m.Sess.SetUnmarkedSessionKindIfGenerationAndName(
		info.ID, info.Generation, info.Name, kind,
	); err != nil {
		return fmt.Errorf("mark legacy session %s as %s: %w", info.Name, kind, err)
	}
	return nil
}

func (m *Manager) validateSwitchTarget(target string) (tmux.SessionInfo, error) {
	isEntry := target == m.WS || strings.HasPrefix(target, m.WS+"/")
	entryPath := ""
	if isEntry {
		paths, ok, err := state.ReadEntryPaths(m.WS)
		if err != nil {
			return tmux.SessionInfo{}, fmt.Errorf("read entry paths before switching to %s: %w", target, err)
		}
		if !ok {
			return tmux.SessionInfo{}, fmt.Errorf("entry path map for workspace %q is missing; relaunch the workspace before switching to %s", m.WS, target)
		}
		entryPath, ok = paths[target]
		if !ok {
			return tmux.SessionInfo{}, fmt.Errorf("session %s has no matching path in current workspace discovery; shut it down or relaunch after restoring the entry", target)
		}
	}
	infos, err := m.Sess.Sessions()
	if err != nil {
		return tmux.SessionInfo{}, fmt.Errorf("list sessions before switching to %s: %w", target, err)
	}
	for _, info := range infos {
		if info.Name == target {
			if info.ID == "" {
				return tmux.SessionInfo{}, fmt.Errorf("session %s has no stable tmux id; refusing a name-racy switch", target)
			}
			if info.Generation == "" {
				return tmux.SessionInfo{}, fmt.Errorf("session %s has no tmux server generation; refusing a restart-racy switch", target)
			}
			if isEntry {
				if err := m.validateEntryInfo(target, entryPath, info); err != nil {
					return tmux.SessionInfo{}, err
				}
			}
			return info, nil
		}
	}
	return tmux.SessionInfo{}, fmt.Errorf("session %s exited before its stable identity could be validated", target)
}

func (m *Manager) markEntrySession(name, entryPath, id, generation, currentKind string) error {
	if id == "" {
		return fmt.Errorf("mark encoded entry session %s: tmux returned no stable session id", name)
	}
	if generation == "" {
		return fmt.Errorf("mark encoded entry session %s: tmux returned no server generation", name)
	}
	if err := m.Sess.SetSessionKindIfGenerationAndNameAndCurrentKind(
		id, generation, name, currentKind, tmux.SessionKindEntry,
	); err != nil {
		return fmt.Errorf("mark encoded entry session %s kind: %w", name, err)
	}
	if err := m.Sess.SetSessionOptionIfGeneration(id, generation, tmux.EntryNameOption, tmux.EncodeEntryName(name)); err != nil {
		return fmt.Errorf("mark encoded entry session %s: %w", name, err)
	}
	if err := m.Sess.SetSessionOptionIfGeneration(id, generation, tmux.EntryPathOption, tmux.EncodeEntryPath(entryPath)); err != nil {
		return fmt.Errorf("mark encoded entry session %s path %s: %w", name, entryPath, err)
	}
	return nil
}

func canonicalEntryPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonical path %s: %w", abs, err)
	}
	return resolved, nil
}

// MigrateLegacyEntrySessions upgrades live entry sessions from the old
// punctuation-collapsing names to the reversible names SessionName now emits.
//
// entries must be the repository/worktree row names the tree will expose.
// topologyComplete says whether all worktree queries succeeded. A live
// migration candidate is refused when it is false because an omitted row could
// share the same legacy name. All ambiguity and target collisions are
// preflighted before the first rename, so a conflict never leaves tmux
// partially migrated.
func (m *Manager) MigrateLegacyEntrySessions(entries []string, topologyComplete bool) error {
	return m.migrateLegacyEntrySessions(entries, nil, topologyComplete)
}

// MigrateLegacyEntrySessionsWithPaths adds path ownership to migration's
// preflight. Production launch uses it so no marker, name, or selection is
// mutated before every planned legacy session is proven to live within the
// current discovered entry root.
func (m *Manager) MigrateLegacyEntrySessionsWithPaths(entries []string, paths map[string]string, topologyComplete bool) error {
	return m.migrateLegacyEntrySessions(entries, paths, topologyComplete)
}

func (m *Manager) migrateLegacyEntrySessions(entries []string, paths map[string]string, topologyComplete bool) error {
	groups := map[string]map[string][]string{}
	encodedOwners := map[string][]string{}
	var legacyOrder []string
	var encodedOrder []string
	for _, entry := range entries {
		old := config.LegacySessionName(m.WS, entry)
		newName := config.SessionName(m.WS, entry)
		if _, ok := encodedOwners[newName]; !ok {
			encodedOrder = append(encodedOrder, newName)
		}
		ownerSeen := false
		for _, existing := range encodedOwners[newName] {
			if existing == entry {
				ownerSeen = true
				break
			}
		}
		if !ownerSeen {
			encodedOwners[newName] = append(encodedOwners[newName], entry)
		}
		if groups[old] == nil {
			groups[old] = map[string][]string{}
			legacyOrder = append(legacyOrder, old)
		}
		seen := false
		for _, existing := range groups[old][newName] {
			if existing == entry {
				seen = true
				break
			}
		}
		if !seen {
			groups[old][newName] = append(groups[old][newName], entry)
		}
	}
	if len(groups) == 0 {
		return nil
	}

	infos, err := m.Sess.Sessions()
	if err != nil {
		return fmt.Errorf("list sessions while migrating legacy names for workspace %q: %w", m.WS, err)
	}
	live := make(map[string]tmux.SessionInfo, len(infos))
	for _, info := range infos {
		live[info.Name] = info
		_, encodedCandidate := encodedOwners[info.Name]
		_, legacyCandidate := groups[info.Name]
		if (encodedCandidate || legacyCandidate) &&
			info.Kind != "" && info.Kind != tmux.SessionKindEntry {
			return fmt.Errorf(
				"session %q carries kind %q; refusing entry migration",
				info.Name, info.Kind,
			)
		}
	}

	sel, hasSelection, err := state.Read(m.WS)
	if err != nil {
		return fmt.Errorf("read selection while migrating legacy names for workspace %q: %w", m.WS, err)
	}

	type migrationPlan struct {
		old, newName, targetID, targetGeneration, targetKind string
	}
	var plans []migrationPlan
	selectionTarget := ""

	// Backfill sessions created by the reversible-name build before identity
	// markers existed. This runs as preflight only: marker writes happen after
	// every conflict check below, so a later ambiguity leaves tmux untouched.
	for _, newName := range encodedOrder {
		info, ok := live[newName]
		if !ok {
			continue
		}
		if info.EntryName != "" && info.EntryName != newName {
			if _, pending := groups[newName][info.EntryName]; pending {
				// The marker was written before a rename that did not land.
				// The legacy pass below validates and retries it.
				continue
			}
			return fmt.Errorf("session %q carries mismatched entry marker %q; recover it explicitly before launching", newName, info.EntryName)
		}
		if info.EntryName == newName {
			continue
		}
		// If this canonical name is also a different current entry's legacy
		// name, an empty marker does not prove which side owns the session.
		// Leave it for the selection-aware ambiguity logic below.
		collidesWithDifferentLegacyEntry := false
		for _, legacyEntries := range groups[newName] {
			for _, legacyEntry := range legacyEntries {
				isEncodedOwner := false
				for _, owner := range encodedOwners[newName] {
					if owner == legacyEntry {
						isEncodedOwner = true
						break
					}
				}
				if !isEncodedOwner {
					collidesWithDifferentLegacyEntry = true
					break
				}
			}
			if collidesWithDifferentLegacyEntry {
				break
			}
		}
		if collidesWithDifferentLegacyEntry {
			continue
		}
		selectionProvesOwner := hasSelection && sel.Session == newName &&
			config.SessionName(m.WS, sel.Entry) == newName
		if hasSelection && sel.Session == newName && !selectionProvesOwner {
			return fmt.Errorf("session %q has saved selection entry %q for a different encoded name; select or restore the owning entry before launching", newName, sel.Entry)
		}
		component := strings.TrimPrefix(newName, m.WS+"/")
		if !selectionProvesOwner && strings.ContainsAny(component, "_~") {
			// "_" may be a collapsed dot/colon/space from the legacy scheme,
			// and "~" may be a literal legacy tilde or the encoded escape.
			// Without a saved selection or marker, current topology alone is
			// not durable proof of ownership.
			continue
		}
		if !topologyComplete {
			return fmt.Errorf("session %q cannot be assigned an entry marker after incomplete entry discovery; resolve the reported repository/worktree error and retry", newName)
		}
		if info.ID == "" {
			return fmt.Errorf("session %q has no stable tmux id; retry discovery before assigning its entry marker", newName)
		}
		plans = append(plans, migrationPlan{
			old: newName, newName: newName, targetID: info.ID,
			targetGeneration: info.Generation, targetKind: info.Kind,
		})
		info.EntryName = newName
		live[newName] = info
	}

	for _, old := range legacyOrder {
		targets := groups[old]
		info, oldAlive := live[old]
		if !oldAlive {
			// If a prior run renamed tmux successfully but could not persist
			// state, a matching selection plus complete current topology proves
			// which encoded session owns it. Marker-aware builds add the marker
			// before rename; the markerless fallback covers the immediately
			// preceding build.
			if hasSelection && sel.Session == old {
				newName := config.SessionName(m.WS, sel.Entry)
				migrated, targetAlive := live[newName]
				ownerCurrent := false
				for _, owner := range encodedOwners[newName] {
					if owner == sel.Entry {
						ownerCurrent = true
						break
					}
				}
				if targetAlive && ownerCurrent && config.LegacySessionName(m.WS, sel.Entry) == old {
					if !topologyComplete {
						return fmt.Errorf("selection for migrated session %q cannot be repaired after incomplete entry discovery", newName)
					}
					switch migrated.EntryName {
					case "":
						if migrated.ID == "" {
							return fmt.Errorf("session %q has no stable tmux id; retry discovery before repairing its marker", newName)
						}
						plans = append(plans, migrationPlan{
							old: newName, newName: newName, targetID: migrated.ID,
							targetGeneration: migrated.Generation, targetKind: migrated.Kind,
						})
					case newName:
					default:
						return fmt.Errorf("session %q carries mismatched entry marker %q while repairing saved selection", newName, migrated.EntryName)
					}
					selectionTarget = newName
				}
			}
			continue
		}
		if info.EntryName == old {
			if hasSelection && sel.Session == old && config.SessionName(m.WS, sel.Entry) != old {
				return fmt.Errorf("encoded session %q has saved selection entry %q for a different session; select or restore the owning entry before launching", old, sel.Entry)
			}
			continue
		}
		if !topologyComplete {
			return fmt.Errorf("legacy session %q cannot be migrated after incomplete entry discovery; resolve the reported repository/worktree error and retry", old)
		}
		if info.EntryName != "" {
			markedTarget, pending := targets[info.EntryName]
			if !pending {
				return fmt.Errorf("session %q carries entry marker %q but current topology has no matching migration target; recover it explicitly before launching", old, info.EntryName)
			}
			targets = map[string][]string{info.EntryName: markedTarget}
		}
		if len(targets) != 1 {
			var conflicting []string
			for _, entries := range targets {
				conflicting = append(conflicting, entries...)
			}
			sort.Strings(conflicting)
			return fmt.Errorf("ambiguous legacy session %q matches entries %q; stop it before launching this workspace", old, conflicting)
		}
		var newName string
		for newName = range targets {
		}
		if info.EntryName == "" {
			var differentEncodedOwners []string
			currentEntries := targets[newName]
			for _, owner := range encodedOwners[old] {
				isCurrentLegacyTarget := false
				for _, current := range currentEntries {
					if owner == current {
						isCurrentLegacyTarget = true
						break
					}
				}
				if !isCurrentLegacyTarget {
					differentEncodedOwners = append(differentEncodedOwners, owner)
				}
			}
			if len(differentEncodedOwners) > 0 {
				selectedName := ""
				if hasSelection && sel.Session == old {
					selectedName = config.SessionName(m.WS, sel.Entry)
				}
				switch selectedName {
				case old:
					// The persisted entry proves this is an unmarked encoded
					// session, not the other row's legacy session.
					if info.ID == "" {
						return fmt.Errorf("session %q has no stable tmux id; retry discovery before assigning its entry marker", old)
					}
					plans = append(plans, migrationPlan{
						old: old, newName: old, targetID: info.ID,
						targetGeneration: info.Generation, targetKind: info.Kind,
					})
					continue
				case newName:
					// The persisted entry proves the legacy interpretation.
				default:
					sort.Strings(differentEncodedOwners)
					return fmt.Errorf("unmarked session %q could be encoded for entries %q or legacy for entries %q; select the owning entry or recover the session explicitly", old, differentEncodedOwners, currentEntries)
				}
			}
			component := strings.TrimPrefix(newName, m.WS+"/")
			selectionProvesOwner := hasSelection && sel.Session == old &&
				config.SessionName(m.WS, sel.Entry) == newName
			if newName == old && !selectionProvesOwner && strings.ContainsAny(component, "_~") {
				return fmt.Errorf("unmarked session %q has no durable ownership evidence; select the owning entry or recover the session explicitly", old)
			}
		}
		if hasSelection && sel.Session == old && config.SessionName(m.WS, sel.Entry) != newName {
			var currentEntries []string
			for _, candidateEntries := range targets {
				currentEntries = append(currentEntries, candidateEntries...)
			}
			sort.Strings(currentEntries)
			return fmt.Errorf("legacy session %q has saved selection entry %q but current entries %q map it to %q; stop the legacy session or restore the selected entry before launching", old, sel.Entry, currentEntries, newName)
		}
		if newName != old {
			if _, exists := live[newName]; exists {
				return fmt.Errorf("legacy session %q and encoded session %q both exist; stop one before launching this workspace", old, newName)
			}
		}
		if info.ID == "" {
			return fmt.Errorf("session %q has no stable tmux id; retry discovery before migrating it", old)
		}
		plans = append(plans, migrationPlan{
			old: old, newName: newName, targetID: info.ID,
			targetGeneration: info.Generation, targetKind: info.Kind,
		})
		if hasSelection && sel.Session == old && newName != old {
			selectionTarget = newName
		}
	}

	if paths != nil {
		liveByID := make(map[string]tmux.SessionInfo, len(live))
		for _, info := range live {
			liveByID[info.ID] = info
		}
		for _, plan := range plans {
			expected, ok := paths[plan.newName]
			if !ok {
				return fmt.Errorf("migration target %q has no path in current workspace discovery", plan.newName)
			}
			expected, err = canonicalEntryPath(expected)
			if err != nil {
				return fmt.Errorf("resolve migration target %q path: %w", plan.newName, err)
			}
			info, ok := liveByID[plan.targetID]
			if !ok {
				return fmt.Errorf("migration target %q with id %s exited before path preflight", plan.old, plan.targetID)
			}
			if err := m.checkEntryPath(plan.newName, expected, info); err != nil {
				return fmt.Errorf("preflight legacy session %q for %q: %w", plan.old, plan.newName, err)
			}
		}
	}

	for _, plan := range plans {
		if plan.targetGeneration == "" {
			return fmt.Errorf("session %q has no tmux server generation; retry discovery before migrating it", plan.old)
		}
		if err := m.Sess.SetSessionKindIfGenerationAndNameAndCurrentKind(
			plan.targetID, plan.targetGeneration, plan.old,
			plan.targetKind, tmux.SessionKindEntry,
		); err != nil {
			return fmt.Errorf("mark legacy session %q kind for migration to %q: %w", plan.old, plan.newName, err)
		}
		if err := m.Sess.SetSessionOptionIfGeneration(plan.targetID, plan.targetGeneration, tmux.EntryNameOption, tmux.EncodeEntryName(plan.newName)); err != nil {
			return fmt.Errorf("mark legacy session %q for migration to %q: %w", plan.old, plan.newName, err)
		}
		if plan.old == plan.newName {
			continue
		}
		if err := m.Sess.RenameSessionIDIfGeneration(plan.targetID, plan.targetGeneration, plan.newName); err != nil {
			return fmt.Errorf("migrate legacy session %q to %q: %w", plan.old, plan.newName, err)
		}
	}
	if selectionTarget != "" {
		sel.Session = selectionTarget
		if err := state.Write(m.WS, sel); err != nil {
			return fmt.Errorf("session migrated to %q but selection update failed: %w", selectionTarget, err)
		}
	}
	return nil
}

// middlePane finds the terminal pane by its wrap-owned role marker rather
// than its index. Matching a marker instead of a fixed index is what makes this
// discovery layout-proof — the tree_side flip (see LaunchUI) assigns the
// terminal pane a different index depending on which side it renders on,
// while paths and shell rendering make start-command matching fragile.
func (m *Manager) middlePane() (tmux.PaneInfo, error) {
	panes, err := m.UI.Panes(m.uiWindow())
	if err != nil {
		return tmux.PaneInfo{}, err
	}
	want := shq(m.Exe) + " attach " + shq(m.WS)
	for _, p := range panes {
		if p.Role == "terminal" || p.Role == "" && p.Command == want {
			return p, nil
		}
	}
	return tmux.PaneInfo{}, fmt.Errorf("terminal pane role not found in %s", m.uiWindow())
}

// middleTTY looks up the terminal pane's client tty. The terminal is no
// longer the middle pane of a three-across row, but it keeps the "middle"
// name here since it's still the one pane every other pane points the
// nested client at.
func (m *Manager) middleTTY() (string, error) {
	pane, err := m.middlePane()
	if err != nil {
		return "", err
	}
	return pane.TTY, nil
}

// ShowInMiddle points the middle pane's nested client at target, without
// touching pane focus. Used by callers (the terms pane) that switch what
// the middle pane displays but want focus to stay where it is.
func (m *Manager) ShowInMiddle(target string) error {
	identity, err := m.validateSwitchTarget(target)
	if err != nil {
		return err
	}
	tty, err := m.middleTTY()
	if err != nil {
		return err
	}
	err = m.Sess.SwitchClientIfGeneration(tty, identity.ID, identity.Generation)
	if err != nil {
		return err
	}
	// Deliberately switching to a session is what "seen" means here. wrap
	// owns this flag precisely because tmux's own one clears on mere
	// attachment, which is the condition the terminal pane is always in —
	// so it can only be cleared by an actual act of looking.
	//
	// The switch has already happened by this point and is not undone by a
	// clear failure. The error is still returned rather than dropped: a
	// bell that cannot be cleared stays lit forever, and a session that
	// permanently claims to need attention is worse than a visible error.
	err = m.AcknowledgeSession(identity.ID, identity.Generation)
	if err != nil {
		return fmt.Errorf("showing %s: bell not cleared: %w", target, err)
	}
	return nil
}

// AcknowledgeSession clears wrap's durable attention flag for exactly one
// generation-guarded session without switching or focusing a desktop client.
func (m *Manager) AcknowledgeSession(id, generation string) error {
	return m.Sess.SetSessionOptionIfGeneration(id, generation, tmux.BellOption, "0")
}

// SwitchMiddle points the middle pane's nested client at target and
// focuses the middle pane. Used by the tree pane, which owns focus
// transfer on selection.
func (m *Manager) SwitchMiddle(target string) error {
	if err := m.ShowInMiddle(target); err != nil {
		return err
	}
	pane, err := m.middlePane()
	if err != nil {
		return err
	}
	return m.UI.SelectPane(pane.ID)
}

// KillScratchSession is the pane-3 lifecycle boundary. It accepts only free
// terminals owned by this workspace; repository, worktree, root, and diff
// sessions stay visible in the terminal monitor but cannot be killed there.
func (m *Manager) KillScratchSession(name, targetID, targetGeneration, successor string) error {
	if !config.IsTermSession(m.WS, name) {
		return errors.New("only scratch terminals can be killed")
	}
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return err
	}
	return errors.Join(m.killEntrySession(name, targetID, targetGeneration, successor, true), release())
}

// KillEntrySession kills the stable tmux session ID captured when the user
// requested confirmation. The display name is retained only for explaining
// errors and deciding whether the terminal pane needs redirecting. If that
// pane was showing the target, it lands on successor.
//
// The "was it showing?" question MUST be asked before the kill. With
// detach-on-destroy off, tmux moves the client the instant the session
// dies, so asking afterwards always reports some other session, the
// comparison never matches, and the redirect is unreachable. That left the
// client wherever tmux's own MRU put it — and because every workspace
// shares one session server, that could be (and was) another workspace's
// terminal, which reads to the user as having killed the wrong window.
//
// A successor that has since died, is the session being killed, or was not
// supplied falls back to this workspace's root session, then to the shared
// home session.
func (m *Manager) KillEntrySession(name, targetID, targetGeneration, successor string) error {
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return err
	}
	return errors.Join(m.killEntrySession(name, targetID, targetGeneration, successor, false), release())
}

func (m *Manager) killEntrySession(name, targetID, targetGeneration, successor string, requireSameName bool) error {
	if targetID == "" {
		return fmt.Errorf("kill session %s: missing stable session id", name)
	}
	if targetGeneration == "" {
		return fmt.Errorf("kill session %s: missing session-server generation", name)
	}
	tty, ttyErr := m.middleTTY()
	var showingID string
	var showErr error
	if ttyErr == nil {
		_, showingID, showErr = m.Sess.ClientSessionIdentity(tty)
	}
	var killErr error
	if requireSameName {
		killErr = m.Sess.KillSessionIDIfGenerationAndNameAndKind(
			targetID, targetGeneration, name, tmux.SessionKindScratch,
		)
	} else {
		killErr = m.Sess.KillSessionIDIfGeneration(targetID, targetGeneration)
	}
	if killErr != nil {
		if errors.Is(killErr, tmux.ErrSessionIdentityChanged) {
			return fmt.Errorf("kill session %s: session changed after confirmation; nothing was killed: %w", name, killErr)
		}
		if errors.Is(killErr, tmux.ErrServerGenerationChanged) {
			return fmt.Errorf("kill session %s: session server restarted after confirmation; nothing was killed: %w", name, killErr)
		}
		return fmt.Errorf("kill session %s by stable ID: %w", name, killErr)
	}
	if ttyErr != nil {
		return fmt.Errorf("session %s killed but terminal pane redirect failed: %w", name, ttyErr)
	}
	if showErr == nil && showingID != targetID {
		// Known not to be showing the killed session: leave the terminal
		// pane exactly where it is.
		return nil
	}
	// Either it WAS showing the killed session, or the read failed and we
	// cannot tell. Redirect either way: a needless switch costs the user
	// one keystroke to undo, whereas skipping a needed one leaves the
	// client wherever tmux's MRU dropped it — which is the bug this
	// function exists to prevent, and it would come back on any transient
	// tmux hiccup.
	landing, landingErr := m.landingAfterKill(name, successor)
	if landing == "" {
		return landingErr
	}
	identity, validationErr := m.validateSwitchTarget(landing)
	if validationErr != nil {
		// Never attach a stale entry merely because it was chosen as the
		// post-kill landing. Skip the workspace root as well and move to the
		// shared non-entry fallback, while still surfacing the identity
		// failure that forced the detour.
		safe, safeErr := m.landingAfterKill(m.WS, "")
		landingErr = errors.Join(landingErr, validationErr, safeErr)
		if safe == "" {
			return landingErr
		}
		landing = safe
		identity, validationErr = m.validateSwitchTarget(landing)
		if validationErr != nil {
			return errors.Join(landingErr, validationErr)
		}
	}
	switchErr := m.Sess.SwitchClientIfGeneration(tty, identity.ID, identity.Generation)
	return errors.Join(landingErr, switchErr)
}

// landingAfterKill picks where the terminal pane goes once the session it
// was showing is gone.
func (m *Manager) landingAfterKill(killed, successor string) (string, error) {
	var checkErrs []error
	if successor != "" && successor != killed {
		exists, err := m.Sess.HasSession(successor)
		if err != nil {
			checkErrs = append(checkErrs, fmt.Errorf("check successor session %s: %w", successor, err))
		} else if exists {
			return successor, errors.Join(checkErrs...)
		}
	}
	if m.WS != killed {
		exists, err := m.Sess.HasSession(m.WS)
		if err != nil {
			checkErrs = append(checkErrs, fmt.Errorf("check workspace session %s: %w", m.WS, err))
		} else if exists {
			return m.WS, errors.Join(checkErrs...)
		}
	}
	// HomeSession is the last resort and is normally guaranteed by
	// EnsureSessionServer, but it can be killed by hand — recreate it
	// rather than switch the client to a session that isn't there.
	hasHome, err := m.Sess.HasSession(HomeSession)
	if err != nil {
		return "", errors.Join(append(checkErrs, fmt.Errorf("check fallback session %s: %w", HomeSession, err))...)
	}
	if !hasHome {
		if _, err := m.ensureHomeSession(""); err != nil {
			return "", errors.Join(append(checkErrs, fmt.Errorf("prepare fallback session %s: %w", HomeSession, err))...)
		}
		// Configure whenever the home session was absent, not only when this
		// pane created it. On a concurrent create ensureHomeSession reports
		// created=false, but the racing winner may not have finished
		// configuring a freshly restarted server — redirecting a client to an
		// unconfigured generation would fail validation. configureSessionServer
		// is idempotent, so a redundant configure by the losing pane is safe.
		if _, err := m.configureSessionServer(); err != nil {
			return HomeSession, errors.Join(append(checkErrs, fmt.Errorf("configure fallback session %s: %w", HomeSession, err))...)
		}
	}
	return HomeSession, errors.Join(checkErrs...)
}

func (m *Manager) Sessions() ([]tmux.SessionInfo, error) {
	return m.Sess.Sessions()
}

func (m *Manager) WriteSelection(ws string, sel state.Selection) error {
	if ws != m.WS {
		return fmt.Errorf("write selection for workspace %q through manager for %q", ws, m.WS)
	}
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return err
	}
	return errors.Join(state.Write(ws, sel), release())
}

// DisplayedSession reports the session the nested client in the terminal pane
// is actually showing. Persisted selection is a tree choice, not display
// ground truth: tmux can redirect the client after a kill or another viewer
// can switch it independently.
func (m *Manager) DisplayedSession() (string, error) {
	tty, err := m.middleTTY()
	if err != nil {
		return "", err
	}
	name, err := m.Sess.ClientSession(tty)
	if err != nil {
		return "", fmt.Errorf("read displayed session: %w", err)
	}
	return name, nil
}

// workspaceTitle is what the outer terminal's tab/window shows for a
// workspace. The bell leads so it survives a truncated tab label.
//
// The name is escaped because set-titles-string is a tmux FORMAT, not a
// literal: "#" introduces a substitution and "#(...)" runs a shell command.
// A workspace name is filepath.Base of a directory the user pointed wrap
// at, so a directory named "#(curl … | sh)" would otherwise execute when
// tmux rendered the tab label. "##" is tmux's escape for a literal "#".
func workspaceTitle(ws string, alert bool) string {
	safe := strings.ReplaceAll(ws, "#", "##")
	if alert {
		return "🔔 wrap: " + safe
	}
	return "wrap: " + safe
}

// SetWorkspaceAlert raises or clears the persistent workspace-level
// attention signal in the OUTER terminal's title.
//
// The per-session 🔔 in the terminals monitor is only visible while you are
// looking at that workspace. Running several wraps across tabs or windows,
// the one that needs you is usually the one you are not looking at — so the
// signal has to escalate somewhere the window manager shows: the title.
func (m *Manager) SetWorkspaceAlert(alert bool) error {
	return m.UI.SetSessionOption(m.UISession(), "set-titles-string", workspaceTitle(m.WS, alert))
}

// RingWorkspaceAlert emits the separate one-shot attention signal. Keeping
// it independent from SetWorkspaceAlert lets the monitor retry a failed
// persistent title update without ringing healthy terminals every two
// seconds until the title server recovers.
func (m *Manager) RingWorkspaceAlert() error {
	return m.ringOuterTerminal()
}

// ringOuterTerminal writes a BEL to every real terminal displaying this
// workspace, so the alert reaches the window manager even when the title
// does not.
//
// The title alone is not enough. Terminals let you rename a tab by hand,
// and at least Ghostty then pins that name and ignores the escape wrap
// sends — so the one signal is silently dead for exactly the user who
// cared enough to label their tabs. A bell has no such override: every
// terminal has native attention UI for it (Ghostty's bell-features,
// iTerm2's tab indicator), which is what makes this the portable half.
//
// It has to be written to the client tty directly. Letting the bell
// propagate outward through tmux does not work: each server swallows it
// when a client is viewing the window that rang — the same viewer-relative
// rule that made bells invisible in the first place, one layer up.
func (m *Manager) ringOuterTerminal() error {
	ttys, err := m.UI.ClientTTYs(m.UISession())
	if err != nil {
		return fmt.Errorf("ring terminal: %w", err)
	}
	var errs []error
	for _, tty := range ttys {
		if err := ringTTY(tty); err != nil {
			errs = append(errs, fmt.Errorf("ring %s: %w", tty, err))
		}
	}
	return errors.Join(errs...)
}

// ringTTY writes one BEL to tty without ever blocking.
//
// Nonblocking is the whole point: this runs inside the terminals pane's
// Update, so a terminal that has stopped draining its output — flow
// control, a stopped process, an emulator wedged behind a modal — must
// not be able to freeze the monitor for everyone else. A dropped bell is
// a far smaller failure than a frozen pane, so EAGAIN comes back as an
// error and the caller reports it.
//
// The raw syscalls are deliberate. os.OpenFile may hand the descriptor to
// the runtime poller depending on platform and file type, and the poller
// answers EAGAIN by parking the goroutine until the tty drains — which is
// exactly the block being avoided here, just harder to see.
func ringTTY(tty string) error {
	var fd int
	var err error
	for {
		fd, err = syscall.Open(tty, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return err
	}
	var werr error
	for {
		if _, werr = syscall.Write(fd, []byte{'\a'}); werr != syscall.EINTR {
			break
		}
	}
	// Close is not retried on EINTR: POSIX leaves the descriptor's state
	// unspecified afterwards, so a retry can close one that a later open
	// has already been handed.
	return errors.Join(werr, syscall.Close(fd))
}

// ShutdownWorkspace tears down this workspace entirely: every session it
// owns is killed, its persisted selection is cleared, and — LAST —
// its own chrome session is killed. That final call terminates the
// calling pane's process; on success, the caller does not outlive it.
//
// Individual session-kill failures are collected rather than aborting
// the sweep (one stuck session shouldn't leave its siblings running),
// but if the sweep or the state clear produced ANY error, the chrome
// kill is skipped entirely and the joined error is returned instead:
// killing the chrome first would make that error unreachable (the pane
// reporting it would already be gone). Only a fully clean sweep proceeds
// to kill the chrome — partial shutdown leaves the pane alive so the
// user can see what failed and retry.
func (m *Manager) ShutdownWorkspace() error {
	release, err := state.LockWorkspace(m.WS)
	if err != nil {
		return fmt.Errorf("lock workspace %q shutdown: %w", m.WS, err)
	}
	if err := state.BeginShutdown(m.WS); err != nil {
		return errors.Join(err, release())
	}
	var errs []error
	infos, err := m.Sessions()
	if err != nil && !errors.Is(err, tmux.ErrNoServer) {
		errs = append(errs, err)
	}
	for _, info := range infos {
		if !config.SessionOwnedBy(m.WS, info.Name) {
			continue
		}
		if info.ID == "" {
			errs = append(errs, fmt.Errorf("kill %s: tmux returned an empty session id", info.Name))
			continue
		}
		if info.Generation == "" {
			errs = append(errs, fmt.Errorf("kill %s: tmux returned an empty server generation", info.Name))
			continue
		}
		if err := m.Sess.KillSessionIDIfGeneration(info.ID, info.Generation); err != nil {
			errs = append(errs, fmt.Errorf("kill %s: %w", info.Name, err))
		}
	}
	remaining, verifyErr := m.Sessions()
	if verifyErr != nil && !errors.Is(verifyErr, tmux.ErrNoServer) {
		errs = append(errs, fmt.Errorf("verify workspace sessions stopped: %w", verifyErr))
	}
	for _, info := range remaining {
		if config.SessionOwnedBy(m.WS, info.Name) {
			errs = append(errs, fmt.Errorf("session %s remains after shutdown sweep", info.Name))
		}
	}
	if err := state.ClearSelection(m.WS); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		// Partial failure: the chrome stays alive so the user sees what
		// broke. Clear the barrier too — the workspace is NOT going away,
		// and leaving it set would refuse every later mutation (and every
		// retry) until a full relaunch. A clean sweep below deliberately
		// leaves the barrier set: that workspace really is gone.
		return errors.Join(errors.Join(errs...), state.ClearShutdown(m.WS), release())
	}
	return errors.Join(m.UI.KillSession(m.UISession()), release())
}

// NewTerm creates the next free terminal (<ws>·term·<n>) at dir running
// cmd (empty = login shell) and returns its session name.
func (m *Manager) NewTerm(dir, cmd string) (string, error) {
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return "", err
	}
	name, createErr := m.newTerm(dir, cmd)
	return name, errors.Join(createErr, release())
}

func (m *Manager) newTerm(dir, cmd string) (string, error) {
	// Before anything else, as EnsureEntrySession does. The session server
	// can vanish while a workspace is still open (reboot, crash, a stray
	// kill-server), and Sessions() reports that as an empty list rather
	// than an error — so without this, new-session below would let tmux
	// auto-start a bare server carrying none of wrap's settings, and every
	// terminal made afterward would silently stop ringing.
	generation, err := m.ensureSessionServer("")
	if err != nil {
		return "", err
	}
	infos, err := m.Sess.Sessions()
	if err != nil {
		return "", err
	}
	prefix := config.TermPrefix(m.WS)
	max := 0
	for _, s := range infos {
		if n, ok := strings.CutPrefix(s.Name, prefix); ok {
			if v, err := strconv.Atoi(n); err == nil && v > max {
				max = v
			}
		}
	}
	name := fmt.Sprintf("%s%d", prefix, max+1)
	id, sessionGeneration, err := m.Sess.NewSessionIdentity(name, dir, cmd, generation)
	if err != nil {
		return "", fmt.Errorf("create terminal %s: %w", name, err)
	}
	if err := m.Sess.SetSessionOptionIfGeneration(
		id, sessionGeneration, tmux.SessionKindOption, tmux.SessionKindScratch,
	); err != nil {
		return "", errors.Join(
			fmt.Errorf("mark terminal %s as scratch: %w", name, err),
			m.Sess.KillSessionIDIfGeneration(id, sessionGeneration),
		)
	}
	return name, nil
}

// RenameTerm renames a scratch terminal (<ws>·term·N) to a slugified
// label. Only sessions under that prefix can be renamed — entry sessions
// (repo/worktree) are off-limits. Renaming to the current name is a
// no-op; renaming onto an existing session name is refused.
func (m *Manager) RenameTerm(oldName, targetID, targetGeneration, label string) (string, error) {
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return "", err
	}
	name, renameErr := m.renameTerm(oldName, targetID, targetGeneration, label)
	return name, errors.Join(renameErr, release())
}

func (m *Manager) renameTerm(oldName, targetID, targetGeneration, label string) (string, error) {
	prefix := config.TermPrefix(m.WS)
	if !strings.HasPrefix(oldName, prefix) {
		return "", errors.New("only scratch terminals can be renamed")
	}
	slug := slugify(label)
	if slug == "" {
		return "", errors.New("empty name")
	}
	newName := prefix + slug
	if newName == oldName {
		return oldName, nil
	}
	infos, err := m.Sess.Sessions()
	if err != nil {
		return "", fmt.Errorf("list terminals before renaming %s: %w", oldName, err)
	}
	for _, info := range infos {
		if info.Name == newName {
			return "", fmt.Errorf("a terminal named %q already exists", newName)
		}
	}
	if targetID == "" || targetGeneration == "" {
		return "", fmt.Errorf("terminal %s exited before its stable identity could be captured", oldName)
	}
	if err := m.Sess.RenameSessionIDIfGenerationAndNameAndKind(
		targetID, targetGeneration, oldName, tmux.SessionKindScratch, newName,
	); err != nil {
		return "", fmt.Errorf("rename terminal %s: %w", oldName, err)
	}
	return newName, nil
}

// slugify sanitizes label into a safe tmux session-name fragment: any
// rune outside [A-Za-z0-9_-] becomes '-', with leading/trailing '-'
// trimmed. Case is preserved.
func slugify(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// DetachUI detaches every client attached to the chrome ("quit wrap").
func (m *Manager) DetachUI() error {
	_, err := m.UI.Run("detach-client", "-s", m.UISession())
	return err
}

// LaunchUI reattaches to the 3-pane chrome if it's alive AND was built
// with these exact params; otherwise it (re)builds the chrome before
// returning. p is the freshly-resolved launch params: p.TreeSide/
// p.TreeWidth/p.Keys shape the pane build itself, while p.Cmd and
// p.WalkDepth don't — no split or bind below reads them — but they're
// part of ChromeParams anyway because pane subprocesses (sidebar/attach/
// watch) bake them in at spawn, so a config change to either only takes
// effect if the panes are respawned and must still count as a
// build-affecting difference here. A missing chrome session, a dead pane,
// missing stored params (upgrade path), or any param mismatch all take
// the same rebuild branch: kill the existing chrome session if present,
// build fresh with p.TreeSide/p.TreeWidth/p.Keys, then persist p via
// state.WriteChromeParams so the next run can compare against it.
//
// Layout: two columns, one holding the tree over the terms/terminals-
// monitor pane, the other the terminal, full height. p.TreeSide picks
// which column is which; p.TreeWidth is the tree/terms column's width as
// a percent of the window (the terminal column always gets the rest).
// Build order matters for the resulting pane indices — tmux inserts a new
// pane immediately after its split target and renumbers the whole window
// by that list order.
//
// LEFT (p.TreeSide != "right"): tree+terms on the left, terminal on the
// right.
//
//  1. new-session             -> [tree]                    tree=0
//  2. split -h -t 0.0 (tree)     -> [tree, terminal]        tree=0 terminal=1
//  3. split -v -t 0.0 (tree)     -> [tree, terms, terminal] tree=0 terms=1 terminal=2
//
// So after step 3: tree=0, terms=1, terminal=2.
//
// RIGHT (p.TreeSide == "right"): mirrored — terminal on the left,
// tree+terms on the right. Adding "-b" ("before") to the first split flips
// both the new pane's position AND its index relative to the split
// target, which is exactly the mirror image of the left sequence:
//
//  1. new-session                -> [tree]                       tree=0
//  2. split -h -b -t 0.0 (tree)     -> [terminal, tree]           terminal=0 tree=1
//  3. split -v -t 0.1 (tree, now index 1) -> [terminal, tree, terms] terminal=0 tree=1 terms=2
//
// So after step 3: terminal=0, tree=1, terms=2. In both layouts "-l"
// always sizes the NEW pane regardless of -b, so the terminal's
// (100-p.TreeWidth)% width applies identically to both split-window -h
// invocations.
func (m *Manager) LaunchUI(p state.ChromeParams) error {
	// Normalize BEFORE the stored-params comparison below, so the value
	// compared and the value persisted are the same one the chrome is
	// actually built with. Defaulting after the comparison instead would
	// store defaulted keys, compare them against undefaulted ones next
	// run, and rebuild the chrome on every launch.
	p.Keys = p.Keys.WithDefaults()
	if p.TreeSide != "right" {
		p.TreeSide = "left"
	}
	if p.TreeWidth <= 0 || p.TreeWidth >= 100 {
		p.TreeWidth = 25
	}
	// Callers describe the workspace; the build version is wrap's own, so
	// it is stamped here rather than plumbed through every call site.
	p.Build = state.ChromeBuild
	stored, storedOK, storedErr := state.ReadChromeParams(m.WS)
	var previousKeys config.Keys
	if storedErr == nil && storedOK {
		previousKeys = stored.Keys.WithDefaults()
	}
	uiAlive, err := m.UI.HasSession(m.UISession())
	if err != nil {
		return fmt.Errorf("check UI session %s: %w", m.UISession(), err)
	}
	var previousUIPID string
	if uiAlive {
		previousUIPID, err = m.UI.ServerPID()
		if err != nil {
			var recheckErr error
			uiAlive, recheckErr = m.UI.HasSession(m.UISession())
			if recheckErr != nil {
				return errors.Join(
					fmt.Errorf("identify live UI server: %w", err),
					fmt.Errorf("recheck UI session %s: %w", m.UISession(), recheckErr),
				)
			}
			if uiAlive {
				return fmt.Errorf("identify live UI server: %w", err)
			}
			previousUIPID = ""
		}
	} else {
		previousUIPID, err = m.UI.ServerPID()
		if errors.Is(err, tmux.ErrNoServer) {
			previousUIPID = ""
		} else if err != nil {
			return fmt.Errorf("identify shared UI server: %w", err)
		}
	}
	if uiAlive {
		panes, err := m.UI.Panes(m.uiWindow())
		if err != nil {
			if !errors.Is(err, tmux.ErrMissingTarget) {
				return fmt.Errorf("inspect live chrome panes: %w", err)
			}
			// A missing window is confirmed broken chrome and should
			// self-heal. Recheck the session because the whole UI server
			// may have exited between HasSession and Panes; in that case
			// there is nothing left to kill before rebuilding.
			uiAlive, err = m.UI.HasSession(m.UISession())
			if err != nil {
				return fmt.Errorf("recheck UI session %s: %w", m.UISession(), err)
			}
		}
		if err == nil && len(panes) == 3 {
			if storedErr != nil {
				return fmt.Errorf("read live chrome params: %w", storedErr)
			}
			if storedOK && stored == p {
				// Focus bindings belong to the shared UI server, not this
				// workspace's chrome session. Another live workspace may
				// have changed them since these params were persisted, so
				// reconcile them even on the healthy-chrome fast path.
				if err := m.configureFocusKeys(p.Keys, previousKeys, true); err != nil {
					return err
				}
				return nil
			}
		}
		// Either a pane died (its process exited), the stored params are
		// missing (upgrade path) or stale, or the config changed: tear
		// down and rebuild the chrome.
		if uiAlive {
			if err := m.UI.KillSession(m.UISession()); err != nil {
				return fmt.Errorf("rebuild chrome: %w", err)
			}
		}
	}
	treeSide, treeWidth := p.TreeSide, p.TreeWidth
	termPct := fmt.Sprintf("%d%%", 100-treeWidth)
	k := p.Keys

	var terminalIdx int
	var steps [][]string
	if treeSide == "right" {
		terminalIdx = 0
		steps = [][]string{
			{"new-session", "-d", "-s", m.UISession(), "-x", "220", "-y", "60", shq(m.Exe) + " sidebar " + shq(m.WS)},
			{"split-window", "-h", "-b", "-t", m.paneTarget(0), "-l", termPct, shq(m.Exe) + " attach " + shq(m.WS)},
			{"split-window", "-v", "-t", m.paneTarget(1), "-l", "30%", shq(m.Exe) + " watch " + shq(m.WS)},
		}
	} else {
		terminalIdx = 2
		steps = [][]string{
			{"new-session", "-d", "-s", m.UISession(), "-x", "220", "-y", "60", shq(m.Exe) + " sidebar " + shq(m.WS)},
			{"split-window", "-h", "-t", m.paneTarget(0), "-l", termPct, shq(m.Exe) + " attach " + shq(m.WS)},
			{"split-window", "-v", "-t", m.paneTarget(0), "-l", "30%", shq(m.Exe) + " watch " + shq(m.WS)},
		}
	}
	steps = append(steps,
		[]string{"set-option", "-p", "-t", m.paneTarget(terminalIdx), "@wrap_role", "terminal"},
		[]string{"set-option", "-g", "status", "off"},
		[]string{"set-option", "-g", "prefix", "C-q"},
		[]string{"set-option", "-g", "mouse", "on"},
		[]string{"set-option", "-g", "set-clipboard", "on"},
		// Drive the OUTER terminal's title from this chrome, so a
		// workspace sitting in a background tab or window can still say
		// it wants attention. Session-scoped, so concurrent workspaces
		// each title their own tab.
		[]string{"set-option", "-t", m.UISession(), "set-titles", "on"},
		[]string{"set-option", "-t", m.UISession(), "set-titles-string", workspaceTitle(m.WS, false)},
		// Key tables are server-global, while pane indices depend on this
		// session's mirrored layout. Keep the bindings identical across
		// every workspace and resolve the target from a session-scoped
		// option at keypress time.
		[]string{"set-option", "-t", m.UISession(), "@wrap_tree_side", treeSide},
	)
	for _, s := range steps {
		if _, err := m.UI.Run(s...); err != nil {
			return fmt.Errorf("build chrome (%s): %w", strings.Join(s, " "), err)
		}
	}
	cleanupHistorical := false
	if previousUIPID != "" {
		currentUIPID, err := m.UI.ServerPID()
		if err != nil {
			return fmt.Errorf("identify rebuilt UI server: %w", err)
		}
		cleanupHistorical = currentUIPID == previousUIPID
	}
	if err := m.configureFocusKeys(k, previousKeys, cleanupHistorical); err != nil {
		return err
	}
	return state.WriteChromeParams(m.WS, p)
}

func (m *Manager) configureFocusKeys(k, previous config.Keys, cleanupHistorical bool) error {
	release, err := state.LockUIServer()
	if err != nil {
		return fmt.Errorf("lock shared UI focus keys: %w", err)
	}
	return errors.Join(m.configureFocusKeysLocked(k, previous, cleanupHistorical), release())
}

func (m *Manager) configureFocusKeysLocked(k, previous config.Keys, cleanupHistorical bool) error {
	bindings := []struct {
		option string
		key    string
		oldKey string
		args   []string
	}{
		{focusTreeOption, k.FocusTree, previous.FocusTree, []string{"if-shell", "-F", "#{==:#{@wrap_tree_side},right}", "select-pane -t .1", "select-pane -t .0"}},
		{focusTerminalOption, k.FocusTerminal, previous.FocusTerminal, []string{"if-shell", "-F", "#{==:#{@wrap_tree_side},right}", "select-pane -t .0", "select-pane -t .2"}},
		{focusTermsOption, k.FocusTerms, previous.FocusTerms, []string{"if-shell", "-F", "#{==:#{@wrap_tree_side},right}", "select-pane -t .2", "select-pane -t .1"}},
	}
	newKeys := map[string]bool{}
	for _, binding := range bindings {
		newKeys[binding.key] = true
	}
	oldKeys := map[string]bool{}
	for _, binding := range bindings {
		old, err := m.UI.Run("show-options", "-gvq", binding.option)
		if err != nil {
			return fmt.Errorf("read previous focus key %s: %w", binding.option, err)
		}
		live := strings.TrimSpace(old)
		if live != "" && !newKeys[live] {
			oldKeys[live] = true
		}
		// A persisted key belongs to an earlier view of this workspace.
		// It is safe to unbind only when the UI server itself survived.
		if cleanupHistorical && binding.oldKey != "" && !newKeys[binding.oldKey] {
			oldKeys[binding.oldKey] = true
		}
	}
	for old := range oldKeys {
		if err := m.UI.UnbindKey(old); err != nil {
			return fmt.Errorf("unbind previous focus key %s: %w", old, err)
		}
	}
	for _, binding := range bindings {
		args := append([]string{"bind-key", "-n", binding.key}, binding.args...)
		if _, err := m.UI.Run(args...); err != nil {
			return fmt.Errorf("bind focus key %s: %w", binding.key, err)
		}
		if _, err := m.UI.Run("set-option", "-g", binding.option, binding.key); err != nil {
			return fmt.Errorf("record focus key %s: %w", binding.key, err)
		}
	}
	return nil
}

// AttachUI replaces this process with the chrome's tmux client.
func (m *Manager) AttachUI() error {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found in PATH: %w", err)
	}
	return syscall.Exec(tmuxPath,
		[]string{"tmux", "-f", os.DevNull, "-L", tmux.SocketUI, "attach-session", "-t", "=" + m.UISession()},
		cleanEnv(os.Environ()))
}

// diffCommand builds the `git diff` invocation for ShowDiff: untracked
// files diff against /dev/null (git has no tracked blob to compare),
// staged files diff --cached, everything else a plain working-tree diff.
// root and rel are shell-quoted with shq since the result is spliced into
// a single shell command line, same as LaunchUI's pane commands.
// diffCommand builds the pager command for one changed file.
//
// --literal-pathspecs stops git treating the filename as a glob. Without
// it a file called "a*b.txt" also matches "aQQb.txt", so the pager shows
// diffs for files the user did not click. The untracked form takes two
// real paths rather than pathspecs, so it does not need the flag — but it
// carries it anyway rather than leave the reader wondering which forms are
// glob-safe.
func diffCommand(root, rel string, staged, untracked bool) string {
	const git = "env GIT_OPTIONAL_LOCKS=0 git -c core.fsmonitor=false -c core.hooksPath=/dev/null"
	switch {
	case untracked:
		return fmt.Sprintf("%s -C %s --literal-pathspecs diff --no-ext-diff --no-textconv --color --no-index -- /dev/null %s", git, shq(root), shq(rel))
	case staged:
		return fmt.Sprintf("%s -C %s --literal-pathspecs diff --no-ext-diff --no-textconv --color --cached -- %s", git, shq(root), shq(rel))
	default:
		return fmt.Sprintf("%s -C %s --literal-pathspecs diff --no-ext-diff --no-textconv --color -- %s", git, shq(root), shq(rel))
	}
}

// ShowDiff opens a one-shot pager session, "<ws>·diff", in the middle
// pane: diffCommand's output piped through `less -R` (so ANSI color
// survives). If a prior "<ws>·diff" session is still alive — e.g. the
// user didn't cleanly quit last time — its stable ID is killed first so
// every diff starts from a fresh pager. A concurrent exit is clean, but a
// transport or permission failure stops replacement rather than hiding
// uncertain cleanup behind a later new-session error.
//
// SwitchMiddle is used rather than ShowInMiddle: the pager wants focus,
// since reading a diff and quitting it is the whole point of opening one.
// Quitting `less` ends the pane's process, which ends the session
// (nothing here sets detach-on-destroy for it — this is deliberately
// one-shot); the middle client then MRU-hops back to whatever session it
// was showing before via tmux's own client-session history. That hop is
// tmux's built-in behavior on the destroyed session's target — no code
// here does it or needs to.
func (m *Manager) ShowDiff(repoRoot, relPath string, staged, untracked bool) error {
	release, err := m.beginWorkspaceMutation()
	if err != nil {
		return err
	}
	return errors.Join(m.showDiff(repoRoot, relPath, staged, untracked), release())
}

func (m *Manager) showDiff(repoRoot, relPath string, staged, untracked bool) error {
	generation, err := m.ensureSessionServer("")
	if err != nil {
		return err
	}
	fullCmd := diffCommand(repoRoot, relPath, staged, untracked) + " | less -R"
	name := config.DiffSession(m.WS)
	infos, err := m.Sess.Sessions()
	if err != nil {
		return fmt.Errorf("list sessions before replacing diff session %s: %w", name, err)
	}
	for _, info := range infos {
		if info.Name != name {
			continue
		}
		if info.Kind == "" {
			if err := m.migrateLegacySessionKind(info, tmux.SessionKindDiff); err != nil {
				return fmt.Errorf("replace diff session %s: %w", name, err)
			}
			info.Kind = tmux.SessionKindDiff
		}
		if info.Kind != tmux.SessionKindDiff {
			return fmt.Errorf("replace diff session %s: existing session has kind %q", name, info.Kind)
		}
		if info.ID == "" {
			return fmt.Errorf("replace diff session %s: tmux returned an empty session id", name)
		}
		if info.Generation == "" {
			return fmt.Errorf("replace diff session %s: tmux returned an empty server generation", name)
		}
		if err := m.Sess.KillSessionIDIfGeneration(info.ID, info.Generation); err != nil {
			return fmt.Errorf("replace diff session %s: %w", name, err)
		}
		break
	}
	id, sessionGeneration, err := m.Sess.NewSessionIdentity(name, repoRoot, fullCmd, generation)
	if err != nil {
		return fmt.Errorf("create diff session %s: %w", name, err)
	}
	if err := m.Sess.SetSessionOptionIfGeneration(
		id, sessionGeneration, tmux.SessionKindOption, tmux.SessionKindDiff,
	); err != nil {
		return errors.Join(
			fmt.Errorf("mark diff session %s: %w", name, err),
			m.Sess.KillSessionIDIfGeneration(id, sessionGeneration),
		)
	}
	return m.SwitchMiddle(name)
}

// RunMiddleAttach is the `wrap attach` subcommand: run inside the middle
// pane, it replaces itself with a nested client on the session server,
// restoring the last selection when that session still exists.
func (m *Manager) RunMiddleAttach() error {
	if err := m.EnsureSessionServer(""); err != nil {
		return err
	}
	target, err := m.validatedAttachTarget()
	if err != nil {
		return err
	}
	attachArgs, err := tmux.AttachSessionIfGenerationArgs(target.ID, target.Generation)
	if err != nil {
		return fmt.Errorf("build generation-guarded attach for %s: %w", target.Name, err)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found in PATH: %w", err)
	}
	args := append([]string{"tmux", "-L", tmux.SocketSessions}, attachArgs...)
	return syscall.Exec(tmuxPath,
		args,
		cleanEnv(os.Environ()))
}

func (m *Manager) validatedAttachTarget() (tmux.SessionInfo, error) {
	target, err := pickAttachTarget(m.WS, m.Sess.HasSession)
	if err != nil {
		return tmux.SessionInfo{}, err
	}
	identity, err := m.validateSwitchTarget(target)
	if err != nil {
		return tmux.SessionInfo{}, err
	}
	return identity, nil
}

func pickAttachTarget(ws string, has func(string) (bool, error)) (string, error) {
	sel, ok, err := state.Read(ws)
	if err != nil {
		return "", fmt.Errorf("read selection for workspace %q while choosing attach target: %w", ws, err)
	}
	if ok && sel.Session != "" {
		exists, err := has(sel.Session)
		if err != nil {
			return "", fmt.Errorf("check selected session %q for workspace %q while choosing attach target: %w", sel.Session, ws, err)
		}
		if exists {
			return sel.Session, nil
		}
	}
	return HomeSession, nil
}

// shq single-quotes s for safe embedding in the shell command line tmux
// splices pane commands into: m.Exe and m.WS are spliced as one string
// (tmux new-session/split-window run their command via the shell), so
// either containing a shell metacharacter would otherwise break out.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cleanEnv strips TMUX* so nested attach doesn't refuse to run.
func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
