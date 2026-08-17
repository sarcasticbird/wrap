package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Store struct {
	StateRoot   string
	RuntimeRoot string
}

type Lease struct {
	file *os.File
}

// ForRecord returns a store whose runtime ownership follows the control socket
// recorded when the worker started. State remains anchored to the caller's
// current StateRoot, while leases and sockets use the recorded runtime root.
func (s Store) ForRecord(record Record) (Store, error) {
	path := filepath.Clean(record.ControlSocket)
	if !filepath.IsAbs(path) || path != record.ControlSocket || filepath.Base(path) != record.ID+".sock" {
		return Store{}, errors.New("instance control socket is outside its recorded runtime path")
	}
	runtimeRoot := filepath.Dir(path)
	if runtimeRoot == string(filepath.Separator) {
		return Store{}, errors.New("instance runtime root is too broad")
	}
	s.RuntimeRoot = runtimeRoot
	return s, nil
}

func (s Store) AcquireLease(id string) (*Lease, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid instance ID %q", id)
	}
	if s.RuntimeRoot == "" {
		return nil, errors.New("instance runtime root is required")
	}
	if err := mkdirPrivate(s.RuntimeRoot); err != nil {
		return nil, err
	}
	path := filepath.Join(s.RuntimeRoot, id+".lease")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("instance lease is not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect instance lease: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lease: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect open instance lease: %w", err)
		}
		return nil, errors.New("instance lease is not a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLeaseHeld
		}
		return nil, fmt.Errorf("lock instance lease: %w", err)
	}
	return &Lease{file: file}, nil
}

func (s Store) LeaseHeld(id string) (bool, error) {
	if !idPattern.MatchString(id) {
		return false, fmt.Errorf("invalid instance ID %q", id)
	}
	path := filepath.Join(s.RuntimeRoot, id+".lease")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect instance lease: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("instance lease is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open instance lease: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("probe instance lease: %w", err)
	}
	return false, syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func (l *Lease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func (s Store) InstancesDir() string {
	return filepath.Join(s.StateRoot, "instances")
}

func (s Store) DiagnosticsDir() string {
	return filepath.Join(s.StateRoot, "diagnostics")
}

// PruneCompletedArtifacts retains the newest completed instance artifacts.
// Active records and held worker leases are never removed. Each deletion holds
// the instance lease, preventing a pre-publication worker from racing cleanup.
func (s Store) PruneCompletedArtifacts(retain int) error {
	if retain < 0 {
		return errors.New("completed artifact retention must not be negative")
	}
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	records, problems, err := s.readAllUnlocked()
	if err != nil {
		return err
	}
	protected := make(map[string]struct{}, len(records)+len(problems))
	for _, record := range records {
		protected[record.ID] = struct{}{}
	}
	for _, problem := range problems {
		name := filepath.Base(problem.Path)
		if id := strings.TrimSuffix(name, ".json"); idPattern.MatchString(id) {
			protected[id] = struct{}{}
		}
	}
	type artifact struct {
		id       string
		modified time.Time
	}
	candidates := make(map[string]time.Time)
	collect := func(directory string, idForName func(string) string) error {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			id := idForName(entry.Name())
			if !idPattern.MatchString(id) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if current, ok := candidates[id]; !ok || info.ModTime().After(current) {
				candidates[id] = info.ModTime()
			}
		}
		return nil
	}
	if err := collect(s.DiagnosticsDir(), diagnosticArtifactID); err != nil {
		return fmt.Errorf("inspect completed diagnostics: %w", err)
	}
	if err := collect(s.RuntimeRoot, func(name string) string {
		if !strings.HasSuffix(name, ".lease") {
			return ""
		}
		return strings.TrimSuffix(name, ".lease")
	}); err != nil {
		return fmt.Errorf("inspect completed leases: %w", err)
	}
	artifacts := make([]artifact, 0, len(candidates))
	for id, modified := range candidates {
		if _, ok := protected[id]; !ok {
			artifacts = append(artifacts, artifact{id: id, modified: modified})
		}
	}
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].modified.Equal(artifacts[j].modified) {
			return artifacts[i].id > artifacts[j].id
		}
		return artifacts[i].modified.After(artifacts[j].modified)
	})
	var pruneErr error
	for _, item := range artifacts[min(retain, len(artifacts)):] {
		if err := s.pruneCompletedArtifact(item.id); err != nil {
			pruneErr = errors.Join(pruneErr, err)
		}
	}
	return pruneErr
}

func diagnosticArtifactID(name string) string {
	if strings.HasSuffix(name, ".jsonl.1") {
		return strings.TrimSuffix(name, ".jsonl.1")
	}
	if strings.HasSuffix(name, ".jsonl") {
		return strings.TrimSuffix(name, ".jsonl")
	}
	return ""
}

func (s Store) pruneCompletedArtifact(id string) error {
	lease, err := s.AcquireLease(id)
	if errors.Is(err, ErrLeaseHeld) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock completed instance %s: %w", id, err)
	}
	leasePath := filepath.Join(s.RuntimeRoot, id+".lease")
	var cleanupErr error
	for _, suffix := range []string{".jsonl", ".jsonl.1"} {
		cleanupErr = errors.Join(cleanupErr, removeRegularArtifact(filepath.Join(s.DiagnosticsDir(), id+suffix)))
	}
	openInfo, openErr := lease.file.Stat()
	pathInfo, pathErr := os.Lstat(leasePath)
	switch {
	case openErr != nil:
		cleanupErr = errors.Join(cleanupErr, openErr)
	case pathErr != nil && !errors.Is(pathErr, fs.ErrNotExist):
		cleanupErr = errors.Join(cleanupErr, pathErr)
	case pathErr == nil && !os.SameFile(openInfo, pathInfo):
		cleanupErr = errors.Join(cleanupErr, errors.New("completed instance lease identity changed"))
	case pathErr == nil:
		cleanupErr = errors.Join(cleanupErr, os.Remove(leasePath))
	}
	return errors.Join(cleanupErr, lease.Close())
}

func removeRegularArtifact(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("completed artifact is not a regular file")
	}
	return os.Remove(path)
}

func (s Store) Create(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	records, _, err := s.readAllUnlocked()
	if err != nil {
		return err
	}
	for _, existing := range records {
		switch {
		case existing.ID == record.ID:
			return fmt.Errorf("instance ID %q already exists", record.ID)
		case existing.Name == record.Name:
			return fmt.Errorf("instance name %q already exists", record.Name)
		case existing.ID == record.Name:
			return fmt.Errorf("instance selector %q collides with an existing name or ID", record.Name)
		case existing.Name == record.ID:
			return fmt.Errorf("instance selector %q collides with an existing name or ID", record.ID)
		case existing.Target.Key() == record.Target.Key():
			return fmt.Errorf("tmux target already belongs to Wrap %q", existing.Name)
		}
	}
	return s.writeAtomic(record, false)
}

func (s Store) RenameIfPID(id string, pid int, name string) (bool, error) {
	if !idPattern.MatchString(id) || pid <= 0 {
		return false, errors.New("valid instance ID and PID are required")
	}
	if err := ValidateName(name); err != nil {
		return false, err
	}
	release, err := s.lock()
	if err != nil {
		return false, err
	}
	defer release()
	records, _, err := s.readAllUnlocked()
	if err != nil {
		return false, err
	}
	var current *Record
	for index := range records {
		record := &records[index]
		if record.Name == name && record.ID != id {
			return false, fmt.Errorf("instance name %q already exists", name)
		}
		if record.ID == name && record.ID != id {
			return false, fmt.Errorf("instance selector %q collides with an existing ID", name)
		}
		if record.ID == id && record.PID == pid {
			current = record
		}
	}
	if current == nil {
		return false, nil
	}
	current.Name = name
	if err := s.writeAtomic(*current, true); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) ReadAll() ([]Record, []RecordError, error) {
	release, err := s.lock()
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return s.readAllUnlocked()
}

func (s Store) Inspect() ([]Record, []RecordError, error) {
	if s.StateRoot == "" {
		return nil, nil, errors.New("instance state root is required")
	}
	info, err := os.Lstat(s.InstancesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect instance directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("instance path is not a regular directory")
	}
	return s.readAllUnlocked()
}

func (s Store) Resolve(selector string) (Record, error) {
	records, _, err := s.ReadAll()
	if err != nil {
		return Record{}, err
	}
	exact := make([]Record, 0, 2)
	for _, record := range records {
		if record.Name == selector || record.ID == selector {
			exact = append(exact, record)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return Record{}, fmt.Errorf("%w: %q matches %d instances", ErrAmbiguous, selector, len(exact))
	}
	matches := make([]Record, 0, 2)
	for _, record := range records {
		if strings.HasPrefix(record.ID, selector) {
			matches = append(matches, record)
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, fmt.Errorf("%w: %q", ErrNotFound, selector)
	case 1:
		return matches[0], nil
	default:
		return Record{}, fmt.Errorf("%w: %q matches %d instances", ErrAmbiguous, selector, len(matches))
	}
}

func (s Store) Remove(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid instance ID %q", id)
	}
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	path := filepath.Join(s.InstancesDir(), id+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove instance record: %w", err)
	}
	return nil
}

func (s Store) RemoveIfPID(id string, pid int) (bool, error) {
	if !idPattern.MatchString(id) {
		return false, fmt.Errorf("invalid instance ID %q", id)
	}
	if pid <= 0 {
		return false, errors.New("instance PID must be positive")
	}
	release, err := s.lock()
	if err != nil {
		return false, err
	}
	defer release()
	path := filepath.Join(s.InstancesDir(), id+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect instance record: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("instance record is not a regular file")
	}
	record, err := readRecord(path)
	if err != nil {
		return false, err
	}
	if record.ID != id || record.PID != pid {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove instance record: %w", err)
	}
	return true, nil
}

func (s Store) lock() (func(), error) {
	if s.StateRoot == "" {
		return nil, errors.New("instance state root is required")
	}
	if err := mkdirPrivate(s.StateRoot); err != nil {
		return nil, err
	}
	if err := mkdirPrivate(s.InstancesDir()); err != nil {
		return nil, err
	}
	if s.RuntimeRoot != "" {
		if err := mkdirPrivate(s.RuntimeRoot); err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(s.InstancesDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock instances: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

func mkdirPrivate(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private path %s is not a regular directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect private directory %s: %w", path, err)
	}
	return nil
}

func (s Store) readAllUnlocked() ([]Record, []RecordError, error) {
	entries, err := os.ReadDir(s.InstancesDir())
	if err != nil {
		return nil, nil, fmt.Errorf("list instance records: %w", err)
	}
	records := make([]Record, 0, len(entries))
	problems := make([]RecordError, 0)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(s.InstancesDir(), name)
		info, err := os.Lstat(path)
		if err != nil {
			problems = append(problems, RecordError{Path: path, Err: err})
			continue
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			problems = append(problems, RecordError{Path: path, Err: errors.New("instance record is not a regular file")})
			continue
		}
		record, err := readRecord(path)
		if err != nil {
			problems = append(problems, RecordError{Path: path, Err: err})
			continue
		}
		if strings.TrimSuffix(name, ".json") != record.ID {
			problems = append(problems, RecordError{Path: path, Err: errors.New("record ID does not match filename")})
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Name == records[j].Name {
			return records[i].ID < records[j].ID
		}
		return records[i].Name < records[j].Name
	})
	return records, problems, nil
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode instance record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Record{}, errors.New("instance record contains trailing JSON")
		}
		return Record{}, err
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s Store) writeAtomic(record Record, replace bool) (retErr error) {
	dir := s.InstancesDir()
	path := filepath.Join(dir, record.ID+".json")
	if info, err := os.Lstat(path); err == nil {
		if !replace {
			return fmt.Errorf("instance record path already exists: %s", path)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("instance record is not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect instance record destination: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+record.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary instance record: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
		_ = temp.Close()
		return fmt.Errorf("encode instance record: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync instance record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close instance record: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install instance record: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open instance directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync instance directory: %w", err)
	}
	return nil
}
