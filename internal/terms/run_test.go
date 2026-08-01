package terms

import (
	"errors"
	"testing"
)

func TestRunWithMirrorPaneCleanupRestoresAfterProgramExit(t *testing.T) {
	programErr := errors.New("program stopped")
	restoreErr := errors.New("restore failed")
	backend := &fakeBackend{mirrorPaneRestoreErr: restoreErr}

	err := runWithMirrorPaneCleanup(backend, func() error { return programErr })
	if !errors.Is(err, programErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("cleanup error = %v, want joined program and restore errors", err)
	}
	if backend.mirrorPaneRestores != 1 {
		t.Fatalf("restore calls = %d, want 1", backend.mirrorPaneRestores)
	}
}
