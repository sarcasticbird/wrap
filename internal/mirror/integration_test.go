package mirror

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type integrationViewer struct {
	output func([]byte) error
	writes chan []byte
	sizes  chan ResizeRequest
	done   chan error
	once   sync.Once
}

func (v *integrationViewer) Write(data []byte) error {
	v.writes <- append([]byte(nil), data...)
	return nil
}

func (v *integrationViewer) Resize(columns, rows uint16) error {
	v.sizes <- ResizeRequest{Columns: columns, Rows: rows}
	return nil
}

func (v *integrationViewer) Close() error {
	v.once.Do(func() {
		close(v.done)
	})
	return nil
}

func (v *integrationViewer) Done() <-chan error {
	return v.done
}

type integrationAcknowledger struct {
	identities chan Identity
}

func (a integrationAcknowledger) AcknowledgeSession(id, generation string) error {
	a.identities <- Identity{ID: id, Generation: generation}
	return nil
}

type integrationCipher struct {
	aead    cipher.AEAD
	counter uint64
}

func newIntegrationCipher(t *testing.T, key []byte) *integrationCipher {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &integrationCipher{aead: aead}
}

func (c *integrationCipher) seal(t *testing.T, tag byte, payload []byte) []byte {
	t.Helper()
	plaintext := append([]byte{tag}, payload...)
	ciphertext := c.aead.Seal(nil, integrationNonce(c.counter), plaintext, nil)
	c.counter++
	return ciphertext
}

func (c *integrationCipher) open(t *testing.T, ciphertext []byte) (byte, []byte) {
	t.Helper()
	plaintext, err := c.aead.Open(nil, integrationNonce(c.counter), ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.counter++
	if len(plaintext) == 0 {
		t.Fatal("encrypted frame has no tag")
	}
	return plaintext[0], plaintext[1:]
}

func integrationNonce(counter uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func TestManagerLocalServerEncryptedLifecycle(t *testing.T) {
	const publicHost = "quiet-river.trycloudflare.com"
	var localServer *LocalServer
	openedViewers := make(chan *integrationViewer, 2)
	acknowledged := make(chan Identity, 2)
	manager, err := NewManager(ManagerOptions{
		Workspace:    "vb",
		Acknowledger: integrationAcknowledger{identities: acknowledged},
		Viewers: viewerFactoryFunc(func(
			_ context.Context,
			_ Identity,
			_, _ uint16,
			output func([]byte) error,
		) (Viewer, error) {
			viewer := &integrationViewer{
				output: output,
				writes: make(chan []byte, 1),
				sizes:  make(chan ResizeRequest, 1),
				done:   make(chan error),
			}
			openedViewers <- viewer
			return viewer, nil
		}),
		StartServer: func(ctx context.Context, options ServerOptions) (ServerResource, error) {
			server, startErr := StartLocalServer(ctx, options)
			localServer = server
			return server, startErr
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return &fakeTunnelResource{
				url:  "https://" + publicHost,
				done: make(chan error),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	secondSession := HostSession{
		ID: "$8", Generation: session.Generation, Name: "vb/web",
	}
	if err := manager.Mirror(t.Context(), secondSession); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })

	secret := pairingSecret(t, manager.Snapshot().PairingURL)
	connection, sealer, opener := dialEncryptedManager(
		t, localServer.LocalURL(), publicHost, secret, 0x30,
	)
	assertSessionFrame(t, connection, opener, TagMirrorList, "vb/api", "vb/web")
	assertSessionFrame(t, connection, opener, TagStatus, "vb/api", "vb/web")

	writeEncryptedControl(t, connection, sealer, TagOpen, OpenRequest{
		ID: session.ID, Generation: session.Generation, Columns: 80, Rows: 24,
	})
	viewer := receiveWithin(t, openedViewers, "viewer open")
	if got := receiveWithin(t, acknowledged, "viewer acknowledgement"); got != (Identity{
		ID: session.ID, Generation: session.Generation,
	}) {
		t.Fatalf("acknowledged identity = %+v", got)
	}
	assertViewedEvent(t, manager.Events(), Identity{
		ID: session.ID, Generation: session.Generation,
	})
	writeEncryptedRaw(t, connection, sealer, TagInput, []byte("hello"))
	if got := string(receiveWithin(t, viewer.writes, "viewer input")); got != "hello" {
		t.Fatalf("viewer input = %q", got)
	}
	writeEncryptedControl(t, connection, sealer, TagResize, ResizeRequest{
		Columns: 120, Rows: 40,
	})
	if got := receiveWithin(t, viewer.sizes, "viewer resize"); got != (ResizeRequest{Columns: 120, Rows: 40}) {
		t.Fatalf("viewer resize = %+v", got)
	}
	if err := viewer.output([]byte("world")); err != nil {
		t.Fatal(err)
	}
	tag, payload := readEncryptedFrame(t, connection, opener)
	if tag != TagOutput || string(payload) != "world" {
		t.Fatalf("viewer output frame = 0x%02x %q", tag, payload)
	}
	writeEncryptedRaw(t, connection, sealer, TagClose, nil)
	_ = receiveWithin(t, viewer.done, "viewer close")
	tag, payload = readEncryptedFrame(t, connection, opener)
	if tag != TagClose || len(payload) != 0 {
		t.Fatalf("close acknowledgement = 0x%02x %x", tag, payload)
	}

	writeEncryptedControl(t, connection, sealer, TagOpen, OpenRequest{
		ID: secondSession.ID, Generation: secondSession.Generation, Columns: 90, Rows: 30,
	})
	secondViewer := receiveWithin(t, openedViewers, "second viewer open")
	if got := receiveWithin(t, acknowledged, "second viewer acknowledgement"); got != (Identity{
		ID: secondSession.ID, Generation: secondSession.Generation,
	}) {
		t.Fatalf("second acknowledged identity = %+v", got)
	}
	assertViewedEvent(t, manager.Events(), Identity{
		ID: secondSession.ID, Generation: secondSession.Generation,
	})
	if err := manager.Revoke(t.Context(), Identity{
		ID: secondSession.ID, Generation: secondSession.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	_ = receiveWithin(t, secondViewer.done, "revoked viewer close")
	assertRevokedFrame(t, connection, opener, Identity{
		ID: secondSession.ID, Generation: secondSession.Generation,
	})
	assertSessionFrame(t, connection, opener, TagStatus, "vb/api")

	rotateResult := make(chan error, 1)
	go func() {
		rotateResult <- manager.Rotate(t.Context())
	}()
	assertShutdownFrame(t, connection, opener, "pairing rotated")
	_ = connection.Close(websocket.StatusNormalClosure, "rotation received")
	if err := receiveWithin(t, rotateResult, "pairing rotation"); err != nil {
		t.Fatal(err)
	}

	rotatedSecret := pairingSecret(t, manager.Snapshot().PairingURL)
	if rotatedSecret == secret {
		t.Fatal("pairing rotation retained the original secret")
	}
	connection, _, opener = dialEncryptedManager(
		t, localServer.LocalURL(), publicHost, rotatedSecret, 0x60,
	)
	assertSessionFrame(t, connection, opener, TagMirrorList, "vb/api")
	assertSessionFrame(t, connection, opener, TagStatus, "vb/api")

	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- manager.Revoke(t.Context(), Identity{
			ID: session.ID, Generation: session.Generation,
		})
	}()
	assertShutdownFrame(t, connection, opener, "no mirrored terminals remain")
	_ = connection.Close(websocket.StatusNormalClosure, "shutdown received")
	if err := receiveWithin(t, revokeResult, "last-session revoke"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().State; got != StateStopped {
		t.Fatalf("state after last-session revoke = %v", got)
	}
}

func TestManagerUnexpectedTunnelExitRevokesBrowserCredential(t *testing.T) {
	const publicHost = "quiet-river.trycloudflare.com"
	var localServer *LocalServer
	tunnel := &fakeTunnelResource{
		url:  "https://" + publicHost,
		done: make(chan error, 1),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(ctx context.Context, options ServerOptions) (ServerResource, error) {
			server, startErr := StartLocalServer(ctx, options)
			localServer = server
			return server, startErr
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })

	secret := pairingSecret(t, manager.Snapshot().PairingURL)
	connection, _, opener := dialEncryptedManager(
		t, localServer.LocalURL(), publicHost, secret, 0x70,
	)
	assertSessionFrame(t, connection, opener, TagMirrorList, "vb/api")
	assertSessionFrame(t, connection, opener, TagStatus, "vb/api")

	tunnel.done <- errors.New("process exited")
	assertShutdownFrame(t, connection, opener, "Quick Tunnel exited")
	_ = connection.Close(websocket.StatusNormalClosure, "shutdown received")
}

func pairingSecret(t *testing.T, pairingURL string) Secret {
	t.Helper()
	parsed, err := url.Parse(pairingURL)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ParseSecret(parsed.Fragment[len("k="):])
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func dialEncryptedManager(
	t *testing.T,
	localURL, publicHost string,
	secret Secret,
	seed byte,
) (*websocket.Conn, *integrationCipher, *integrationCipher) {
	t.Helper()
	socketURL := "ws://" + strings.TrimPrefix(localURL, "http://") + "/ws"
	connection, _, err := websocket.Dial(t.Context(), socketURL, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + publicHost}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	kind, serverNonce, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary || len(serverNonce) != 16 {
		t.Fatalf("server nonce kind/len/error = %v/%d/%v", kind, len(serverNonce), err)
	}
	clientNonce := make([]byte, 16)
	for i := range clientNonce {
		clientNonce[i] = seed + byte(i)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, clientNonce); err != nil {
		t.Fatal(err)
	}
	salt := append(append([]byte(nil), serverNonce...), clientNonce...)
	c2s, err := hkdf.Key(sha256.New, secret[:], salt, "wrap-mirror/v1/c2s", 32)
	if err != nil {
		t.Fatal(err)
	}
	s2c, err := hkdf.Key(sha256.New, secret[:], salt, "wrap-mirror/v1/s2c", 32)
	if err != nil {
		t.Fatal(err)
	}
	sealer := newIntegrationCipher(t, c2s)
	opener := newIntegrationCipher(t, s2c)
	writeEncryptedControl(t, connection, sealer, TagClientHello, ClientHello{
		Version: ProtocolVersion,
	})
	return connection, sealer, opener
}

func writeEncryptedControl(
	t *testing.T,
	connection *websocket.Conn,
	sealer *integrationCipher,
	tag byte,
	value any,
) {
	t.Helper()
	payload, err := EncodeControl(tag, value)
	if err != nil {
		t.Fatal(err)
	}
	writeEncryptedRaw(t, connection, sealer, tag, payload)
}

func writeEncryptedRaw(
	t *testing.T,
	connection *websocket.Conn,
	sealer *integrationCipher,
	tag byte,
	payload []byte,
) {
	t.Helper()
	ciphertext := sealer.seal(t, tag, payload)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageBinary, ciphertext); err != nil {
		t.Fatal(err)
	}
}

func readEncryptedFrame(
	t *testing.T,
	connection *websocket.Conn,
	opener *integrationCipher,
) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	kind, ciphertext, err := connection.Read(ctx)
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("encrypted frame read = %v/%v", kind, err)
	}
	return opener.open(t, ciphertext)
}

func assertSessionFrame(
	t *testing.T,
	connection *websocket.Conn,
	opener *integrationCipher,
	wantTag byte,
	wantNames ...string,
) {
	t.Helper()
	tag, payload := readEncryptedFrame(t, connection, opener)
	if tag != wantTag {
		t.Fatalf("session frame tag = 0x%02x, want 0x%02x", tag, wantTag)
	}
	var list SessionList
	if err := DecodeControl(tag, payload, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != len(wantNames) {
		t.Fatalf("session frame = %+v", list.Sessions)
	}
	for i, wantName := range wantNames {
		if list.Sessions[i].Name != wantName {
			t.Fatalf("session %d name = %q, want %q", i, list.Sessions[i].Name, wantName)
		}
	}
}

func assertShutdownFrame(
	t *testing.T,
	connection *websocket.Conn,
	opener *integrationCipher,
	wantReason string,
) {
	t.Helper()
	tag, payload := readEncryptedFrame(t, connection, opener)
	if tag != TagShutdown {
		t.Fatalf("shutdown frame tag = 0x%02x, want 0x%02x", tag, TagShutdown)
	}
	var shutdown Shutdown
	if err := DecodeControl(tag, payload, &shutdown); err != nil {
		t.Fatal(err)
	}
	if shutdown.Reason != wantReason || shutdown.Retry {
		t.Fatalf("shutdown frame = %+v", shutdown)
	}
}

func assertRevokedFrame(
	t *testing.T,
	connection *websocket.Conn,
	opener *integrationCipher,
	want Identity,
) {
	t.Helper()
	tag, payload := readEncryptedFrame(t, connection, opener)
	if tag != TagRevoked {
		t.Fatalf("revoked frame tag = 0x%02x, want 0x%02x", tag, TagRevoked)
	}
	var revoked Revoked
	if err := DecodeControl(tag, payload, &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.ID != want.ID || revoked.Generation != want.Generation ||
		revoked.Reason != "mirror revoked" {
		t.Fatalf("revoked frame = %+v", revoked)
	}
}

func assertViewedEvent(t *testing.T, events <-chan Event, want Identity) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.Viewed == nil {
				continue
			}
			if event.Viewed.ID != want.ID || event.Viewed.Generation != want.Generation {
				t.Fatalf("viewed event = %+v", event.Viewed)
			}
			return
		case <-timeout:
			t.Fatalf("timed out waiting for viewed event %+v", want)
		}
	}
}

func receiveWithin[T any](t *testing.T, values <-chan T, description string) T {
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
