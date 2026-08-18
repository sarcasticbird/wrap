package target

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/creack/pty"
)

func TestCreateDefaultSessionUsesOrdinaryServerAndPhysicalDirectory(t *testing.T) {
	t.Parallel()
	runner := &helperRunner{}
	if err := CreateDefaultSession("/work/api", "'/opt/bin/wrap' _bootstrap", runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if got := strings.Join(call, " "); got != "new-session -c /work/api '/opt/bin/wrap' _bootstrap" {
		t.Fatalf("tmux call = %q", got)
	}
	for _, arg := range call {
		if arg == "-L" || arg == "-S" {
			t.Fatalf("CreateDefaultSession used private endpoint: %v", call)
		}
	}
}

func TestCreateDefaultSessionUsesInteractiveStdio(t *testing.T) {
	if os.Getenv("WRAP_CREATE_SESSION_PTY_HELPER") == "1" {
		if err := CreateDefaultSession("/tmp", "wrap _bootstrap", nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n[ -t 0 ] && [ -t 1 ] && [ -t 2 ] || exit 91\nprintf 'interactive-tmux:%s\\n' \"$*\"\n"
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCreateDefaultSessionUsesInteractiveStdio$")
	command.Env = append(os.Environ(),
		"WRAP_CREATE_SESSION_PTY_HELPER=1",
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(terminal)
	_ = terminal.Close()
	waitErr := command.Wait()
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		t.Fatal(readErr)
	}
	if waitErr != nil {
		t.Fatalf("PTY helper = %v, output=%q", waitErr, output)
	}
	if !strings.Contains(string(output), "interactive-tmux:new-session -c /tmp wrap _bootstrap") {
		t.Fatalf("PTY output = %q", output)
	}
}

func TestCreateDefaultSessionRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		dir     string
		command string
	}{
		{dir: "relative", command: "wrap _bootstrap"},
		{dir: "/work", command: ""},
		{dir: "/work", command: "wrap\n_bootstrap"},
	} {
		if err := CreateDefaultSession(test.dir, test.command, &helperRunner{}); err == nil {
			t.Fatalf("CreateDefaultSession(%q, %q) succeeded", test.dir, test.command)
		}
	}
}
