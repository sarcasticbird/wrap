package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/target"
)

func doctorRecord(root string) instance.Record {
	return instance.Record{
		Version:       instance.RecordVersion,
		ID:            "01KWRAPDOCTOR",
		Name:          "api",
		PID:           os.Getpid(),
		ControlSocket: filepath.Join(root, "runtime", "01KWRAPDOCTOR.sock"),
		StartedAt:     time.Unix(100, 0).UTC(),
		Directory:     "/work/api",
		Target: target.Target{
			SocketPath: "/tmp/tmux.sock", Generation: "0123456789abcdef0123456789abcdef",
			SessionID: "$1", WindowID: "@2", Directory: "/work/api",
		},
	}
}

func TestCheckReportsDependenciesStateAndLegacyServersReadOnly(t *testing.T) {
	root := t.TempDir()
	store := instance.Store{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime")}
	record := doctorRecord(root)
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(store.InstancesDir(), "01KWRAPBROKEN.json")
	if err := os.WriteFile(malformed, []byte(`{"broken":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	report := Check(t.Context(), Options{
		Platform: "darwin",
		Store:    store,
		LookPath: func(name string) (string, error) { return "/opt/bin/" + name, nil },
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch joined {
			case "-V":
				return "tmux 3.5a", nil
			case "--version":
				return "cloudflared version 2026.7.0", nil
			case "-L wrap list-sessions -F #{session_name}":
				return " legacy api \nother", nil
			case "-L wrap-ui list-sessions -F #{session_name}":
				return "wrap-ui", nil
			default:
				return "", errors.New("unexpected command: " + joined)
			}
		},
		Call: func(context.Context, string, control.Request) (control.Status, error) {
			return control.Status{ID: record.ID, State: "ready"}, nil
		},
	})
	if !report.Tmux.OK || report.Tmux.Version != "3.5a" ||
		!report.Cloudflared.OK || report.Cloudflared.Version != "2026.7.0" {
		t.Fatalf("dependencies = %+v / %+v", report.Tmux, report.Cloudflared)
	}
	if report.State.Live != 1 || report.State.Stale != 0 || len(report.State.Malformed) != 1 {
		t.Fatalf("state = %+v", report.State)
	}
	if len(report.Legacy) != 2 || report.Legacy[0].ListCommand != "tmux -L wrap list-sessions" ||
		report.Legacy[0].AttachCommand != "tmux -L wrap attach-session -t ' legacy api '" {
		t.Fatalf("legacy = %+v", report.Legacy)
	}
}

func TestCheckReportsMissingAndOldDependenciesWithoutStopping(t *testing.T) {
	root := t.TempDir()
	report := Check(t.Context(), Options{
		Platform: "linux",
		Store: instance.Store{
			StateRoot: filepath.Join(root, "absent-state"), RuntimeRoot: filepath.Join(root, "absent-runtime"),
		},
		LookPath: func(name string) (string, error) {
			if name == "cloudflared" {
				return "", errors.New("not found")
			}
			return "/usr/bin/tmux", nil
		},
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			if len(args) == 1 && args[0] == "-V" {
				return "tmux 3.1", nil
			}
			return "", errors.New("no legacy server")
		},
	})
	if report.Tmux.OK || !strings.Contains(report.Tmux.Error, "3.2") {
		t.Fatalf("tmux = %+v", report.Tmux)
	}
	if report.Cloudflared.OK || report.Cloudflared.Error == "" {
		t.Fatalf("cloudflared = %+v", report.Cloudflared)
	}
	if _, err := os.Stat(filepath.Join(root, "absent-state")); !os.IsNotExist(err) {
		t.Fatalf("doctor created absent state: %v", err)
	}
}

func TestCheckReportsUnsafePermissionsAndStaleRecord(t *testing.T) {
	root := t.TempDir()
	store := instance.Store{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime")}
	record := doctorRecord(root)
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.StateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	report := Check(t.Context(), Options{
		Store:    store,
		LookPath: func(string) (string, error) { return "", errors.New("missing") },
		Run:      func(context.Context, string, ...string) (string, error) { return "", errors.New("missing") },
		Call: func(context.Context, string, control.Request) (control.Status, error) {
			return control.Status{}, errors.New("unreachable")
		},
	})
	if report.State.SafePermissions || report.State.Stale != 1 || report.State.Unreachable != 0 || report.State.Live != 0 {
		t.Fatalf("state = %+v", report.State)
	}
}

func TestCheckDistinguishesHeldUnreachableWorkerLease(t *testing.T) {
	root := t.TempDir()
	store := instance.Store{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime")}
	record := doctorRecord(root)
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	report := Check(t.Context(), Options{
		Store:    store,
		LookPath: func(string) (string, error) { return "", errors.New("missing") },
		Run:      func(context.Context, string, ...string) (string, error) { return "", errors.New("missing") },
		Call: func(context.Context, string, control.Request) (control.Status, error) {
			return control.Status{}, errors.New("unreachable")
		},
	})
	if report.State.Unreachable != 1 || report.State.Stale != 0 || report.State.Live != 0 {
		t.Fatalf("state = %+v", report.State)
	}
}

func TestCheckUsesRecordedRuntimeLeaseAfterRuntimeRootChanges(t *testing.T) {
	root := t.TempDir()
	store := instance.Store{
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "current-runtime"),
	}
	record := doctorRecord(root)
	record.ControlSocket = filepath.Join(root, "recorded-runtime", record.ID+".sock")
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	recordStore, err := store.ForRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := recordStore.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	report := Check(t.Context(), Options{
		Store:    store,
		LookPath: func(string) (string, error) { return "", errors.New("missing") },
		Call: func(context.Context, string, control.Request) (control.Status, error) {
			return control.Status{}, errors.New("unreachable")
		},
	})
	if report.State.Unreachable != 1 || report.State.Stale != 0 || report.State.Error != "" {
		t.Fatalf("state = %+v", report.State)
	}
}

func TestCheckMarksCanceledStateInspectionIncomplete(t *testing.T) {
	root := t.TempDir()
	store := instance.Store{
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"),
	}
	record := doctorRecord(root)
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := Check(ctx, Options{
		Store:    store,
		LookPath: func(string) (string, error) { return "", errors.New("missing") },
		Call: func(ctx context.Context, _ string, _ control.Request) (control.Status, error) {
			return control.Status{}, ctx.Err()
		},
	})
	if report.State.Error == "" || report.State.Live != 0 || report.State.Stale != 0 || report.State.Unreachable != 0 {
		t.Fatalf("canceled state inspection = %+v", report.State)
	}
}
