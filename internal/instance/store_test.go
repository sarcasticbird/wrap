package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/target"
)

func TestStorePrunesCompletedArtifactsWithBoundedRetention(t *testing.T) {
	store := testStore(t)
	diagnosticsDir := filepath.Join(store.StateRoot, "diagnostics")
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 5)
	for index := range ids {
		id := fmt.Sprintf("%016x", index+1)
		ids[index] = id
		lease, err := store.AcquireLease(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".jsonl", ".jsonl.1"} {
			path := filepath.Join(diagnosticsDir, id+suffix)
			if err := os.WriteFile(path, []byte("diagnostic\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stamp := time.Unix(int64(index+1), 0)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		leasePath := filepath.Join(store.RuntimeRoot, id+".lease")
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(leasePath, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(testRecord(ids[0], "active", "@1")); err != nil {
		t.Fatal(err)
	}
	held, err := store.AcquireLease(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := store.PruneCompletedArtifacts(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ids[0], ids[1], ids[4]} {
		if _, err := os.Stat(filepath.Join(diagnosticsDir, id+".jsonl")); err != nil {
			t.Fatalf("retained diagnostics %s: %v", id, err)
		}
	}
	for _, id := range []string{ids[2], ids[3]} {
		for _, path := range []string{
			filepath.Join(diagnosticsDir, id+".jsonl"),
			filepath.Join(diagnosticsDir, id+".jsonl.1"),
			filepath.Join(store.RuntimeRoot, id+".lease"),
		} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pruned artifact %s still exists: %v", path, err)
			}
		}
	}
}

func testStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	return Store{
		StateRoot:   filepath.Join(root, "state"),
		RuntimeRoot: filepath.Join(root, "runtime"),
	}
}

func TestStoreForRecordUsesValidatedControlSocketDirectory(t *testing.T) {
	store := testStore(t)
	record := testRecord("01KWRAPRUNTIME", "api", "@1")
	record.ControlSocket = filepath.Join(t.TempDir(), record.ID+".sock")
	recordStore, err := store.ForRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if recordStore.StateRoot != store.StateRoot || recordStore.RuntimeRoot != filepath.Dir(record.ControlSocket) {
		t.Fatalf("record store = %+v", recordStore)
	}
	record.ControlSocket = filepath.Join(string(filepath.Separator), record.ID+".sock")
	if _, err := store.ForRecord(record); err == nil {
		t.Fatal("ForRecord accepted filesystem root as a runtime directory")
	}
}

func testRecord(id, name, window string) Record {
	return Record{
		Version:       RecordVersion,
		ID:            id,
		Name:          name,
		PID:           42,
		ControlSocket: "/tmp/" + id + ".sock",
		StartedAt:     time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC),
		Directory:     "/work/" + name,
		Target: target.Target{
			SocketPath: "/tmp/tmux/default",
			Generation: "0123456789abcdef0123456789abcdef",
			SessionID:  "$7",
			WindowID:   window,
		},
	}
}

func TestRecordJSONContainsNoPairingMaterial(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(testRecord("0123456789abcdef", "api", "@1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pairing", "secret", "public_url", "pairing_url"} {
		if bytes.Contains(bytes.ToLower(data), []byte(forbidden)) {
			t.Fatalf("record contains %q: %s", forbidden, data)
		}
	}
}

func TestStoreCreateUsesPrivateAtomicFiles(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	record := testRecord("0123456789abcdef", "api", "@1")
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(store.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("instances dir mode = %o, want 700", got)
	}
	recordPath := filepath.Join(store.InstancesDir(), record.ID+".json")
	info, err := os.Stat(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("record mode = %o, want 600", got)
	}
	matches, err := filepath.Glob(filepath.Join(store.InstancesDir(), ".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary records remain: %v", matches)
	}
	got, problems, err := store.ReadAll()
	if err != nil || len(problems) != 0 || len(got) != 1 || got[0].ID != record.ID {
		t.Fatalf("ReadAll() = %+v, %+v, %v", got, problems, err)
	}
}

func TestStoreRejectsDuplicateNamesTargetsAndSymlinks(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	first := testRecord("0123456789abcdef", "api", "@1")
	if err := store.Create(first); err != nil {
		t.Fatal(err)
	}
	duplicateName := testRecord("fedcba9876543210", "api", "@2")
	if err := store.Create(duplicateName); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("duplicate name error = %v", err)
	}
	duplicateTarget := testRecord("0011223344556677", "other", "@1")
	if err := store.Create(duplicateTarget); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("duplicate target error = %v", err)
	}
	symlinkPath := filepath.Join(store.InstancesDir(), "8899aabbccddeeff.json")
	if err := os.Symlink(filepath.Join(store.InstancesDir(), first.ID+".json"), symlinkPath); err != nil {
		t.Fatal(err)
	}
	_, problems, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Err.Error(), "regular") {
		t.Fatalf("symlink problems = %+v", problems)
	}
}

func TestStoreRejectsNameIDCrossCollisions(t *testing.T) {
	t.Run("new name matches existing ID", func(t *testing.T) {
		store := testStore(t)
		first := testRecord("0123456789abcdef", "api", "@1")
		if err := store.Create(first); err != nil {
			t.Fatal(err)
		}
		collision := testRecord("fedcba9876543210", first.ID, "@2")
		if err := store.Create(collision); err == nil || !strings.Contains(err.Error(), "selector") {
			t.Fatalf("Create() name/ID collision = %v", err)
		}
	})

	t.Run("new ID matches existing name", func(t *testing.T) {
		store := testStore(t)
		first := testRecord("0123456789abcdef", "shared-selector", "@1")
		if err := store.Create(first); err != nil {
			t.Fatal(err)
		}
		collision := testRecord(first.Name, "web", "@2")
		if err := store.Create(collision); err == nil || !strings.Contains(err.Error(), "selector") {
			t.Fatalf("Create() ID/name collision = %v", err)
		}
	})
}

func TestStoreResolveRequiresUnambiguousIdentity(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	for _, record := range []Record{
		testRecord("0123456789abcdef", "api", "@1"),
		testRecord("0123fedcba987654", "web", "@2"),
	} {
		if err := store.Create(record); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.Resolve("api"); err != nil || got.ID != "0123456789abcdef" {
		t.Fatalf("Resolve(name) = %+v, %v", got, err)
	}
	if got, err := store.Resolve("0123f"); err != nil || got.Name != "web" {
		t.Fatalf("Resolve(prefix) = %+v, %v", got, err)
	}
	if _, err := store.Resolve("0123"); err == nil || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous Resolve error = %v", err)
	}
	if _, err := store.Resolve("missing"); err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Resolve error = %v", err)
	}
}

func TestStoreResolveRejectsLegacyNameIDCollision(t *testing.T) {
	store := testStore(t)
	if err := os.MkdirAll(store.InstancesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	byName := testRecord("0123456789abcdef", "shared-selector", "@1")
	byID := testRecord("shared-selector", "web", "@2")
	if err := store.writeAtomic(byName, false); err != nil {
		t.Fatal(err)
	}
	if err := store.writeAtomic(byID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve("shared-selector"); err == nil || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve() legacy name/ID collision = %v", err)
	}
}

func TestStoreReturnsMalformedRecordsSeparately(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := os.MkdirAll(store.InstancesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.InstancesDir(), "0123456789abcdef.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	records, problems, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(problems) != 1 || problems[0].Path != path {
		t.Fatalf("ReadAll() = %+v, %+v", records, problems)
	}
}

func TestStoreCreateNeverOverwritesExistingMalformedRecord(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := os.MkdirAll(store.InstancesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	record := testRecord("0123456789abcdef", "api", "@1")
	path := filepath.Join(store.InstancesDir(), record.ID+".json")
	original := []byte(`{"broken":true}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(record); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create() = %v, want existing-path refusal", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("existing malformed record changed: data=%q err=%v", data, err)
	}
}

func TestStoreRemoveChecksExactID(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	record := testRecord("0123456789abcdef", "api", "@1")
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.InstancesDir(), record.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("record remains after Remove: %v", err)
	}
	if err := store.Remove("../../escape"); err == nil {
		t.Fatal("Remove accepted unsafe ID")
	}
}

func TestStoreRemoveIfPIDDoesNotDeleteReplacementOwner(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	record := testRecord("0123456789abcdef", "api", "@1")
	record.PID = 42
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveIfPID(record.ID, 99)
	if err != nil || removed {
		t.Fatalf("RemoveIfPID(wrong owner) = %v, %v", removed, err)
	}
	if _, err := store.Resolve(record.ID); err != nil {
		t.Fatalf("replacement owner record was removed: %v", err)
	}
	removed, err = store.RemoveIfPID(record.ID, 42)
	if err != nil || !removed {
		t.Fatalf("RemoveIfPID(owner) = %v, %v", removed, err)
	}
}

func TestStoreRenameIfPIDIsAtomicAndUnique(t *testing.T) {
	store := testStore(t)
	first := testRecord("0123456789abcdef", "api", "@1")
	second := testRecord("fedcba9876543210", "web", "@2")
	if err := store.Create(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(second); err != nil {
		t.Fatal(err)
	}
	if renamed, err := store.RenameIfPID(first.ID, first.PID+1, "renamed"); err != nil || renamed {
		t.Fatalf("RenameIfPID wrong PID = %v, %v", renamed, err)
	}
	if renamed, err := store.RenameIfPID(first.ID, first.PID, second.Name); err == nil || renamed {
		t.Fatalf("RenameIfPID duplicate = %v, %v", renamed, err)
	}
	if renamed, err := store.RenameIfPID(first.ID, first.PID, second.ID); err == nil || renamed {
		t.Fatalf("RenameIfPID ID collision = %v, %v", renamed, err)
	}
	if renamed, err := store.RenameIfPID(first.ID, first.PID, "renamed"); err != nil || !renamed {
		t.Fatalf("RenameIfPID = %v, %v", renamed, err)
	}
	record, err := store.Resolve("renamed")
	if err != nil || record.ID != first.ID {
		t.Fatalf("renamed record = %+v, %v", record, err)
	}
}

func TestStoreLeaseProvesExactWorkerLivenessAcrossPIDReuse(t *testing.T) {
	store := testStore(t)
	leasePath := filepath.Join(store.RuntimeRoot, "01KWRAPLEASE.lease")
	first, err := store.AcquireLease("01KWRAPLEASE")
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease("01KWRAPLEASE"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second AcquireLease = %v, want held", err)
	}
	if held, err := store.LeaseHeld("01KWRAPLEASE"); err != nil || !held {
		t.Fatalf("LeaseHeld while owned = %v, %v", held, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	releasedInfo, err := os.Stat(leasePath)
	if err != nil {
		t.Fatalf("lease file after release = %v", err)
	}
	if !os.SameFile(firstInfo, releasedInfo) {
		t.Fatal("lease release replaced the lock inode")
	}
	if held, err := store.LeaseHeld("01KWRAPLEASE"); err != nil || held {
		t.Fatalf("LeaseHeld after release = %v, %v", held, err)
	}
	second, err := store.AcquireLease("01KWRAPLEASE")
	if err != nil {
		t.Fatalf("AcquireLease after release = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreLeaseRejectsSymlink(t *testing.T) {
	store := testStore(t)
	if err := os.MkdirAll(store.RuntimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store.RuntimeRoot, "01KWRAPLEASE.lease")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease("01KWRAPLEASE"); err == nil {
		t.Fatal("AcquireLease accepted symlink")
	}
}
