package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/sarcasticbird/wrap/internal/target"
)

const (
	ActionStatus   = "status"
	ActionRename   = "rename"
	ActionRotate   = "rotate"
	ActionShutdown = "shutdown"

	CodeNotFound       = "not_found"
	CodeInvalidRequest = "invalid_request"
	CodeNotReady       = "not_ready"
	CodeInternal       = "internal"

	MaxRequestBytes  = 64 << 10
	maxResponseBytes = 1 << 20
)

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{5,63}$`)

var ErrNotReady = errors.New("wrap is not ready")

type Request struct {
	InstanceID string `json:"instance_id"`
	Action     string `json:"action"`
	Name       string `json:"name,omitempty"`
}

func (r Request) validate() error {
	if !instanceIDPattern.MatchString(r.InstanceID) {
		return errors.New("invalid instance ID")
	}
	switch r.Action {
	case ActionStatus, ActionRotate, ActionShutdown:
		if r.Name != "" {
			return errors.New("request name is only valid for rename")
		}
		return nil
	case ActionRename:
		if r.Name == "" {
			return errors.New("rename requires a name")
		}
		return nil
	default:
		return fmt.Errorf("unknown action %q", r.Action)
	}
}

type Status struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	State      string        `json:"state"`
	PairingURL string        `json:"pairing_url"`
	QR         string        `json:"qr"`
	Directory  string        `json:"directory"`
	Clients    int           `json:"clients"`
	StartedAt  time.Time     `json:"started_at"`
	Target     target.Target `json:"target"`
	Warning    string        `json:"warning,omitempty"`
}

type Handler interface {
	Status() Status
	Rename(context.Context, string) (Status, error)
	Rotate(context.Context) (Status, error)
	Shutdown(context.Context) error
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type response struct {
	Status *Status `json:"status,omitempty"`
	Error  *Error  `json:"error,omitempty"`
}
