package target

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sarcasticbird/wrap/internal/tmux"
)

func CreateDefaultSession(dir, bootstrapCommand string, run tmux.Runner) error {
	if !filepath.IsAbs(dir) {
		return errors.New("tmux session directory must be absolute")
	}
	if bootstrapCommand == "" || strings.ContainsAny(bootstrapCommand, "\r\n") {
		return errors.New("tmux bootstrap command must be one nonempty line")
	}
	args := []string{"new-session", "-c", filepath.Clean(dir), bootstrapCommand}
	if run != nil {
		if _, err := run.Run(args...); err != nil {
			return fmt.Errorf("create default tmux session: %w", err)
		}
		return nil
	}
	command := exec.Command("tmux", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("create default tmux session: %w", err)
	}
	return nil
}
