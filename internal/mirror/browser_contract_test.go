package mirror

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func TestBrowserContractUsesFragmentScopedWebCryptoAndLocalAssets(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`from "/assets/third_party/xterm/xterm.mjs"`,
		`from "/assets/third_party/xterm/addon-web-links.mjs"`,
		`new URLSearchParams(location.hash.slice(1))`,
		`history.replaceState(null, "", location.pathname + location.search)`,
		`sessionStorage`,
		`crypto.subtle`,
		`HKDF`,
		`SHA-256`,
		`AES-GCM`,
		`wrap-mirror/v3/c2s`,
		`wrap-mirror/v3/s2c`,
		`binaryType = "arraybuffer"`,
		`await cryptoSelfTest()`,
		`new WebSocket`,
		`setBigUint64(4, counter, false)`,
		`Math.min(5000`,
		`event.code === 1008`,
		`sessionStorage.removeItem(STORAGE_KEY)`,
		`showMessage("Pairing rejected"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing %q", want)
		}
	}
	if strings.Index(source, "await cryptoSelfTest()") > strings.Index(source, "new WebSocket") {
		t.Fatal("browser client constructs WebSocket before its crypto self-test")
	}
	for _, forbidden := range []string{
		"local" + "Storage",
		"document." + "cookie",
		"navigator.send" + "Beacon",
		"service" + "Worker",
		"eval(",
		"new Function",
		"http://",
		"https://",
		"wrap-mirror-state.js",
		"TAG.list",
		"TAG.status",
		"TAG.open",
		"TAG.revoked",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("browser client contains forbidden capability %q", forbidden)
		}
	}
}

func TestBrowserContractAutoOpensSoleTargetWithoutPicker(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	html := string(htmlBytes)
	for _, forbidden := range []string{
		"Mirrored terminals",
		"AVAILABLE NOW",
		"sendJSON(TAG.open",
	} {
		if strings.Contains(source+html, forbidden) {
			t.Errorf("single-target browser retains picker behavior %q", forbidden)
		}
	}
	for _, required := range []string{
		"prepareAutomaticTerminal()",
		"case TAG.ready:",
		"target.authenticated = true",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("single-target browser missing %q", required)
		}
	}
}

func TestBrowserRetryableTerminalErrorClosesSocketForReconnect(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "handleTerminalError")
	runtime := goja.New()
	_, err = runtime.RunString(`
let viewerState = {current: {id: "terminal"}, closing: true};
let geometryAccepted = true;
let stopped = false;
let resetCount = 0;
let renderedTitle = "";
function resetTerminalViewport() { resetCount += 1; }
function showMessage(title) { renderedTitle = title; }
` + handler + `
}
const target = {socket: {closeCount: 0, close() { this.closeCount += 1; }}};
handleTerminalError(target, {retry: true, message: "viewer ended"});
if (target.socket.closeCount !== 1) throw new Error("retryable error left socket open");
if (stopped) throw new Error("retryable error stopped reconnection");
if (viewerState.current !== null || viewerState.closing) throw new Error("viewer state was not reset");
if (geometryAccepted || resetCount !== 1) throw new Error("terminal geometry was not reset");
if (renderedTitle !== "Terminal unavailable") throw new Error("terminal error was not rendered");
`)
	if err != nil {
		t.Fatalf("retryable terminal error behavior: %v", err)
	}
}

func TestBrowserCloseAcknowledgementSuppressesSocketReconnect(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "handleCloseAcknowledgement")
	runtime := goja.New()
	_, err = runtime.RunString(`
let stopped = false;
let viewerState = {current: {id: "terminal"}, closing: true};
let geometryAccepted = true;
let reconnects = 0;
function resetTerminalViewport() {}
function showMessage() {}
` + handler + `
}
handleCloseAcknowledgement();
if (!stopped) throw new Error("close acknowledgement did not stop connection");
if (viewerState.current !== null || viewerState.closing || geometryAccepted) {
  throw new Error("close acknowledgement did not reset terminal state");
}
// This is the reconnect decision made by the socket close listener after the
// server follows the encrypted acknowledgement with a normal WebSocket close.
if (!stopped) reconnects += 1;
if (reconnects !== 0) throw new Error("intentional terminal close reconnected");
`)
	if err != nil {
		t.Fatalf("close acknowledgement behavior: %v", err)
	}
}

func TestBrowserReconnectNowReplacesStaleSocket(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "reconnectNow")
	runtime := goja.New()
	_, err = runtime.RunString(`
let secret = new Uint8Array([1]);
let stopped = false;
let viewerState = {current: {id: "terminal"}, closing: false};
let geometryAccepted = true;
let reconnectTimer = 41;
let connection = {socket: {closeCount: 0, close() { this.closeCount += 1; }}};
const staleConnection = connection;
let clearTimeoutValue = 0;
let connectCount = 0;
let resetCount = 0;
const window = {clearTimeout(value) { clearTimeoutValue = value; }};
function resetTerminalViewport() { resetCount += 1; }
function connect() {
  if (viewerState.current !== null || viewerState.closing || geometryAccepted || resetCount !== 1) {
    throw new Error("replacement connection started before stale viewer state was reset");
  }
  connectCount += 1;
}
` + handler + `
}
reconnectNow();
if (clearTimeoutValue !== 41 || reconnectTimer !== 0) {
  throw new Error("pending reconnect timer was not cleared");
}
if (connection !== null || staleConnection.socket.closeCount !== 1) {
  throw new Error("stale connection was not detached and closed");
}
if (!staleConnection.superseded) throw new Error("stale connection was not marked superseded");
if (connectCount !== 1) throw new Error("fresh connection was not started exactly once");
`)
	if err != nil {
		t.Fatalf("immediate reconnect behavior: %v", err)
	}
}

func TestBrowserAutomaticReconnectPreservesHealthyConnection(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "reconnectIfStale")
	runtime := goja.New()
	_, err = runtime.RunString(`
const WebSocket = {OPEN: 1};
let reconnectCount = 0;
let connection = {authenticated: true, poisoned: false, socket: {readyState: WebSocket.OPEN}};
function reconnectNow() { reconnectCount += 1; }
` + handler + `
}
reconnectIfStale();
if (reconnectCount !== 0) throw new Error("healthy connection was replaced");
connection.authenticated = false;
reconnectIfStale();
connection = {authenticated: true, poisoned: true, socket: {readyState: WebSocket.OPEN}};
reconnectIfStale();
connection = {authenticated: true, poisoned: false, socket: {readyState: 3}};
reconnectIfStale();
connection = null;
reconnectIfStale();
if (reconnectCount !== 4) throw new Error("stale connection did not reconnect");
`)
	if err != nil {
		t.Fatalf("automatic reconnect behavior: %v", err)
	}
}

func TestBrowserReconnectNowRespectsStoppedState(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "reconnectNow")
	runtime := goja.New()
	_, err = runtime.RunString(`
let secret = new Uint8Array([1]);
let stopped = true;
let reconnectTimer = 41;
let connection = {socket: {closeCount: 0, close() { this.closeCount += 1; }}};
const originalConnection = connection;
let connectCount = 0;
const window = {clearTimeout() { throw new Error("stopped reconnect cleared timer"); }};
function connect() { connectCount += 1; }
` + handler + `
}
reconnectNow();
if (reconnectTimer !== 41 || connection !== originalConnection) {
  throw new Error("stopped reconnect changed connection state");
}
if (originalConnection.socket.closeCount !== 0 || connectCount !== 0) {
  throw new Error("stopped reconnect touched the socket");
}
`)
	if err != nil {
		t.Fatalf("stopped reconnect behavior: %v", err)
	}
}

func TestBrowserReconnectNowRespectsMissingSecretAndClosingState(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "reconnectNow")
	for _, setup := range []string{
		`let secret = null; let viewerState = {closing: false};`,
		`let secret = new Uint8Array([1]); let viewerState = {closing: true};`,
	} {
		runtime := goja.New()
		_, err = runtime.RunString(setup + `
let stopped = false;
let reconnectTimer = 41;
let connection = {socket: {closeCount: 0, close() { this.closeCount += 1; }}};
const originalConnection = connection;
let connectCount = 0;
const window = {clearTimeout() { throw new Error("unavailable reconnect cleared timer"); }};
function connect() { connectCount += 1; }
` + handler + `
}
reconnectNow();
if (reconnectTimer !== 41 || connection !== originalConnection) {
  throw new Error("unavailable reconnect changed connection state");
}
if (originalConnection.socket.closeCount !== 0 || connectCount !== 0) {
  throw new Error("unavailable reconnect touched the socket");
}
`)
		if err != nil {
			t.Fatalf("unavailable reconnect behavior: %v", err)
		}
	}
}

func TestBrowserStalePoisonCannotStopReplacementConnection(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "poison")
	runtime := goja.New()
	_, err = runtime.RunString(`
let stopped = false;
const stale = {poisoned: false, superseded: true, socket: {closeCount: 0, close() { this.closeCount += 1; }}};
const replacement = {poisoned: false, superseded: false, socket: {closeCount: 0, close() { this.closeCount += 1; }}};
let connection = replacement;
` + handler + `
}
poison(stale, false);
if (!stale.poisoned || stale.socket.closeCount !== 1) {
  throw new Error("stale connection was not poisoned and closed");
}
if (stopped || replacement.poisoned || replacement.socket.closeCount !== 0) {
  throw new Error("stale poison affected replacement connection");
}
poison(replacement, false);
if (!stopped) throw new Error("current poison did not stop reconnection");
`)
	if err != nil {
		t.Fatalf("stale poison behavior: %v", err)
	}
}

func TestBrowserClosedCurrentSocketFailureStillStopsReconnect(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "poison")
	runtime := goja.New()
	_, err = runtime.RunString(`
let stopped = false;
const target = {
  poisoned: false,
  superseded: false,
  socket: {closeCount: 0, close() { this.closeCount += 1; }},
};
let connection = target;
` + handler + `
}
connection = null;
poison(target, false);
if (!stopped) throw new Error("closed current socket failure did not stop reconnect");
if (!target.poisoned || target.socket.closeCount !== 1) {
  throw new Error("failed current socket was not poisoned and closed");
}
`)
	if err != nil {
		t.Fatalf("closed current socket failure behavior: %v", err)
	}
}

func TestBrowserSupersededMessageCannotMutateReplacementState(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "receiveMessage")
	runtime := goja.New()
	_, err = runtime.RunString(`
const TAG = {close: 5, output: 7, ready: 8, shutdown: 10, error: 11};
let resolveDecrypt;
function decryptFrame() {
  return new Promise((resolve) => { resolveDecrypt = resolve; });
}
const stale = {
  poisoned: false,
  sendKey: {},
  authenticated: true,
  receiveKey: {},
  receiveCounter: 0n,
};
const replacement = {poisoned: false};
let connection = stale;
let geometryAccepted = true;
let viewerState = {current: {id: "terminal"}, closing: false};
let terminalWrites = 0;
const terminal = {write() { terminalWrites += 1; }};
function parseJSON() { throw new Error("unexpected JSON parse"); }
function handleTerminalError() { throw new Error("unexpected terminal error"); }
function handleCloseAcknowledgement() { throw new Error("unexpected close acknowledgement"); }
function validateOpened() { throw new Error("unexpected ready frame"); }
function setOnline() {}
function resizeTerminalToHostGeometry() {}
const viewportReducer = {open() {}};
const terminalViewportState = {};
function scheduleTerminalMeasurement() {}
function focusTerminalForPhysicalKeyboard() {}
let reconnectAttempt = 0;
let stopped = false;
const sessionStorage = {removeItem() {}};
let secret = new Uint8Array([1]);
` + handler + `
}
receiveMessage(stale, new ArrayBuffer(1));
connection = replacement;
resolveDecrypt({tag: TAG.output, payload: new Uint8Array([65])});
`)
	if err != nil {
		t.Fatalf("start superseded message: %v", err)
	}
	if _, err := runtime.RunString(`
if (terminalWrites !== 0) throw new Error("superseded output reached terminal");
if (stale.receiveCounter !== 0n) throw new Error("superseded receive counter advanced");
if (connection !== replacement) throw new Error("replacement connection changed");
`); err != nil {
		t.Fatalf("superseded message behavior: %v", err)
	}
}

func TestBrowserSupersededHandshakeCannotSendOnReplacement(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "receiveMessage")
	runtime := goja.New()
	_, err = runtime.RunString(`
const TAG = {close: 5, hello: 1, output: 7, ready: 8, shutdown: 10, error: 11};
let resolveFirstDerivation;
let derivationCount = 0;
function deriveDirectionalKey() {
  derivationCount += 1;
  if (derivationCount === 1) {
    return new Promise((resolve) => { resolveFirstDerivation = resolve; });
  }
  return Promise.resolve({});
}
const stale = {
  poisoned: false,
  sendKey: null,
  sendChain: Promise.resolve(),
  socket: {sendCount: 0, send() { this.sendCount += 1; }},
};
const replacement = {poisoned: false};
let connection = stale;
let secret = new Uint8Array([1]);
const crypto = {getRandomValues(value) { return value; }};
let queuedFrames = 0;
function queueFrame() { queuedFrames += 1; }
let preparedTerminals = 0;
function prepareAutomaticTerminal() { preparedTerminals += 1; }
` + handler + `
}
receiveMessage(stale, new ArrayBuffer(16));
connection = replacement;
resolveFirstDerivation({});
`)
	if err != nil {
		t.Fatalf("start superseded handshake: %v", err)
	}
	if _, err := runtime.RunString(`
if (derivationCount !== 2) throw new Error("handshake did not reach supersession check");
if (stale.socket.sendCount !== 0 || queuedFrames !== 0 || preparedTerminals !== 0) {
  throw new Error("superseded handshake affected a socket or terminal");
}
if (connection !== replacement) throw new Error("replacement connection changed");
`); err != nil {
		t.Fatalf("superseded handshake behavior: %v", err)
	}
}

func TestBrowserReconnectButtonHiddenWhileClosing(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "showMessage")
	runtime := goja.New()
	_, err = runtime.RunString(`
let secret = new Uint8Array([1]);
let stopped = false;
let viewerState = {closing: true};
const classList = {remove() {}};
const elements = {
  messageTitle: {textContent: ""},
  messageDetail: {textContent: ""},
  connection: {textContent: "", classList},
  reconnect: {hidden: false},
};
function showOnly() {}
` + handler + `
}
showMessage("Closing terminal…", "Waiting", "Encrypted");
if (!elements.reconnect.hidden) throw new Error("reconnect button shown while closing");
viewerState.closing = false;
showMessage("Connection lost", "Retry", "Reconnecting");
if (elements.reconnect.hidden) throw new Error("reconnect button hidden while retryable");
`)
	if err != nil {
		t.Fatalf("reconnect button visibility: %v", err)
	}
}

func TestBrowserVisibilityRecoveryRunsOnceAfterForegrounding(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	handler := browserFunctionSource(t, string(sourceBytes), "handleVisibilityChange")
	runtime := goja.New()
	_, err = runtime.RunString(`
const document = {hidden: false};
let pageWasHidden = false;
let reconnectCount = 0;
function reconnectIfStale() { reconnectCount += 1; }
` + handler + `
}
handleVisibilityChange();
if (reconnectCount !== 0) throw new Error("initial visible page reconnected");
document.hidden = true;
handleVisibilityChange();
if (!pageWasHidden || reconnectCount !== 0) throw new Error("hidden page was not recorded");
document.hidden = false;
handleVisibilityChange();
handleVisibilityChange();
if (pageWasHidden || reconnectCount !== 1) {
  throw new Error("foreground recovery did not reconnect exactly once");
}
`)
	if err != nil {
		t.Fatalf("visibility reconnect behavior: %v", err)
	}
}

func TestBrowserContractOffersManualAndLifecycleReconnect(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	html := string(htmlBytes)
	for _, want := range []string{
		`id="reconnect-button"`,
		`id="terminal-reconnect-button"`,
		`elements.reconnect.addEventListener("click", reconnectNow)`,
		`elements.terminalReconnect.addEventListener("click", reconnectNow)`,
		`document.addEventListener("visibilitychange", handleVisibilityChange)`,
		`window.addEventListener("online", reconnectIfStale)`,
	} {
		if !strings.Contains(source+html, want) {
			t.Errorf("browser client missing reconnect contract %q", want)
		}
	}
}

func TestBrowserContractCollapsesKeyboardWhenLeavingTerminal(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	showOnlyStart := strings.Index(source, "function showOnly(view)")
	if showOnlyStart < 0 {
		t.Fatal("browser client missing showOnly function")
	}
	showOnlyEnd := strings.Index(source[showOnlyStart:], "\n}\n")
	if showOnlyEnd < 0 {
		t.Fatal("browser client has incomplete showOnly function")
	}
	showOnly := source[showOnlyStart : showOnlyStart+showOnlyEnd]
	if !strings.Contains(showOnly, `view !== "terminal"`) ||
		!strings.Contains(showOnly, `setTypingMode(false)`) {
		t.Error("leaving the terminal view does not collapse the software keyboard")
	}
}

func TestBrowserContractUsesHostOwnedGeometryWithoutRemoteResize(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`import "/assets/wrap-mirror-viewport.js"`,
		`ready: 0x08`,
		`case TAG.ready:`,
		`terminal.resize(opened.columns, opened.rows)`,
		`querySelector(".xterm-viewport")`,
		`scrollbar.offsetWidth - scrollbar.clientWidth`,
		`terminal.options.fontSize = viewportReducer.fontSize(`,
		`globalThis.matchMedia?.("(pointer: fine)").matches`,
		`focusTerminalForPhysicalKeyboard()`,
		`version":3`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing host-geometry contract %q", want)
		}
	}
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	cssBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.css")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	css := string(cssBytes)
	for _, want := range []string{`id="terminal-viewport"`, `id="terminal-spacer"`, `id="terminal-surface"`} {
		if !strings.Contains(html, want) {
			t.Errorf("browser HTML missing viewport layer %q", want)
		}
	}
	if !strings.Contains(css, `.terminal-viewport`) || !strings.Contains(css, `overflow: auto`) {
		t.Error("browser CSS does not make wide terminal geometry pannable")
	}
	for _, forbidden := range []string{
		`FitAddon`,
		`TAG.resize`,
		`scheduleResize`,
		`wrap-mirror/v1/c2s`,
		`wrap-mirror/v1/s2c`,
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("browser client retains viewport-owned behavior %q", forbidden)
		}
	}
	applyStart := strings.Index(source, "function applyTerminalViewport()")
	if applyStart < 0 {
		t.Fatal("browser client missing applyTerminalViewport")
	}
	applyEnd := strings.Index(source[applyStart:], "\n}\n")
	if applyEnd < 0 {
		t.Fatal("browser client has incomplete applyTerminalViewport")
	}
	apply := source[applyStart : applyStart+applyEnd]
	if strings.Contains(apply, `.style.transform`) {
		t.Error("committed terminal layout uses a viewport transform")
	}
}

func TestBrowserContractRestoresMetricsAfterIdenticalGeometryReopen(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	openStart := strings.Index(source, "function prepareAutomaticTerminal()")
	if openStart < 0 {
		t.Fatal("browser client missing automatic terminal preparation")
	}
	openEnd := strings.Index(source[openStart:], "\n}\n")
	if openEnd < 0 {
		t.Fatal("browser client missing automatic terminal preparation")
	}
	prepare := source[openStart : openStart+openEnd]
	show := strings.Index(prepare, `showOnly("terminal")`)
	restore := strings.Index(prepare, `restoreBaseTerminalMetrics()`)
	if show < 0 || restore < 0 || show > restore {
		t.Fatal("browser does not restore base metrics after making the terminal visible")
	}
	for _, want := range []string{
		`const probeRows = opened.rows === 2 ? 3 : opened.rows - 1`,
		`terminal.resize(opened.columns, probeRows)`,
		`terminal.resize(opened.columns, opened.rows)`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing identical-geometry refresh %q", want)
		}
	}
}

func TestBrowserContractFitsVisibleTerminalBeforeFirstReveal(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	cssBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.css")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	combined := string(htmlBytes) + "\n" + string(cssBytes) + "\n" + source
	for _, want := range []string{
		`id="terminal-loading"`,
		`Opening terminal…`,
		`function ensureTerminalMounted()`,
		`terminal.open(elements.terminal);`,
		`const MAX_VIEWPORT_MEASURE_ATTEMPTS = 60`,
		`setTerminalDisplayPending("Opening terminal…")`,
		`viewportReducer.readable(terminalViewportState)`,
		`pendingTerminalFit = true`,
		`if (pendingTerminalFit)`,
		`revealTerminalDisplay()`,
		`Terminal display unavailable`,
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("browser missing first-visible Fit gate %q", want)
		}
	}

	mountStart := strings.Index(source, "function ensureTerminalMounted()")
	if mountStart >= 0 {
		mountEnd := strings.Index(source[mountStart:], "\n}\n")
		if mountEnd < 0 || !strings.Contains(source[mountStart:mountStart+mountEnd], `terminal.open(elements.terminal);`) {
			t.Fatal("xterm mount is not isolated in ensureTerminalMounted")
		}
	}
	if count := strings.Count(source, `terminal.open(elements.terminal);`); count != 1 {
		t.Fatalf("xterm must be mounted exactly once by the visible-view gate, got %d mount sites", count)
	}

	openStart := strings.Index(source, "function openSession(session)")
	if openStart >= 0 {
		openEnd := strings.Index(source[openStart:], "\n}\n")
		if openEnd < 0 {
			t.Fatal("browser client has incomplete openSession function")
		}
		openSession := source[openStart : openStart+openEnd]
		show := strings.Index(openSession, `showOnly("terminal")`)
		mount := strings.Index(openSession, `ensureTerminalMounted()`)
		if show < 0 || mount < 0 || show > mount {
			t.Fatal("xterm is mounted before its terminal view is visible")
		}
	}
	measureStart := strings.Index(source, "function scheduleTerminalMeasurement()")
	if measureStart < 0 {
		t.Fatal("browser client missing terminal measurement function")
	}
	measureEnd := strings.Index(source[measureStart:], "\n}\n")
	if measureEnd < 0 {
		t.Fatal("browser client has incomplete terminal measurement function")
	}
	measurement := source[measureStart : measureStart+measureEnd]
	readable := strings.Index(measurement, "viewportReducer.readable(terminalViewportState)")
	apply := strings.Index(measurement, "applyTerminalViewport()")
	reveal := strings.Index(measurement, "revealTerminalDisplay()")
	if readable < 0 || apply < 0 || reveal < 0 || readable > apply || apply > reveal {
		t.Fatal("terminal is revealed before readable layout is applied")
	}
	if strings.Contains(combined, `Fitting terminal…`) {
		t.Fatal("browser retains misleading Fit-only opening copy")
	}
}

func TestBrowserContractKeepsPendingTerminalMeasurableOnReopen(t *testing.T) {
	cssBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, `.terminal-shell.display-pending .terminal-loading`) ||
		!strings.Contains(css, `background: #080a09`) {
		t.Fatal("pending terminal does not have an opaque loading cover")
	}
	if strings.Contains(css, `.terminal-shell.display-pending .terminal-spacer {
  visibility: hidden;
}`) {
		t.Fatal("pending state hides xterm from mobile Safari layout during reopen")
	}
}

func TestBrowserContractRecoversFromDeferredTerminalMountFailure(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	openSession := browserFunctionSource(t, string(sourceBytes), "prepareAutomaticTerminal")
	for _, want := range []string{
		"try {",
		"ensureTerminalMounted()",
		"catch",
		"viewerState.current = null",
		"viewerState.closing = false",
		`"Terminal display unavailable"`,
	} {
		if !strings.Contains(openSession, want) {
			t.Errorf("terminal mount recovery missing %q", want)
		}
	}
	if strings.Contains(openSession, "sendJSON(TAG.open") {
		t.Fatal("automatic terminal preparation sends a target-selection request")
	}
}

func TestBrowserContractSkipsHiddenOrZeroWidthViewportRefits(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	start := strings.Index(source, "function refitTerminalViewport()")
	if start < 0 {
		t.Fatal("browser client missing refitTerminalViewport")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("browser client has incomplete refitTerminalViewport")
	}
	refit := source[start : start+end]
	for _, want := range []string{
		`elements.terminalView.classList.contains("hidden")`,
		`viewportWidth <= 0`,
	} {
		if !strings.Contains(refit, want) {
			t.Errorf("hidden viewport refit guard missing %q", want)
		}
	}
	guard := strings.Index(refit, `viewportWidth <= 0`)
	resize := strings.Index(refit, `viewportReducer.resize(`)
	if guard < 0 || resize < 0 || guard > resize {
		t.Fatal("viewport width is validated after reducer resize")
	}
}

func TestBrowserContractCancelsHiddenTerminalMeasurementDuringClose(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	measurement := browserFunctionSource(t, source, "scheduleTerminalMeasurement")
	for _, want := range []string{
		`elements.terminalView.classList.contains("hidden")`,
		`elements.terminalViewport.clientWidth <= 0`,
	} {
		if !strings.Contains(measurement, want) {
			t.Errorf("hidden measurement guard missing %q", want)
		}
	}
	closeSession := browserFunctionSource(t, source, "closeSession")
	for _, want := range []string{"geometryAccepted = false", "cancelTerminalMeasurement()"} {
		if !strings.Contains(closeSession, want) {
			t.Errorf("close path does not cancel terminal measurement: missing %q", want)
		}
	}
	openedStart := strings.Index(source, "case TAG.ready:")
	if openedStart < 0 {
		t.Fatal("browser client is missing opened handler")
	}
	outputStart := strings.Index(source[openedStart:], "case TAG.output:")
	if outputStart < 0 {
		t.Fatal("browser client is missing opened/output handlers")
	}
	opened := source[openedStart : openedStart+outputStart]
	authenticated := strings.Index(opened, "target.authenticated = true")
	resize := strings.Index(opened, "resizeTerminalToHostGeometry(opened)")
	if authenticated < 0 || resize < 0 || authenticated > resize {
		t.Fatal("automatic ready frame is rendered before authentication is committed")
	}
}

func TestBrowserContractKeepsViewerActiveWhenTerminalMeasurementTimesOut(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	measurement := browserFunctionSource(t, string(sourceBytes), "scheduleTerminalMeasurement")
	if !strings.Contains(measurement, "showTerminalDisplayError()") {
		t.Fatal("measurement timeout does not preserve the active viewer error state")
	}
	if strings.Contains(measurement, "renderSessions(") {
		t.Fatal("measurement timeout renders the list before close acknowledgement")
	}
	measure := strings.Index(measurement, "getBoundingClientRect()")
	timeout := strings.Index(measurement, "viewportMeasureAttempts >= MAX_VIEWPORT_MEASURE_ATTEMPTS")
	if measure < 0 || timeout < 0 || timeout < measure {
		t.Fatal("measurement timeout is checked before retry dimensions are re-read")
	}
}

func TestBrowserContractSessionReopenResetsViewportPan(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	reset := browserFunctionSource(t, string(sourceBytes), "resetTerminalViewport")
	for _, want := range []string{
		"elements.terminalViewport.scrollLeft = 0",
		"elements.terminalViewport.scrollTop = 0",
	} {
		if !strings.Contains(reset, want) {
			t.Errorf("terminal viewport reset does not clear prior pan: missing %q", want)
		}
	}
}

func browserFunctionSource(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, "function "+name+"(")
	if start < 0 {
		t.Fatalf("browser client missing %s", name)
	}
	if start >= len("async ") && source[start-len("async "):start] == "async " {
		start -= len("async ")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("browser client has incomplete %s", name)
	}
	return source[start : start+end]
}

func TestBrowserContractPreviewsPinchWithoutRefreshingXterm(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`requestAnimationFrame`,
		`viewportReducer.previewPinch(terminalViewportState, pinchGesture, input)`,
		`terminalScreen.style.transform =`,
		`terminal.element?.querySelector(".xterm-screen")`,
		`viewportReducer.commitPinch(terminalViewportState, pinchPreview)`,
		`function discardPinchPreview()`,
		`function restoreCommittedPinchPreview()`,
		`applyPinchPreview(preview)`,
		`cancelAnimationFrame(pinchPreviewFrame)`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser missing smooth pinch contract %q", want)
		}
	}
	if !strings.Contains(source, "if (!preview) {\n    restoreCommittedPinchPreview();\n    return;\n  }") {
		t.Fatal("invalid latest pinch preview does not restore committed Fit")
	}
	if !strings.Contains(source, "if (latestInput) {\n    const preview = calculatePinchPreview(latestInput);\n    applyPinchPreview(preview);\n  }") {
		t.Fatal("pinch release can overwrite the last rendered preview without new input")
	}

	moveStart := strings.Index(source, `elements.terminalViewport.addEventListener("pointermove",`)
	if moveStart < 0 {
		t.Fatal("browser client missing terminal pointermove callback")
	}
	moveEnd := strings.Index(source[moveStart:], "\n});")
	if moveEnd < 0 {
		t.Fatal("browser client has incomplete terminal pointermove callback")
	}
	pointerMove := source[moveStart : moveStart+moveEnd]
	if !strings.Contains(pointerMove, `schedulePinchPreview(input)`) {
		t.Error("pinch move does not schedule a coalesced preview")
	}
	for _, forbidden := range []string{
		`applyTerminalViewport()`,
		`terminal.options.fontSize`,
		`terminal.refresh(`,
		`terminal.resize(`,
	} {
		if strings.Contains(pointerMove, forbidden) {
			t.Errorf("pinch move repaints xterm with %q", forbidden)
		}
	}

	commitStart := strings.Index(source, "function commitPinchPreview()")
	if commitStart < 0 {
		t.Fatal("browser client missing commitPinchPreview")
	}
	commitEnd := strings.Index(source[commitStart:], "\n}\n")
	if commitEnd < 0 {
		t.Fatal("browser client has incomplete commitPinchPreview")
	}
	commit := source[commitStart : commitStart+commitEnd]
	if count := strings.Count(commit, `applyTerminalViewport()`); count != 1 {
		t.Fatalf("pinch commit must apply xterm metrics exactly once, got %d", count)
	}
	if strings.Contains(source, `elements.terminalSurface.style.transform =`) {
		t.Fatal("pinch preview scales fixed terminal chrome")
	}
	cancelStart := strings.Index(source, `elements.terminalViewport.addEventListener("pointercancel",`)
	if cancelStart < 0 {
		t.Fatal("browser client missing terminal pointercancel callback")
	}
	cancelEnd := strings.Index(source[cancelStart:], "\n});")
	if cancelEnd < 0 {
		t.Fatal("browser client has incomplete terminal pointercancel callback")
	}
	pointerCancel := source[cancelStart : cancelStart+cancelEnd]
	guard := strings.Index(pointerCancel, `if (!viewportPointers.has(event.pointerId))`)
	commitPinch := strings.Index(pointerCancel, `commitPinchPreview()`)
	if guard < 0 || commitPinch < 0 || guard > commitPinch {
		t.Fatal("untracked pointer cancellation can end an active pinch")
	}
}

func TestBrowserContractIncludesTerminalAndMobileControls(t *testing.T) {
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, control := range []string{
		`id="keyboard-toggle"`,
		`id="fit-button"`,
		`id="pinch-scale"`,
		`id="close-button"`,
	} {
		if !strings.Contains(html, control) {
			t.Errorf("browser HTML missing mobile terminal control %s", control)
		}
	}
	for _, forbidden := range []string{
		`id="utility-rail"`,
		`id="zoom-out"`,
		`id="zoom-level"`,
		`id="zoom-in"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("browser HTML retains obsolete mobile utility control %s", forbidden)
		}
	}
	headingStart := strings.Index(html, `<div class="terminal-heading">`)
	headingEnd := strings.Index(html, `<div id="terminal-viewport"`)
	if headingStart < 0 || headingEnd < 0 || headingStart > headingEnd {
		t.Fatal("browser HTML has an invalid terminal heading")
	}
	heading := html[headingStart:headingEnd]
	for _, control := range []string{`id="keyboard-toggle"`, `id="fit-button"`, `id="close-button"`} {
		if !strings.Contains(heading, control) {
			t.Errorf("terminal heading missing %s", control)
		}
	}
	if strings.Contains(html, `id="pinch-scale" aria-live=`) {
		t.Error("transient pinch scale must not announce every gesture update")
	}
	for _, control := range []string{
		`id="copy-button"`,
		`id="paste-button"`,
		`data-key="enter"`,
		`data-key="escape"`,
		`data-key="tab"`,
		`data-key="shift-tab"`,
		`data-key="control"`,
		`data-key="up"`,
		`data-key="down"`,
		`data-key="left"`,
		`data-key="right"`,
		`data-key="ctrl-c"`,
		`data-key="ctrl-d"`,
		`data-key="ctrl-l"`,
		`data-key="ctrl-z"`,
	} {
		if !strings.Contains(html, control) {
			t.Errorf("browser HTML missing %s", control)
		}
	}
	cssBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	for _, want := range []string{
		`.terminal-actions`,
		`.pinch-scale`,
		`touch-action: none`,
		`min-height: 2.75rem`,
		`--mirror-viewport-height`,
		`--mirror-viewport-top`,
		`--mirror-viewport-left`,
		`position: fixed`,
		`.terminal-shell.typing .toolbar`,
		`body.terminal-active .masthead`,
		`height: var(--mirror-viewport-height)`,
		`.terminal-shell.compact.typing .terminal-heading`,
		`(any-pointer: coarse)`,
		`(max-height: 30rem)`,
		`padding: env(safe-area-inset-top) env(safe-area-inset-right)`,
		`env(safe-area-inset-bottom) env(safe-area-inset-left)`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("browser CSS missing mobile control behavior %q", want)
		}
	}
	for _, forbidden := range []string{`min-height: 16rem`, `min-height: 12rem`} {
		if strings.Contains(css, forbidden) {
			t.Errorf("browser CSS can exceed a short visual viewport with %q", forbidden)
		}
	}
	if strings.Contains(css, `touch-action: pan-x pan-y`) {
		t.Error("terminal viewport still delegates touch panning to the browser")
	}
	if strings.Contains(css, ".terminal-shell.compact.typing .terminal-heading {\n  display: none;") {
		t.Error("short-screen typing hides the keyboard collapse control")
	}
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`function setTypingMode(value)`,
		`terminal.blur()`,
		`setAttribute("inputmode", typingMode ? "text" : "none")`,
		`viewportReducer.beginPinch(terminalViewportState,`,
		`viewportReducer.previewPinch(terminalViewportState,`,
		`viewportReducer.fit(terminalViewportState)`,
		`viewportPointers`,
		`pinchGesture`,
		`pointer.scrollLeft - (pointer.x - pointer.startX)`,
		`viewportReducer.panVertical(terminalViewportState,`,
		`terminal.scrollLines(lineDelta)`,
		`const activeBuffer = terminal.buffer.active`,
		`viewportReducer.trackLineOffset(`,
		`activeBuffer.baseY`,
		`scrollHeight: elements.terminalViewport.scrollHeight`,
		`function showPinchScale(`,
		`typingMode ? "Hide keyboard" : "Keyboard"`,
		`elements.copy.addEventListener("click"`,
		`elements.paste.addEventListener("click"`,
		`globalThis.matchMedia?.("(any-pointer: coarse)").matches`,
		`event.pointerType === "touch" || event.pointerType === "pen"`,
		`elements.terminalViewport.addEventListener("pointerup"`,
		`--mirror-viewport-height`,
		`globalThis.visualViewport?.offsetTop || 0`,
		`globalThis.visualViewport?.offsetLeft || 0`,
		`document.body.classList.toggle("terminal-active", view === "terminal")`,
		`elements.terminalView.classList.toggle("compact", height < 360)`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing mobile control behavior %q", want)
		}
	}
}

func TestBrowserUtilityMenuKeepsSecondaryActionsOutOfPrimaryHeader(t *testing.T) {
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	menuStart := strings.Index(html, `<div id="terminal-utility-menu"`)
	menuEnd := -1
	if menuStart >= 0 {
		if relativeEnd := strings.Index(html[menuStart:], "\n            </div>"); relativeEnd >= 0 {
			menuEnd = menuStart + relativeEnd
		}
	}
	if menuStart < 0 || menuEnd < 0 {
		t.Fatal("terminal secondary actions are not grouped in a utility menu")
	}
	primary := html[:menuStart]
	menu := html[menuStart:menuEnd]
	for _, control := range []string{`id="copy-button"`, `id="paste-button"`, `id="more-button"`} {
		if !strings.Contains(primary, control) {
			t.Errorf("primary terminal header missing %s", control)
		}
	}
	for _, control := range []string{`id="keyboard-toggle"`, `id="fit-button"`, `id="terminal-reconnect-button"`, `id="close-button"`} {
		if strings.Contains(primary, control) || !strings.Contains(menu, control) {
			t.Errorf("secondary terminal action is not confined to the utility menu: %s", control)
		}
	}
	for _, contract := range []string{
		`id="more-button" type="button" aria-expanded="false" aria-controls="terminal-utility-menu"`,
		`id="terminal-utility-menu" class="terminal-utility-menu"`,
		`aria-label="More terminal controls" hidden`,
	} {
		if !strings.Contains(html, contract) {
			t.Errorf("terminal utility menu missing accessible contract %q", contract)
		}
	}

	setOpen := browserFunctionSource(t, string(sourceBytes), "setUtilityMenuOpen")
	runtime := goja.New()
	_, err = runtime.RunString(`
let utilityMenuOpen = false;
const attributes = {};
const elements = {
  more: {setAttribute(name, value) { attributes[name] = value; }},
  utilityMenu: {hidden: true},
};
` + setOpen + `
}
setUtilityMenuOpen(true);
if (!utilityMenuOpen || elements.utilityMenu.hidden || attributes["aria-expanded"] !== "true") {
  throw new Error("opening More did not expose the utility menu");
}
setUtilityMenuOpen(false);
if (utilityMenuOpen || !elements.utilityMenu.hidden || attributes["aria-expanded"] !== "false") {
  throw new Error("closing More did not hide the utility menu");
}
`)
	if err != nil {
		t.Fatalf("terminal utility menu state: %v", err)
	}
}

func TestBrowserUtilityMenuDismissesAfterActionsAndOutsideInput(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	setOpen := browserFunctionSource(t, source, "setUtilityMenuOpen")
	dismissAction := browserFunctionSource(t, source, "dismissUtilityMenuAfterAction")
	dismissPointer := browserFunctionSource(t, source, "dismissUtilityMenuFromPointer")
	dismissKey := browserFunctionSource(t, source, "dismissUtilityMenuFromKey")
	runtime := goja.New()
	_, err = runtime.RunString(`
let utilityMenuOpen = true;
let closed = 0;
let prevented = 0;
let stopped = 0;
let focused = 0;
const inside = {};
const elements = {
  more: {setAttribute() {}, focus() { focused += 1; }},
  utilityMenu: {hidden: false},
  terminalActions: {contains(target) { return target === inside; }},
};
` + setOpen + `
}
` + dismissAction + `
}
` + dismissPointer + `
}
` + dismissKey + `
}
dismissUtilityMenuFromPointer({target: inside});
if (!utilityMenuOpen) throw new Error("an inside pointer closed More before its action");
dismissUtilityMenuAfterAction({target: {closest(selector) { return selector === "button" ? {} : null; }}});
if (utilityMenuOpen) throw new Error("a utility action left More open");
setUtilityMenuOpen(true);
dismissUtilityMenuFromPointer({target: {}});
if (utilityMenuOpen) throw new Error("an outside pointer left More open");
setUtilityMenuOpen(true);
dismissUtilityMenuFromKey({key: "Enter", preventDefault() {}, stopPropagation() {}});
if (!utilityMenuOpen) throw new Error("a non-Escape key closed More");
dismissUtilityMenuFromKey({
  key: "Escape",
  preventDefault() { prevented += 1; },
  stopPropagation() { stopped += 1; },
});
if (utilityMenuOpen || prevented !== 1 || stopped !== 1 || focused !== 1) {
  throw new Error("Escape did not exclusively dismiss More");
}
`)
	if err != nil {
		t.Fatalf("terminal utility menu dismissal: %v", err)
	}
}

func TestBrowserContractLoadsDependencyFreeBootstrap(t *testing.T) {
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	bootstrapBytes, err := fs.ReadFile(assets, "assets/wrap-mirror-bootstrap.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	bootstrap := string(bootstrapBytes)
	if !strings.Contains(html, `src="/assets/wrap-mirror-bootstrap.js"`) {
		t.Fatal("browser HTML does not load the dependency-free bootstrap")
	}
	if strings.Contains(html, `src="/assets/wrap-mirror.js"`) {
		t.Fatal("browser HTML bypasses the bootstrap and loads the client directly")
	}
	if !strings.Contains(bootstrap, `() => import("/assets/wrap-mirror.js")`) {
		t.Fatal("bootstrap does not dynamically import the main client")
	}
	for _, forbidden := range []string{
		`import {`,
		`import *`,
		`import "/`,
		`import '/`,
	} {
		if strings.Contains(bootstrap, forbidden) {
			t.Fatalf("bootstrap has a static dependency %q", forbidden)
		}
	}
}

func TestBrowserCoarsePointerDownSuppressesCompatibilityMouseEvents(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	start := strings.Index(source, `elements.terminalViewport.addEventListener("pointerdown"`)
	if start < 0 {
		t.Fatal("browser client is missing the terminal pointerdown handler")
	}
	end := strings.Index(source[start:], `elements.terminalViewport.addEventListener("pointermove"`)
	if end < 0 {
		t.Fatal("browser client is missing the terminal pointermove handler")
	}
	handler := source[start : start+end]
	prevent := strings.Index(handler, `event.preventDefault()`)
	track := strings.Index(handler, `viewportPointers.set(event.pointerId`)
	if prevent < 0 || track < 0 || prevent > track {
		t.Fatal("accepted coarse pointerdown can reach xterm before its browser default is canceled")
	}
}

func TestBrowserCoarseTapForwardsPrimaryMouseAndOpensKeyboard(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	dispatch := browserFunctionSource(t, source, "dispatchTerminalMouse")
	finish := browserFunctionSource(t, source, "finishCoarsePointer")
	runtime := goja.New()
	_, err = runtime.RunString(`
const dispatched = [];
function MouseEvent(type, options) {
  this.type = type;
  Object.assign(this, options);
}
const terminalScreen = {dispatchEvent(event) { dispatched.push(event); }};
const terminal = {element: {
  querySelector(selector) {
    if (selector !== ".xterm-screen") throw new Error("tap queried the wrong xterm target");
    return terminalScreen;
  },
  dispatchEvent() { throw new Error("tap dispatched above xterm's link target"); },
}};
const navigator = {platform: "Linux armv8l", userAgent: "Mobile"};
let selectionMode = false;
let typingMode = false;
const viewerState = {current: {id: "terminal"}};
let typingChanges = 0;
function setTypingMode(value) { typingMode = value; typingChanges += 1; }
` + dispatch + `
}
` + finish + `
}
finishCoarsePointer({x: 41, y: 73, moved: false}, false);
if (dispatched.length !== 3) throw new Error("tap did not resolve links before one mouse press");
if (dispatched[0].type !== "mousemove" || dispatched[1].type !== "mousedown" ||
    dispatched[2].type !== "mouseup") {
  throw new Error("tap sent the wrong mouse event sequence");
}
if (dispatched[0].clientX !== 41 || dispatched[0].clientY !== 73 ||
    dispatched[0].button !== 0 || dispatched[0].buttons !== 0 ||
    dispatched[1].buttons !== 1 || dispatched[2].buttons !== 0) {
  throw new Error("tap mouse event lost its position or primary-button state");
}
if (dispatched[0].shiftKey || dispatched[0].altKey) {
  throw new Error("ordinary tap forced terminal selection");
}
for (const event of dispatched) {
  if (event.wrapTerminalIntent !== "tap") throw new Error("tap lacked its synthetic intent");
}
if (!typingMode || typingChanges !== 1) throw new Error("tap did not preserve keyboard opening");
`)
	if err != nil {
		t.Fatalf("coarse tap behavior: %v", err)
	}
}

func TestBrowserTerminalLinksRequireDesktopModifierButOpenFromTouch(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	shouldActivate := browserFunctionSource(t, source, "shouldActivateTerminalLink")
	activate := browserFunctionSource(t, source, "activateTerminalLink")
	runtime := goja.New()
	_, err = runtime.RunString(`
const opened = [];
const navigator = {platform: "Linux x86_64"};
const location = {href: "https://mirror.example.test/session"};
function URL(value) {
  this.href = value;
  this.protocol = value.slice(0, value.indexOf(":") + 1);
}
const window = {
  open(url, target, features) { opened.push({url, target, features}); },
};
let selectionMode = false;
` + shouldActivate + `
}
` + activate + `
}
activateTerminalLink({isTrusted: true, ctrlKey: false, metaKey: false}, "https://example.test/plain");
if (opened.length !== 0) throw new Error("unmodified desktop click opened a terminal link");
activateTerminalLink({isTrusted: true, ctrlKey: true, metaKey: false}, "https://example.test/control");
if (opened.length !== 1 || opened[0].url !== "https://example.test/control") {
  throw new Error("control-click did not open a terminal link");
}
navigator.platform = "MacIntel";
activateTerminalLink({isTrusted: true, ctrlKey: true, metaKey: false}, "https://example.test/mac-control");
if (opened.length !== 2) throw new Error("control-click did not open a Mac terminal link");
activateTerminalLink({isTrusted: true, ctrlKey: false, metaKey: true}, "http://example.test/command");
if (opened.length !== 3) throw new Error("command-click did not open a terminal link");
activateTerminalLink({isTrusted: false, wrapTerminalIntent: "tap", ctrlKey: false, metaKey: false}, "https://example.test/touch");
if (opened.length !== 4 || opened[3].target !== "_blank" ||
    opened[3].features !== "noopener,noreferrer") {
  throw new Error("touch tap did not safely open a terminal link");
}
activateTerminalLink({isTrusted: false, wrapTerminalIntent: "selection"}, "https://example.test/selection");
activateTerminalLink({isTrusted: false}, "https://example.test/unmarked");
selectionMode = true;
activateTerminalLink({isTrusted: false, wrapTerminalIntent: "tap"}, "https://example.test/copy-mode");
activateTerminalLink({isTrusted: true, ctrlKey: true}, "https://example.test/copy-mode-control");
if (opened.length !== 4) throw new Error("selection gesture opened a terminal link");
selectionMode = false;
activateTerminalLink({isTrusted: false, wrapTerminalIntent: "tap"}, "javascript:alert(1)");
if (opened.length !== 4) throw new Error("unsafe terminal link protocol was opened");
`)
	if err != nil {
		t.Fatalf("terminal link activation behavior: %v", err)
	}
}

func TestBrowserCoarsePanAndPinchNeverForwardMouse(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	finish := browserFunctionSource(t, string(sourceBytes), "finishCoarsePointer")
	runtime := goja.New()
	_, err = runtime.RunString(`
let selectionMode = false;
let typingMode = false;
const viewerState = {current: {id: "terminal"}};
let mouseEvents = 0;
let typingChanges = 0;
function dispatchTerminalMouse() { mouseEvents += 1; }
function setTypingMode() { typingChanges += 1; }
` + finish + `
}
finishCoarsePointer({x: 41, y: 73, moved: true}, false);
finishCoarsePointer({x: 41, y: 73, moved: false}, true);
if (mouseEvents !== 0 || typingChanges !== 0) {
  throw new Error("pan or pinch emitted a terminal click");
}
`)
	if err != nil {
		t.Fatalf("coarse gesture behavior: %v", err)
	}
}

func TestBrowserSelectionGestureStagesXtermSelectionWithoutClipboardAccess(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	dispatch := browserFunctionSource(t, source, "dispatchTerminalMouse")
	begin := browserFunctionSource(t, source, "beginTerminalSelection")
	update := browserFunctionSource(t, source, "updateTerminalSelection")
	finish := browserFunctionSource(t, source, "finishTerminalSelection")
	runtime := goja.New()
	_, err = runtime.RunString(`
const dispatched = [];
function MouseEvent(type, options) {
  this.type = type;
  Object.assign(this, options);
}
const terminal = {
  element: {dispatchEvent(event) { dispatched.push(event); }},
  getSelection() { return "selected terminal text"; },
};
const navigator = {platform: "iPhone", userAgent: "iPhone Mobile Safari"};
const elements = {copy: {textContent: "Cancel copy"}};
let selectionMode = true;
function copyTerminalSelection() { throw new Error("drag accessed the clipboard"); }
` + dispatch + `
}
` + begin + `
}
` + update + `
}
` + finish + `
}
const pointer = {x: 20, y: 30};
beginTerminalSelection(pointer);
pointer.x = 80;
pointer.y = 90;
updateTerminalSelection(pointer);
finishTerminalSelection(pointer);
if (dispatched.length !== 3 || dispatched[0].type !== "mousedown" ||
    dispatched[1].type !== "mousemove" || dispatched[2].type !== "mouseup") {
  throw new Error("selection did not send a complete mouse drag");
}
for (const event of dispatched) {
  if (!event.shiftKey || event.altKey) throw new Error("iPhone touch did not force xterm selection");
  if (event.wrapTerminalIntent !== "selection") throw new Error("copy drag lacked its synthetic intent");
}
if (!selectionMode || elements.copy.textContent !== "Copy selected") {
  throw new Error("completed selection was not staged for an explicit copy action");
}
`)
	if err != nil {
		t.Fatalf("staged mobile selection behavior: %v", err)
	}
}

func TestBrowserSelectionModeTracksDesktopMousePointers(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	shouldTrack := browserFunctionSource(t, string(sourceBytes), "shouldTrackTerminalPointer")
	runtime := goja.New()
	_, err = runtime.RunString(`
let selectionMode = false;
function isCoarsePointerEvent(event) {
  return event.pointerType === "touch" || event.pointerType === "pen";
}
` + shouldTrack + `
}
if (shouldTrackTerminalPointer({pointerType: "mouse"})) {
  throw new Error("ordinary mouse interaction was intercepted outside copy mode");
}
if (!shouldTrackTerminalPointer({pointerType: "touch"})) {
  throw new Error("touch interaction was not tracked");
}
selectionMode = true;
if (!shouldTrackTerminalPointer({pointerType: "mouse"})) {
  throw new Error("copy mode did not track desktop mouse interaction");
}
`)
	if err != nil {
		t.Fatalf("selection pointer tracking behavior: %v", err)
	}
}

func TestBrowserWheelScrollsLocalTerminalWithTmuxMouseEnabled(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	if !strings.Contains(source, `terminal.attachCustomWheelEventHandler(handleTerminalWheel)`) {
		t.Fatal("browser terminal does not install its local wheel handler")
	}
	handleWheel := browserFunctionSource(t, source, "handleTerminalWheel")
	runtime := goja.New()
	_, err = runtime.RunString(`
const scrolls = [];
const inputs = [];
const screen = {getBoundingClientRect() { return {height: 240}; }};
const terminal = {
  rows: 24,
  buffer: {active: {type: "normal"}},
  modes: {applicationCursorKeysMode: false},
  element: {querySelector(selector) { return selector === ".xterm-screen" ? screen : null; }},
  options: {fontSize: 14},
  scrollLines(amount) { scrolls.push(amount); },
  input(value) { inputs.push(value); },
};
` + handleWheel + `
}
if (handleTerminalWheel({deltaY: 4, deltaMode: 0}) !== false) {
  throw new Error("pixel wheel event was forwarded to tmux");
}
handleTerminalWheel({deltaY: 4, deltaMode: 0});
handleTerminalWheel({deltaY: 4, deltaMode: 0});
handleTerminalWheel({deltaY: 25, deltaMode: 0});
handleTerminalWheel({deltaY: 3, deltaMode: 1});
handleTerminalWheel({deltaY: 1, deltaMode: 2});
handleTerminalWheel({deltaY: 0, deltaMode: 0});
if (scrolls.length !== 4 || scrolls[0] !== 1 || scrolls[1] !== 2 ||
    scrolls[2] !== 3 || scrolls[3] !== 23) {
  throw new Error("wheel events did not accumulate rendered-line local scrolling");
}
terminal.buffer.active.type = "alternate";
handleTerminalWheel({deltaY: -4, deltaMode: 0});
handleTerminalWheel({deltaY: -4, deltaMode: 0});
handleTerminalWheel({deltaY: -4, deltaMode: 0});
handleTerminalWheel({deltaY: 2, deltaMode: 1});
terminal.modes.applicationCursorKeysMode = true;
handleTerminalWheel({deltaY: -2, deltaMode: 1});
if (scrolls.length !== 4 || inputs.length !== 3 ||
    inputs[0] !== "\u001b[A" || inputs[1] !== "\u001b[B\u001b[B" ||
    inputs[2] !== "\u001bOA\u001bOA") {
  throw new Error("alternate-buffer wheel events did not preserve xterm cursor-key fallback");
}
`)
	if err != nil {
		t.Fatalf("browser terminal wheel behavior: %v", err)
	}
}

func TestBrowserCopyUsesClipboardAPIFromExplicitAction(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	copySynchronously := browserFunctionSource(t, source, "copyTextSynchronously")
	copySelection := browserFunctionSource(t, source, "copyTerminalSelection")
	runtime := goja.New()
	_, err = runtime.RunString(`
let clipboardValue = "";
let legacyCopies = 0;
let cleared = 0;
const document = {
  body: {appendChild() {}},
  createElement() {
    return {focus() {}, select() {}, setSelectionRange() {}, remove() {}, style: {}};
  },
  execCommand() { legacyCopies += 1; return true; },
};
const navigator = {
  clipboard: {
    writeText(value) {
      clipboardValue = value;
      return {then(resolve) { resolve(); return {catch() {}}; }};
    },
  },
};
const terminal = {
  getSelection() { return "selected terminal text"; },
  clearSelection() { cleared += 1; },
};
const elements = {copy: {textContent: "Copy selected"}};
let selectionMode = true;
function setSelectionMode(value) { selectionMode = value; }
function showCopyFeedback(value) { elements.copy.textContent = value; }
` + copySynchronously + `
}
` + copySelection + `
}
copyTerminalSelection();
if (clipboardValue !== "selected terminal text" || legacyCopies !== 0) {
  throw new Error("explicit copy did not prefer the modern Clipboard API");
}
if (selectionMode || cleared !== 1 || elements.copy.textContent !== "Copied") {
  throw new Error("explicit copy did not leave selection mode cleanly");
}
`)
	if err != nil {
		t.Fatalf("explicit browser copy behavior: %v", err)
	}
}

func TestBrowserCopyButtonCopiesStagedSelection(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	handleCopy := browserFunctionSource(t, source, "handleCopyAction")
	copySelection := browserFunctionSource(t, source, "copyTerminalSelection")
	runtime := goja.New()
	_, err = runtime.RunString(`
let clipboardValue = "";
let cleared = 0;
const viewerState = {current: {id: "terminal"}};
const navigator = {
  clipboard: {
    writeText(value) {
      clipboardValue = value;
      return {then(resolve) { resolve(); return {catch() {}}; }};
    },
  },
};
const terminal = {
  getSelection() { return "staged selection"; },
  clearSelection() { cleared += 1; },
};
const elements = {copy: {textContent: "Copy selected"}};
let selectionMode = true;
function copyTextSynchronously() { throw new Error("legacy copy path was used"); }
function setSelectionMode(value) { selectionMode = value; }
function showCopyFeedback(value) { elements.copy.textContent = value; }
function setTypingMode() {}
function clearViewportGestures() {}
` + copySelection + `
}
` + handleCopy + `
}
handleCopyAction();
if (clipboardValue !== "staged selection") {
  throw new Error("Copy button did not write the staged selection");
}
if (selectionMode || cleared !== 1 || elements.copy.textContent !== "Copied") {
  throw new Error("Copy button did not clean up the staged selection");
}
`)
	if err != nil {
		t.Fatalf("staged selection Copy button behavior: %v", err)
	}
}

func TestBrowserSelectionCopiesSynchronouslyWhenClipboardAPIIsUnavailable(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	copySynchronously := browserFunctionSource(t, source, "copyTextSynchronously")
	copySelection := browserFunctionSource(t, source, "copyTerminalSelection")
	runtime := goja.New()
	_, err = runtime.RunString(`
let appended = 0;
let removed = 0;
let selected = false;
let copiedValue = "";
const document = {
  body: {appendChild(element) { appended += 1; copiedValue = element.value; }},
  createElement(type) {
    if (type !== "textarea") throw new Error("copy fallback did not use a textarea");
    return {
      value: "",
      readOnly: false,
      style: {},
      focus() {},
      select() { selected = true; },
      setSelectionRange(start, end) {
        if (start !== 0 || end !== this.value.length) {
          throw new Error("copy fallback selected the wrong range");
        }
      },
      remove() { removed += 1; },
    };
  },
  execCommand(command) {
    if (command !== "copy" || !selected) return false;
    return true;
  },
};
const navigator = {platform: "iPhone"};
let cleared = 0;
const terminal = {
  getSelection() { return "selected terminal text"; },
  clearSelection() { cleared += 1; },
};
const elements = {copy: {textContent: "Cancel copy"}};
let selectionMode = true;
function setSelectionMode(value) { selectionMode = value; }
function showCopyFeedback(value) { elements.copy.textContent = value; }
` + copySynchronously + `
}
` + copySelection + `
}
copyTerminalSelection();
if (copiedValue !== "selected terminal text" || appended !== 1 || removed !== 1) {
  throw new Error("synchronous fallback did not copy and clean up the selected text");
}
if (selectionMode || cleared !== 1 || elements.copy.textContent !== "Copied") {
  throw new Error("synchronous copy did not leave selection mode cleanly");
}
`)
	if err != nil {
		t.Fatalf("synchronous mobile copy fallback: %v", err)
	}
}

func TestBrowserPasteSendsExactClipboardTextThroughXterm(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let pasted = "";
let feedback = "";
let menuOpen = true;
let controlSticky = true;
const viewerState = {current: {id: "terminal"}};
const connection = {authenticated: true};
const terminal = {
  paste(value) {
    if (controlSticky) throw new Error("paste reached xterm before Ctrl was cleared");
    pasted = value;
  },
};
const navigator = {
  clipboard: {
    readText() {
      return {
        then(resolve) {
          resolve("first line\nsecond line");
          return {catch() {}};
        },
      };
    },
  },
};
function setControlSticky(value) { controlSticky = value; }
function setUtilityMenuOpen(value) { menuOpen = value; }
function showPasteFeedback(value) { feedback = value; }
` + pasteClipboard + `
}
pasteClipboardText();
if (pasted !== "first line\nsecond line") {
  throw new Error("paste did not preserve the exact clipboard text");
}
if (controlSticky || menuOpen || feedback !== "Pasted") {
  throw new Error("successful paste did not clean up terminal controls");
}
`)
	if err != nil {
		t.Fatalf("browser clipboard paste behavior: %v", err)
	}
}

func TestBrowserPasteDoesNotReadClipboardWithoutAuthenticatedTerminal(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let reads = 0;
let pastes = 0;
let controlSticky = false;
const viewerState = {current: null};
const connection = {authenticated: false};
const terminal = {paste() { pastes += 1; }};
const navigator = {
  clipboard: {
    readText() {
      reads += 1;
      return {then(resolve) { resolve("secret"); }};
    },
  },
};
function setControlSticky() {}
function setUtilityMenuOpen() {}
function showPasteFeedback() {}
` + pasteClipboard + `
}
pasteClipboardText();
if (reads !== 0 || pastes !== 0) {
  throw new Error("paste read the clipboard without an authenticated terminal");
}
`)
	if err != nil {
		t.Fatalf("disconnected browser paste behavior: %v", err)
	}
}

func TestBrowserPasteCancelsWhenViewerChangesDuringClipboardRead(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let feedback = "";
let pastes = 0;
let controlSticky = false;
const originalViewer = {id: "first"};
const viewerState = {current: originalViewer};
let connection = {authenticated: true, id: "first"};
let resolveRead;
const terminal = {paste() { pastes += 1; }};
const navigator = {
  clipboard: {
    readText() {
      return {
        then(resolve) {
          resolveRead = resolve;
          return {catch() {}};
        },
      };
    },
  },
};
function setControlSticky(value) { controlSticky = value; }
function setUtilityMenuOpen() {}
function showPasteFeedback(value) { feedback = value; }
` + pasteClipboard + `
}
pasteClipboardText();
connection = {authenticated: true, id: "replacement"};
viewerState.current = {id: "replacement"};
resolveRead("pending clipboard text");
if (pastes !== 0 || feedback !== "Paste canceled") {
  throw new Error("stale clipboard result reached the replacement viewer");
}
`)
	if err != nil {
		t.Fatalf("stale browser clipboard behavior: %v", err)
	}
}

func TestBrowserPasteReportsUnavailableClipboardWithoutInput(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let feedback = "";
let pastes = 0;
let controlSticky = false;
const viewerState = {current: {id: "terminal"}};
const connection = {authenticated: true};
const terminal = {paste() { pastes += 1; }};
const navigator = {};
function setControlSticky() {}
function setUtilityMenuOpen() {}
function showPasteFeedback(value) { feedback = value; }
` + pasteClipboard + `
}
pasteClipboardText();
if (pastes !== 0 || feedback !== "Paste unavailable") {
  throw new Error("unavailable clipboard did not produce actionable paste feedback");
}
`)
	if err != nil {
		t.Fatalf("unavailable browser paste behavior: %v", err)
	}
}

func TestBrowserPasteReportsEmptyClipboardWithoutInput(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let feedback = "";
let pastes = 0;
let controlSticky = false;
const viewerState = {current: {id: "terminal"}};
const connection = {authenticated: true};
const terminal = {paste() { pastes += 1; }};
const navigator = {
  clipboard: {
    readText() {
      return {then(resolve) { resolve(""); return {catch() {}}; }};
    },
  },
};
function setControlSticky() {}
function setUtilityMenuOpen() {}
function showPasteFeedback(value) { feedback = value; }
` + pasteClipboard + `
}
pasteClipboardText();
if (pastes !== 0 || feedback !== "Clipboard empty") {
  throw new Error("empty clipboard was pasted or reported as successful");
}
`)
	if err != nil {
		t.Fatalf("empty browser clipboard behavior: %v", err)
	}
}

func TestBrowserPasteReportsDeniedClipboardWithoutInput(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	pasteClipboard := browserFunctionSource(t, string(sourceBytes), "pasteClipboardText")
	runtime := goja.New()
	_, err = runtime.RunString(`
let feedback = "";
let pastes = 0;
let controlSticky = false;
const viewerState = {current: {id: "terminal"}};
const connection = {authenticated: true};
const terminal = {paste() { pastes += 1; }};
const navigator = {
  clipboard: {
    readText() {
      return {
        then() {
          return {catch(reject) { reject(new Error("not allowed")); }};
        },
      };
    },
  },
};
function setControlSticky() {}
function setUtilityMenuOpen() {}
function showPasteFeedback(value) { feedback = value; }
` + pasteClipboard + `
}
pasteClipboardText();
if (pastes !== 0 || feedback !== "Paste denied") {
  throw new Error("denied clipboard read was pasted or lacked useful feedback");
}
`)
	if err != nil {
		t.Fatalf("denied browser clipboard behavior: %v", err)
	}
}

func TestBrowserPasteFeedbackReturnsButtonToCompactLabel(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	showFeedback := browserFunctionSource(t, string(sourceBytes), "showPasteFeedback")
	runtime := goja.New()
	_, err = runtime.RunString(`
let pasteFeedbackTimer = 0;
let clearedTimer = 0;
let pending;
const elements = {paste: {textContent: "Paste"}};
function clearTimeout(timer) { clearedTimer = timer; }
function setTimeout(callback, delay) {
  if (delay !== 1200) throw new Error("paste feedback used the wrong duration");
  pending = callback;
  return 9;
}
` + showFeedback + `
}
showPasteFeedback("Pasted");
if (elements.paste.textContent !== "Pasted" || pasteFeedbackTimer !== 9) {
  throw new Error("paste feedback was not shown");
}
showPasteFeedback("Paste denied");
if (clearedTimer !== 9 || elements.paste.textContent !== "Paste denied") {
  throw new Error("repeated paste feedback left a competing timer");
}
pending();
if (pasteFeedbackTimer !== 0 || elements.paste.textContent !== "Paste") {
  throw new Error("paste feedback did not restore the compact label");
}
`)
	if err != nil {
		t.Fatalf("paste button feedback behavior: %v", err)
	}
}

func TestBrowserSelectionUsesXtermMacModifierOnIPadDesktopMode(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, option := range []string{
		`macOptionClickForcesSelection: true`,
		`altClickMovesCursor: false`,
	} {
		if !strings.Contains(source, option) {
			t.Errorf("browser terminal config missing %q", option)
		}
	}
	dispatch := browserFunctionSource(t, source, "dispatchTerminalMouse")
	runtime := goja.New()
	_, err = runtime.RunString(`
let dispatched;
function MouseEvent(type, options) { Object.assign(this, options); }
const terminal = {element: {dispatchEvent(event) { dispatched = event; }}};
const navigator = {platform: "MacIntel", userAgent: "Safari"};
` + dispatch + `
}
dispatchTerminalMouse("mousedown", {x: 10, y: 20}, true);
if (!dispatched.altKey || dispatched.shiftKey) {
  throw new Error("Mac-platform touch did not use xterm's forced-selection modifier");
}
`)
	if err != nil {
		t.Fatalf("iPad desktop selection modifier: %v", err)
	}
}

func TestBrowserContractHandlesCloseAndLargeInputWithoutPoisoning(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`const MAX_FRAME_PAYLOAD = MAX_WIRE_MESSAGE - 17`,
		`case TAG.close:`,
		`function sendInput(data)`,
		`payload.subarray(offset, offset + MAX_FRAME_PAYLOAD)`,
		`viewerState.closing = true`,
		`"Terminal closed"`,
		`"Incompatible browser"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing close/input guard %q", want)
		}
	}
	for _, forbidden := range []string{"validateSessionList", "TAG.status", "TAG.revoked", "sendJSON(TAG.open"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("browser client retains selection protocol %q", forbidden)
		}
	}
}
