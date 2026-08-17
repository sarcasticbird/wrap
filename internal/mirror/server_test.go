package mirror

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

func TestLocalServerRecordsSafeLifecycleHandshakeAndMissingAsset(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	record := func(event DiagnosticRecord) { _ = sink.Write(event) }
	server, err := StartLocalServer(t.Context(), ServerOptions{
		PublicHost: "mirror.example",
		Record:     record,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.LocalURL()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://attacker.invalid")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-origin handshake status = %d", response.StatusCode)
	}

	server.assetFS = fstest.MapFS{}
	recorder := httptest.NewRecorder()
	server.serveAsset(
		recorder,
		"assets/wrap-mirror.js",
		"text/javascript; charset=utf-8",
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing known asset status = %d", recorder.Code)
	}
	viewportRecorder := httptest.NewRecorder()
	server.serveAsset(
		viewportRecorder,
		"assets/wrap-mirror-viewport.js",
		"text/javascript; charset=utf-8",
	)
	if viewportRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing viewport asset status = %d", viewportRecorder.Code)
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	records := sink.snapshot()
	foundViewportDiagnostic := false
	for _, want := range []struct{ component, event, code string }{
		{"server", "started", ""},
		{"handshake", "rejected", "origin_rejected"},
		{"server", "asset_missing", "client_asset_unavailable"},
		{"server", "stopped", ""},
	} {
		if !containsDiagnostic(records, want.component, want.event, want.code) {
			t.Fatalf("missing diagnostic %s/%s/%s in %+v", want.component, want.event, want.code, records)
		}
	}
	for _, record := range records {
		if record.Component == "server" && record.Event == "asset_missing" &&
			record.Path == "assets/wrap-mirror-viewport.js" {
			foundViewportDiagnostic = true
		}
		if strings.Contains(record.Path, "SENTINEL") || strings.Contains(record.Path, "credential") {
			t.Fatalf("missing asset diagnostic leaked request data: %+v", record)
		}
	}
	if !foundViewportDiagnostic {
		t.Fatalf("missing viewport asset diagnostic in %+v", records)
	}
}

func TestMissingAssetDiagnosticsRequireCanonicalAssetAndDoNotBlockResponses(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var missing atomic.Int32
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Record: func(record DiagnosticRecord) {
			if record.Component != "server" || record.Event != "asset_missing" {
				return
			}
			if missing.Add(1) == 1 {
				close(entered)
			}
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.assetFS = fstest.MapFS{}

	responsesDone := make(chan struct{})
	go func() {
		defer close(responsesDone)
		server.serveAsset(httptest.NewRecorder(), "assets/wrap-mirror.js?noise.js", "text/javascript")
		for range 25 {
			server.serveAsset(httptest.NewRecorder(), "assets/wrap-mirror.js", "text/javascript")
		}
	}()
	select {
	case <-responsesDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("missing asset responses blocked on diagnostic writes")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("canonical missing asset diagnostic was not queued")
	}
	if got := missing.Load(); got != 1 {
		t.Fatalf("missing asset diagnostic writes = %d, want one canonical rate-limited write", got)
	}
}

func TestHandshakeRejectionDiagnosticsAreRateLimitedAndNonBlocking(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var rejected atomic.Int32
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Record: func(record DiagnosticRecord) {
			if record.Component == "handshake" && record.Event == "rejected" {
				rejected.Add(1)
				<-release
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	for range MaxHandshakes {
		server.handshakes <- struct{}{}
	}

	client := &http.Client{Timeout: 250 * time.Millisecond}
	for range 25 {
		response, err := client.Get(server.LocalURL() + "/ws")
		if err != nil {
			t.Fatalf("overflow handshake blocked on diagnostics: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("overflow handshake status = %d", response.StatusCode)
		}
	}
	deadline := time.Now().Add(time.Second)
	for rejected.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejection diagnostic writes = %d, want one rate-limited write", got)
	}
}

func TestLocalServerCloseDrainsRemoteDiagnosticsBeforeStopped(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var events []string
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Record: func(record DiagnosticRecord) {
			if record.Component == "handshake" && record.Event == "rejected" {
				close(entered)
				<-release
			}
			mu.Lock()
			events = append(events, record.Component+"/"+record.Event)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.recordHandshakeRejection("server_busy")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("remote diagnostic worker did not start queued write")
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close(context.Background()) }()
	returnedEarly := false
	select {
	case <-closed:
		returnedEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if !returnedEarly {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if returnedEarly {
		t.Fatal("server Close returned before the queued diagnostic completed")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"server/started", "handshake/rejected", "server/stopped"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("diagnostic shutdown order = %v, want %v", events, want)
	}
}

type blockingRandomReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRandomReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	for i := range data {
		data[i] = byte(i)
	}
	return len(data), nil
}

func TestLocalServerCloseWaitsForAcceptedHandshakeDiagnostics(t *testing.T) {
	diagnostics := &recordingDiagnosticSink{}
	random := &blockingRandomReader{entered: make(chan struct{}), release: make(chan struct{})}
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Random: random,
		Record: func(record DiagnosticRecord) {
			_ = diagnostics.Write(record)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	connection, _, err := websocket.Dial(t.Context(), "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://" + host}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.CloseNow() }()
	select {
	case <-random.entered:
	case <-time.After(time.Second):
		t.Fatal("accepted handshake did not reach random source")
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close(context.Background()) }()
	returnedEarly := false
	select {
	case <-closed:
		returnedEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	close(random.release)
	if !returnedEarly {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if returnedEarly {
		t.Fatal("server Close returned while an accepted handshake could still log")
	}

	records := diagnostics.snapshot()
	rejected := -1
	stopped := -1
	for index, record := range records {
		if record.Component == "handshake" && record.Event == "rejected" {
			rejected = index
		}
		if record.Component == "server" && record.Event == "stopped" {
			stopped = index
		}
	}
	if rejected < 0 || stopped < 0 || rejected > stopped {
		t.Fatalf("accepted-handshake shutdown diagnostics = %+v", records)
	}
}

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
		{"/assets/wrap-mirror-bootstrap.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/assets/wrap-mirror.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/assets/wrap-mirror-viewport.js", http.StatusOK, "text/javascript; charset=utf-8"},
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

type automaticClientHandler struct {
	ready Ready
}

func (h automaticClientHandler) Connected(ctx context.Context, client *Client) error {
	client.viewerOpen.Store(true)
	return client.SendControl(ctx, TagReady, h.ready)
}
func (automaticClientHandler) Close(*Client) error         { return nil }
func (automaticClientHandler) Input(*Client, []byte) error { return nil }
func (automaticClientHandler) Disconnected(*Client)        {}

type failingConnectedHandler struct {
	connected    chan *Client
	disconnected chan struct{}
}

func (h *failingConnectedHandler) Connected(_ context.Context, client *Client) error {
	h.connected <- client
	return errors.New("viewer attach failed")
}
func (*failingConnectedHandler) Close(*Client) error         { return nil }
func (*failingConnectedHandler) Input(*Client, []byte) error { return nil }
func (h *failingConnectedHandler) Disconnected(*Client) {
	select {
	case h.disconnected <- struct{}{}:
	default:
	}
}

func TestWebSocketViewerOpenFailureDisconnectsClientAndWriter(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	handler := &failingConnectedHandler{
		connected: make(chan *Client, 1), disconnected: make(chan struct{}, 1),
	}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: secret, Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	connection, opener, _ := dialAuthenticatedMirror(t, host, secret, 0x54)
	t.Cleanup(func() { _ = connection.CloseNow() })
	client := <-handler.connected
	kind, encrypted, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("viewer-open failure frame = kind:%v error:%v", kind, err)
	}
	tag, payload, err := opener.Open(encrypted)
	if err != nil || tag != TagError {
		t.Fatalf("viewer-open failure tag = 0x%02x, error:%v", tag, err)
	}
	var problem ProtocolError
	if err := DecodeControl(tag, payload, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "terminal_unavailable" || problem.Retry {
		t.Fatalf("viewer-open failure payload = %+v", problem)
	}
	if _, _, err := connection.Read(t.Context()); err == nil {
		t.Fatal("viewer-open failure left WebSocket open after terminal error")
	}
	select {
	case <-handler.disconnected:
	case <-time.After(time.Second):
		t.Fatal("viewer-open failure did not disconnect handler")
	}
	select {
	case <-client.writerDone:
	case <-time.After(time.Second):
		t.Fatal("viewer-open failure leaked writer goroutine")
	}
}

func TestWebSocketAutomaticTargetOpensWithoutSessionList(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	ready := Ready{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Columns: 80, Rows: 24,
	}
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Secret:  secret,
		Handler: automaticClientHandler{ready: ready},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	connection, opener, _ := dialAuthenticatedMirror(t, host, secret, 0x55)
	t.Cleanup(func() { _ = connection.CloseNow() })
	kind, ciphertext, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("automatic frame kind/error = %v/%v", kind, err)
	}
	tag, payload, err := opener.Open(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if tag != TagReady {
		t.Fatalf("first automatic frame tag = 0x%02x, want ready", tag)
	}
	var opened Ready
	if err := DecodeControl(tag, payload, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.ID != ready.ID || opened.Generation != ready.Generation {
		t.Fatalf("opened = %+v", opened)
	}
}

func TestWebSocketRequiresExactOriginAndEncryptedHello(t *testing.T) {
	diagnostics := &recordingDiagnosticSink{}
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	ready := Ready{ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Columns: 80, Rows: 24}
	handler := automaticClientHandler{ready: ready}
	server, err := StartLocalServer(t.Context(), ServerOptions{
		Secret:  secret,
		Handler: handler,
		Random:  rand.Reader,
		Record: func(record DiagnosticRecord) {
			_ = diagnostics.Write(record)
		},
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
	kind, encryptedReady, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("encrypted ready read = %v/%v", kind, err)
	}
	opener, _ := NewOpener(s2c)
	tag, payload, err := opener.Open(encryptedReady)
	if err != nil || tag != TagReady {
		t.Fatalf("ready frame tag/error = %x/%v", tag, err)
	}
	var opened Ready
	if err := DecodeControl(tag, payload, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.ID != ready.ID {
		t.Fatalf("ready = %+v", opened)
	}
	if !containsDiagnostic(diagnostics.snapshot(), "handshake", "authenticated", "") {
		t.Fatalf("successful handshake diagnostic missing: %+v", diagnostics.snapshot())
	}
}

func TestWebSocketAuthenticatedClientCapacityPrecedesInitialList(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	ready := Ready{ID: "$7", Generation: "generation", Columns: 80, Rows: 24}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: secret, Handler: automaticClientHandler{ready: ready}})
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
		connection, opener, _ := dialAuthenticatedMirror(t, host, secret, byte(i))
		connections = append(connections, connection)
		kind, ciphertext, err := connection.Read(t.Context())
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("client %d ready read = %v/%v", i, kind, err)
		}
		tag, _, err := opener.Open(ciphertext)
		if err != nil || tag != TagReady {
			t.Fatalf("client %d ready tag/error = %x/%v", i, tag, err)
		}
	}
	if got := len(server.handshakes); got != 0 {
		t.Fatalf("authenticated clients retained %d handshake permits", got)
	}

	ninth, _, _ := dialAuthenticatedMirror(t, host, secret, 0xf0)
	connections = append(connections, ninth)
	_, _, err = ninth.Read(t.Context())
	if status := websocket.CloseStatus(err); status != websocket.StatusTryAgainLater {
		t.Fatalf("ninth client close = %v (status %v)", err, status)
	}
}

func TestWebSocketTerminalCloseReleasesClientSlot(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	ready := Ready{ID: "$7", Generation: "generation", Columns: 80, Rows: 24}
	server, err := StartLocalServer(t.Context(), ServerOptions{Secret: secret, Handler: automaticClientHandler{ready: ready}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	host := strings.TrimPrefix(server.LocalURL(), "http://")
	server.SetPublicHost(host)
	connection, opener, sealer := dialAuthenticatedMirror(t, host, secret, 0x71)
	t.Cleanup(func() { _ = connection.CloseNow() })
	kind, encrypted, err := connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("ready frame = kind:%v error:%v", kind, err)
	}
	if tag, _, err := opener.Open(encrypted); err != nil || tag != TagReady {
		t.Fatalf("ready tag = 0x%02x, error:%v", tag, err)
	}
	closeFrame, err := sealer.Seal(TagClose, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), websocket.MessageBinary, closeFrame); err != nil {
		t.Fatal(err)
	}
	kind, encrypted, err = connection.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary {
		t.Fatalf("close acknowledgement = kind:%v error:%v", kind, err)
	}
	if tag, payload, err := opener.Open(encrypted); err != nil || tag != TagClose || len(payload) != 0 {
		t.Fatalf("close acknowledgement tag/payload/error = 0x%02x/%d/%v", tag, len(payload), err)
	}
	readCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err = connection.Read(readCtx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("terminal close did not promptly release connection: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for len(server.clients) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(server.clients); got != 0 {
		t.Fatalf("closed terminal retained %d client slots", got)
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

	wrong, _, _ := dialAuthenticatedMirror(t, host, wrongSecret, 0x20)
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

func dialAuthenticatedMirror(t *testing.T, host string, secret Secret, seed byte) (*websocket.Conn, *Opener, *Sealer) {
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
	return connection, opener, sealer
}
