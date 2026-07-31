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

	"github.com/coder/websocket"
)

type ServerOptions struct {
	Secret     Secret
	PublicHost string
	Handler    ClientHandler
	Random     io.Reader
}

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
	listener net.Listener
	server   *http.Server
	ctx      context.Context
	cancel   context.CancelFunc
	secret   Secret
	handler  ClientHandler
	random   io.Reader

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
		listener:      listener,
		secret:        options.Secret,
		handler:       options.Handler,
		random:        options.Random,
		publicHost:    options.PublicHost,
		handshakes:    make(chan struct{}, MaxHandshakes),
		clients:       make(chan struct{}, MaxClients),
		secretVersion: 1,
		connections:   make(map[*Client]uint64),
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
	return server, nil
}

func (s *LocalServer) LocalURL() string {
	return "http://" + s.listener.Addr().String()
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
	return s.server.Shutdown(ctx)
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
	select {
	case s.handshakes <- struct{}{}:
	default:
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
		http.Error(response, "mirror is starting", http.StatusServiceUnavailable)
		return
	}
	origins := request.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != "https://"+publicHost {
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
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
		_ = connection.Close(websocket.StatusPolicyViolation, "authentication failed")
		return
	}
	select {
	case s.clients <- struct{}{}:
	default:
		_ = connection.Close(websocket.StatusTryAgainLater, "mirror client capacity reached")
		return
	}
	defer func() { <-s.clients }()
	s.mu.Lock()
	if secretVersion != s.secretVersion {
		s.mu.Unlock()
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
		_ = connection.Close(websocket.StatusInternalError, "mirror unavailable")
		return
	}
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
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		http.NotFound(response, nil)
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
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
