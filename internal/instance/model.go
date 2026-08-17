package instance

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sarcasticbird/wrap/internal/target"
)

const RecordVersion = 1

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{5,63}$`)

type Record struct {
	Version       int           `json:"version"`
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	PID           int           `json:"pid"`
	ControlSocket string        `json:"control_socket"`
	StartedAt     time.Time     `json:"started_at"`
	Directory     string        `json:"directory"`
	Target        target.Target `json:"target"`
}

func (r Record) Validate() error {
	if r.Version != RecordVersion {
		return fmt.Errorf("unsupported instance record version %d", r.Version)
	}
	if !idPattern.MatchString(r.ID) {
		return fmt.Errorf("invalid instance ID %q", r.ID)
	}
	if err := ValidateName(r.Name); err != nil {
		return fmt.Errorf("invalid instance name: %w", err)
	}
	if r.PID <= 0 {
		return errors.New("instance PID must be positive")
	}
	if !filepath.IsAbs(r.ControlSocket) {
		return errors.New("instance control socket must be absolute")
	}
	if r.StartedAt.IsZero() {
		return errors.New("instance start time is required")
	}
	if !filepath.IsAbs(r.Directory) {
		return errors.New("instance directory must be absolute")
	}
	if !filepath.IsAbs(r.Target.SocketPath) || r.Target.Generation == "" ||
		r.Target.SessionID == "" || r.Target.WindowID == "" {
		return errors.New("complete tmux target identity is required")
	}
	return nil
}

type RecordError struct {
	Path string
	Err  error
}

func (e RecordError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

var (
	ErrNotFound  = errors.New("wrap instance not found")
	ErrAmbiguous = errors.New("wrap instance selector is ambiguous")
	ErrLeaseHeld = errors.New("wrap instance lease is held")
)
