// Package tmux wraps the tmux CLI. All commands run against an explicit
// socket (-L), keeping wrap's servers away from the user's own tmux.
package tmux

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// SocketSessions hosts one plain session per entry.
	SocketSessions = "wrap"
	// SocketUI hosts the single 3-pane chrome session.
	SocketUI = "wrap-ui"
)

var (
	// ErrNoServer reports that the selected tmux socket has no running
	// server. Consumers must choose explicitly whether that means stale live
	// data or an inactive workspace; Sessions never silently converts it to
	// an empty list.
	ErrNoServer = errors.New("tmux server not running")
	// ErrMissingTarget reports that a command's named session or window is
	// confirmed absent. It is intentionally narrower than a general tmux
	// command failure so callers can self-heal without treating transport or
	// permission errors as evidence that live state is disposable.
	ErrMissingTarget = errors.New("tmux target not found")
)

type Runner interface {
	Run(args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	// tmux adds one output delimiter. Remove exactly one so a path whose
	// actual name ends in a newline remains distinguishable.
	return strings.TrimSuffix(out.String(), "\n"), nil
}

type Server struct {
	Socket string
	// ConfigFile, when set, is passed as tmux -f on every command.
	// Separate sockets isolate server STATE but not configuration: a new
	// server still reads the user's tmux.conf, so options like
	// base-index/pane-base-index would break wrap's fixed pane targets.
	// The UI (chrome) server sets this to os.DevNull to stay hermetic;
	// the session server leaves it empty so user config applies there.
	ConfigFile string
	R          Runner
}

func NewServer(socket string) *Server {
	return &Server{Socket: socket, R: execRunner{}}
}

func (s *Server) Run(args ...string) (string, error) {
	prefix := []string{"-L", s.Socket}
	if s.ConfigFile != "" {
		prefix = append([]string{"-f", s.ConfigFile}, prefix...)
	}
	return s.R.Run(append(prefix, args...)...)
}

func (s *Server) HasSession(name string) (bool, error) {
	_, err := s.Run("has-session", "-t", "="+name)
	if err == nil {
		return true, nil
	}
	if isMissingSessionError(err) {
		return false, nil
	}
	return false, err
}

func isMissingSessionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no such session") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to") && strings.Contains(msg, "no such file or directory")
}

func isNoServerError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to") &&
			strings.Contains(msg, "no such file or directory")
}

func isMissingTargetError(err error) bool {
	msg := strings.ToLower(err.Error())
	return isMissingSessionError(err) ||
		strings.Contains(msg, "can't find window") ||
		strings.Contains(msg, "no such window")
}

// IsMissingTargetError reports whether err proves that a tmux session,
// window, or its server no longer exists.
func IsMissingTargetError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrMissingTarget) || isMissingTargetError(err)
}

// NewSession creates a detached session. Empty cmd starts the default shell.
func (s *Server) NewSession(name, dir, cmd string) error {
	args := []string{"new-session", "-d", "-s", name, "-c", dir}
	if cmd != "" {
		args = append(args, cmd)
	}
	_, err := s.Run(args...)
	return err
}

// NewSessionID creates a detached session and returns tmux's stable session ID.
// Callers that immediately mutate session metadata should target this ID, not
// the name: set-option may prefix-match a name after the intended session exits.
func (s *Server) NewSessionID(name, dir, cmd string) (string, error) {
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", name, "-c", dir}
	if cmd != "" {
		args = append(args, cmd)
	}
	id, err := s.Run(args...)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("tmux new-session returned an empty session id")
	}
	return id, nil
}

// NewSessionIdentity creates a session and returns both identifiers needed for
// later safe mutation. Session IDs can be reused after a server restart, so an
// ID without its server generation is not an authorization token.
func (s *Server) NewSessionIdentity(name, dir, cmd, generation string) (string, string, error) {
	commandArgs := []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", name, "-c", dir}
	if cmd != "" {
		commandArgs = append(commandArgs, cmd)
	}
	args, err := serverCommandIfGenerationArgs(generation, tmuxCommand(commandArgs...))
	if err != nil {
		return "", "", err
	}
	out, err := s.Run(args...)
	if err != nil {
		return "", "", err
	}
	out = strings.TrimSpace(out)
	if out == generationMismatchMessage {
		return "", "", ErrServerGenerationChanged
	}
	if !isSessionID(out) {
		return "", "", fmt.Errorf("tmux new-session returned invalid identity %q", out)
	}
	return out, generation, nil
}

func (s *Server) KillSession(name string) error {
	_, err := s.Run("kill-session", "-t", "="+name)
	return err
}

// KillSessionID kills exactly the stable tmux session ID. A session that
// already exited is successful cleanup; importantly, no replacement that
// reused its name can be targeted.
func (s *Server) KillSessionID(id string) error {
	_, err := s.Run("kill-session", "-t", id)
	if err != nil && isMissingSessionError(err) {
		return nil
	}
	return err
}

var ErrServerGenerationChanged = errors.New("tmux server generation changed")
var ErrSessionIdentityChanged = errors.New("tmux session identity changed")

const ServerGenerationOption = "@wrap_server_generation"

// EnsureServerGeneration initializes one unpredictable generation marker for
// this tmux server lifetime. The if-shell command is serialized by tmux, so
// concurrent wrap clients cannot overwrite a generation another client just
// installed.
func (s *Server) EnsureServerGeneration(candidate string) (string, error) {
	if !isGeneration(candidate) {
		return "", fmt.Errorf("invalid tmux server generation %q", candidate)
	}
	condition := "#{==:#{" + ServerGenerationOption + "},}"
	set := "set-option -g " + ServerGenerationOption + " " + candidate
	if _, err := s.Run("if-shell", "-F", condition, set, ""); err != nil {
		return "", err
	}
	generation, err := s.Run("show-options", "-gvq", ServerGenerationOption)
	if err != nil {
		return "", err
	}
	generation = strings.TrimSpace(generation)
	if !isGeneration(generation) {
		return "", fmt.Errorf("tmux server returned invalid generation %q", generation)
	}
	return generation, nil
}

// KillSessionIDIfGeneration kills id only when the connected tmux server is
// the same generation that reported it. The check and kill are one tmux
// command queue operation, so a restarted server cannot reuse id between them.
func (s *Server) KillSessionIDIfGeneration(id, generation string) error {
	err := s.runSessionCommandIfGeneration(id, generation,
		tmuxCommand("kill-session", "-t", id))
	if err != nil && isMissingSessionError(err) {
		return nil
	}
	return err
}

// KillSessionIDIfGenerationAndNameAndKind kills id only while its server
// generation, current name, and durable Wrap kind still match the identity
// captured by the UI.
func (s *Server) KillSessionIDIfGenerationAndNameAndKind(id, generation, name, kind string) error {
	if !isSessionID(id) {
		return fmt.Errorf("invalid tmux session id %q", id)
	}
	if !isGeneration(generation) {
		return fmt.Errorf("invalid tmux server generation %q", generation)
	}
	if name == "" {
		return errors.New("tmux session name is empty")
	}
	if !isSessionKind(kind) {
		return fmt.Errorf("invalid tmux session kind %q", kind)
	}
	condition := "#{&&:#{==:#{" + ServerGenerationOption + "}," + generation +
		"},#{&&:#{==:#{session_name}," + escapeFormatLiteral(name) +
		"},#{==:#{" + SessionKindOption + "}," + kind + "}}}"
	reject := tmuxCommand("display-message", "-p", sessionIdentityMismatchMessage)
	out, err := s.Run(
		"if-shell", "-F", "-t", id, condition,
		tmuxCommand("kill-session", "-t", id), reject,
	)
	if err != nil && isMissingSessionError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == sessionIdentityMismatchMessage {
		return ErrSessionIdentityChanged
	}
	return nil
}

// RenameSessionIDIfGenerationAndNameAndKind renames id only while its server
// generation, current name, and durable Wrap kind still match the identity
// captured by the UI.
func (s *Server) RenameSessionIDIfGenerationAndNameAndKind(id, generation, oldName, kind, newName string) error {
	if !isSessionID(id) {
		return fmt.Errorf("invalid tmux session id %q", id)
	}
	if !isGeneration(generation) {
		return fmt.Errorf("invalid tmux server generation %q", generation)
	}
	if oldName == "" || newName == "" {
		return errors.New("tmux session name is empty")
	}
	if !isSessionKind(kind) {
		return fmt.Errorf("invalid tmux session kind %q", kind)
	}
	condition := "#{&&:#{==:#{" + ServerGenerationOption + "}," + generation +
		"},#{&&:#{==:#{session_name}," + escapeFormatLiteral(oldName) +
		"},#{==:#{" + SessionKindOption + "}," + kind + "}}}"
	reject := tmuxCommand("display-message", "-p", sessionIdentityMismatchMessage)
	out, err := s.Run(
		"if-shell", "-F", "-t", id, condition,
		tmuxCommand("rename-session", "-t", id, newName), reject,
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == sessionIdentityMismatchMessage {
		return ErrSessionIdentityChanged
	}
	return nil
}

// SetUnmarkedSessionKindIfGenerationAndName upgrades one legacy Wrap session
// only while its server generation, stable ID, current name, empty kind, and
// absent entry identity markers all still match. The marker checks prevent a
// protected entry session that was externally renamed from being claimed as a
// scratch or diff session.
func (s *Server) SetUnmarkedSessionKindIfGenerationAndName(id, generation, name, kind string) error {
	if !isSessionID(id) {
		return fmt.Errorf("invalid tmux session id %q", id)
	}
	if !isGeneration(generation) {
		return fmt.Errorf("invalid tmux server generation %q", generation)
	}
	if name == "" {
		return errors.New("tmux session name is empty")
	}
	if !isSessionKind(kind) {
		return fmt.Errorf("invalid tmux session kind %q", kind)
	}
	condition := "#{&&:#{==:#{" + ServerGenerationOption + "}," + generation +
		"},#{&&:#{==:#{session_name}," + escapeFormatLiteral(name) +
		"},#{&&:#{==:#{" + SessionKindOption + "},}" +
		",#{&&:#{==:#{" + EntryNameOption + "},}" +
		",#{==:#{" + EntryPathOption + "},}}}}}"
	reject := tmuxCommand("display-message", "-p", sessionIdentityMismatchMessage)
	out, err := s.Run(
		"if-shell", "-F", "-t", id, condition,
		tmuxCommand("set-option", "-t", id, SessionKindOption, kind), reject,
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == sessionIdentityMismatchMessage {
		return ErrSessionIdentityChanged
	}
	return nil
}

// SetSessionKindIfGenerationAndNameAndCurrentKind changes one kind marker only
// while the captured server generation, stable ID, current name, and current
// kind still match. expectedKind may be empty for sessions created before kind
// markers existed.
func (s *Server) SetSessionKindIfGenerationAndNameAndCurrentKind(
	id, generation, name, expectedKind, newKind string,
) error {
	if !isSessionID(id) {
		return fmt.Errorf("invalid tmux session id %q", id)
	}
	if !isGeneration(generation) {
		return fmt.Errorf("invalid tmux server generation %q", generation)
	}
	if name == "" {
		return errors.New("tmux session name is empty")
	}
	if expectedKind != "" && !isSessionKind(expectedKind) {
		return fmt.Errorf("invalid expected tmux session kind %q", expectedKind)
	}
	if !isSessionKind(newKind) {
		return fmt.Errorf("invalid tmux session kind %q", newKind)
	}
	condition := "#{&&:#{==:#{" + ServerGenerationOption + "}," + generation +
		"},#{&&:#{==:#{session_name}," + escapeFormatLiteral(name) +
		"},#{==:#{" + SessionKindOption + "}," + expectedKind + "}}}"
	reject := tmuxCommand("display-message", "-p", sessionIdentityMismatchMessage)
	out, err := s.Run(
		"if-shell", "-F", "-t", id, condition,
		tmuxCommand("set-option", "-t", id, SessionKindOption, newKind), reject,
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == sessionIdentityMismatchMessage {
		return ErrSessionIdentityChanged
	}
	return nil
}

// SetSessionOptionIfGeneration mutates exactly id on the server generation
// that reported it, refusing a restarted server that may have reused the ID.
func (s *Server) SetSessionOptionIfGeneration(id, generation, option, value string) error {
	if option == "" {
		return errors.New("tmux option name is empty")
	}
	return s.runSessionCommandIfGeneration(id, generation,
		tmuxCommand("set-option", "-t", id, option, value))
}

// RenameSessionIDIfGeneration renames exactly id on its original server.
func (s *Server) RenameSessionIDIfGeneration(id, generation, newName string) error {
	if newName == "" {
		return errors.New("tmux session name is empty")
	}
	return s.runSessionCommandIfGeneration(id, generation,
		tmuxCommand("rename-session", "-t", id, newName))
}

// SwitchClientIfGeneration switches a nested client only to the session ID
// captured on its original server.
func (s *Server) SwitchClientIfGeneration(clientTTY, id, generation string) error {
	if clientTTY == "" {
		return errors.New("tmux client tty is empty")
	}
	return s.runSessionCommandIfGeneration(id, generation,
		tmuxCommand("switch-client", "-c", clientTTY, "-t", id))
}

// AttachSessionIfGenerationArgs returns one tmux command queue operation that
// attaches to id only on the server generation that reported it. Executing the
// returned if-shell command through a fresh tmux client closes the validation-
// to-attach race: a restarted server rejects the stale identity instead of
// attaching to an unrelated session that reused id.
func AttachSessionIfGenerationArgs(id, generation string) ([]string, error) {
	return attachSessionIfGenerationArgs(id, generation, false)
}

// AttachSessionIgnoringSizeIfGenerationArgs returns one tmux command queue
// operation that validates the server generation and attaches an ignored-size
// client. Remote mirror viewers pair this flag with a temporary per-window
// manual-size pin.
func AttachSessionIgnoringSizeIfGenerationArgs(id, generation string) ([]string, error) {
	return attachSessionIfGenerationArgs(id, generation, true)
}

// AttachWindowIgnoringSizeIfGenerationArgs attaches an ignored-size client
// only while id still has windowID selected on the expected server
// generation. The window guard ensures a viewer cannot pin one window and
// then attach to another if the session's current window changes.
func AttachWindowIgnoringSizeIfGenerationArgs(
	id, generation, windowID string,
) ([]string, error) {
	attach := tmuxCommand("attach-session", "-f", "ignore-size", "-t", id)
	return sessionWindowCommandIfGenerationArgs(id, generation, windowID, attach)
}

// PinWindowSizeIfGenerationArgs returns one tmux command queue operation that
// prints the stable current window ID and its effective window-size option
// (including whether the value is inherited), then switches that exact
// generation's window to manual sizing.
func PinWindowSizeIfGenerationArgs(id, generation string) ([]string, error) {
	pin := tmuxCommand(
		"display-message", "-p", "-t", id, "#{window_id}",
	) + " ; " + tmuxCommand(
		"show-options", "-w", "-A", "-t", id, "window-size",
	) + " ; " + tmuxCommand(
		"set-option", "-w", "-t", id, "window-size", "manual",
	)
	return sessionCommandIfGenerationArgs(id, generation, pin)
}

// RestoreWindowSizeIfGenerationArgs returns one generation-guarded tmux
// command queue operation that restores the stable window captured by
// PinWindowSizeIfGenerationArgs. Inherited values are restored by removing the
// temporary window-local override rather than freezing the old effective
// value.
func RestoreWindowSizeIfGenerationArgs(
	generation, windowID, mode string,
	inherited bool,
) ([]string, error) {
	if !isWindowID(windowID) {
		return nil, fmt.Errorf("invalid tmux window id %q", windowID)
	}
	switch mode {
	case "largest", "smallest", "manual", "latest":
	default:
		return nil, fmt.Errorf("invalid tmux window-size mode %q", mode)
	}
	var restore string
	if inherited {
		restore = tmuxCommand("set-option", "-w", "-u", "-t", windowID, "window-size")
	} else {
		restore = tmuxCommand("set-option", "-w", "-t", windowID, "window-size", mode)
	}
	return serverCommandIfGenerationArgs(generation, restore)
}

func attachSessionIfGenerationArgs(id, generation string, ignoreSize bool) ([]string, error) {
	attach := []string{"attach-session"}
	if ignoreSize {
		attach = append(attach, "-f", "ignore-size")
	}
	attach = append(attach, "-t", id)
	return sessionCommandIfGenerationArgs(id, generation,
		tmuxCommand(attach...))
}

func (s *Server) runSessionCommandIfGeneration(id, generation, command string) error {
	args, err := sessionCommandIfGenerationArgs(id, generation, command)
	if err != nil {
		return err
	}
	out, err := s.Run(args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == generationMismatchMessage {
		return ErrServerGenerationChanged
	}
	return nil
}

const generationMismatchMessage = "wrap-server-generation-mismatch"
const sessionIdentityMismatchMessage = "wrap-session-identity-mismatch"

// IsGenerationMismatchOutput reports whether tmux emitted the sentinel used by
// generation-guarded commands when their server identity no longer matches.
func IsGenerationMismatchOutput(out string) bool {
	return strings.TrimSpace(out) == generationMismatchMessage
}

func escapeFormatLiteral(value string) string {
	value = strings.ReplaceAll(value, "#", "##")
	value = strings.ReplaceAll(value, ",", "#,")
	return strings.ReplaceAll(value, "}", "#}")
}

func sessionCommandIfGenerationArgs(id, generation, command string) ([]string, error) {
	if !isSessionID(id) {
		return nil, fmt.Errorf("invalid tmux session id %q", id)
	}
	return serverCommandIfGenerationArgs(generation, command)
}

func sessionWindowCommandIfGenerationArgs(
	id, generation, windowID, command string,
) ([]string, error) {
	if !isSessionID(id) {
		return nil, fmt.Errorf("invalid tmux session id %q", id)
	}
	if !isWindowID(windowID) {
		return nil, fmt.Errorf("invalid tmux window id %q", windowID)
	}
	if !isGeneration(generation) {
		return nil, fmt.Errorf("invalid tmux server generation %q", generation)
	}
	generationCondition := "#{==:#{" + ServerGenerationOption + "}," + generation + "}"
	windowCondition := "#{==:#{window_id}," + windowID + "}"
	condition := "#{&&:" + generationCondition + "," + windowCondition + "}"
	reject := tmuxCommand("display-message", "-p", generationMismatchMessage)
	return []string{"if-shell", "-F", "-t", id, condition, command, reject}, nil
}

func serverCommandIfGenerationArgs(generation, command string) ([]string, error) {
	if !isGeneration(generation) {
		return nil, fmt.Errorf("invalid tmux server generation %q", generation)
	}
	condition := "#{==:#{" + ServerGenerationOption + "}," + generation + "}"
	reject := tmuxCommand("display-message", "-p", generationMismatchMessage)
	return []string{"if-shell", "-F", condition, command, reject}, nil
}

// tmuxCommand builds one command string for tmux to parse as an if-shell
// branch. Ordinary tmux identifiers retain their familiar bare form; every
// other argument is single-quoted so printable workspace punctuation cannot
// become tmux command syntax. A literal apostrophe is represented by ending
// the quote, escaping it, and reopening the quote.
func tmuxCommand(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		// Session IDs are tmux's own "$N" identity syntax, not user
		// input. Keep those literal while quoting every other dollar so
		// a workspace such as "project$USER" cannot expand through the
		// tmux command parser.
		if isSessionID(arg) || isBareTmuxCommandArg(arg) {
			quoted[i] = arg
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

func isBareTmuxCommandArg(arg string) bool {
	if arg == "" {
		return false
	}
	for i, r := range arg {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || strings.ContainsRune("@_-./·", r) ||
			r == '~' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// RenameSession renames session old to newName.
func (s *Server) RenameSession(old, newName string) error {
	_, err := s.Run("rename-session", "-t", "="+old, newName)
	return err
}

// RenameSessionID renames exactly the stable tmux session ID.
func (s *Server) RenameSessionID(id, newName string) error {
	_, err := s.Run("rename-session", "-t", id, newName)
	return err
}

// BellOption is the session-scoped user option wrap uses to record that a
// session rang the bell. tmux's own window_bell_flag is viewer-relative —
// it is never raised on a session that has a client attached — and wrap's
// terminal pane keeps a client attached to whatever it last displayed. So
// the session actually running your agent was the one session that could
// never report a bell. The alert-bell hook fires regardless of attachment;
// this option is where it records that, and wrap decides when it is seen.
const BellOption = "@wrap_bell"

// BellHook is where wrap installs its alert-bell handler. tmux hooks are
// arrays, and the session server intentionally reads the user's tmux.conf,
// so wrap claims one high index instead of replacing "alert-bell" outright
// — otherwise installing wrap's handler would silently delete theirs.
const BellHook = "alert-bell[99]"

// EntryNameOption records the versioned wire encoding of the entry-session
// name a session belongs to. It makes legacy-name migration idempotent when
// one canonical name is also another entry's legacy name.
const EntryNameOption = "@wrap_entry_name"

const (
	SessionKindOption  = "@wrap_session_kind"
	SessionKindEntry   = "entry"
	SessionKindScratch = "scratch"
	SessionKindDiff    = "diff"
)

func isSessionKind(kind string) bool {
	return kind == SessionKindEntry || kind == SessionKindScratch || kind == SessionKindDiff
}

// Dots cannot occur in Wrap's canonical session names: workspace dots are
// sanitized to underscores and entry dots are encoded as ~2e. That makes this
// prefix distinguishable from every legacy plain marker without reserving a
// user-visible repository name.
const entryNameEncodingPrefix = "wrap.entry.v1."

// EncodeEntryName makes a canonical entry-session name safe across supported
// tmux versions, including tmux 3.4's dollar-sign escaping behavior.
func EncodeEntryName(name string) string {
	return entryNameEncodingPrefix + base64.RawURLEncoding.EncodeToString([]byte(name))
}

// DecodeEntryName decodes the current wire format and accepts pre-beta plain
// markers so live development sessions can migrate in place.
func DecodeEntryName(marker string) (string, error) {
	if !strings.HasPrefix(marker, entryNameEncodingPrefix) {
		return marker, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(marker, entryNameEncodingPrefix))
	if err != nil {
		return "", fmt.Errorf("decode entry name marker: %w", err)
	}
	if len(b) == 0 || !utf8.Valid(b) {
		return "", errors.New("decode entry name marker: value is empty or invalid UTF-8")
	}
	return string(b), nil
}

// EntryPathOption records a canonical entry path as a URL-safe base64 token.
// Paths may legally contain tabs and newlines, while Sessions uses a
// tab-delimited tmux format; encoding keeps the protocol unambiguous.
const EntryPathOption = "@wrap_entry_path"

func EncodeEntryPath(path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

func DecodeEntryPath(token string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("decode entry path marker: %w", err)
	}
	return string(b), nil
}

type SessionInfo struct {
	ID             string
	Generation     string
	Name           string
	Activity       int64 // unix seconds of last output
	Attached       bool
	Bell           bool
	EntryName      string
	EntryPathToken string
	Kind           string
	Created        int64 // unix seconds when tmux created the session
}

// Sessions lists sessions; a not-yet-started server yields (nil, nil).
func (s *Server) Sessions() ([]SessionInfo, error) {
	out, err := s.Run("list-sessions", "-F", "#{session_name}\t#{session_activity}\t#{session_attached}\t#{window_bell_flag}\t#{"+BellOption+"}\t#{"+EntryNameOption+"}\t#{"+EntryPathOption+"}\t#{session_id}\t#{"+ServerGenerationOption+"}\t#{session_created}\t#{"+SessionKindOption+"}")
	if err != nil {
		if isNoServerError(err) {
			return nil, fmt.Errorf("%w: %v", ErrNoServer, err)
		}
		return nil, err
	}
	var infos []SessionInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		act, parseErr := strconv.ParseInt(f[1], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse activity for tmux session %q: %w", f[0], parseErr)
		}
		// Either source counts. window_bell_flag still catches a bell in
		// a detached session; BellOption catches the attached one it
		// cannot see. The option is unset ("") until a bell fires.
		bell := f[3] != "0" || f[4] == "1"
		entryName := ""
		if len(f) >= 6 {
			entryName, err = DecodeEntryName(f[5])
			if err != nil {
				return nil, fmt.Errorf("decode entry name for tmux session %q: %w", f[0], err)
			}
		}
		entryPathToken := ""
		if len(f) >= 7 {
			entryPathToken = f[6]
		}
		id := ""
		if len(f) >= 8 {
			id = f[7]
		}
		generation := ""
		if len(f) >= 9 {
			generation = f[8]
		}
		created := int64(0)
		if len(f) >= 10 {
			created, parseErr = strconv.ParseInt(f[9], 10, 64)
			if parseErr != nil {
				return nil, fmt.Errorf("parse creation time for tmux session %q: %w", f[0], parseErr)
			}
		}
		kind := ""
		if len(f) >= 11 {
			kind = f[10]
		}
		infos = append(infos, SessionInfo{
			ID:             id,
			Generation:     generation,
			Name:           f[0],
			Activity:       act,
			Attached:       f[2] != "0",
			Bell:           bell,
			EntryName:      entryName,
			EntryPathToken: entryPathToken,
			Kind:           kind,
			Created:        created,
		})
	}
	return infos, nil
}

// SessionCurrentPath is deliberately a separate, single-value query. Paths
// may contain tabs and newlines, so including one in Sessions' multi-record
// tab/newline protocol cannot be parsed reversibly.
func (s *Server) SessionCurrentPath(id string) (string, error) {
	out, err := s.Run("display-message", "-p", "-t", id, "#{pane_current_path}")
	if err != nil {
		if isMissingTargetError(err) {
			return "", fmt.Errorf("%w: %v", ErrMissingTarget, err)
		}
		return "", err
	}
	return out, nil
}

// SessionCurrentPathIfGeneration reads the active pane's current path only if
// id still belongs to the server generation that reported it.
func (s *Server) SessionCurrentPathIfGeneration(id, generation string) (string, error) {
	path, err := s.sessionValueIfGeneration(id, generation,
		tmuxCommand("display-message", "-p", "-t", id, "#{pane_current_path}"))
	if err != nil {
		return "", fmt.Errorf("read current path for session %s: %w", id, err)
	}
	return path, nil
}

func (s *Server) sessionValueIfGeneration(id, generation, command string) (string, error) {
	args, err := sessionCommandIfGenerationArgs(id, generation, command)
	if err != nil {
		return "", err
	}
	out, err := s.Run(args...)
	if err != nil {
		if isMissingTargetError(err) {
			return "", fmt.Errorf("%w: %v", ErrMissingTarget, err)
		}
		return "", err
	}
	if out == generationMismatchMessage {
		return "", ErrServerGenerationChanged
	}
	return out, nil
}

// SwitchClient points an attached client (by tty) at another session.
func (s *Server) SwitchClient(clientTTY, target string) error {
	if !isSessionID(target) {
		target = "=" + target
	}
	_, err := s.Run("switch-client", "-c", clientTTY, "-t", target)
	return err
}

func isSessionID(target string) bool {
	if len(target) < 2 || target[0] != '$' {
		return false
	}
	for _, r := range target[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isWindowID(target string) bool {
	if len(target) < 2 || target[0] != '@' {
		return false
	}
	for _, r := range target[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	for _, r := range generation {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ClientSession asks the session server which session the client
// attached at tty is actually displaying right now. Ground truth for
// kill-redirect decisions — a persisted state.Selection can be stale.
func (s *Server) ClientSession(tty string) (string, error) {
	return s.Run("display-message", "-p", "-c", tty, "#{client_session}")
}

// ClientSessionIdentity returns the displayed session's name and stable ID
// from one tmux snapshot. Kill confirmation must compare the ID: a session
// can be renamed between x and y while remaining the same target.
func (s *Server) ClientSessionIdentity(tty string) (string, string, error) {
	out, err := s.Run("display-message", "-p", "-c", tty, "#{client_session}\t#{session_id}")
	if err != nil {
		return "", "", err
	}
	fields := strings.SplitN(out, "\t", 2)
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return "", "", fmt.Errorf("tmux returned incomplete client session identity %q", out)
	}
	return fields[0], fields[1], nil
}

// ServerPID identifies the current tmux server process. It is used only to
// detect whether killing the last UI session also replaced the shared server;
// work-session authorization uses the stronger random generation marker.
func (s *Server) ServerPID() (string, error) {
	pid, err := s.Run("display-message", "-p", "#{pid}")
	if err != nil {
		if isNoServerError(err) {
			return "", fmt.Errorf("%w: %v", ErrNoServer, err)
		}
		return "", err
	}
	pid = strings.TrimSpace(pid)
	if pid == "" {
		return "", errors.New("tmux returned an empty server pid")
	}
	return pid, nil
}

// UnbindKey removes a root-table key and treats an already-absent key as
// success. Historical cleanup is idempotent and may race another workspace
// rebuilding the same shared UI server.
func (s *Server) UnbindKey(key string) error {
	_, err := s.Run("unbind-key", "-n", key)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown key") {
		return nil
	}
	return err
}

// ClientTTYs lists the ttys of every client attached to session — that is,
// every real terminal currently displaying it. A session may have several
// (the same workspace opened in two tabs) or none (nothing attached).
func (s *Server) ClientTTYs(session string) ([]string, error) {
	out, err := s.Run("list-clients", "-t", "="+session, "-F", "#{client_tty}")
	if err != nil {
		return nil, err
	}
	var ttys []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ttys = append(ttys, line)
		}
	}
	return ttys, nil
}

func (s *Server) SelectPane(target string) error {
	_, err := s.Run("select-pane", "-t", target)
	return err
}

// PaneInfo describes one pane in a tmux window: its id (stable across
// resizes and pane-index renumbering, e.g. "%3"), the tty of the client
// attached to it, and the shell command line it was started with.
type PaneInfo struct {
	ID      string
	TTY     string
	Command string
	Role    string
}

// Panes lists every pane in window with its id, tty, start command, and
// wrap-owned role marker. Pane indices are stable within one chrome build,
// but mirrored layouts assign different indices to the same logical pane;
// callers use Role to find it without parsing shell-rendered commands.
func (s *Server) Panes(window string) ([]PaneInfo, error) {
	out, err := s.Run("list-panes", "-t", window, "-F", "#{pane_id}\t#{pane_tty}\t#{pane_start_command}\t#{@wrap_role}")
	if err != nil {
		if isMissingTargetError(err) {
			return nil, fmt.Errorf("%w: %v", ErrMissingTarget, err)
		}
		return nil, err
	}
	var panes []PaneInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 {
			continue
		}
		p := PaneInfo{ID: f[0], TTY: f[1], Command: f[2]}
		if len(f) == 4 {
			p.Role = f[3]
		}
		panes = append(panes, p)
	}
	return panes, nil
}

func (s *Server) Set(option, value string) error {
	_, err := s.Run("set-option", "-g", option, value)
	return err
}

// SetW sets a global window option (e.g. monitor-bell).
func (s *Server) SetW(option, value string) error {
	_, err := s.Run("set-option", "-wg", option, value)
	return err
}

// SetSessionOption sets a session-scoped option on one session.
//
// Unlike every other target in this file, this one carries no "="
// exact-match prefix: set-option rejects it outright ("no such session:
// =name"). tmux still prefers an exact name match before falling back to
// prefix matching, so a session whose full name is given resolves to
// itself even when longer names share its prefix.
func (s *Server) SetSessionOption(target, option, value string) error {
	_, err := s.Run("set-option", "-t", target, option, value)
	return err
}

// SetHook installs a global hook. cmd is a tmux command run in the context
// of whatever triggered the hook, so it can target that object implicitly.
func (s *Server) SetHook(name, cmd string) error {
	_, err := s.Run("set-hook", "-g", name, cmd)
	return err
}

// CheckVersion errors when tmux is missing or older than 3.2.
func CheckVersion(r Runner) error {
	out, err := r.Run("-V")
	if err != nil {
		return errors.New("tmux not found — install it (brew install tmux, apt install tmux) and retry")
	}
	ver := strings.TrimPrefix(strings.TrimSpace(out), "tmux ")
	ver = strings.TrimPrefix(ver, "next-")
	var maj, min int
	if _, err := fmt.Sscanf(ver, "%d.%d", &maj, &min); err != nil {
		return nil // unparseable (e.g. "master") → assume new enough
	}
	if maj > 3 || (maj == 3 && min >= 2) {
		return nil
	}
	return fmt.Errorf("tmux %s is too old — wrap needs 3.2 or newer", ver)
}
