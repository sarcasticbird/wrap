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
