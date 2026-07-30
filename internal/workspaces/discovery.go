// Package workspaces reconciles persisted workspace identity with Wrap's live
// tmux servers. It deliberately owns no UI or launch behavior.
package workspaces

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

// Workspace is one active or recoverable Wrap workspace.
type Workspace struct {
	Name     string
	Root     string
	Attached bool
	Recover  bool
}

// Snapshot is one reconciled view of live workspaces. Warnings identify live
// workspace state that cannot safely become a selectable row.
type Snapshot struct {
	Workspaces []Workspace
	Warnings   []string
}

// Source supplies the persisted and live inputs to discovery.
type Source interface {
	MetaRecords() ([]state.MetaRecord, error)
	UISessions() ([]tmux.SessionInfo, error)
	WorkSessions() ([]tmux.SessionInfo, error)
}

// RuntimeSource reads Wrap's real state directory and dedicated tmux servers.
type RuntimeSource struct {
	UI   *tmux.Server
	Work *tmux.Server
}

func NewRuntimeSource() RuntimeSource {
	return RuntimeSource{
		UI:   tmux.NewServer(tmux.SocketUI),
		Work: tmux.NewServer(tmux.SocketSessions),
	}
}

func (s RuntimeSource) MetaRecords() ([]state.MetaRecord, error) {
	return state.ListMeta()
}

func (s RuntimeSource) UISessions() ([]tmux.SessionInfo, error) {
	return liveSessions(s.UI)
}

func (s RuntimeSource) WorkSessions() ([]tmux.SessionInfo, error) {
	return liveSessions(s.Work)
}

func liveSessions(server *tmux.Server) ([]tmux.SessionInfo, error) {
	infos, err := server.Sessions()
	if errors.Is(err, tmux.ErrNoServer) {
		return nil, nil
	}
	return infos, err
}

type liveState struct {
	ui       bool
	work     bool
	attached bool
}

// Discover returns only metadata-backed workspaces proven live by chrome or
// owned work sessions. Persisted state by itself never creates a row.
func Discover(source Source) (Snapshot, error) {
	records, err := source.MetaRecords()
	if err != nil {
		return Snapshot{}, fmt.Errorf("discover workspace metadata: %w", err)
	}
	uiSessions, err := source.UISessions()
	if err != nil && !errors.Is(err, tmux.ErrNoServer) {
		return Snapshot{}, fmt.Errorf("discover workspace chrome: %w", err)
	}
	workSessions, err := source.WorkSessions()
	if err != nil && !errors.Is(err, tmux.ErrNoServer) {
		return Snapshot{}, fmt.Errorf("discover workspace sessions: %w", err)
	}

	metadata := make(map[string]state.MetaRecord, len(records))
	for _, record := range records {
		metadata[record.Name] = record
	}
	live := map[string]liveState{}
	for _, session := range uiSessions {
		name, ok := strings.CutPrefix(session.Name, "wrap-")
		if !ok || config.ValidateWorkspaceName(name) != nil {
			continue
		}
		status := live[name]
		status.ui = true
		status.attached = status.attached || session.Attached
		live[name] = status
	}
	for _, session := range workSessions {
		name, ok := config.SessionWorkspace(session.Name)
		if !ok {
			continue
		}
		status := live[name]
		status.work = true
		live[name] = status
	}

	names := make([]string, 0, len(live))
	for name := range live {
		names = append(names, name)
	}
	sort.Strings(names)

	var snapshot Snapshot
	for _, name := range names {
		record, ok := metadata[name]
		switch {
		case !ok:
			snapshot.Warnings = append(snapshot.Warnings,
				fmt.Sprintf("workspace %q is live but metadata is missing", name))
			continue
		case record.Err != nil:
			snapshot.Warnings = append(snapshot.Warnings,
				fmt.Sprintf("workspace %q metadata unavailable: %v", name, record.Err))
			continue
		}
		status := live[name]
		snapshot.Workspaces = append(snapshot.Workspaces, Workspace{
			Name:     name,
			Root:     record.Meta.Root,
			Attached: status.attached,
			Recover:  status.work && !status.ui,
		})
	}
	sort.Strings(snapshot.Warnings)
	return snapshot, nil
}
