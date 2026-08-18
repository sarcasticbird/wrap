package mirror

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type lifecycleViewer struct {
	output func([]byte) error
	writes chan []byte
	done   chan error
	once   sync.Once
}

func (viewer *lifecycleViewer) Write(data []byte) error {
	viewer.writes <- append([]byte(nil), data...)
	return nil
}

func (viewer *lifecycleViewer) Close() error {
	viewer.once.Do(func() { close(viewer.done) })
	return nil
}

func (viewer *lifecycleViewer) Done() <-chan error { return viewer.done }

type lifecyclePreparedViewer struct {
	viewer *lifecycleViewer
}

func (*lifecyclePreparedViewer) Geometry() ViewerGeometry {
	return ViewerGeometry{Columns: 132, Rows: 41}
}
func (prepared *lifecyclePreparedViewer) Start() (Viewer, error) { return prepared.viewer, nil }
func (*lifecyclePreparedViewer) Close() error                    { return nil }

type lifecycleViewerFactory func(context.Context, Identity, func([]byte) error) (PreparedViewer, error)

func (factory lifecycleViewerFactory) Prepare(
	ctx context.Context,
	identity Identity,
	output func([]byte) error,
) (PreparedViewer, error) {
	return factory(ctx, identity, output)
}

type lifecycleTunnel struct {
	url  string
	done chan error
}

func (tunnel *lifecycleTunnel) URL() string        { return tunnel.url }
func (tunnel *lifecycleTunnel) Done() <-chan error { return tunnel.done }
func (*lifecycleTunnel) Close() error              { return nil }

func TestManagerLocalServerEncryptedLifecycle(t *testing.T) {
	const publicHost = "quiet-river.trycloudflare.com"
	var localServer *LocalServer
	opened := make(chan *lifecycleViewer, 4)
	manager, err := NewManager(ManagerOptions{
		Workspace: "api",
		Target: &HostSession{
			ID: "$7", WindowID: "@3",
			Generation: "0123456789abcdef0123456789abcdef",
			Name:       "api", Kind: "terminal",
		},
		Viewers: lifecycleViewerFactory(func(
			_ context.Context,
			_ Identity,
			output func([]byte) error,
		) (PreparedViewer, error) {
			viewer := &lifecycleViewer{
				output: output,
				writes: make(chan []byte, 1),
				done:   make(chan error),
			}
			opened <- viewer
			return &lifecyclePreparedViewer{viewer: viewer}, nil
		}),
		StartServer: func(ctx context.Context, options ServerOptions) (ServerResource, error) {
			server, startErr := StartLocalServer(ctx, options)
			localServer = server
			return server, startErr
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return &lifecycleTunnel{
				url: "https://" + publicHost, done: make(chan error),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})

	secret := lifecyclePairingSecret(t, manager.Snapshot().PairingURL)
	connection, opener, sealer := dialLifecycleMirror(
		t, localServer.LocalURL(), publicHost, secret, 0x30,
	)
	viewer := receiveLifecycle(t, opened, "automatically opened viewer")
	assertLifecycleReady(t, connection, opener)

	writeLifecycleFrame(t, connection, sealer, TagInput, []byte("hello"))
	if got := string(receiveLifecycle(t, viewer.writes, "viewer input")); got != "hello" {
		t.Fatalf("viewer input = %q", got)
	}
	if err := viewer.output([]byte("world")); err != nil {
		t.Fatal(err)
	}
	tag, payload := readLifecycleFrame(t, connection, opener)
	if tag != TagOutput || string(payload) != "world" {
		t.Fatalf("viewer output = 0x%02x %q", tag, payload)
	}
	writeLifecycleFrame(t, connection, sealer, TagClose, nil)
	_ = receiveLifecycle(t, viewer.done, "viewer close")
	tag, payload = readLifecycleFrame(t, connection, opener)
	if tag != TagClose || len(payload) != 0 {
		t.Fatalf("close acknowledgement = 0x%02x %x", tag, payload)
	}
	waitForLifecycleClients(t, manager, 0)

	rotatedConnection, rotatedOpener, _ := dialLifecycleMirror(
		t, localServer.LocalURL(), publicHost, secret, 0x50,
	)
	rotatedViewer := receiveLifecycle(t, opened, "viewer before rotation")
	assertLifecycleReady(t, rotatedConnection, rotatedOpener)
	if err := manager.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = receiveLifecycle(t, rotatedViewer.done, "rotated viewer close")
	readCtx, cancelRead := context.WithTimeout(t.Context(), time.Second)
	_, _, readErr := rotatedConnection.Read(readCtx)
	cancelRead()
	if readErr == nil {
		t.Fatal("credential rotation left the old WebSocket connected")
	}

	staleConnection, staleOpener, _ := dialLifecycleMirror(
		t, localServer.LocalURL(), publicHost, secret, 0x60,
	)
	staleCtx, cancelStale := context.WithTimeout(t.Context(), time.Second)
	_, staleCiphertext, staleErr := staleConnection.Read(staleCtx)
	cancelStale()
	if staleErr == nil {
		if _, _, openErr := staleOpener.Open(staleCiphertext); openErr == nil {
			t.Fatal("rotated pairing credential authenticated")
		}
	}
	_ = staleConnection.CloseNow()

	newSecret := lifecyclePairingSecret(t, manager.Snapshot().PairingURL)
	if newSecret == secret {
		t.Fatal("pairing rotation retained the old secret")
	}
	newConnection, newOpener, newSealer := dialLifecycleMirror(
		t, localServer.LocalURL(), publicHost, newSecret, 0x70,
	)
	newViewer := receiveLifecycle(t, opened, "viewer after rotation")
	assertLifecycleReady(t, newConnection, newOpener)
	writeLifecycleFrame(t, newConnection, newSealer, TagClose, nil)
	_ = receiveLifecycle(t, newViewer.done, "new viewer close")
	tag, payload = readLifecycleFrame(t, newConnection, newOpener)
	if tag != TagClose || len(payload) != 0 {
		t.Fatalf("new close acknowledgement = 0x%02x %x", tag, payload)
	}
	waitForLifecycleClients(t, manager, 0)
}

func lifecyclePairingSecret(t *testing.T, pairingURL string) Secret {
	t.Helper()
	parsed, err := url.Parse(pairingURL)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ParseSecret(strings.TrimPrefix(parsed.Fragment, "k="))
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func dialLifecycleMirror(
	t *testing.T,
	localURL, publicHost string,
	secret Secret,
	seed byte,
) (*websocket.Conn, *Opener, *Sealer) {
	t.Helper()
	host := strings.TrimPrefix(localURL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + publicHost}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	kind, serverNonce, err := connection.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || len(serverNonce) != 16 {
		t.Fatalf("server nonce = kind:%v len:%d error:%v", kind, len(serverNonce), err)
	}
	clientNonce := make([]byte, 16)
	for index := range clientNonce {
		clientNonce[index] = seed + byte(index)
	}
	if err := connection.Write(ctx, websocket.MessageBinary, clientNonce); err != nil {
		t.Fatal(err)
	}
	c2s, s2c, err := DeriveKeys(secret[:], serverNonce, clientNonce)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(c2s)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewOpener(s2c)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := EncodeControl(TagClientHello, ClientHello{Version: ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleFrame(t, connection, sealer, TagClientHello, hello)
	return connection, opener, sealer
}

func writeLifecycleFrame(
	t *testing.T,
	connection *websocket.Conn,
	sealer *Sealer,
	tag byte,
	payload []byte,
) {
	t.Helper()
	ciphertext, err := sealer.Seal(tag, payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageBinary, ciphertext); err != nil {
		t.Fatal(err)
	}
}

func readLifecycleFrame(
	t *testing.T,
	connection *websocket.Conn,
	opener *Opener,
) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	kind, ciphertext, err := connection.Read(ctx)
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("encrypted frame = kind:%v error:%v", kind, err)
	}
	tag, payload, err := opener.Open(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	return tag, payload
}

func assertLifecycleReady(t *testing.T, connection *websocket.Conn, opener *Opener) {
	t.Helper()
	tag, payload := readLifecycleFrame(t, connection, opener)
	if tag != TagReady {
		t.Fatalf("ready tag = 0x%02x", tag)
	}
	var ready Ready
	if err := DecodeControl(tag, payload, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.ID != "$7" || ready.Generation == "" || ready.Columns != 132 || ready.Rows != 41 {
		t.Fatalf("ready = %+v", ready)
	}
}

func waitForLifecycleClients(t *testing.T, manager *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for manager.ClientCount() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := manager.ClientCount(); got != want {
		t.Fatalf("clients = %d, want %d", got, want)
	}
}

func receiveLifecycle[T any](t *testing.T, values <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}
