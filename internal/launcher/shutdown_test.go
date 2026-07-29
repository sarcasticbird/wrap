package launcher

import (
	"testing"

	"github.com/sarcasticbird/wrap/internal/state"
)

// TestShutdownWorkspaceClearsBarrierOnPartialFailure guards the recovery
// path: when the session sweep fails partway, ShutdownWorkspace keeps the
// chrome (and this pane) alive so the user can see the error — but it must
// also clear the durable "shutting-down" barrier it published up front.
// Leaving it set refuses every later mutation (new terminals, selection
// writes) until a full relaunch, bricking a workspace that is still alive.
func TestShutdownWorkspaceClearsBarrierOnPartialFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// failContains makes every list-sessions error with a non-no-server
	// error, so the sweep collects failures and takes the partial-failure
	// branch without ever reaching the chrome kill.
	f := &fakeRunner{failContains: "list-sessions"}
	m := newTestManagerWS(f, "vb")

	if err := m.ShutdownWorkspace(); err == nil {
		t.Fatal("expected shutdown to fail when the session sweep errors")
	}

	shuttingDown, err := state.IsShuttingDown("vb")
	if err != nil {
		t.Fatal(err)
	}
	if shuttingDown {
		t.Fatal("shutdown barrier left set after a partial failure: the still-alive workspace refuses all future mutation until relaunch")
	}
}
