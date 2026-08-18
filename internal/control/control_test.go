package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/target"
)

const testInstanceID = "01KWRAPTEST"

type testHandler struct {
	status       Status
	rotateDelay  time.Duration
	rotateErr    error
	rotating     atomic.Int32
	maxRotating  atomic.Int32
	rotateCalls  atomic.Int32
	shutdownCall atomic.Int32
}

func (h *testHandler) Status() Status { return h.status }

func (h *testHandler) Rename(_ context.Context, name string) (Status, error) {
	h.status.Name = name
	return h.status, nil
}

func (h *testHandler) Rotate(ctx context.Context) (Status, error) {
	current := h.rotating.Add(1)
	defer h.rotating.Add(-1)
	for {
		maximum := h.maxRotating.Load()
		if current <= maximum || h.maxRotating.CompareAndSwap(maximum, current) {
			break
		}
	}
	h.rotateCalls.Add(1)
	select {
	case <-ctx.Done():
		return Status{}, ctx.Err()
	case <-time.After(h.rotateDelay):
	}
	if h.rotateErr != nil {
		return Status{}, h.rotateErr
	}
	return h.status, nil
}

func (h *testHandler) Shutdown(context.Context) error {
	h.shutdownCall.Add(1)
	return nil
}

func testStatus() Status {
	return Status{
		ID:         testInstanceID,
		Name:       "api",
		State:      "ready",
		PairingURL: "https://example.trycloudflare.com/#k=secret",
		QR:         "qr",
		Directory:  "/work/api",
		Clients:    2,
		StartedAt:  time.Unix(100, 0).UTC(),
		Target: target.Target{
			SocketPath: "/tmp/tmux.sock",
			Generation: "generation",
			SessionID:  "$1",
			WindowID:   "@2",
			Directory:  "/work/api",
		},
	}
}

func startTestServer(t *testing.T, handler Handler) (string, <-chan error) {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "runtime")
	socket := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, socket, testInstanceID, handler)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Serve() cleanup = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve() did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == 0o600 {
			return socket, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket was not created")
		}
		time.Sleep(time.Millisecond)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wrapctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestServeStatusAndPrivatePermissions(t *testing.T) {
	handler := &testHandler{status: testStatus()}
	socket, _ := startTestServer(t, handler)

	dirInfo, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime directory mode = %o, want 700", got)
	}
	socketInfo, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := socketInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("control socket mode = %o, want 600", got)
	}

	got, err := Call(context.Background(), socket, Request{InstanceID: testInstanceID, Action: ActionStatus})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != testInstanceID || got.PairingURL != handler.status.PairingURL || got.Target.WindowID != "@2" {
		t.Fatalf("Call(status) = %#v", got)
	}
}

func TestServeRenamesThroughSerializedControlOperation(t *testing.T) {
	handler := &testHandler{status: testStatus()}
	socket, _ := startTestServer(t, handler)
	status, err := Call(t.Context(), socket, Request{InstanceID: testInstanceID, Action: ActionRename, Name: "api two"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "api two" || handler.Status().Name != "api two" {
		t.Fatalf("rename status = %+v", status)
	}
}

func TestServeRejectsWrongInstanceAndUnknownAction(t *testing.T) {
	socket, _ := startTestServer(t, &testHandler{status: testStatus()})
	for _, test := range []struct {
		name string
		req  Request
		code string
	}{
		{name: "wrong instance", req: Request{InstanceID: "01KWRAPOTHER", Action: ActionStatus}, code: CodeNotFound},
		{name: "unknown action", req: Request{InstanceID: testInstanceID, Action: "delete"}, code: CodeInvalidRequest},
		{name: "invalid instance", req: Request{InstanceID: "../bad", Action: ActionStatus}, code: CodeInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Call(context.Background(), socket, test.req)
			var rpcErr *Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != test.code {
				t.Fatalf("Call() error = %v, want code %q", err, test.code)
			}
		})
	}
}

func TestServeReturnsSafeOperationErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "not ready", err: ErrNotReady, code: CodeNotReady},
		{name: "internal", err: errors.New("sensitive internal detail"), code: CodeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &testHandler{status: testStatus(), rotateErr: test.err}
			socket, _ := startTestServer(t, handler)
			_, err := Call(context.Background(), socket, Request{InstanceID: testInstanceID, Action: ActionRotate})
			var rpcErr *Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != test.code {
				t.Fatalf("Call(rotate) error = %v, want code %q", err, test.code)
			}
			if strings.Contains(rpcErr.Message, "sensitive") {
				t.Fatalf("internal error leaked detail: %q", rpcErr.Message)
			}
		})
	}
}

func TestServeSerializesRotateAndAllowsConcurrentStatus(t *testing.T) {
	handler := &testHandler{status: testStatus(), rotateDelay: 20 * time.Millisecond}
	socket, _ := startTestServer(t, handler)

	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := Call(context.Background(), socket, Request{InstanceID: testInstanceID, Action: ActionRotate}); err != nil {
				t.Errorf("Call(rotate) = %v", err)
			}
		}()
	}
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := Call(context.Background(), socket, Request{InstanceID: testInstanceID, Action: ActionStatus}); err != nil {
				t.Errorf("Call(status) = %v", err)
			}
		}()
	}
	group.Wait()
	if got := handler.maxRotating.Load(); got != 1 {
		t.Fatalf("maximum concurrent Rotate calls = %d, want 1", got)
	}
}

func TestServeShutdownRespondsThenStops(t *testing.T) {
	handler := &testHandler{status: testStatus()}
	socket, done := startTestServer(t, handler)
	if _, err := Call(context.Background(), socket, Request{InstanceID: testInstanceID, Action: ActionShutdown}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not stop after shutdown")
	}
	if got := handler.shutdownCall.Load(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after shutdown: %v", err)
	}
}

func TestServeRejectsOversizedRequest(t *testing.T) {
	socket, _ := startTestServer(t, &testHandler{status: testStatus()})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(append([]byte(`{"instance_id":"`+testInstanceID+`","action":"status","padding":"`), append([]byte(strings.Repeat("x", MaxRequestBytes)), []byte(`"}\n`)...)...)); err != nil {
		t.Fatal(err)
	}
	var response response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != CodeInvalidRequest {
		t.Fatalf("oversized response = %#v", response)
	}
}

func TestCallHonorsContextTimeout(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "hung.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer func() { _ = conn.Close() }()
			time.Sleep(time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = Call(ctx, socket, Request{InstanceID: testInstanceID, Action: ActionStatus})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call() error = %v, want deadline exceeded", err)
	}
}

func TestServeRefusesPreexistingSymlink(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target")
	if err := os.WriteFile(targetPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "control.sock")
	if err := os.Symlink(targetPath, socket); err != nil {
		t.Fatal(err)
	}
	err := Serve(context.Background(), socket, testInstanceID, &testHandler{status: testStatus()})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Serve() = %v, want preexisting-path error", err)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestServeDoesNotRemoveReplacementPath(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, testInstanceID, &testHandler{status: testStatus()}) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket was not created")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := Call(context.Background(), socket, Request{
		InstanceID: testInstanceID,
		Action:     ActionStatus,
	}); err != nil {
		t.Fatalf("wait for control server readiness: %v", err)
	}
	ownedPath := socket + ".owned"
	if err := os.Rename(socket, ownedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not stop")
	}
	data, err := os.ReadFile(socket)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement path changed: data=%q err=%v", data, err)
	}
}
