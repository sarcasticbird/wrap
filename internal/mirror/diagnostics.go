package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxDiagnosticLogSize   = 1 << 20
	maxDiagnosticEventSize = 4 << 10
	maxDiagnosticPathSize  = 256
)

var diagnosticTokenPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var diagnosticComponents = map[string]struct{}{
	"credential": {},
	"handshake":  {},
	"server":     {},
	"tunnel":     {},
	"viewer":     {},
}

var diagnosticEvents = map[string]struct{}{
	"asset_missing":        {},
	"authenticated":        {},
	"closed":               {},
	"ended":                {},
	"exit":                 {},
	"geometry_corrected":   {},
	"geometry_failed":      {},
	"geometry_preparing":   {},
	"geometry_verified":    {},
	"open_failed":          {},
	"opened":               {},
	"preparing":            {},
	"process_exit":         {},
	"process_start_failed": {},
	"process_started":      {},
	"ready":                {},
	"rejected":             {},
	"revoked":              {},
	"rotated":              {},
	"start_failed":         {},
	"started":              {},
	"starting":             {},
	"stopped":              {},
}

var diagnosticCodes = map[string]struct{}{
	"authentication_failed":    {},
	"client_asset_unavailable": {},
	"client_capacity":          {},
	"credential_expired":       {},
	"origin_rejected":          {},
	"process_unavailable":      {},
	"server_busy":              {},
	"server_starting":          {},
	"server_unavailable":       {},
	"session_list_failed":      {},
	"terminal_ended":           {},
	"terminal_unavailable":     {},
	"tunnel_unavailable":       {},
	"unexpected_exit":          {},
	"upgrade_failed":           {},
}

type DiagnosticRecord struct {
	Level          string
	Component      string
	Event          string
	Code           string
	Path           string
	ViewerGeometry *ViewerGeometryDiagnostic
}

type ViewerGeometryDiagnostic struct {
	CapturedColumns uint16 `json:"captured_columns"`
	CapturedRows    uint16 `json:"captured_rows"`
	ClientColumns   uint16 `json:"client_columns"`
	ClientRows      uint16 `json:"client_rows"`
	WindowColumns   uint16 `json:"window_columns"`
	WindowRows      uint16 `json:"window_rows"`
	StatusRows      uint16 `json:"status_rows"`
	Corrected       bool   `json:"corrected"`
}

type DiagnosticSink interface {
	Write(DiagnosticRecord) error
}

type DiagnosticSinkFunc func(DiagnosticRecord) error

func (f DiagnosticSinkFunc) Write(record DiagnosticRecord) error {
	return f(record)
}

func emitDiagnostic(record func(DiagnosticRecord), event DiagnosticRecord) {
	if record != nil {
		record(event)
	}
}

type jsonlDiagnosticSink struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

type diagnosticLine struct {
	Timestamp      string                    `json:"timestamp"`
	Level          string                    `json:"level"`
	Component      string                    `json:"component"`
	Event          string                    `json:"event"`
	Code           string                    `json:"code,omitempty"`
	Path           string                    `json:"path,omitempty"`
	ViewerGeometry *ViewerGeometryDiagnostic `json:"viewer_geometry,omitempty"`
}

func NewJSONLDiagnosticSink(path string) DiagnosticSink {
	return newJSONLDiagnosticSink(path, time.Now)
}

func newJSONLDiagnosticSink(path string, now func() time.Time) *jsonlDiagnosticSink {
	return &jsonlDiagnosticSink{path: path, now: now}
}

func (s *jsonlDiagnosticSink) Write(record DiagnosticRecord) error {
	line, err := s.marshal(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return errors.New("mirror diagnostic path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create mirror diagnostic directory: %w", err)
	}
	info, err := os.Lstat(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect mirror diagnostic log: %w", err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("mirror diagnostic log is not a regular file")
	}
	if err == nil && info.Size()+int64(len(line)) > maxDiagnosticLogSize {
		backup := s.path + ".1"
		backupInfo, backupErr := os.Lstat(backup)
		if backupErr == nil {
			if !backupInfo.Mode().IsRegular() {
				return errors.New("mirror diagnostic backup is not a regular file")
			}
			if err := os.Remove(backup); err != nil {
				return fmt.Errorf("replace mirror diagnostic backup: %w", err)
			}
		} else if !os.IsNotExist(backupErr) {
			return fmt.Errorf("inspect mirror diagnostic backup: %w", backupErr)
		}
		if err := os.Rename(s.path, backup); err != nil {
			return fmt.Errorf("rotate mirror diagnostic log: %w", err)
		}
		if err := secureDiagnosticLog(backup); err != nil {
			removeErr := os.Remove(backup)
			return errors.Join(
				fmt.Errorf("secure mirror diagnostic backup: %w", err),
				removeErr,
			)
		}
	}
	file, err := openDiagnosticLog(s.path)
	if err != nil {
		return fmt.Errorf("open mirror diagnostic log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure mirror diagnostic log: %w", err)
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return fmt.Errorf("write mirror diagnostic log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close mirror diagnostic log: %w", err)
	}
	return nil
}

func secureDiagnosticLog(path string) error {
	file, err := openDiagnosticLog(path)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (s *jsonlDiagnosticSink) marshal(record DiagnosticRecord) ([]byte, error) {
	if record.Level != "info" && record.Level != "warn" && record.Level != "error" {
		return nil, errors.New("invalid mirror diagnostic level")
	}
	for name, value := range map[string]string{
		"component": record.Component,
		"event":     record.Event,
	} {
		if !diagnosticTokenPattern.MatchString(value) {
			return nil, fmt.Errorf("invalid mirror diagnostic %s", name)
		}
	}
	if _, ok := diagnosticComponents[record.Component]; !ok {
		return nil, errors.New("unsupported mirror diagnostic component")
	}
	if _, ok := diagnosticEvents[record.Event]; !ok {
		return nil, errors.New("unsupported mirror diagnostic event")
	}
	if _, ok := diagnosticCodes[record.Code]; record.Code != "" && !ok {
		return nil, errors.New("invalid mirror diagnostic code")
	}
	encoded, err := json.Marshal(diagnosticLine{
		Timestamp:      s.now().UTC().Format(time.RFC3339Nano),
		Level:          record.Level,
		Component:      record.Component,
		Event:          record.Event,
		Code:           record.Code,
		Path:           sanitizeDiagnosticPath(record.Path),
		ViewerGeometry: record.ViewerGeometry,
	})
	if err != nil {
		return nil, fmt.Errorf("encode mirror diagnostic event: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxDiagnosticEventSize {
		return nil, errors.New("mirror diagnostic event exceeds size limit")
	}
	return encoded, nil
}

func sanitizeDiagnosticPath(value string) string {
	if cutoff := strings.IndexAny(value, "?#"); cutoff >= 0 {
		value = value[:cutoff]
	}
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxDiagnosticPathSize {
		return value
	}
	value = value[:maxDiagnosticPathSize]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
