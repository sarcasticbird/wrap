package launcher

import (
	"errors"
	"strings"
	"testing"
)

// TestEnsureHomeSessionToleratesConcurrentCreation guards the check-then-act
// race: two panes on the session server can both observe the home session
// absent and both issue new-session. tmux rejects the loser as a duplicate.
// That loser must not fail the whole operation (a new terminal, a switch) —
// the session it wanted now exists, created by the winner. ensureHomeSession
// re-checks on a new-session error and treats an existing session as success.
func TestEnsureHomeSessionToleratesConcurrentCreation(t *testing.T) {
	f := &fakeRunner{
		// has-session: absent on the first probe, present on the recheck
		// after a racing pane won the create.
		hasSessionResults: []bool{false, true},
		failContains:      "new-session",
		failErr:           errors.New("duplicate session: wrap-home"),
	}
	m := newTestManagerWS(f, "vb")

	created, err := m.ensureHomeSession(t.TempDir())
	if err != nil {
		t.Fatalf("ensureHomeSession should tolerate a concurrent create, got: %v", err)
	}
	if created {
		t.Fatal("ensureHomeSession reported it created the session, but the racing pane did")
	}
}

// TestLandingAfterKillConfiguresServerOnConcurrentHomeCreation guards the
// redirect path. When the home session is absent and a racing pane wins its
// creation, ensureHomeSession returns created=false. landingAfterKill must
// still configure the session server before returning the home session as the
// landing spot: on a freshly restarted server the winning pane may not have
// finished setting the generation and required options yet, and redirecting a
// client to an unconfigured server fails validation. configureSessionServer is
// idempotent, so configuring whenever the home session was initially absent is
// safe even when the winner also configures.
func TestLandingAfterKillConfiguresServerOnConcurrentHomeCreation(t *testing.T) {
	f := &fakeRunner{
		// has-session order:
		//   1. landingAfterKill's own HomeSession probe        -> absent
		//   2. ensureHomeSession's probe                       -> absent
		//   3. ensureHomeSession's post-create recheck         -> present
		//      (a racing pane created it while our new-session lost)
		hasSessionResults: []bool{false, false, true},
		failContains:      "new-session",
		failErr:           errors.New("duplicate session: wrap-home"),
	}
	m := newTestManagerWS(f, "vb")

	// killed == m.WS skips the workspace-successor branch; successor "" skips
	// the successor branch, so control reaches the HomeSession fallback.
	landing, err := m.landingAfterKill("vb", "")
	if err != nil {
		t.Fatalf("landingAfterKill on concurrent home creation: %v", err)
	}
	if landing != HomeSession {
		t.Fatalf("landing = %q, want %q", landing, HomeSession)
	}
	if !strings.Contains(f.all(), "set-option -g set-clipboard on") {
		t.Fatalf("server not configured before redirect; a racing pane may not have finished configuration:\n%s", f.all())
	}
}
