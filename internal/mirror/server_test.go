package mirror

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestLocalServerBindsLoopbackAndServesOnlyKnownRoutes(t *testing.T) {
	server, err := StartLocalServer(t.Context(), ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close local server: %v", err)
		}
	})
	if !strings.HasPrefix(server.LocalURL(), "http://127.0.0.1:") {
		t.Fatalf("local URL = %q", server.LocalURL())
	}

	for _, test := range []struct {
		path        string
		status      int
		contentType string
	}{
		{"/", http.StatusOK, "text/html; charset=utf-8"},
		{"/assets/wrap-mirror.css", http.StatusOK, "text/css; charset=utf-8"},
		{"/assets/wrap-mirror.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/assets/wrap-mirror-state.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/assets/third_party/xterm/xterm.mjs", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/missing", http.StatusNotFound, "text/plain; charset=utf-8"},
		{"/assets/", http.StatusNotFound, "text/plain; charset=utf-8"},
	} {
		response, err := http.Get(server.LocalURL() + test.path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != test.status {
			t.Errorf("GET %s status = %d, body=%q", test.path, response.StatusCode, body)
		}
		if got := response.Header.Get("Content-Type"); got != test.contentType {
			t.Errorf("GET %s Content-Type = %q", test.path, got)
		}
		for header, want := range map[string]string{
			"Cache-Control":             "no-store",
			"Referrer-Policy":           "no-referrer",
			"X-Content-Type-Options":    "nosniff",
			"X-Frame-Options":           "DENY",
			"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		} {
			if got := response.Header.Get(header); got != want {
				t.Errorf("GET %s %s = %q, want %q", test.path, header, got, want)
			}
		}
		if response.Header.Get("Content-Security-Policy") == "" ||
			response.Header.Get("Permissions-Policy") == "" {
			t.Errorf("GET %s missing browser security headers", test.path)
		}
		csp := response.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") ||
			strings.Contains(csp, "script-src 'self' 'unsafe-inline'") ||
			!strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
			t.Errorf("GET %s incompatible or unsafe CSP = %q", test.path, csp)
		}
	}
}

func TestLocalServerRejectsWrongMethods(t *testing.T) {
	server, err := StartLocalServer(t.Context(), ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	request, err := http.NewRequest(http.MethodPost, server.LocalURL()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d", response.StatusCode)
	}
}

type staticClientHandler struct {
	sessions []Session
}

func (h staticClientHandler) InitialSessions() []Session { return h.sessions }
func (staticClientHandler) Connected(*Client)            {}
func (staticClientHandler) Open(context.Context, *Client, OpenRequest) error {
	return nil
}
func (staticClientHandler) Close(*Client) error                 { return nil }
func (staticClientHandler) Input(*Client, []byte) error         { return nil }
func (staticClientHandler) Resize(*Client, ResizeRequest) error { return nil }
func (staticClientHandler) Disconnected(*Client)                {}

func TestWebSocketRequiresExactOriginAndEncryptedHello(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	handler := staticClientHandler{sessions: []Session{{
		ID:         "$7",
		Generation: "0123456789abcdef0123456789abcdef",
		Name:       "vb/api",
		Kind:       "entry",
	}}}
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Secret:  secret,
		Handler: handler,
		Random:  rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	socketURL := "ws://" + host + "/ws"

	_, response, err := websocket.Dial(t.Context(), socketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin dial response=%v err=%v", response, err)
	}

	connection, _, err := websocket.Dial(t.Context(), socketURL, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + host}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	kind, serverNonce, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary || len(serverNonce) != 16 {
		t.Fatalf("server nonce kind/len/error = %v/%d/%v", kind, len(serverNonce), err)
	}
	clientNonce := make([]byte, 16)
	for i := range clientNonce {
		clientNonce[i] = byte(0xb0 + i)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, clientNonce); err != nil {
		t.Fatal(err)
	}
	c2s, s2c, err := DeriveKeys(secret[:], serverNonce, clientNonce)
	if err != nil {
		t.Fatal(err)
	}
	helloPayload, _ := json.Marshal(ClientHello{Version: ProtocolVersion})
	sealer, _ := NewSealer(c2s)
	hello, err := sealer.Seal(TagClientHello, helloPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, hello); err != nil {
		t.Fatal(err)
	}
	kind, encryptedList, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("encrypted list read = %v/%v", kind, err)
	}
	opener, _ := NewOpener(s2c)
	tag, payload, err := opener.Open(encryptedList)
	if err != nil || tag != TagMirrorList {
		t.Fatalf("list frame tag/error = %x/%v", tag, err)
	}
	var list SessionList
	if err := DecodeControl(tag, payload, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Name != "vb/api" {
		t.Fatalf("initial sessions = %+v", list.Sessions)
	}
}

func TestWebSocketAuthenticatedClientCapacityPrecedesInitialList(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	var connections []*websocket.Conn
	t.Cleanup(func() {
		for _, connection := range connections {
			_ = connection.CloseNow()
		}
	})
	for i := 0; i < MaxClients; i++ {
		connection, opener := dialAuthenticatedMirror(t, host, secret, byte(i))
		connections = append(connections, connection)
		kind, ciphertext, err := connection.Read(t.Context())
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("client %d initial list read = %v/%v", i, kind, err)
		}
		tag, _, err := opener.Open(ciphertext)
		if err != nil || tag != TagMirrorList {
			t.Fatalf("client %d initial list tag/error = %x/%v", i, tag, err)
		}
	}
	if got := len(server.handshakes); got != 0 {
		t.Fatalf("authenticated clients retained %d handshake permits", got)
	}

	ninth, _ := dialAuthenticatedMirror(t, host, secret, 0xf0)
	connections = append(connections, ninth)
	_, _, err = ninth.Read(t.Context())
	if status := websocket.CloseStatus(err); status != websocket.StatusTryAgainLater {
		t.Fatalf("ninth client close = %v (status %v)", err, status)
	}
}

func TestWebSocketRejectsHandshakeCapturedBeforeSecretRotation(t *testing.T) {
	var oldSecret, newSecret Secret
	for i := range oldSecret {
		oldSecret[i] = byte(i)
		newSecret[i] = byte(i + 32)
	}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: oldSecret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	connection, _, err := websocket.Dial(t.Context(), "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + host}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	kind, serverNonce, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary || len(serverNonce) != 16 {
		t.Fatalf("server nonce kind/len/error = %v/%d/%v", kind, len(serverNonce), err)
	}

	server.SetSecret(newSecret)
	clientNonce := make([]byte, 16)
	for i := range clientNonce {
		clientNonce[i] = byte(0xb0 + i)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, clientNonce); err != nil {
		t.Fatal(err)
	}
	c2s, _, err := DeriveKeys(oldSecret[:], serverNonce, clientNonce)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(c2s)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(ClientHello{Version: ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sealer.Seal(TagClientHello, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, hello); err != nil {
		t.Fatal(err)
	}
	_, _, err = connection.Read(t.Context())
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("old-secret handshake close = %v (status %v)", err, status)
	}
}

func TestWebSocketRejectsWrongSecretAndOversizedHandshake(t *testing.T) {
	var secret, wrongSecret Secret
	for i := range secret {
		secret[i] = byte(i)
		wrongSecret[i] = byte(i + 1)
	}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)

	wrong, _ := dialAuthenticatedMirror(t, host, wrongSecret, 0x20)
	t.Cleanup(func() { _ = wrong.CloseNow() })
	_, _, err = wrong.Read(t.Context())
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("wrong-secret close = %v (status %v)", err, status)
	}

	oversized, _, err := websocket.Dial(t.Context(), "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + host}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oversized.CloseNow() })
	if _, _, err := oversized.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := oversized.Write(
		t.Context(),
		websocket.MessageBinary,
		make([]byte, MaxWireMessage+1),
	); err != nil {
		t.Fatal(err)
	}
	_, _, err = oversized.Read(t.Context())
	if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
		t.Fatalf("oversized-handshake close = %v (status %v)", err, status)
	}
}

func dialAuthenticatedMirror(t *testing.T, host string, secret Secret, seed byte) (*websocket.Conn, *Opener) {
	t.Helper()
	connection, _, err := websocket.Dial(t.Context(), "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Origin": []string{"https://" + host}},
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
	c2s, s2c, err := DeriveKeys(secret[:], serverNonce, clientNonce)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(c2s)
	if err != nil {
		t.Fatal(err)
	}
	helloPayload, err := json.Marshal(ClientHello{Version: ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sealer.Seal(TagClientHello, helloPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, hello); err != nil {
		t.Fatal(err)
	}
	opener, err := NewOpener(s2c)
	if err != nil {
		t.Fatal(err)
	}
	return connection, opener
}
