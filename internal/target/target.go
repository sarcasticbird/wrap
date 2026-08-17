package target

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sarcasticbird/wrap/internal/tmux"
)

type Target struct {
	SocketPath  string `json:"socket_path"`
	Generation  string `json:"generation"`
	SessionID   string `json:"session_id"`
	WindowID    string `json:"window_id"`
	SessionName string `json:"session_name"`
	WindowName  string `json:"window_name"`
	Directory   string `json:"directory"`
}

func (t Target) Key() string {
	sum := sha256.Sum256([]byte(t.SocketPath + "\x00" + t.Generation + "\x00" + t.WindowID))
	return hex.EncodeToString(sum[:])
}

func ResolveCurrent(getenv func(string) string, run tmux.Runner) (Target, error) {
	if getenv == nil {
		return Target{}, errors.New("environment reader is required")
	}
	socketPath, err := parseTMUX(getenv("TMUX"))
	if err != nil {
		return Target{}, err
	}
	server := tmux.NewServerPath(socketPath)
	if run != nil {
		server.R = run
	}
	candidate, err := randomGeneration()
	if err != nil {
		return Target{}, err
	}
	generation, err := server.EnsureServerGeneration(candidate)
	if err != nil {
		return Target{}, fmt.Errorf("establish tmux server identity: %w", err)
	}
	formatField := func(name string) string {
		return "#{n:" + name + "}:#{" + name + "}"
	}
	format := strings.Join([]string{
		formatField("session_id"),
		formatField("window_id"),
		formatField("session_name"),
		formatField("window_name"),
		formatField("pane_current_path"),
		formatField(tmux.ServerGenerationOption),
	}, "")
	out, err := server.Run("display-message", "-p", format)
	if err != nil {
		return Target{}, fmt.Errorf("resolve current tmux window: %w", err)
	}
	return parseTarget(socketPath, generation, out)
}

func ResolveDefaultShell(getenv func(string) string, run tmux.Runner) (string, error) {
	if getenv == nil {
		return "", errors.New("environment reader is required")
	}
	socketPath, err := parseTMUX(getenv("TMUX"))
	if err != nil {
		return "", err
	}
	server := tmux.NewServerPath(socketPath)
	if run != nil {
		server.R = run
	}
	shell, err := server.Run("show-options", "-gv", "default-shell")
	if err != nil {
		return "", fmt.Errorf("read tmux default-shell: %w", err)
	}
	shell = strings.TrimSpace(shell)
	if !filepath.IsAbs(shell) {
		return "", errors.New("tmux default-shell must be an absolute path")
	}
	info, err := os.Stat(shell)
	if err != nil {
		return "", fmt.Errorf("inspect tmux default-shell: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("tmux default-shell is not executable")
	}
	return filepath.Clean(shell), nil
}

func ResolveDefaultCommand(getenv func(string) string, run tmux.Runner) (string, error) {
	if getenv == nil {
		return "", errors.New("environment reader is required")
	}
	socketPath, err := parseTMUX(getenv("TMUX"))
	if err != nil {
		return "", err
	}
	server := tmux.NewServerPath(socketPath)
	if run != nil {
		server.R = run
	}
	command, err := server.Run("show-options", "-gv", "default-command")
	if err != nil {
		return "", fmt.Errorf("read tmux default-command: %w", err)
	}
	return command, nil
}

func parseTMUX(value string) (string, error) {
	if value == "" {
		return "", errors.New("not inside tmux")
	}
	last := strings.LastIndexByte(value, ',')
	if last <= 0 || last == len(value)-1 {
		return "", errors.New("malformed TMUX environment")
	}
	previous := strings.LastIndexByte(value[:last], ',')
	if previous <= 0 || previous == last-1 {
		return "", errors.New("malformed TMUX environment")
	}
	socketPath := value[:previous]
	if !filepath.IsAbs(socketPath) {
		return "", errors.New("TMUX socket path must be absolute")
	}
	pid, err := strconv.Atoi(value[previous+1 : last])
	if err != nil || pid <= 0 {
		return "", errors.New("malformed TMUX server pid")
	}
	client, err := strconv.Atoi(value[last+1:])
	if err != nil || client < 0 {
		return "", errors.New("malformed TMUX client id")
	}
	return filepath.Clean(socketPath), nil
}

func parseTarget(socketPath, generation, output string) (Target, error) {
	fields, err := parseLengthPrefixedFields(output, 6)
	if err != nil {
		return Target{}, fmt.Errorf("tmux target fields: %w", err)
	}
	if !strings.HasPrefix(fields[0], "$") || len(fields[0]) == 1 {
		return Target{}, fmt.Errorf("invalid tmux session id %q", fields[0])
	}
	if !strings.HasPrefix(fields[1], "@") || len(fields[1]) == 1 {
		return Target{}, fmt.Errorf("invalid tmux window id %q", fields[1])
	}
	if fields[2] == "" {
		return Target{}, errors.New("tmux session name is empty")
	}
	if !filepath.IsAbs(fields[4]) {
		return Target{}, fmt.Errorf("tmux pane directory is not absolute: %q", fields[4])
	}
	if fields[5] != generation {
		return Target{}, errors.New("tmux server generation changed during target resolution")
	}
	return Target{
		SocketPath:  socketPath,
		Generation:  generation,
		SessionID:   fields[0],
		WindowID:    fields[1],
		SessionName: fields[2],
		WindowName:  fields[3],
		Directory:   filepath.Clean(fields[4]),
	}, nil
}

func parseLengthPrefixedFields(input string, count int) ([]string, error) {
	fields := make([]string, 0, count)
	for len(fields) < count {
		colon := strings.IndexByte(input, ':')
		if colon <= 0 {
			return nil, errors.New("missing length prefix")
		}
		length, err := strconv.Atoi(input[:colon])
		if err != nil || length < 0 {
			return nil, errors.New("invalid length prefix")
		}
		input = input[colon+1:]
		if length > len(input) {
			return nil, errors.New("field shorter than length prefix")
		}
		fields = append(fields, input[:length])
		input = input[length:]
	}
	if input != "" {
		return nil, errors.New("trailing data")
	}
	return fields, nil
}

func randomGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate tmux server identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
