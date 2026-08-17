package mirror

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"
)

type State uint8

const (
	StateStopped State = iota
	StateStarting
	StateReady
	StateFailed
)

type HostSession struct {
	ID           string
	WindowID     string
	Generation   string
	Name         string
	Kind         string
	Bell         bool
	Activity     int64
	SeenActivity int64
}

type Snapshot struct {
	State              State
	PublicURL          string
	PairingURL         string
	QR                 string
	Err                string
	DiagnosticsWarning string
	CleanupWarning     string
}

type Event struct {
	Snapshot *Snapshot
}

type TunnelResource interface {
	URL() string
	Done() <-chan error
	Close() error
}

type ServerResource interface {
	LocalURL() string
	SetPublicHost(string)
	SetSecret(Secret)
	Close(context.Context) error
}

type ManagerOptions struct {
	Workspace   string
	Target      *HostSession
	Viewers     ViewerFactory
	StartServer func(context.Context, ServerOptions) (ServerResource, error)
	StartTunnel func(context.Context, string) (TunnelResource, error)
	Random      io.Reader
	Diagnostics DiagnosticSink
}

type Manager struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	workspace    string
	target       HostSession
	viewers      ViewerFactory
	startServer  func(context.Context, ServerOptions) (ServerResource, error)
	startTunnel  func(context.Context, string) (TunnelResource, error)
	random       io.Reader
	events       chan Event
	eventMu      sync.Mutex
	diagnosticMu sync.Mutex
	diagnostics  DiagnosticSink

	state              State
	publicURL          string
	pairingURL         string
	qr                 string
	errText            string
	secret             Secret
	server             ServerResource
	tunnel             TunnelResource
	runtimeCancel      context.CancelFunc
	startup            *managerStartup
	clients            map[*Client]struct{}
	active             map[*Client]activeViewer
	opening            map[*Client]*openingViewer
	shutdown           bool
	shutdownAsked      bool
	diagnosticsFailed  bool
	diagnosticsWarning string
	cleanupWarning     string
}

type activeViewer struct {
	identity Identity
	viewer   Viewer
	opening  *openingViewer
}

type openingViewer struct {
	identity Identity
	viewer   Viewer
}

type managerStartup struct {
	cancel   context.CancelFunc
	canceled bool
}

func NewManager(options ManagerOptions) (*Manager, error) {
	if options.Workspace == "" {
		return nil, errors.New("mirror workspace is required")
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Target == nil {
		return nil, errors.New("mirror target is required")
	}
	target := *options.Target
	if err := validateIdentity(target.ID, target.Generation); err != nil {
		return nil, err
	}
	manager := &Manager{
		workspace:   options.Workspace,
		target:      target,
		viewers:     options.Viewers,
		random:      options.Random,
		diagnostics: options.Diagnostics,
		events:      make(chan Event, 64),
		clients:     make(map[*Client]struct{}),
		active:      make(map[*Client]activeViewer),
		opening:     make(map[*Client]*openingViewer),
	}
	if manager.viewers == nil {
		manager.viewers = &PTYViewerFactory{
			Record: manager.recordDiagnostic,
		}
	}
	if options.StartServer != nil {
		manager.startServer = options.StartServer
	} else {
		manager.startServer = func(ctx context.Context, options ServerOptions) (ServerResource, error) {
			options.Record = manager.recordDiagnostic
			return StartLocalServer(ctx, options)
		}
	}
	if options.StartTunnel != nil {
		manager.startTunnel = options.StartTunnel
	} else {
		manager.startTunnel = func(ctx context.Context, localURL string) (TunnelResource, error) {
			return StartTunnel(ctx, localURL, TunnelOptions{Record: manager.recordDiagnostic})
		}
	}
	return manager, nil
}

func (m *Manager) Start(ctx context.Context) error {
	return m.start(ctx)
}

func (m *Manager) Events() <-chan Event {
	return m.events
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotLocked()
}

func (m *Manager) ClientCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.clients)
}

func (m *Manager) start(ctx context.Context) error {
	session := m.target
	if err := validateIdentity(session.ID, session.Generation); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	if m.shutdown || m.shutdownAsked {
		m.mu.Unlock()
		return errors.New("mirror manager is shut down")
	}
	if m.shutdownAsked {
		m.mu.Unlock()
		return errors.New("mirror manager is shutting down")
	}
	if m.state == StateReady {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		m.publishSnapshot(snapshot)
		return nil
	}
	m.state = StateStarting
	m.errText = ""
	runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(ctx))
	startup := &managerStartup{cancel: runtimeCancel}
	stopOperationCancel := context.AfterFunc(ctx, runtimeCancel)
	m.startup = startup
	starting := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(starting)

	startupComplete := false
	defer func() {
		stopOperationCancel()
		m.mu.Lock()
		if m.startup == startup {
			m.startup = nil
		}
		m.mu.Unlock()
		if !startupComplete {
			runtimeCancel()
		}
	}()

	secret, err := NewSecret(m.random)
	if err != nil {
		return m.failStart(err)
	}
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "server", Event: "starting"})
	server, err := m.startServer(runtimeCtx, ServerOptions{
		Secret:  secret,
		Handler: m,
		Random:  m.random,
	})
	if err != nil {
		m.recordDiagnostic(DiagnosticRecord{Level: "error", Component: "server", Event: "start_failed", Code: "server_unavailable"})
		return m.failStart(err)
	}
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "tunnel", Event: "starting"})
	tunnel, err := m.startTunnel(runtimeCtx, server.LocalURL())
	if err != nil {
		m.recordDiagnostic(DiagnosticRecord{Level: "error", Component: "tunnel", Event: "start_failed", Code: "tunnel_unavailable"})
		_ = closePartialServer(server)
		return m.failStart(err)
	}
	pairingURL, err := PairingURL(tunnel.URL(), secret)
	if err != nil {
		_ = tunnel.Close()
		_ = closePartialServer(server)
		return m.failStart(err)
	}
	qr, err := TerminalQR(pairingURL)
	if err != nil {
		_ = tunnel.Close()
		_ = closePartialServer(server)
		return m.failStart(err)
	}
	parsed, err := url.Parse(tunnel.URL())
	if err != nil {
		_ = tunnel.Close()
		_ = closePartialServer(server)
		return m.failStart(err)
	}
	server.SetPublicHost(parsed.Host)
	operationCanceled := !stopOperationCancel()

	m.mu.Lock()
	canceled := startup.canceled ||
		m.shutdownAsked ||
		operationCanceled ||
		ctx.Err() != nil ||
		runtimeCtx.Err() != nil
	if canceled {
		m.startup = nil
		m.mu.Unlock()
		_ = tunnel.Close()
		_ = closePartialServer(server)
		err := ctx.Err()
		if err == nil {
			err = context.Canceled
		}
		return m.failStart(err)
	}
	m.secret = secret
	m.server = server
	m.tunnel = tunnel
	m.runtimeCancel = runtimeCancel
	m.startup = nil
	m.publicURL = tunnel.URL()
	m.pairingURL = pairingURL
	m.qr = qr
	m.state = StateReady
	snapshot := m.snapshotLocked()
	startupComplete = true
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	go m.watchTunnel(tunnel)
	return nil
}

func (m *Manager) Rotate(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	if m.state != StateReady || m.server == nil || m.tunnel == nil {
		m.mu.RUnlock()
		return errors.New("mirror is not ready")
	}
	publicURL := m.publicURL
	m.mu.RUnlock()
	secret, err := NewSecret(m.random)
	if err != nil {
		return err
	}
	pairingURL, err := PairingURL(publicURL, secret)
	if err != nil {
		return err
	}
	qr, err := TerminalQR(pairingURL)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.state != StateReady || m.server == nil {
		m.mu.Unlock()
		return errors.New("mirror stopped during pairing rotation")
	}
	server := m.server
	// Cut off the old credential before potentially slow viewer and WebSocket
	// cleanup. SetSecret atomically advances the server secret version and
	// rejects both new old-key handshakes and already-authenticated clients. The
	// manager lock keeps their Disconnected callbacks from taking ownership of
	// viewer cleanup before rotation has detached the viewers and their errors.
	server.SetSecret(secret)
	m.secret = secret
	m.pairingURL = pairingURL
	m.qr = qr
	viewers := m.detachClientsLocked()
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "credential", Event: "rotated"})
	// Rotation is committed once SetSecret returns. Cleanup failures cannot make
	// the credential result indeterminate, but must remain visible to callers.
	cleanupErr := errors.Join(
		closeViewers(viewers),
		cleanupViewerFactory(m.viewers),
	)
	m.mu.Lock()
	m.cleanupWarning = ""
	if cleanupErr != nil {
		m.cleanupWarning = "pairing rotated, but terminal cleanup was incomplete"
	}
	snapshot = m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	if cleanupErr != nil {
		m.recordDiagnostic(DiagnosticRecord{
			Level: "warn", Component: "credential", Event: "rotated", Code: "cleanup_incomplete",
		})
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	var cancelStartup context.CancelFunc
	m.mu.Lock()
	m.shutdownAsked = true
	if m.startup != nil {
		m.startup.canceled = true
		cancelStartup = m.startup.cancel
	}
	m.mu.Unlock()
	if cancelStartup != nil {
		cancelStartup()
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return nil
	}
	m.shutdown = true
	cleanup := m.clearLocked(StateStopped, "")
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	err := cleanup.close(ctx, Shutdown{Reason: "wrap mirror stopped", Retry: false})
	m.publishSnapshot(snapshot)
	return err
}

func (m *Manager) Connected(ctx context.Context, client *Client) error {
	m.mu.Lock()
	if m.state != StateReady || m.shutdown {
		m.mu.Unlock()
		return errors.New("mirror target is unavailable")
	}
	m.clients[client] = struct{}{}
	m.mu.Unlock()
	if !client.viewerOpen.CompareAndSwap(false, true) {
		return errors.New("a terminal is already open")
	}
	if err := m.open(ctx, client); err != nil {
		client.viewerOpen.Store(false)
		return err
	}
	return nil
}

func (m *Manager) open(ctx context.Context, client *Client) error {
	session := m.target
	identity := Identity{ID: session.ID, Generation: session.Generation}
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "preparing"})
	var opening *openingViewer
	viewerIdentity := identity
	viewerIdentity.WindowID = session.WindowID
	prepared, err := m.viewers.Prepare(ctx, viewerIdentity, func(output []byte) error {
		return m.sendViewerOutput(client, opening, output)
	})
	if err != nil {
		m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "open_failed", Code: "terminal_unavailable"})
		return err
	}
	geometry := prepared.Geometry()
	opened := Ready{
		ID:         identity.ID,
		Generation: identity.Generation,
		Columns:    geometry.Columns,
		Rows:       geometry.Rows,
	}
	if err := ValidateServerFrame(TagReady, opened); err != nil {
		_ = prepared.Close()
		m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "open_failed", Code: "terminal_unavailable"})
		return err
	}
	m.mu.Lock()
	_, connected := m.clients[client]
	_, exists := m.active[client]
	_, alreadyOpening := m.opening[client]
	if !connected || exists || alreadyOpening {
		m.mu.Unlock()
		_ = prepared.Close()
		if !connected {
			return errors.New("mirror client disconnected")
		}
		if alreadyOpening {
			return errors.New("a terminal is already opening")
		}
		return errors.New("a terminal is already open")
	}
	if m.state != StateReady || m.shutdown {
		m.mu.Unlock()
		_ = prepared.Close()
		return errors.New("mirrored terminal ended")
	}
	opening = &openingViewer{identity: identity}
	m.opening[client] = opening
	if err := client.SendControl(ctx, TagReady, opened); err != nil {
		delete(m.opening, client)
		m.mu.Unlock()
		_ = prepared.Close()
		return err
	}
	m.mu.Unlock()
	viewer, err := prepared.Start()
	if err != nil {
		m.removeOpening(client, opening)
		_ = prepared.Close()
		m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "open_failed", Code: "terminal_unavailable"})
		return err
	}
	m.mu.Lock()
	if m.opening[client] != opening {
		m.mu.Unlock()
		_ = viewer.Close()
		return errors.New("viewer opening was canceled")
	}
	opening.viewer = viewer
	m.mu.Unlock()
	m.mu.Lock()
	reserved := m.opening[client] == opening
	if reserved {
		delete(m.opening, client)
	}
	_, connected = m.clients[client]
	_, exists = m.active[client]
	stillMirrored := m.state == StateReady && !m.shutdown
	if !reserved || !connected || exists || !stillMirrored {
		m.mu.Unlock()
		_ = viewer.Close()
		switch {
		case !connected:
			return errors.New("mirror client disconnected")
		case exists:
			return errors.New("a terminal is already open")
		case !stillMirrored:
			return errors.New("mirrored terminal ended")
		default:
			return errors.New("viewer opening was canceled")
		}
	}
	m.active[client] = activeViewer{identity: identity, viewer: viewer, opening: opening}
	m.mu.Unlock()
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "opened"})
	go m.watchViewer(client, identity, viewer)
	return nil
}

func (m *Manager) Close(client *Client) error {
	m.mu.Lock()
	active, ok := m.active[client]
	if ok {
		delete(m.active, client)
	}
	opening := m.opening[client]
	delete(m.opening, client)
	m.mu.Unlock()
	if ok {
		err := active.viewer.Close()
		m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "closed"})
		m.recordViewerCleanupFailure(err)
		return err
	}
	if opening != nil && opening.viewer != nil {
		err := opening.viewer.Close()
		m.recordViewerCleanupFailure(err)
		return err
	}
	return nil
}

func (m *Manager) Input(client *Client, data []byte) error {
	m.mu.RLock()
	active, ok := m.active[client]
	m.mu.RUnlock()
	if !ok {
		return errors.New("no terminal is open")
	}
	return active.viewer.Write(data)
}

func (m *Manager) sendViewerOutput(
	client *Client,
	opening *openingViewer,
	output []byte,
) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	active := m.active[client]
	if opening == nil ||
		(m.opening[client] != opening && active.opening != opening) {
		return nil
	}
	return client.SendOutput(context.Background(), output)
}

func (m *Manager) Disconnected(client *Client) {
	m.mu.Lock()
	delete(m.clients, client)
	active, ok := m.active[client]
	if ok {
		delete(m.active, client)
	}
	opening := m.opening[client]
	delete(m.opening, client)
	m.mu.Unlock()
	if ok {
		m.recordViewerCleanupFailure(active.viewer.Close())
		m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "closed"})
	}
	if opening != nil && opening.viewer != nil {
		m.recordViewerCleanupFailure(opening.viewer.Close())
	}
}

func (m *Manager) removeOpening(client *Client, opening *openingViewer) {
	m.mu.Lock()
	if m.opening[client] == opening {
		delete(m.opening, client)
	}
	m.mu.Unlock()
}

func (m *Manager) failStart(err error) error {
	m.mu.Lock()
	if errors.Is(err, context.Canceled) {
		m.state = StateStopped
		m.errText = ""
	} else {
		m.state = StateFailed
		m.errText = err.Error()
	}
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	return err
}

func closePartialServer(server ServerResource) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Close(ctx)
}

func (m *Manager) watchTunnel(tunnel TunnelResource) {
	err, ok := <-tunnel.Done()
	if !ok {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.tunnel != tunnel || m.shutdown {
		m.mu.Unlock()
		return
	}
	cleanup := m.clearLocked(StateFailed, fmt.Sprintf("Quick Tunnel exited: %v", err))
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.recordDiagnostic(DiagnosticRecord{Level: "error", Component: "tunnel", Event: "exit", Code: "unexpected_exit"})
	_ = cleanup.close(context.Background(), Shutdown{
		Reason: "Quick Tunnel exited",
		Retry:  false,
	})
	m.publishSnapshot(snapshot)
}

func (m *Manager) watchViewer(client *Client, identity Identity, viewer Viewer) {
	viewerErr := <-viewer.Done()
	m.mu.Lock()
	active, current := m.active[client]
	if !current || active.identity != identity || active.viewer != viewer {
		m.mu.Unlock()
		return
	}
	delete(m.active, client)
	client.markViewerClosed()
	m.mu.Unlock()
	m.recordViewerCleanupFailure(viewerErr)
	m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "ended", Code: "terminal_ended"})
	_ = client.SendControl(context.Background(), TagError, ProtocolError{
		Code:    "terminal_ended",
		Message: "The terminal viewer ended.",
		Retry:   true,
	})
}

func (m *Manager) recordViewerCleanupFailure(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	m.cleanupWarning = "terminal cleanup was incomplete"
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	m.recordDiagnostic(DiagnosticRecord{
		Level: "warn", Component: "viewer", Event: "closed", Code: "cleanup_incomplete",
	})
}

type managerCleanup struct {
	server        ServerResource
	tunnel        TunnelResource
	cancel        context.CancelFunc
	clients       []*Client
	viewers       []Viewer
	viewerFactory ViewerFactory
}

func (m *Manager) clearLocked(state State, errText string) managerCleanup {
	cleanup := managerCleanup{
		server:        m.server,
		tunnel:        m.tunnel,
		cancel:        m.runtimeCancel,
		clients:       m.clientsLocked(),
		viewers:       m.detachClientsLocked(),
		viewerFactory: m.viewers,
	}
	m.server = nil
	m.tunnel = nil
	m.runtimeCancel = nil
	m.secret = Secret{}
	m.publicURL = ""
	m.pairingURL = ""
	m.qr = ""
	m.cleanupWarning = ""
	clear(m.clients)
	m.state = state
	m.errText = errText
	return cleanup
}

func (m *Manager) clientsLocked() []*Client {
	clients := make([]*Client, 0, len(m.clients))
	for client := range m.clients {
		clients = append(clients, client)
	}
	return clients
}

func (m *Manager) detachClientsLocked() []Viewer {
	viewers := make([]Viewer, 0, len(m.active)+len(m.opening))
	for client, active := range m.active {
		viewers = append(viewers, active.viewer)
		delete(m.active, client)
	}
	for client, opening := range m.opening {
		if opening.viewer != nil {
			viewers = append(viewers, opening.viewer)
		}
		delete(m.opening, client)
		client.markViewerClosed()
	}
	clear(m.clients)
	return viewers
}

func (m *Manager) snapshotLocked() Snapshot {
	return Snapshot{
		State:              m.state,
		PublicURL:          m.publicURL,
		PairingURL:         m.pairingURL,
		QR:                 m.qr,
		Err:                m.errText,
		DiagnosticsWarning: m.diagnosticsWarning,
		CleanupWarning:     m.cleanupWarning,
	}
}

func (m *Manager) recordDiagnostic(record DiagnosticRecord) {
	m.diagnosticMu.Lock()
	defer m.diagnosticMu.Unlock()
	m.mu.RLock()
	sink := m.diagnostics
	failed := m.diagnosticsFailed
	m.mu.RUnlock()
	if sink == nil || failed {
		return
	}
	if err := sink.Write(record); err == nil {
		return
	}
	m.mu.Lock()
	if m.diagnosticsFailed {
		m.mu.Unlock()
		return
	}
	m.diagnosticsFailed = true
	m.diagnosticsWarning = "diagnostics unavailable"
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
}

func (m *Manager) publishSnapshot(snapshot Snapshot) {
	event := Event{Snapshot: &snapshot}
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	select {
	case m.events <- event:
		return
	default:
	}
	for {
		select {
		case <-m.events:
		default:
			m.events <- event
			return
		}
	}
}

func (c managerCleanup) close(ctx context.Context, shutdown Shutdown) error {
	ctx, cancel := boundedCleanupContext(ctx)
	defer cancel()
	viewerErr := closeViewers(c.viewers)
	viewerFactoryErr := cleanupViewerFactory(c.viewerFactory)
	clientErr := closeClients(ctx, c.clients, shutdown)
	var serverErr, tunnelErr error
	if c.tunnel != nil {
		tunnelErr = c.tunnel.Close()
	}
	if c.server != nil {
		serverErr = c.server.Close(ctx)
	}
	if c.cancel != nil {
		c.cancel()
	}
	return errors.Join(viewerErr, viewerFactoryErr, clientErr, serverErr, tunnelErr)
}

func closeViewers(viewers []Viewer) error {
	var errs []error
	for _, viewer := range viewers {
		errs = append(errs, viewer.Close())
	}
	return errors.Join(errs...)
}

func cleanupViewerFactory(factory ViewerFactory) error {
	cleaner, ok := factory.(interface{ Cleanup() error })
	if !ok {
		return nil
	}
	return cleaner.Cleanup()
}

func closeClients(ctx context.Context, clients []*Client, shutdown Shutdown) error {
	var errs []error
	for _, client := range clients {
		clientCtx, cancel := context.WithTimeout(ctx, time.Second)
		errs = append(errs, client.closeWithControl(clientCtx, TagShutdown, shutdown))
		cancel()
	}
	return errors.Join(errs...)
}

func boundedCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, 5*time.Second)
}
