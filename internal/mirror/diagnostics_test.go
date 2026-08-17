package mirror

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type recordingDiagnosticSink struct {
	mu      sync.Mutex
	records []DiagnosticRecord
}

func (s *recordingDiagnosticSink) Write(record DiagnosticRecord) error {
	if _, err := newJSONLDiagnosticSink("", time.Now).marshal(record); err != nil {
		return err
	}
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	return nil
}

func (s *recordingDiagnosticSink) snapshot() []DiagnosticRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DiagnosticRecord(nil), s.records...)
}

func containsDiagnostic(records []DiagnosticRecord, component, event, code string) bool {
	for _, record := range records {
		if record.Component == component && record.Event == event && record.Code == code {
			return true
		}
	}
	return false
}

func TestJSONLDiagnosticsWritesPrivateBoundedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "mirror.log")
	now := time.Date(2026, time.July, 31, 12, 0, 0, 123, time.UTC)
	sink := newJSONLDiagnosticSink(path, func() time.Time { return now })
	if err := sink.Write(DiagnosticRecord{
		Level:     "warn",
		Component: "server",
		Event:     "asset_missing",
		Code:      "client_asset_unavailable",
		Path:      "/assets/" + strings.Repeat("é", 180) + ".js?token=SENTINEL#credential",
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxDiagnosticEventSize || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("diagnostic event length/newline = %d/%v", len(data), bytes.HasSuffix(data, []byte("\n")))
	}
	for _, forbidden := range []string{"SENTINEL", "credential", "token="} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("diagnostic record leaked %q: %s", forbidden, data)
		}
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("decode diagnostic JSONL: %v", err)
	}
	if got := record["timestamp"]; got != "2026-07-31T12:00:00.000000123Z" {
		t.Fatalf("timestamp = %v", got)
	}
	if got := record["path"].(string); len(got) > maxDiagnosticPathSize || !utf8.ValidString(got) {
		t.Fatalf("sanitized path length/UTF-8 = %d/%v", len(got), utf8.ValidString(got))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mirror log mode = %o, want 600", got)
	}
}

func TestJSONLDiagnosticsWritesSafeViewerGeometry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.log")
	sink := newJSONLDiagnosticSink(path, func() time.Time {
		return time.Date(2026, time.July, 31, 18, 0, 0, 0, time.UTC)
	})
	if err := sink.Write(DiagnosticRecord{
		Level:     "info",
		Component: "viewer",
		Event:     "geometry_verified",
		ViewerGeometry: &ViewerGeometryDiagnostic{
			CapturedColumns: 160,
			CapturedRows:    50,
			ClientColumns:   160,
			ClientRows:      50,
			WindowColumns:   160,
			WindowRows:      49,
			StatusRows:      1,
			Corrected:       true,
		},
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"@4", "$7", "/dev/tty", "trycloudflare", "#k="} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("viewer geometry diagnostic leaked %q: %s", forbidden, data)
		}
	}
	var line struct {
		ViewerGeometry ViewerGeometryDiagnostic `json:"viewer_geometry"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &line); err != nil {
		t.Fatal(err)
	}
	want := ViewerGeometryDiagnostic{
		CapturedColumns: 160,
		CapturedRows:    50,
		ClientColumns:   160,
		ClientRows:      50,
		WindowColumns:   160,
		WindowRows:      49,
		StatusRows:      1,
		Corrected:       true,
	}
	if line.ViewerGeometry != want {
		t.Fatalf("viewer geometry = %+v, want %+v", line.ViewerGeometry, want)
	}
}

func TestJSONLDiagnosticsRejectsUnsafeSchemaTokens(t *testing.T) {
	for index, record := range []DiagnosticRecord{
		{Level: "debug", Component: "server", Event: "started"},
		{Level: "info", Component: "server\nsecret", Event: "started"},
		{Level: "info", Component: "server", Event: "terminal output"},
		{Level: "info", Component: "server", Event: "started", Code: "raw:error"},
		{Level: "info", Component: "session", Event: "started"},
		{Level: "info", Component: "viewer", Event: "terminal_output"},
		{Level: "info", Component: "handshake", Event: "nonce"},
		{Level: "warn", Component: "handshake", Event: "rejected", Code: "credential"},
		{Level: "warn", Component: "handshake", Event: "rejected", Code: "cookie"},
	} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("mirror-%d.log", index))
		sink := newJSONLDiagnosticSink(path, time.Now)
		if err := sink.Write(record); err == nil {
			t.Fatalf("unsafe diagnostic record was accepted: %+v", record)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unsafe record created a log: %v", err)
		}
	}
}

func TestJSONLDiagnosticsAcceptsAutomaticTargetUnavailable(t *testing.T) {
	sink := newJSONLDiagnosticSink(filepath.Join(t.TempDir(), "mirror.jsonl"), time.Now)
	err := sink.Write(DiagnosticRecord{
		Level: "warn", Component: "handshake", Event: "rejected", Code: "automatic_target_unavailable",
	})
	if err != nil {
		t.Fatalf("Write() automatic target diagnostic = %v", err)
	}
}

func TestJSONLDiagnosticsAcceptsRotationCleanupWarning(t *testing.T) {
	sink := newJSONLDiagnosticSink(filepath.Join(t.TempDir(), "mirror.jsonl"), time.Now)
	err := sink.Write(DiagnosticRecord{
		Level: "warn", Component: "credential", Event: "rotated", Code: "cleanup_incomplete",
	})
	if err != nil {
		t.Fatalf("cleanup warning diagnostic = %v", err)
	}
}

func TestJSONLDiagnosticsRotatesOneBackupAtLimit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "mirror.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxDiagnosticLogSize-8), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := newJSONLDiagnosticSink(path, time.Now)
	record := DiagnosticRecord{Level: "info", Component: "server", Event: "started"}
	if err := sink.Write(record); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(backup) != maxDiagnosticLogSize-8 {
		t.Fatalf("first rotated log length = %d", len(backup))
	}
	backupInfo, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if got := backupInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("rotated mirror log mode = %o, want 600", got)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), maxDiagnosticLogSize-8), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(record); err != nil {
		t.Fatal(err)
	}
	backup, err = os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(backup) == 0 || backup[0] != 'y' {
		t.Fatal("second rotation did not replace the single backup")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("diagnostic rotation retained %d files, want 2", len(entries))
	}
}

func TestJSONLDiagnosticsRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mirror.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	err := newJSONLDiagnosticSink(path, time.Now).Write(DiagnosticRecord{
		Level: "info", Component: "server", Event: "started",
	})
	if err == nil {
		t.Fatal("symlink diagnostic log was accepted")
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if string(data) != "sentinel" || info.Mode().Perm() != 0o640 {
		t.Fatalf("symlink target content/mode = %q/%o", data, info.Mode().Perm())
	}
}

func TestPortableDiagnosticOpenRejectsSymlinkInsertedAfterMissingCheck(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mirror.log")
	inserted := false
	ops := portableDiagnosticFileOps{
		lstat: os.Lstat,
		openFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
			if flags&os.O_EXCL != 0 && !inserted {
				inserted = true
				if err := os.Symlink(target, path); err != nil {
					return nil, err
				}
			}
			return os.OpenFile(name, flags, mode)
		},
	}
	if _, err := openDiagnosticLogPortable(path, ops); err == nil {
		t.Fatal("symlink inserted after missing-file check was accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sentinel" || info.Mode().Perm() != 0o640 {
		t.Fatalf("raced symlink target content/mode = %q/%o", data, info.Mode().Perm())
	}
}

func TestJSONLDiagnosticsRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.log")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- newJSONLDiagnosticSink(path, time.Now).Write(DiagnosticRecord{
			Level: "info", Component: "server", Event: "started",
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO diagnostic log was accepted")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("FIFO diagnostic log blocked the writer")
	}
}

func TestJSONLDiagnosticsSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.log")
	sink := newJSONLDiagnosticSink(path, time.Now)
	const writers = 50
	var wait sync.WaitGroup
	errors := make(chan error, writers)
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errors <- sink.Write(DiagnosticRecord{
				Level: "info", Component: "viewer", Event: "opened",
			})
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != writers {
		t.Fatalf("diagnostic lines = %d, want %d", len(lines), writers)
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("concurrent writer produced invalid JSON: %v", err)
		}
	}
}
