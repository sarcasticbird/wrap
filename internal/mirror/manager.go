package mirror

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
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
	Sessions           []Session
	Err                string
	DiagnosticsWarning string
}

type ViewedEvent struct {
	ID         string
	Generation string
	Activity   int64
}

type Event struct {
	Snapshot *Snapshot
	Viewed   *ViewedEvent
}

type Acknowledger interface {
	AcknowledgeSession(id, generation string) error
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
	Workspace     string
	SessionSocket string
	Acknowledger  Acknowledger
	Viewers       ViewerFactory
	StartServer   func(context.Context, ServerOptions) (ServerResource, error)
	StartTunnel   func(context.Context, string) (TunnelResource, error)
	Random        io.Reader
	Diagnostics   DiagnosticSink
}

type Manager struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	workspace    string
	acknowledger Acknowledger
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
	generation         string
	sessions           map[Identity]HostSession
	server             ServerResource
	tunnel             TunnelResource
	runtimeCancel      context.CancelFunc
	startup            *managerStartup
	clients            map[*Client]struct{}
	active             map[*Client]activeViewer
	shutdown           bool
	shutdownAsked      bool
	diagnosticsFailed  bool
	diagnosticsWarning string
}

type activeViewer struct {
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
	manager := &Manager{
		workspace:    options.Workspace,
		acknowledger: options.Acknowledger,
		viewers:      options.Viewers,
		random:       options.Random,
		diagnostics:  options.Diagnostics,
		events:       make(chan Event, 64),
		sessions:     make(map[Identity]HostSession),
		clients:      make(map[*Client]struct{}),
		active:       make(map[*Client]activeViewer),
	}
	if manager.viewers == nil {
		manager.viewers = &PTYViewerFactory{
			SessionSocket: options.SessionSocket,
			Record:        manager.recordDiagnostic,
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

func (m *Manager) Events() <-chan Event {
	return m.events
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotLocked()
}

func (m *Manager) Mirror(ctx context.Context, session HostSession) error {
	identity := Identity{ID: session.ID, Generation: session.Generation}
	if err := validateIdentity(identity.ID, identity.Generation); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	if m.shutdown || m.shutdownAsked {
		m.mu.Unlock()
		return errors.New("mirror manager is shut down")
	}
	if m.generation != "" && m.generation != session.Generation {
		cleanup := m.clearLocked(StateStopped, "")
		m.generation = session.Generation
		changed := m.snapshotLocked()
		m.mu.Unlock()
		_ = cleanup.close(ctx, Shutdown{Reason: "session server restarted", Retry: false})
		m.publishSnapshot(changed)
		m.mu.Lock()
	}
	if m.shutdownAsked {
		m.mu.Unlock()
		return errors.New("mirror manager is shutting down")
	}
	if m.generation == "" {
		m.generation = session.Generation
	}
	if m.state == StateReady {
		m.sessions[identity] = session
		snapshot := m.snapshotLocked()
		clients := m.clientsLocked()
		m.mu.Unlock()
		m.publishSnapshot(snapshot)
		broadcastStatus(ctx, clients, snapshot.Sessions)
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
	m.sessions[identity] = session
	m.state = StateReady
	snapshot := m.snapshotLocked()
	clients := m.clientsLocked()
	startupComplete = true
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	broadcastStatus(ctx, clients, snapshot.Sessions)
	go m.watchTunnel(tunnel)
	return nil
}

func (m *Manager) Revoke(ctx context.Context, identity Identity) error {
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "credential", Event: "revoked"})
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	delete(m.sessions, identity)
	var viewers []Viewer
	for client, active := range m.active {
		if active.identity == identity {
			viewers = append(viewers, active.viewer)
			delete(m.active, client)
			client.markViewerClosed()
		}
	}
	clients := m.clientsLocked()
	if len(m.sessions) != 0 {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		err := closeViewers(viewers)
		broadcastRevoked(ctx, clients, identity, "mirror revoked")
		broadcastStatus(ctx, clients, snapshot.Sessions)
		m.publishSnapshot(snapshot)
		return err
	}
	cleanup := m.clearLocked(StateStopped, "")
	cleanup.viewers = append(cleanup.viewers, viewers...)
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	err := cleanup.close(ctx, Shutdown{Reason: "no mirrored terminals remain", Retry: false})
	m.publishSnapshot(snapshot)
	return err
}

func (m *Manager) Reconcile(ctx context.Context, sessions []HostSession) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	live := make(map[Identity]HostSession, len(sessions))
	var generation string
	for _, session := range sessions {
		if err := validateIdentity(session.ID, session.Generation); err != nil {
			return err
		}
		if generation == "" {
			generation = session.Generation
		} else if generation != session.Generation {
			return errors.New("session poll contains multiple server generations")
		}
		live[Identity{ID: session.ID, Generation: session.Generation}] = session
	}
	m.mu.Lock()
	if generation != "" && m.generation != "" && generation != m.generation {
		cleanup := m.clearLocked(StateStopped, "")
		m.generation = generation
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		err := cleanup.close(ctx, Shutdown{Reason: "session server restarted", Retry: false})
		m.publishSnapshot(snapshot)
		return err
	}
	if generation != "" {
		m.generation = generation
	}
	var viewers []Viewer
	for identity := range m.sessions {
		session, ok := live[identity]
		if !ok {
			delete(m.sessions, identity)
			for client, active := range m.active {
				if active.identity == identity {
					viewers = append(viewers, active.viewer)
					delete(m.active, client)
					client.markViewerClosed()
				}
			}
			continue
		}
		m.sessions[identity] = session
	}
	if m.state == StateReady && len(m.sessions) == 0 {
		cleanup := m.clearLocked(StateStopped, "")
		cleanup.viewers = append(cleanup.viewers, viewers...)
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		err := cleanup.close(ctx, Shutdown{Reason: "mirrored terminals ended", Retry: false})
		m.publishSnapshot(snapshot)
		return err
	}
	snapshot := m.snapshotLocked()
	clients := m.clientsLocked()
	m.mu.Unlock()
	err := closeViewers(viewers)
	broadcastStatus(ctx, clients, snapshot.Sessions)
	m.publishSnapshot(snapshot)
	return err
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
	clients := m.clientsLocked()
	viewers := m.detachClientsLocked()
	m.mu.Unlock()
	err = errors.Join(
		closeViewers(viewers),
		closeClients(ctx, clients, Shutdown{Reason: "pairing rotated", Retry: false}),
	)
	server.SetSecret(secret)
	m.mu.Lock()
	if m.state != StateReady || m.server != server {
		m.mu.Unlock()
		return errors.Join(err, errors.New("mirror stopped during pairing rotation"))
	}
	m.secret = secret
	m.pairingURL = pairingURL
	m.qr = qr
	snapshot := m.snapshotLocked()
	m.mu.Unlock()
	m.publishSnapshot(snapshot)
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "credential", Event: "rotated"})
	return err
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

func (m *Manager) InitialSessions() []Session {
	return m.Snapshot().Sessions
}

func (m *Manager) Connected(client *Client) {
	m.mu.Lock()
	if (m.state == StateStarting || m.state == StateReady) && !m.shutdown {
		m.clients[client] = struct{}{}
	}
	sessions := m.snapshotLocked().Sessions
	m.mu.Unlock()
	_ = client.SendControl(context.Background(), TagStatus, SessionList{Sessions: sessions})
}

func (m *Manager) Open(ctx context.Context, client *Client, request OpenRequest) error {
	identity := Identity{ID: request.ID, Generation: request.Generation}
	m.mu.RLock()
	session, ok := m.sessions[identity]
	m.mu.RUnlock()
	if !ok {
		return errors.New("mirrored terminal ended")
	}
	m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "preparing"})
	viewer, err := m.viewers.Open(ctx, identity, request.Columns, request.Rows, func(output []byte) error {
		return client.SendOutput(context.Background(), output)
	})
	if err != nil {
		m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "open_failed", Code: "terminal_unavailable"})
		return err
	}
	if m.acknowledger != nil {
		if err := m.acknowledger.AcknowledgeSession(identity.ID, identity.Generation); err != nil {
			_ = viewer.Close()
			return err
		}
	}
	m.mu.Lock()
	_, connected := m.clients[client]
	_, exists := m.active[client]
	if !connected || exists {
		m.mu.Unlock()
		_ = viewer.Close()
		if !connected {
			return errors.New("mirror client disconnected")
		}
		return errors.New("a terminal is already open")
	}
	if _, stillMirrored := m.sessions[identity]; !stillMirrored {
		m.mu.Unlock()
		_ = viewer.Close()
		return errors.New("mirrored terminal ended")
	}
	m.active[client] = activeViewer{identity: identity, viewer: viewer}
	m.mu.Unlock()
	event := Event{Viewed: &ViewedEvent{
		ID: identity.ID, Generation: identity.Generation, Activity: session.Activity,
	}}
	if m.publishViewed(event) {
		m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "opened"})
		go m.watchViewer(client, identity, viewer)
		return nil
	}
	_ = viewer.Close()
	m.mu.Lock()
	delete(m.active, client)
	m.mu.Unlock()
	return errors.New("mirror host event queue is full")
}

func (m *Manager) Close(client *Client) error {
	m.mu.Lock()
	active, ok := m.active[client]
	if ok {
		delete(m.active, client)
	}
	m.mu.Unlock()
	if ok {
		err := active.viewer.Close()
		m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "closed"})
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

func (m *Manager) Resize(client *Client, request ResizeRequest) error {
	m.mu.RLock()
	active, ok := m.active[client]
	m.mu.RUnlock()
	if !ok {
		return errors.New("no terminal is open")
	}
	return active.viewer.Resize(request.Columns, request.Rows)
}

func (m *Manager) Disconnected(client *Client) {
	m.mu.Lock()
	delete(m.clients, client)
	active, ok := m.active[client]
	if ok {
		delete(m.active, client)
	}
	m.mu.Unlock()
	if ok {
		_ = active.viewer.Close()
		m.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "viewer", Event: "closed"})
	}
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
	<-viewer.Done()
	m.mu.Lock()
	active, current := m.active[client]
	if !current || active.identity != identity || active.viewer != viewer {
		m.mu.Unlock()
		return
	}
	delete(m.active, client)
	client.markViewerClosed()
	m.mu.Unlock()
	m.recordDiagnostic(DiagnosticRecord{Level: "warn", Component: "viewer", Event: "ended", Code: "terminal_ended"})
	_ = client.SendControl(context.Background(), TagError, ProtocolError{
		Code:    "terminal_ended",
		Message: "The terminal viewer ended.",
		Retry:   true,
	})
}

type managerCleanup struct {
	server  ServerResource
	tunnel  TunnelResource
	cancel  context.CancelFunc
	clients []*Client
	viewers []Viewer
}

func (m *Manager) clearLocked(state State, errText string) managerCleanup {
	cleanup := managerCleanup{
		server:  m.server,
		tunnel:  m.tunnel,
		cancel:  m.runtimeCancel,
		clients: m.clientsLocked(),
		viewers: m.detachClientsLocked(),
	}
	m.server = nil
	m.tunnel = nil
	m.runtimeCancel = nil
	m.secret = Secret{}
	m.publicURL = ""
	m.pairingURL = ""
	m.qr = ""
	clear(m.sessions)
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
	viewers := make([]Viewer, 0, len(m.active))
	for client, active := range m.active {
		viewers = append(viewers, active.viewer)
		delete(m.active, client)
	}
	clear(m.clients)
	return viewers
}

func (m *Manager) snapshotLocked() Snapshot {
	sessions := make([]Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, Session{
			ID:         session.ID,
			Generation: session.Generation,
			Name:       session.Name,
			Kind:       session.Kind,
			Bell:       session.Bell,
			Activity:   session.Activity > session.SeenActivity,
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Name != sessions[j].Name {
			return sessions[i].Name < sessions[j].Name
		}
		return sessions[i].ID < sessions[j].ID
	})
	return Snapshot{
		State:              m.state,
		PublicURL:          m.publicURL,
		PairingURL:         m.pairingURL,
		QR:                 m.qr,
		Sessions:           sessions,
		Err:                m.errText,
		DiagnosticsWarning: m.diagnosticsWarning,
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
	viewed, _ := m.drainEventsLocked()
	m.refillEventsLocked(viewed)
	if len(viewed) < cap(m.events) {
		m.events <- event
	}
}

func (m *Manager) publishViewed(event Event) bool {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	select {
	case m.events <- event:
		return true
	default:
	}
	viewed, latestSnapshot := m.drainEventsLocked()
	if len(viewed) >= cap(m.events) {
		m.refillEventsLocked(viewed)
		return false
	}
	viewed = append(viewed, event)
	m.refillEventsLocked(viewed)
	if latestSnapshot != nil && len(viewed) < cap(m.events) {
		m.events <- *latestSnapshot
	}
	return true
}

// drainEventsLocked removes the current queue contents while retaining every
// non-coalescible viewer acknowledgement and only the newest state snapshot.
// eventMu must be held by the caller.
func (m *Manager) drainEventsLocked() ([]Event, *Event) {
	viewed := make([]Event, 0, cap(m.events))
	var latestSnapshot Event
	hasSnapshot := false
	for {
		select {
		case event := <-m.events:
			if event.Viewed != nil {
				viewed = append(viewed, event)
			} else if event.Snapshot != nil {
				latestSnapshot = event
				hasSnapshot = true
			}
		default:
			if !hasSnapshot {
				return viewed, nil
			}
			return viewed, &latestSnapshot
		}
	}
}

// refillEventsLocked restores a queue slice known to fit the channel. eventMu
// must be held by the caller.
func (m *Manager) refillEventsLocked(events []Event) {
	for _, event := range events {
		m.events <- event
	}
}

func (c managerCleanup) close(ctx context.Context, shutdown Shutdown) error {
	ctx, cancel := boundedCleanupContext(ctx)
	defer cancel()
	viewerErr := closeViewers(c.viewers)
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
	return errors.Join(viewerErr, clientErr, serverErr, tunnelErr)
}

func closeViewers(viewers []Viewer) error {
	var errs []error
	for _, viewer := range viewers {
		errs = append(errs, viewer.Close())
	}
	return errors.Join(errs...)
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

func broadcastStatus(ctx context.Context, clients []*Client, sessions []Session) {
	for _, client := range clients {
		_ = client.SendControl(ctx, TagStatus, SessionList{Sessions: sessions})
	}
}

func broadcastRevoked(ctx context.Context, clients []*Client, identity Identity, reason string) {
	for _, client := range clients {
		_ = client.SendControl(ctx, TagRevoked, Revoked{
			ID:         identity.ID,
			Generation: identity.Generation,
			Reason:     reason,
		})
	}
}
