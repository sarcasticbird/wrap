package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxTunnelDiagnostics = 64 << 10

var cloudflaredVersionPattern = regexp.MustCompile(`\b(\d{4})\.(\d+)\.(\d+)\b`)

func parseCloudflaredVersion(output string) (string, error) {
	match := cloudflaredVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return "", errors.New("cloudflared version could not be parsed")
	}
	parts := make([]int, 3)
	for i := range parts {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return "", errors.New("cloudflared version could not be parsed")
		}
		parts[i] = value
	}
	minimum := [3]int{2020, 5, 1}
	for i, value := range parts {
		if value > minimum[i] {
			return match[0], nil
		}
		if value < minimum[i] {
			return "", fmt.Errorf("cloudflared %s is older than required 2020.5.1", match[0])
		}
	}
	return match[0], nil
}

func extractQuickTunnelURL(output string) (string, error) {
	var found string
	for _, field := range strings.Fields(output) {
		candidate := strings.Trim(field, "|[]()<>\"',")
		if !strings.Contains(candidate, "trycloudflare.com") {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil ||
			parsed.Scheme != "https" ||
			!quickTunnelHost.MatchString(parsed.Hostname()) ||
			parsed.Host != parsed.Hostname() ||
			parsed.User != nil ||
			parsed.Path != "" ||
			parsed.RawQuery != "" ||
			parsed.Fragment != "" {
			continue
		}
		origin := parsed.String()
		if found != "" && origin != found {
			return "", errors.New("cloudflared reported multiple Quick Tunnel URLs")
		}
		found = origin
	}
	if found == "" {
		return "", errors.New("cloudflared did not report a valid Quick Tunnel URL")
	}
	return found, nil
}

type diagnosticTail struct {
	mu   sync.Mutex
	data []byte
}

type commandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd
type pathLookup func(string) (string, error)

type TunnelOptions struct {
	LookPath       pathLookup
	Command        commandFactory
	StartupTimeout time.Duration
	StopTimeout    time.Duration
}

type Tunnel struct {
	url     string
	command *exec.Cmd
	cancel  context.CancelFunc
	stop    time.Duration
	done    chan error
	waited  chan struct{}

	mu      sync.Mutex
	waitErr error
	closing bool
	once    sync.Once
}

func StartTunnel(ctx context.Context, localURL string, options TunnelOptions) (*Tunnel, error) {
	if err := validateTunnelOrigin(localURL); err != nil {
		return nil, err
	}
	lookPath := options.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	commandFactory := options.Command
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	startupTimeout := options.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = TunnelStartupTimeout
	}
	stopTimeout := options.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = 2 * time.Second
	}
	path, err := lookPath("cloudflared")
	if err != nil {
		return nil, fmt.Errorf("cloudflared not found in PATH: %w", err)
	}

	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()
	versionOutput, err := commandFactory(startupCtx, path, "--version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("read cloudflared version: %w", err)
	}
	if _, err := parseCloudflaredVersion(string(versionOutput)); err != nil {
		return nil, err
	}

	processCtx, cancelProcess := context.WithCancel(ctx)
	command := commandFactory(
		processCtx,
		path,
		"tunnel",
		"--no-autoupdate",
		"--url",
		localURL,
	)
	var diagnostics diagnosticTail
	outputChanged := make(chan struct{}, 1)
	output := io.MultiWriter(&diagnostics, notifyWriter{changed: outputChanged})
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		cancelProcess()
		return nil, fmt.Errorf("start cloudflared Quick Tunnel: %w", err)
	}

	tunnel := &Tunnel{
		command: command,
		cancel:  cancelProcess,
		stop:    stopTimeout,
		done:    make(chan error, 1),
		waited:  make(chan struct{}),
	}
	go tunnel.wait()

	for {
		if publicURL, parseErr := extractQuickTunnelURL(diagnostics.String()); parseErr == nil {
			tunnel.url = publicURL
			return tunnel, nil
		}
		select {
		case <-outputChanged:
		case <-tunnel.waited:
			return nil, fmt.Errorf(
				"cloudflared exited before reporting a Quick Tunnel URL: %w: %s",
				tunnel.waitError(),
				strings.TrimSpace(diagnostics.String()),
			)
		case <-startupCtx.Done():
			_ = tunnel.Close()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf(
				"cloudflared did not report a Quick Tunnel URL within %s: %s",
				startupTimeout,
				strings.TrimSpace(diagnostics.String()),
			)
		case <-ctx.Done():
			_ = tunnel.Close()
			return nil, ctx.Err()
		}
	}
}

func (t *Tunnel) URL() string {
	return t.url
}

func (t *Tunnel) Done() <-chan error {
	return t.done
}

func (t *Tunnel) Close() error {
	t.once.Do(func() {
		t.mu.Lock()
		t.closing = true
		t.mu.Unlock()
		if t.command.Process != nil {
			_ = t.command.Process.Signal(os.Interrupt)
		}
		timer := time.NewTimer(t.stop)
		defer timer.Stop()
		select {
		case <-t.waited:
		case <-timer.C:
			if t.command.Process != nil {
				_ = t.command.Process.Kill()
			}
			t.cancel()
			<-t.waited
		}
		t.cancel()
	})
	return t.waitError()
}

func (t *Tunnel) wait() {
	err := t.command.Wait()
	t.mu.Lock()
	closing := t.closing
	err = tunnelWaitError(err, closing)
	t.waitErr = err
	t.mu.Unlock()
	if !closing {
		t.done <- err
	}
	close(t.done)
	close(t.waited)
}

func tunnelWaitError(err error, closing bool) error {
	if !closing {
		return err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (t *Tunnel) waitError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waitErr
}

type notifyWriter struct {
	changed chan<- struct{}
}

func (w notifyWriter) Write(data []byte) (int, error) {
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return len(data), nil
}

func validateTunnelOrigin(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse tunnel origin: %w", err)
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if parsed.Scheme != "http" ||
		ip == nil ||
		!ip.IsLoopback() ||
		parsed.Port() == "" ||
		parsed.User != nil ||
		parsed.Path != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return errors.New("tunnel origin must be an HTTP loopback origin with an explicit port")
	}
	return nil
}

func (t *diagnosticTail) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	original := len(data)
	if len(data) >= maxTunnelDiagnostics {
		t.data = append(t.data[:0], data[len(data)-maxTunnelDiagnostics:]...)
		return original, nil
	}
	overflow := len(t.data) + len(data) - maxTunnelDiagnostics
	if overflow > 0 {
		copy(t.data, t.data[overflow:])
		t.data = t.data[:len(t.data)-overflow]
	}
	t.data = append(t.data, data...)
	return original, nil
}

func (t *diagnosticTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(append([]byte(nil), t.data...))
}
