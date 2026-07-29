package launcher

import "testing"

// TestUIWindowAnchorsSessionName guards against cross-workspace prefix
// collisions. tmux resolves an unanchored target by exact match, then by
// prefix, then by fnmatch — so while "wrap-vb" is transiently absent during
// teardown, a target "wrap-vb:0" prefix-matches a live "wrap-vb2", aiming a
// split/respawn/kill at the wrong workspace's chrome. The "=" prefix forces
// an exact session-name match.
func TestUIWindowAnchorsSessionName(t *testing.T) {
	m := &Manager{WS: "vb"}
	if got, want := m.uiWindow(), "=wrap-vb:0"; got != want {
		t.Fatalf("uiWindow() = %q, want anchored %q", got, want)
	}
	if got, want := m.paneTarget(1), "=wrap-vb:0.1"; got != want {
		t.Fatalf("paneTarget(1) = %q, want anchored %q", got, want)
	}
}
