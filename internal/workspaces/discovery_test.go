package workspaces

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type fakeSource struct {
	meta    []state.MetaRecord
	ui      []tmux.SessionInfo
	work    []tmux.SessionInfo
	metaErr error
	uiErr   error
	workErr error
}

func (f fakeSource) MetaRecords() ([]state.MetaRecord, error) {
	return f.meta, f.metaErr
}

func (f fakeSource) UISessions() ([]tmux.SessionInfo, error) {
	return f.ui, f.uiErr
}

func (f fakeSource) WorkSessions() ([]tmux.SessionInfo, error) {
	return f.work, f.workErr
}

func meta(name, root string) state.MetaRecord {
	return state.MetaRecord{
		Name: name,
		Meta: state.Meta{Kind: "folder", Root: root},
	}
}

func TestDiscoverClassifiesAndSortsActiveWorkspaces(t *testing.T) {
	got, err := Discover(fakeSource{
		meta: []state.MetaRecord{
			meta("gamma", "/work/gamma"),
			meta("alpha", "/work/alpha"),
			meta("beta", "/work/beta"),
			meta("stale", "/work/stale"),
		},
		ui: []tmux.SessionInfo{
			{Name: "wrap-beta", Attached: true},
			{Name: "wrap-gamma"},
		},
		work: []tmux.SessionInfo{
			{Name: "alpha/api"},
			{Name: "betamax/api"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Workspace{
		{Name: "alpha", Root: "/work/alpha", Recover: true},
		{Name: "beta", Root: "/work/beta", Attached: true},
		{Name: "gamma", Root: "/work/gamma"},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "betamax") {
		t.Fatalf("warnings = %v, want missing metadata warning for betamax", got.Warnings)
	}
}

func TestDiscoverExcludesStaleMetadata(t *testing.T) {
	got, err := Discover(fakeSource{meta: []state.MetaRecord{
		meta("alpha", "/work/alpha"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("snapshot = %+v, want empty", got)
	}
}

func TestDiscoverWarnsForLiveWorkspaceWithoutUsableMetadata(t *testing.T) {
	broken := errors.New("broken metadata")
	got, err := Discover(fakeSource{
		meta: []state.MetaRecord{{Name: "broken", Err: broken}},
		ui: []tmux.SessionInfo{
			{Name: "wrap-broken"},
			{Name: "wrap-missing"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 0 {
		t.Fatalf("workspaces = %+v, want none", got.Workspaces)
	}
	if len(got.Warnings) != 2 ||
		!strings.Contains(got.Warnings[0], "broken") ||
		!strings.Contains(got.Warnings[1], "missing") {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

func TestDiscoverUsesExactWorkSessionOwnership(t *testing.T) {
	got, err := Discover(fakeSource{
		meta: []state.MetaRecord{
			meta("beta", "/work/beta"),
			meta("betamax", "/work/betamax"),
		},
		work: []tmux.SessionInfo{{Name: "betamax/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Workspace{{Name: "betamax", Root: "/work/betamax", Recover: true}}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
}

func TestDiscoverTreatsMissingTmuxServersAsEmpty(t *testing.T) {
	got, err := Discover(fakeSource{
		uiErr:   tmux.ErrNoServer,
		workErr: tmux.ErrNoServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("snapshot = %+v, want empty", got)
	}
}

func TestDiscoverReturnsSourceFailures(t *testing.T) {
	tests := []struct {
		name string
		src  fakeSource
		want string
	}{
		{name: "metadata", src: fakeSource{metaErr: errors.New("state denied")}, want: "state denied"},
		{name: "ui", src: fakeSource{uiErr: errors.New("ui denied")}, want: "ui denied"},
		{name: "work", src: fakeSource{workErr: errors.New("work denied")}, want: "work denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Discover(tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Discover error = %v, want %q", err, tt.want)
			}
		})
	}
}
