package mirror

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type ServerOptions struct {
	Secret     Secret
	PublicHost string
	Handler    ClientHandler
	Random     io.Reader
	Record     func(DiagnosticRecord)
}

const (
	remoteDiagnosticQueueSize = 8
	remoteDiagnosticInterval  = time.Minute
)

type ClientHandler interface {
	InitialSessions() []Session
	Connected(*Client)
	Open(context.Context, *Client, OpenRequest) error
	Close(*Client) error
	Input(*Client, []byte) error
	Resize(*Client, ResizeRequest) error
	Disconnected(*Client)
}

type LocalServer struct {
	listener             net.Listener
	server               *http.Server
	ctx                  context.Context
	cancel               context.CancelFunc
	secret               Secret
	handler              ClientHandler
	random               io.Reader
	assetFS              fs.FS
	record               func(DiagnosticRecord)
	stopOnce             sync.Once
	remoteDiagnostics    chan DiagnosticRecord
	remoteDiagnosticStop chan struct{}
	remoteDiagnosticDone chan struct{}
	remoteStopOnce       sync.Once
	remoteDiagnosticMu   sync.Mutex
	remoteDiagnosticLast map[string]time.Time
	remoteStopped        bool
	now                  func() time.Time
	websocketHandlers    sync.WaitGroup

	handshakes chan struct{}
	clients    chan struct{}

	mu            sync.RWMutex
	publicHost    string
	secretVersion uint64
	connections   map[*Client]uint64
}

func StartLocalServer(ctx context.Context, options ServerOptions) (*LocalServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for mirror server: %w", err)
	}
	server := &LocalServer{
		listener:             listener,
		secret:               options.Secret,
		handler:              options.Handler,
		random:               options.Random,
		assetFS:              assets,
		record:               options.Record,
		publicHost:           options.PublicHost,
		handshakes:           make(chan struct{}, MaxHandshakes),
		clients:              make(chan struct{}, MaxClients),
		secretVersion:        1,
		connections:          make(map[*Client]uint64),
		remoteDiagnostics:    make(chan DiagnosticRecord, remoteDiagnosticQueueSize),
		remoteDiagnosticStop: make(chan struct{}),
		remoteDiagnosticDone: make(chan struct{}),
		remoteDiagnosticLast: make(map[string]time.Time),
		now:                  time.Now,
	}
	if server.handler == nil {
		server.handler = noOpClientHandler{}
	}
	if server.random == nil {
		server.random = rand.Reader
	}
	server.ctx, server.cancel = context.WithCancel(ctx)
	server.server = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: HandshakeTimeout,
	}
	go server.runRemoteDiagnostics()
	go func() {
		<-server.ctx.Done()
		_ = server.Close(context.Background())
	}()
	go func() {
		err := server.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = listener.Close()
		}
	}()
	emitDiagnostic(server.record, DiagnosticRecord{Level: "info", Component: "server", Event: "started"})
	return server, nil
}

func (s *LocalServer) LocalURL() string {
	return "http://" + s.listener.Addr().String()
}

func (s *LocalServer) runRemoteDiagnostics() {
	defer close(s.remoteDiagnosticDone)
	for {
		select {
		case record := <-s.remoteDiagnostics:
			emitDiagnostic(s.record, record)
		case <-s.remoteDiagnosticStop:
			for {
				select {
				case record := <-s.remoteDiagnostics:
					emitDiagnostic(s.record, record)
				default:
					return
				}
			}
		}
	}
}

func (s *LocalServer) stopRemoteDiagnostics() {
	s.remoteStopOnce.Do(func() {
		s.remoteDiagnosticMu.Lock()
		s.remoteStopped = true
		close(s.remoteDiagnosticStop)
		s.remoteDiagnosticMu.Unlock()
	})
}

func (s *LocalServer) recordHandshakeRejection(code string) {
	s.recordRemoteDiagnostic("handshake:"+code, DiagnosticRecord{
		Level: "warn", Component: "handshake", Event: "rejected", Code: code,
	})
}

func (s *LocalServer) recordMissingAsset(name string) {
	if !requiredMirrorAsset(name) {
		return
	}
	s.recordRemoteDiagnostic("asset:"+name, DiagnosticRecord{
		Level: "error", Component: "server", Event: "asset_missing",
		Code: "client_asset_unavailable", Path: name,
	})
}

func (s *LocalServer) recordRemoteDiagnostic(key string, record DiagnosticRecord) {
	if s.record == nil {
		return
	}
	now := s.now()
	s.remoteDiagnosticMu.Lock()
	defer s.remoteDiagnosticMu.Unlock()
	if s.remoteStopped {
		return
	}
	last := s.remoteDiagnosticLast[key]
	if !last.IsZero() && now.Sub(last) < remoteDiagnosticInterval {
		return
	}

	select {
	case s.remoteDiagnostics <- record:
		s.remoteDiagnosticLast[key] = now
	default:
	}
}

func (s *LocalServer) SetPublicHost(host string) {
	s.mu.Lock()
	s.publicHost = host
	s.mu.Unlock()
}

func (s *LocalServer) SetSecret(secret Secret) {
	s.mu.Lock()
	s.secret = secret
	s.secretVersion++
	clients := make([]*Client, 0, len(s.connections))
	for client := range s.connections {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	for _, client := range clients {
		client.closeNow(errors.New("mirror pairing credential rotated"))
	}
}

func (s *LocalServer) Close(ctx context.Context) error {
	s.cancel()
	err := s.server.Shutdown(ctx)
	handlersDone := make(chan struct{})
	go func() {
		s.websocketHandlers.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-ctx.Done():
		return errors.Join(err, fmt.Errorf("wait for mirror WebSocket handlers: %w", ctx.Err()))
	}
	s.stopRemoteDiagnostics()
	select {
	case <-s.remoteDiagnosticDone:
		s.stopOnce.Do(func() {
			emitDiagnostic(s.record, DiagnosticRecord{Level: "info", Component: "server", Event: "stopped"})
		})
		return err
	case <-ctx.Done():
		return errors.Join(err, fmt.Errorf("drain mirror diagnostics: %w", ctx.Err()))
	}
}

func (s *LocalServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response.Header())
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case request.URL.Path == "/":
		s.serveAsset(response, "assets/index.html", "text/html; charset=utf-8")
	case request.URL.Path == "/ws":
		s.handleWebSocket(response, request)
	case strings.HasPrefix(request.URL.Path, "/assets/"):
		name := path.Clean(strings.TrimPrefix(request.URL.Path, "/"))
		if name == "assets" || strings.HasSuffix(request.URL.Path, "/") {
			http.NotFound(response, request)
			return
		}
		contentType := assetContentType(name)
		if contentType == "" {
			http.NotFound(response, request)
			return
		}
		s.serveAsset(response, name, contentType)
	default:
		http.NotFound(response, request)
	}
}

func (s *LocalServer) handleWebSocket(response http.ResponseWriter, request *http.Request) {
	s.websocketHandlers.Add(1)
	defer s.websocketHandlers.Done()
	select {
	case s.handshakes <- struct{}{}:
	default:
		s.recordHandshakeRejection("server_busy")
		http.Error(response, "mirror is busy", http.StatusServiceUnavailable)
		return
	}
	handshakeHeld := true
	defer func() {
		if handshakeHeld {
			<-s.handshakes
		}
	}()
	s.mu.RLock()
	publicHost := s.publicHost
	secret := s.secret
	secretVersion := s.secretVersion
	s.mu.RUnlock()
	if publicHost == "" {
		s.recordHandshakeRejection("server_starting")
		http.Error(response, "mirror is starting", http.StatusServiceUnavailable)
		return
	}
	origins := request.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != "https://"+publicHost {
		s.recordHandshakeRejection("origin_rejected")
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.recordHandshakeRejection("upgrade_failed")
		return
	}
	connection.SetReadLimit(MaxWireMessage)
	handshakeCtx, cancel := context.WithTimeout(s.ctx, HandshakeTimeout)
	client, err := authenticateClient(
		handshakeCtx,
		connection,
		secret,
		s.random,
		s.handler,
	)
	cancel()
	<-s.handshakes
	handshakeHeld = false
	if err != nil {
		s.recordHandshakeRejection("authentication_failed")
		if s.ctx.Err() != nil {
			_ = connection.CloseNow()
		} else {
			_ = connection.Close(websocket.StatusPolicyViolation, "authentication failed")
		}
		return
	}
	select {
	case s.clients <- struct{}{}:
	default:
		s.recordHandshakeRejection("client_capacity")
		_ = connection.Close(websocket.StatusTryAgainLater, "mirror client capacity reached")
		return
	}
	defer func() { <-s.clients }()
	s.mu.Lock()
	if secretVersion != s.secretVersion {
		s.mu.Unlock()
		s.recordHandshakeRejection("credential_expired")
		_ = connection.Close(websocket.StatusPolicyViolation, "pairing credential expired")
		return
	}
	s.connections[client] = secretVersion
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.connections, client)
		s.mu.Unlock()
	}()
	client.startWriter(s.ctx)
	if err := client.SendControl(
		s.ctx,
		TagMirrorList,
		SessionList{Sessions: s.handler.InitialSessions()},
	); err != nil {
		s.recordHandshakeRejection("session_list_failed")
		_ = connection.Close(websocket.StatusInternalError, "mirror unavailable")
		return
	}
	emitDiagnostic(s.record, DiagnosticRecord{Level: "info", Component: "handshake", Event: "authenticated"})
	s.handler.Connected(client)
	client.run(s.ctx)
}

type noOpClientHandler struct{}

func (noOpClientHandler) InitialSessions() []Session                       { return nil }
func (noOpClientHandler) Connected(*Client)                                {}
func (noOpClientHandler) Open(context.Context, *Client, OpenRequest) error { return nil }
func (noOpClientHandler) Close(*Client) error                              { return nil }
func (noOpClientHandler) Input(*Client, []byte) error                      { return nil }
func (noOpClientHandler) Resize(*Client, ResizeRequest) error              { return nil }
func (noOpClientHandler) Disconnected(*Client)                             {}

func (s *LocalServer) serveAsset(response http.ResponseWriter, name, contentType string) {
	assetFS := s.assetFS
	if assetFS == nil {
		assetFS = assets
	}
	data, err := fs.ReadFile(assetFS, name)
	if err != nil {
		s.recordMissingAsset(name)
		http.NotFound(response, nil)
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}

func requiredMirrorAsset(name string) bool {
	switch name {
	case "assets/index.html",
		"assets/wrap-mirror.css",
		"assets/wrap-mirror-bootstrap.js",
		"assets/wrap-mirror.js",
		"assets/wrap-mirror-state.js",
		"assets/third_party/xterm/xterm.mjs",
		"assets/third_party/xterm/xterm.css",
		"assets/third_party/xterm/addon-fit.mjs":
		return true
	default:
		return false
	}
}

func assetContentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	default:
		return ""
	}
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set(
		"Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; "+
			"img-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; "+
			"frame-ancestors 'none'; form-action 'none'",
	)
	header.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
