package target

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sarcasticbird/wrap/internal/tmux"
)

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{5,63}$`)

type Helper struct {
	SessionID  string
	Name       string
	InstanceID string
	Target     Target

	run tmux.Runner
}

func (h *Helper) ID() string {
	if h == nil {
		return ""
	}
	return h.SessionID
}

func (h *Helper) Validate(ctx context.Context) error {
	if h == nil {
		return errors.New("wrap helper is unavailable")
	}
	server := tmux.NewServerPath(h.Target.SocketPath)
	if h.run != nil {
		server.R = h.run
	}
	format := strings.Join([]string{
		"#{" + tmux.ServerGenerationOption + "}",
		"#{session_id}",
		"#{session_name}",
		"#{" + tmux.InstanceIDOption + "}",
		"#{window_id}",
	}, "\t")
	out, err := server.RunContext(ctx, "display-message", "-p", "-t", h.SessionID, format)
	if err != nil {
		return fmt.Errorf("validate Wrap helper: %w", err)
	}
	want := strings.Join([]string{
		h.Target.Generation,
		h.SessionID,
		h.Name,
		h.InstanceID,
		h.Target.WindowID,
	}, "\t")
	if strings.TrimSpace(out) != want {
		return errors.New("wrap helper identity changed")
	}
	sourceFormat := strings.Join([]string{
		"#{" + tmux.ServerGenerationOption + "}",
		"#{session_id}",
		"#{window_id}",
	}, "\t")
	sourceTarget := h.Target.SessionID + ":" + h.Target.WindowID
	source, err := server.RunContext(ctx, "display-message", "-p", "-t", sourceTarget, sourceFormat)
	if err != nil {
		return fmt.Errorf("validate source tmux window: %w", err)
	}
	wantSource := strings.Join([]string{
		h.Target.Generation,
		h.Target.SessionID,
		h.Target.WindowID,
	}, "\t")
	if strings.TrimSpace(source) != wantSource {
		return errors.New("source tmux window identity changed")
	}
	return nil
}

func CreateHelper(target Target, instanceID string, run tmux.Runner) (*Helper, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return nil, fmt.Errorf("invalid Wrap instance ID %q", instanceID)
	}
	if target.SocketPath == "" || target.Generation == "" || target.SessionID == "" || target.WindowID == "" {
		return nil, errors.New("complete tmux target identity is required")
	}
	name := helperName(instanceID)
	server := tmux.NewServerPath(target.SocketPath)
	if run != nil {
		server.R = run
	}
	args, err := tmux.NewStandaloneSessionIfWindowGenerationArgs(
		target.SessionID,
		target.Generation,
		target.WindowID,
		name,
	)
	if err != nil {
		return nil, err
	}
	sessionID, err := server.Run(args...)
	if err != nil {
		return nil, fmt.Errorf("create Wrap helper session: %w", err)
	}
	created := strings.Split(strings.TrimSpace(sessionID), "\t")
	if len(created) != 2 || created[0] == "wrap-session-window-mismatch" {
		return nil, errors.New("tmux target changed before helper creation")
	}
	sessionID = created[0]
	placeholderWindowID := created[1]
	helper := &Helper{
		SessionID:  sessionID,
		Name:       name,
		InstanceID: instanceID,
		Target:     target,
		run:        server.R,
	}
	linkArgs, err := tmux.LinkWindowReplacingCurrentIfGenerationArgs(
		target.SessionID,
		target.Generation,
		target.WindowID,
		sessionID,
		placeholderWindowID,
	)
	if err != nil {
		return nil, errors.Join(err, helper.closeUnmarked())
	}
	linkOutput, err := server.Run(linkArgs...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("link captured tmux window: %w", err), helper.closeUnmarked())
	}
	if tmux.IsGenerationMismatchOutput(linkOutput) {
		return nil, errors.Join(errors.New("tmux target changed before helper linking"), helper.closeUnmarked())
	}
	if err := server.SetSessionOptionIfGeneration(
		sessionID,
		target.Generation,
		tmux.InstanceIDOption,
		instanceID,
	); err != nil {
		return nil, errors.Join(fmt.Errorf("mark Wrap helper session: %w", err), helper.closeUnmarked())
	}
	for _, setting := range []struct {
		option string
		value  string
	}{
		{option: "prefix", value: "None"},
		{option: "prefix2", value: "None"},
		{option: "status", value: "off"},
		{option: "key-table", value: helperKeyTable(instanceID)},
	} {
		if err := server.SetSessionOptionIfGeneration(
			sessionID,
			target.Generation,
			setting.option,
			setting.value,
		); err != nil {
			return nil, errors.Join(
				fmt.Errorf("configure Wrap helper %s: %w", setting.option, err),
				helper.Close(),
			)
		}
	}
	return helper, nil
}

func CleanupHelper(target Target, instanceID string, run tmux.Runner) error {
	if !instanceIDPattern.MatchString(instanceID) {
		return fmt.Errorf("invalid Wrap instance ID %q", instanceID)
	}
	name := helperName(instanceID)
	server := tmux.NewServerPath(target.SocketPath)
	if run != nil {
		server.R = run
	}
	format := strings.Join([]string{
		"#{" + tmux.ServerGenerationOption + "}",
		"#{session_id}",
		"#{session_name}",
		"#{" + tmux.InstanceIDOption + "}",
		"#{window_id}",
	}, "\t")
	output, err := server.Run("display-message", "-p", "-t", name, format)
	if err != nil {
		if tmux.IsMissingTargetError(err) {
			return nil
		}
		return fmt.Errorf("inspect stale Wrap helper: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) != 5 || fields[0] != target.Generation || fields[2] != name ||
		fields[3] != instanceID || fields[4] != target.WindowID {
		return nil
	}
	helper := &Helper{
		SessionID: fields[1], Name: name, InstanceID: instanceID, Target: target, run: server.R,
	}
	return helper.Close()
}

func helperName(instanceID string) string {
	namePart := strings.ToLower(instanceID)
	if len(namePart) > 12 {
		namePart = namePart[:12]
	}
	return "__wrap_" + namePart
}

func helperKeyTable(instanceID string) string {
	return "__wrap_keys_" + strings.ToLower(instanceID)
}

func (h *Helper) Close() error {
	if h == nil {
		return nil
	}
	server := tmux.NewServerPath(h.Target.SocketPath)
	if h.run != nil {
		server.R = h.run
	}
	err := server.KillSessionIDIfGenerationAndNameAndOption(
		h.SessionID,
		h.Target.Generation,
		h.Name,
		tmux.InstanceIDOption,
		h.InstanceID,
	)
	if errors.Is(err, tmux.ErrSessionIdentityChanged) || errors.Is(err, tmux.ErrServerGenerationChanged) {
		return nil
	}
	return err
}

func (h *Helper) closeUnmarked() error {
	if h == nil {
		return nil
	}
	server := tmux.NewServerPath(h.Target.SocketPath)
	if h.run != nil {
		server.R = h.run
	}
	err := server.KillSessionIDIfGenerationAndName(
		h.SessionID,
		h.Target.Generation,
		h.Name,
	)
	if errors.Is(err, tmux.ErrSessionIdentityChanged) || errors.Is(err, tmux.ErrServerGenerationChanged) {
		return nil
	}
	return err
}
