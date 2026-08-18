package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type Dependency struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

type StateReport struct {
	StateRoot       string   `json:"state_root"`
	RuntimeRoot     string   `json:"runtime_root"`
	SafePermissions bool     `json:"safe_permissions"`
	Live            int      `json:"live"`
	Stale           int      `json:"stale"`
	Unreachable     int      `json:"unreachable"`
	Malformed       []string `json:"malformed,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type LegacyServer struct {
	Name          string `json:"name"`
	Session       string `json:"session"`
	ListCommand   string `json:"list_command"`
	AttachCommand string `json:"attach_command"`
}

type Report struct {
	Platform    string         `json:"platform"`
	Tmux        Dependency     `json:"tmux"`
	Cloudflared Dependency     `json:"cloudflared"`
	State       StateReport    `json:"state"`
	Legacy      []LegacyServer `json:"legacy,omitempty"`
}

type Options struct {
	Platform     string
	Store        instance.Store
	LookPath     func(string) (string, error)
	Run          func(context.Context, string, ...string) (string, error)
	Call         func(context.Context, string, control.Request) (control.Status, error)
	ProbeTimeout time.Duration
}

const defaultProbeTimeout = 5 * time.Second

func Check(ctx context.Context, options Options) Report {
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	if options.LookPath == nil {
		options.LookPath = exec.LookPath
	}
	if options.Run == nil {
		options.Run = runCommand
	}
	if options.Call == nil {
		options.Call = control.Call
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = defaultProbeTimeout
	}
	report := Report{Platform: options.Platform}
	tmuxCtx, cancelTmux := context.WithTimeout(ctx, options.ProbeTimeout)
	report.Tmux = checkDependency(tmuxCtx, "tmux", options.LookPath, options.Run, []string{"-V"}, tmux.CheckVersionOutput)
	cancelTmux()
	cloudCtx, cancelCloud := context.WithTimeout(ctx, options.ProbeTimeout)
	report.Cloudflared = checkDependency(cloudCtx, "cloudflared", options.LookPath, options.Run, []string{"--version"}, mirror.ParseCloudflaredVersion)
	cancelCloud()
	report.State = checkState(ctx, options.ProbeTimeout, options.Store, options.Call)
	if report.Tmux.Path != "" {
		report.Legacy = checkLegacy(ctx, options.ProbeTimeout, report.Tmux.Path, options.Run)
	}
	return report
}

func CheckDependencies(
	ctx context.Context,
	lookPath func(string) (string, error),
	run func(context.Context, string, ...string) (string, error),
) (Dependency, Dependency) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if run == nil {
		run = runCommand
	}
	return checkDependency(ctx, "tmux", lookPath, run, []string{"-V"}, tmux.CheckVersionOutput),
		checkDependency(ctx, "cloudflared", lookPath, run, []string{"--version"}, mirror.ParseCloudflaredVersion)
}

func checkDependency(
	ctx context.Context,
	name string,
	lookPath func(string) (string, error),
	run func(context.Context, string, ...string) (string, error),
	args []string,
	parse func(string) (string, error),
) Dependency {
	dependency := Dependency{Name: name}
	path, err := lookPath(name)
	if err != nil {
		dependency.Error = name + " not found on PATH"
		return dependency
	}
	dependency.Path = path
	output, err := run(ctx, path, args...)
	if err != nil {
		dependency.Error = "could not read " + name + " version"
		return dependency
	}
	version, err := parse(output)
	if err != nil {
		dependency.Error = err.Error()
		return dependency
	}
	dependency.Version = version
	dependency.OK = true
	return dependency
}

func checkState(
	ctx context.Context,
	probeTimeout time.Duration,
	store instance.Store,
	call func(context.Context, string, control.Request) (control.Status, error),
) StateReport {
	report := StateReport{
		StateRoot: store.StateRoot, RuntimeRoot: store.RuntimeRoot, SafePermissions: true,
	}
	for _, path := range []string{store.StateRoot, store.InstancesDir(), store.RuntimeRoot} {
		if path == "" {
			report.SafePermissions = false
			continue
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			report.SafePermissions = false
		}
	}
	records, problems, err := store.Inspect()
	if err != nil {
		report.Error = err.Error()
		return report
	}
	for _, problem := range problems {
		report.Malformed = append(report.Malformed, problem.Path)
	}
	for _, record := range records {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		status, err := call(probeCtx, record.ControlSocket, control.Request{
			InstanceID: record.ID, Action: control.ActionStatus,
		})
		cancel()
		if err == nil && status.ID == record.ID {
			report.Live++
		} else {
			if ctx.Err() != nil {
				report.Error = fmt.Sprintf("state inspection canceled: %v", ctx.Err())
				return report
			}
			recordStore, runtimeErr := store.ForRecord(record)
			if runtimeErr != nil {
				report.Error = runtimeErr.Error()
				continue
			}
			held, leaseErr := recordStore.LeaseHeld(record.ID)
			if leaseErr != nil {
				report.Error = leaseErr.Error()
			} else if held {
				report.Unreachable++
			} else {
				report.Stale++
			}
		}
	}
	return report
}

func checkLegacy(
	ctx context.Context,
	probeTimeout time.Duration,
	tmuxPath string,
	run func(context.Context, string, ...string) (string, error),
) []LegacyServer {
	var legacy []LegacyServer
	for _, name := range []string{"wrap", "wrap-ui"} {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		output, err := run(probeCtx, tmuxPath, "-L", name, "list-sessions", "-F", "#{session_name}")
		cancel()
		if err != nil {
			continue
		}
		var session string
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) != "" {
				session = line
				break
			}
		}
		if session == "" {
			continue
		}
		legacy = append(legacy, LegacyServer{
			Name:          name,
			Session:       session,
			ListCommand:   "tmux -L " + name + " list-sessions",
			AttachCommand: "tmux -L " + name + " attach-session -t " + shellWord(session),
		})
	}
	return legacy
}

func shellWord(value string) string {
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && !strings.ContainsRune("._-", char) {
			return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
		}
	}
	return value
}

func runCommand(ctx context.Context, path string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return string(output), nil
}
