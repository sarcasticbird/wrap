package mirror

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCloudflaredVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		want   string
		ok     bool
	}{
		{"cloudflared version 2026.7.0 (built 2026-07-01)", "2026.7.0", true},
		{"cloudflared version 2020.5.1", "2020.5.1", true},
		{"cloudflared version 2020.5.0", "", false},
		{"cloudflared dev", "", false},
	} {
		got, err := parseCloudflaredVersion(test.output)
		if test.ok && (err != nil || got != test.want) {
			t.Errorf("parseCloudflaredVersion(%q) = %q, %v", test.output, got, err)
		}
		if !test.ok && err == nil {
			t.Errorf("parseCloudflaredVersion(%q) unexpectedly succeeded with %q", test.output, got)
		}
	}
}

func TestExtractQuickTunnelURLAcceptsOnlyExactOrigin(t *testing.T) {
	got, err := extractQuickTunnelURL(strings.Join([]string{
		"INF Requesting new quick Tunnel on trycloudflare.com...",
		"INF https://quiet-river.trycloudflare.com",
	}, "\n"))
	if err != nil || got != "https://quiet-river.trycloudflare.com" {
		t.Fatalf("extractQuickTunnelURL = %q, %v", got, err)
	}
	for _, invalid := range []string{
		"https://quiet-river.trycloudflare.com/path",
		"http://quiet-river.trycloudflare.com",
		"https://quiet-river.trycloudflare.com.evil.test",
		"https://one.trycloudflare.com https://two.trycloudflare.com",
	} {
		if got, err := extractQuickTunnelURL(invalid); err == nil || got != "" {
			t.Errorf("extractQuickTunnelURL(%q) = %q, %v", invalid, got, err)
		}
	}
}

func TestDiagnosticTailIsBounded(t *testing.T) {
	var tail diagnosticTail
	input := []byte(strings.Repeat("a", maxTunnelDiagnostics+100))
	if n, err := tail.Write(input); err != nil || n != len(input) {
		t.Fatalf("diagnostic tail write = %d, %v", n, err)
	}
	got := tail.String()
	if len(got) != maxTunnelDiagnostics {
		t.Fatalf("diagnostic tail length = %d", len(got))
	}
	if strings.Contains(got, strings.Repeat("a", maxTunnelDiagnostics+1)) {
		t.Fatal("diagnostic tail retained more than its bound")
	}
}

func TestStartTunnelUsesDocumentedCommandAndReapsOnClose(t *testing.T) {
	script := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "cloudflared version 2026.7.0"
  exit 0
fi
echo "INF https://quiet-river.trycloudflare.com"
trap 'exit 0' INT TERM
while :; do sleep 1; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls [][]string
	diagnostics := &recordingDiagnosticSink{}
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		calls = append(calls, append([]string{name}, args...))
		mu.Unlock()
		return exec.CommandContext(ctx, name, args...)
	}
	tunnel, err := StartTunnel(t.Context(), "http://127.0.0.1:43210", TunnelOptions{
		LookPath:       func(string) (string, error) { return script, nil },
		Command:        command,
		StartupTimeout: time.Second,
		StopTimeout:    time.Second,
		Record: func(record DiagnosticRecord) {
			_ = diagnostics.Write(record)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.URL() != "https://quiet-river.trycloudflare.com" {
		t.Fatalf("tunnel URL = %q", tunnel.URL())
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("cloudflared calls = %v", calls)
	}
	got := strings.Join(calls[1], " ")
	if !strings.HasSuffix(got, "tunnel --no-autoupdate --url http://127.0.0.1:43210") {
		t.Fatalf("tunnel command = %q", got)
	}
	records := diagnostics.snapshot()
	for _, event := range []string{"process_started", "ready", "stopped"} {
		if !containsDiagnostic(records, "tunnel", event, "") {
			t.Fatalf("missing tunnel diagnostic %q in %+v", event, records)
		}
	}
}

func TestStartTunnelRejectsNonLoopbackOrigin(t *testing.T) {
	_, err := StartTunnel(t.Context(), "http://0.0.0.0:43210", TunnelOptions{})
	if err == nil {
		t.Fatal("accepted non-loopback tunnel origin")
	}
}

func TestTunnelWaitErrorSuppressesOnlyIntentionalShutdown(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 7")
	exitErr := command.Run()
	if exitErr == nil {
		t.Fatal("fixture command unexpectedly succeeded")
	}
	otherErr := errors.New("wait failed")

	for _, test := range []struct {
		name    string
		err     error
		closing bool
		want    error
	}{
		{"intentional context cancellation", context.Canceled, true, nil},
		{"unexpected context cancellation", context.Canceled, false, context.Canceled},
		{"intentional process exit", exitErr, true, nil},
		{"unexpected process exit", exitErr, false, exitErr},
		{"intentional unrelated error", otherErr, true, otherErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := tunnelWaitError(test.err, test.closing)
			if !errors.Is(got, test.want) {
				t.Fatalf("tunnelWaitError(%v, %v) = %v, want %v",
					test.err, test.closing, got, test.want)
			}
		})
	}
}
