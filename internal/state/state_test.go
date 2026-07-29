package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/config"
)

func TestLockWorkspaceSerializesSameName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	releaseFirst, err := LockWorkspace("api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseFirst() })

	started := make(chan struct{})
	acquired := make(chan func() error, 1)
	errs := make(chan error, 1)
	go func() {
		close(started)
		release, err := LockWorkspace("api")
		if err != nil {
			errs <- err
			return
		}
		acquired <- release
	}()
	<-started
	select {
	case release := <-acquired:
		_ = release()
		t.Fatal("second lock acquired before the first was released")
	case err := <-errs:
		t.Fatalf("second lock failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatalf("second lock failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second lock did not acquire after release")
	}
}

func TestLockUIServerSerializesAcrossWorkspaces(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	releaseFirst, err := LockUIServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseFirst() })

	acquired := make(chan func() error, 1)
	errs := make(chan error, 1)
	go func() {
		release, err := LockUIServer()
		if err != nil {
			errs <- err
			return
		}
		acquired <- release
	}()
	select {
	case release := <-acquired:
		_ = release()
		t.Fatal("second UI-server lock acquired before the first was released")
	case err := <-errs:
		t.Fatalf("second UI-server lock failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatalf("second UI-server lock failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second UI-server lock did not acquire after release")
	}
}

func TestWriteRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok, err := Read("wrap"); err != nil || ok {
		t.Fatalf("empty read: ok=%v err=%v", ok, err)
	}
	sel := Selection{Entry: "e", Session: "p/e", Path: "/tmp/x"}
	if err := Write("wrap", sel); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Read("wrap")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got != sel {
		t.Errorf("got %+v, want %+v", got, sel)
	}
	// Workspaces are isolated from each other.
	if _, ok, _ := Read("other"); ok {
		t.Error("other workspace should be empty")
	}
}

func TestEntryPathsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok, err := ReadEntryPaths("vb"); err != nil || ok {
		t.Fatalf("absent entry paths: ok=%v err=%v", ok, err)
	}
	want := map[string]string{
		"vb":      "/workspace",
		"vb/repo": "/workspace/repo",
	}
	if err := WriteEntryPaths("vb", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadEntryPaths("vb")
	if err != nil || !ok {
		t.Fatalf("read entry paths: ok=%v err=%v", ok, err)
	}
	if len(got) != len(want) || got["vb"] != want["vb"] || got["vb/repo"] != want["vb/repo"] {
		t.Fatalf("entry paths = %#v, want %#v", got, want)
	}
}

func TestClearSelection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Write("wrap", Selection{Session: "wrap/x"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearSelection("wrap"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Read("wrap"); err != nil || ok {
		t.Fatalf("read after clear: ok=%v err=%v", ok, err)
	}
	// Clearing when no selection file exists is not an error.
	if err := ClearSelection("wrap"); err != nil {
		t.Fatalf("clear when absent: %v", err)
	}
}

func TestChromeParamsAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok, err := ReadChromeParams("vb"); err != nil || ok {
		t.Fatalf("absent chrome params: ok=%v err=%v", ok, err)
	}
}

func TestChromeBuildIncludesStateErrorHandling(t *testing.T) {
	if ChromeBuild < 2 {
		t.Fatalf("ChromeBuild = %d, want at least 2 so existing panes restart with state-error handling", ChromeBuild)
	}
}

func TestShutdownBarrierRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if shuttingDown, err := IsShuttingDown("vb"); err != nil || shuttingDown {
		t.Fatalf("initial shutdown state = %v, %v", shuttingDown, err)
	}
	if err := BeginShutdown("vb"); err != nil {
		t.Fatal(err)
	}
	if shuttingDown, err := IsShuttingDown("vb"); err != nil || !shuttingDown {
		t.Fatalf("published shutdown state = %v, %v", shuttingDown, err)
	}
	if err := ClearShutdown("vb"); err != nil {
		t.Fatal(err)
	}
	if shuttingDown, err := IsShuttingDown("vb"); err != nil || shuttingDown {
		t.Fatalf("cleared shutdown state = %v, %v", shuttingDown, err)
	}
}

func TestChromeBuildIncludesIndependentSelectionPolling(t *testing.T) {
	if ChromeBuild < 3 {
		t.Fatalf("ChromeBuild = %d, want at least 3 so existing panes restart with independent session/selection polling", ChromeBuild)
	}
}

func TestChromeBuildIncludesPaneRoleMarker(t *testing.T) {
	if ChromeBuild < 4 {
		t.Fatalf("ChromeBuild = %d, want at least 4 so existing panes restart with explicit role markers", ChromeBuild)
	}
}

func TestChromeBuildIncludesEntryIdentityMarkers(t *testing.T) {
	if ChromeBuild < 5 {
		t.Fatalf("ChromeBuild = %d, want at least 5 so existing panes restart with entry identity markers", ChromeBuild)
	}
}

func TestChromeBuildIncludesPathIdentityAndSessionAwareFocus(t *testing.T) {
	if ChromeBuild < 6 {
		t.Fatalf("ChromeBuild = %d, want at least 6 so existing panes restart with path identity and session-aware focus", ChromeBuild)
	}
}

func TestChromeBuildIncludesSwitchTimeEntryValidation(t *testing.T) {
	if ChromeBuild < 7 {
		t.Fatalf("ChromeBuild = %d, want at least 7 so existing panes revalidate entry paths before switching", ChromeBuild)
	}
}

func TestChromeBuildIncludesTopologyFingerprint(t *testing.T) {
	if ChromeBuild < 8 {
		t.Fatalf("ChromeBuild = %d, want at least 8 so topology changes rebuild existing chrome", ChromeBuild)
	}
}

func TestChromeBuildIncludesGenerationSafePaneMutations(t *testing.T) {
	if ChromeBuild < 9 {
		t.Fatalf("ChromeBuild = %d, want at least 9 so existing panes restart with shutdown barriers and generation-safe mutations", ChromeBuild)
	}
}

func TestChromeBuildIncludesTermsDetails(t *testing.T) {
	if ChromeBuild < 10 {
		t.Fatalf("ChromeBuild = %d, want at least 10 so existing terms panes restart with PWD details support", ChromeBuild)
	}
}

func TestChromeBuildIncludesCreationOrderAndScratchProtection(t *testing.T) {
	if ChromeBuild < 11 {
		t.Fatalf("ChromeBuild = %d, want at least 11 so existing terms panes restart with creation ordering and scratch-only lifecycle actions", ChromeBuild)
	}
}

func TestChromeBuildIncludesCompactTermsRendering(t *testing.T) {
	if ChromeBuild < 12 {
		t.Fatalf("ChromeBuild = %d, want at least 12 so existing terms panes restart with compact row and path rendering", ChromeBuild)
	}
}

func TestChromeParamsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := ChromeParams{
		TreeSide:  "right",
		TreeWidth: 40,
		Keys:      config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
		Cmd:       "claude",
		WalkDepth: 3,
		Topology:  "abc123",
	}
	if err := WriteChromeParams("vb", p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadChromeParams("vb")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got != p {
		t.Errorf("got %+v, want %+v", got, p)
	}
	// Workspaces are isolated from each other.
	if _, ok, _ := ReadChromeParams("other"); ok {
		t.Error("other workspace should have no chrome params")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok, err := ReadMeta("vb"); err != nil || ok {
		t.Fatalf("empty meta: ok=%v err=%v", ok, err)
	}
	m := Meta{Kind: "folder", Root: "/Users/x/Projects/demo"}
	if err := WriteMeta("vb", m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadMeta("vb")
	if err != nil || !ok || got != m {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
}

// Concurrent writers must never publish a spliced document. A shared
// fixed temp path let two writes interleave into one file before either
// rename, so the winner could publish a mixture of both.
func TestWriteAtomicConcurrentWritersStayIntact(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sel := Selection{
				Entry:   strings.Repeat("e", i+1),
				Session: "ws/" + strings.Repeat("s", i+1),
				Path:    "/p/" + strings.Repeat("d", i+1),
			}
			for n := 0; n < 25; n++ {
				if err := Write("ws", sel); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				got, ok, err := Read("ws")
				if err != nil {
					t.Errorf("read: %v", err) // a spliced file fails to unmarshal
					return
				}
				if !ok {
					t.Error("selection missing")
					return
				}
				// Whatever we read must be exactly one writer's document,
				// not a blend: the three fields have to agree in length.
				if len(got.Entry) != len(got.Session)-3 || len(got.Entry) != len(got.Path)-3 {
					t.Errorf("torn document: %+v", got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// No temp files may survive a write.
func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := Write("ws", Selection{Session: "ws/x"}); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "wrap", "ws")
	ents, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
