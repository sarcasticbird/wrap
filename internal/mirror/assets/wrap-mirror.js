import { Terminal } from "/assets/third_party/xterm/xterm.mjs";
import "/assets/wrap-mirror-state.js";
import "/assets/wrap-mirror-viewport.js";

const STORAGE_KEY = "wrap.mirror.v2.secret";
const MAX_WIRE_MESSAGE = 128 * 1024;
const MAX_FRAME_PAYLOAD = MAX_WIRE_MESSAGE - 17;
const TAG = Object.freeze({
  hello: 0x01,
  list: 0x02,
  status: 0x03,
  open: 0x04,
  close: 0x05,
  input: 0x06,
  output: 0x07,
  opened: 0x08,
  revoked: 0x09,
  shutdown: 0x0a,
  error: 0x0b,
});
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });

function decodeBase64URL(value) {
  if (!/^[A-Za-z0-9_-]{43}$/.test(value)) {
    throw new Error("invalid pairing key");
  }
  const padded = value.replaceAll("-", "+").replaceAll("_", "/") + "=";
  const binary = atob(padded);
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  if (bytes.length !== 32) {
    bytes.fill(0);
    throw new Error("invalid pairing key");
  }
  return bytes;
}

function fromHex(value) {
  return Uint8Array.from(value.match(/../g), (pair) => Number.parseInt(pair, 16));
}

function toHex(value) {
  return Array.from(value, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function concat(...arrays) {
  const length = arrays.reduce((total, value) => total + value.length, 0);
  const result = new Uint8Array(length);
  let offset = 0;
  for (const value of arrays) {
    result.set(value, offset);
    offset += value.length;
  }
  return result;
}

async function deriveDirectionalKey(secretBytes, serverNonce, clientNonce, info) {
  const material = await crypto.subtle.importKey("raw", secretBytes, "HKDF", false, ["deriveKey"]);
  return crypto.subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: concat(serverNonce, clientNonce),
      info: encoder.encode(info),
    },
    material,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

function counterNonce(counter) {
  const nonce = new Uint8Array(12);
  new DataView(nonce.buffer).setBigUint64(4, counter, false);
  return nonce;
}

async function encryptFrame(key, counter, tag, payload) {
  if (counter === 0xffffffffffffffffn) {
    throw new Error("encryption counter exhausted");
  }
  const plain = concat(Uint8Array.of(tag), payload);
  if (plain.length + 16 > MAX_WIRE_MESSAGE) {
    throw new Error("frame too large");
  }
  return new Uint8Array(await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: counterNonce(counter) },
    key,
    plain,
  ));
}

async function decryptFrame(key, counter, ciphertext) {
  if (counter === 0xffffffffffffffffn || ciphertext.length > MAX_WIRE_MESSAGE) {
    throw new Error("invalid encrypted frame");
  }
  const plain = new Uint8Array(await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: counterNonce(counter) },
    key,
    ciphertext,
  ));
  if (plain.length === 0) {
    throw new Error("encrypted frame has no tag");
  }
  return { tag: plain[0], payload: plain.subarray(1) };
}

async function cryptoSelfTest() {
  const secretBytes = fromHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f");
  const serverNonce = fromHex("a0a1a2a3a4a5a6a7a8a9aaabacadaeaf");
  const clientNonce = fromHex("b0b1b2b3b4b5b6b7b8b9babbbcbdbebf");
  const key = await deriveDirectionalKey(
    secretBytes,
    serverNonce,
    clientNonce,
    "wrap-mirror/v2/c2s",
  );
  const raw = new Uint8Array(await crypto.subtle.exportKey(
    "raw",
    await crypto.subtle.deriveKey(
      {
        name: "HKDF",
        hash: "SHA-256",
        salt: concat(serverNonce, clientNonce),
        info: encoder.encode("wrap-mirror/v2/s2c"),
      },
      await crypto.subtle.importKey("raw", secretBytes, "HKDF", false, ["deriveKey"]),
      { name: "AES-GCM", length: 256 },
      true,
      ["encrypt", "decrypt"],
    ),
  ));
  if (toHex(raw) !== "a7574bd01ad71ecf2d2339aa9a417029f860d957836d8883a2307599d9f51161") {
    throw new Error("key derivation self-test failed");
  }
  const hello = await encryptFrame(key, 0n, TAG.hello, encoder.encode('{"version":2}'));
  if (toHex(hello) !== "49ac75e8504742f9e9aef44d69fefb855a3694abe34934356b6b0b8d8ffb") {
    throw new Error("encryption self-test failed");
  }
  secretBytes.fill(0);
  serverNonce.fill(0);
  clientNonce.fill(0);
  raw.fill(0);
}

const fragment = new URLSearchParams(location.hash.slice(1));
const fragmentKey = fragment.get("k");
history.replaceState(null, "", location.pathname + location.search);
if (fragmentKey !== null) {
  try {
    decodeBase64URL(fragmentKey).fill(0);
    sessionStorage.setItem(STORAGE_KEY, fragmentKey);
  } catch {
    sessionStorage.removeItem(STORAGE_KEY);
  }
}

let secret = null;
let incompatibleBrowser = false;
try {
  secret = decodeBase64URL(sessionStorage.getItem(STORAGE_KEY) || "");
} catch {
  sessionStorage.removeItem(STORAGE_KEY);
}
if (secret) {
  try {
    if (!globalThis.isSecureContext || !crypto.subtle) {
      throw new Error("secure browser context required");
    }
    await cryptoSelfTest();
  } catch {
    secret.fill(0);
    secret = null;
    incompatibleBrowser = true;
  }
}

const elements = {
  connection: document.querySelector("#connection-state"),
  message: document.querySelector("#message-view"),
  messageTitle: document.querySelector("#message-title"),
  messageDetail: document.querySelector("#message-detail"),
  list: document.querySelector("#list-view"),
  sessionList: document.querySelector("#session-list"),
  sessionCount: document.querySelector("#session-count"),
  terminalView: document.querySelector("#terminal-view"),
  terminalTitle: document.querySelector("#terminal-title"),
  terminalViewport: document.querySelector("#terminal-viewport"),
  terminalLoading: document.querySelector("#terminal-loading"),
  terminalSpacer: document.querySelector("#terminal-spacer"),
  terminalSurface: document.querySelector("#terminal-surface"),
  terminal: document.querySelector("#terminal"),
  pinchScale: document.querySelector("#pinch-scale"),
  back: document.querySelector("#back-button"),
  close: document.querySelector("#close-button"),
  toolbar: document.querySelector("#toolbar"),
  keyboardToggle: document.querySelector("#keyboard-toggle"),
  fit: document.querySelector("#fit-button"),
};

const BASE_TERMINAL_FONT_SIZE = 14;
const MAX_VIEWPORT_MEASURE_ATTEMPTS = 60;
const terminal = new Terminal({
  convertEol: false,
  cursorBlink: true,
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
  fontSize: BASE_TERMINAL_FONT_SIZE,
  scrollback: 5000,
  theme: {
    background: "#080a09",
    foreground: "#edf2ed",
    cursor: "#b7f34a",
    selectionBackground: "#34402f",
  },
});

let connection = null;
const closeState = globalThis.WrapMirrorCloseState;
const viewerState = closeState.create();
const viewportReducer = globalThis.WrapMirrorViewportState;
const terminalViewportState = viewportReducer.create();
let reconnectAttempt = 0;
let reconnectTimer = 0;
let stopped = false;
let controlSticky = false;
let geometryAccepted = false;
let viewportMeasureFrame = 0;
let viewportMeasureAttempts = 0;
let pendingTerminalFit = false;
let typingMode = false;
const viewportPointers = new Map();
let pinchGesture = null;
let pinchPreview = null;
let pinchPreviewFrame = 0;
let pendingPinchInput = null;
let pinchPreviewTarget = null;
let pinchScaleTimer = 0;
let terminalMounted = false;

function ensureTerminalMounted() {
  if (terminalMounted) {
    return;
  }
  terminal.open(elements.terminal);
  terminalMounted = true;
  terminal.textarea?.setAttribute("inputmode", typingMode ? "text" : "none");
}

function cancelTerminalMeasurement() {
  if (viewportMeasureFrame) {
    cancelAnimationFrame(viewportMeasureFrame);
    viewportMeasureFrame = 0;
  }
}

function setTerminalDisplayPending(message) {
  elements.terminalLoading.textContent = message;
  elements.terminalView.classList.add("display-pending");
  elements.terminalView.classList.remove("display-error");
}

function revealTerminalDisplay() {
  elements.terminalView.classList.remove("display-pending", "display-error");
}

function showTerminalDisplayError() {
  setTerminalDisplayPending(
    "Terminal display unavailable — press Fit to retry or return to Terminals. " +
    "Code: terminal_display_unavailable",
  );
  elements.terminalView.classList.add("display-error");
}

function showOnly(view) {
  if (view !== "terminal") {
    setTypingMode(false);
    clearViewportGestures();
    cancelTerminalMeasurement();
    revealTerminalDisplay();
  }
  document.body.classList.toggle("terminal-active", view === "terminal");
  elements.message.classList.toggle("hidden", view !== "message");
  elements.list.classList.toggle("hidden", view !== "list");
  elements.terminalView.classList.toggle("hidden", view !== "terminal");
}

function showMessage(title, detail, status = "Offline") {
  elements.messageTitle.textContent = title;
  elements.messageDetail.textContent = detail;
  elements.connection.textContent = status;
  elements.connection.classList.remove("online");
  showOnly("message");
}

function setOnline() {
  elements.connection.textContent = "Encrypted";
  elements.connection.classList.add("online");
}

function resetTerminalViewport() {
  cancelTerminalMeasurement();
  viewportMeasureAttempts = 0;
  pendingTerminalFit = false;
  viewportReducer.reset(terminalViewportState);
  elements.terminalSpacer.removeAttribute("style");
  elements.terminalSurface.removeAttribute("style");
  elements.terminal.removeAttribute("style");
  elements.terminalViewport.scrollLeft = 0;
  elements.terminalViewport.scrollTop = 0;
  updateViewportControls();
}

function restoreBaseTerminalMetrics() {
  terminal.options.fontSize = BASE_TERMINAL_FONT_SIZE;
  terminal.refresh(0, terminal.rows - 1);
}

function resizeTerminalToHostGeometry(opened) {
  const probeRows = opened.rows === 2 ? 3 : opened.rows - 1;
  terminal.resize(opened.columns, probeRows);
  terminal.resize(opened.columns, opened.rows);
}

function applyTerminalBox(layout) {
  elements.terminalSurface.style.width = `${layout.width}px`;
  elements.terminalSurface.style.height = `${layout.height}px`;
  elements.terminal.style.width = `${layout.width}px`;
  elements.terminal.style.height = `${layout.height}px`;
  elements.terminalSpacer.style.width = `${layout.width}px`;
  elements.terminalSpacer.style.height = `${layout.height}px`;
}

function applyTerminalViewport() {
  const layout = viewportReducer.layout(terminalViewportState);
  if (!layout) {
    return;
  }
  terminal.options.fontSize = viewportReducer.fontSize(
    terminalViewportState,
    BASE_TERMINAL_FONT_SIZE,
  );
  applyTerminalBox(layout);
  terminal.refresh(0, terminal.rows - 1);
  updateViewportControls();
}

function updateViewportControls() {
  elements.fit.setAttribute("aria-pressed", String(terminalViewportState.mode === "fit"));
}

function hidePinchScale() {
  if (pinchScaleTimer) {
    clearTimeout(pinchScaleTimer);
    pinchScaleTimer = 0;
  }
  elements.pinchScale.classList.add("hidden");
}

function showPinchScale(scale) {
  if (pinchScaleTimer) {
    clearTimeout(pinchScaleTimer);
    pinchScaleTimer = 0;
  }
  elements.pinchScale.textContent = `${Math.round(scale * 100)}%`;
  elements.pinchScale.classList.remove("hidden");
}

function finishPinchScale() {
  if (pinchScaleTimer) {
    clearTimeout(pinchScaleTimer);
  }
  pinchScaleTimer = setTimeout(hidePinchScale, 500);
}

function clearPinchPreviewStyles() {
  pinchPreviewTarget?.style.removeProperty("transform");
  pinchPreviewTarget?.style.removeProperty("transform-origin");
  pinchPreviewTarget?.style.removeProperty("will-change");
  pinchPreviewTarget = null;
}

function restoreCommittedPinchPreview() {
  pinchPreview = null;
  clearPinchPreviewStyles();
  const layout = viewportReducer.layout(terminalViewportState);
  if (layout) {
    applyTerminalBox(layout);
  }
  if (pinchGesture) {
    elements.terminalViewport.scrollLeft = pinchGesture.scrollLeft;
    elements.terminalViewport.scrollTop = pinchGesture.scrollTop;
    showPinchScale(terminalViewportState.scale);
  }
}

function discardPinchPreview() {
  if (pinchPreviewFrame) {
    cancelAnimationFrame(pinchPreviewFrame);
    pinchPreviewFrame = 0;
  }
  pendingPinchInput = null;
  restoreCommittedPinchPreview();
}

function applyPinchPreview(preview) {
  if (!pinchGesture) {
    return;
  }
  if (!preview) {
    restoreCommittedPinchPreview();
    return;
  }
  const terminalScreen = terminal.element?.querySelector(".xterm-screen");
  if (!terminalScreen) {
    return;
  }
  pinchPreview = preview;
  pinchPreviewTarget = terminalScreen;
  const ratio = preview.scale / pinchGesture.scale;
  terminalScreen.style.transformOrigin = "top left";
  terminalScreen.style.willChange = "transform";
  terminalScreen.style.transform = `scale(${ratio})`;
  applyTerminalBox(preview);
  elements.terminalViewport.scrollLeft = preview.scrollLeft;
  elements.terminalViewport.scrollTop = preview.scrollTop;
  showPinchScale(preview.scale);
}

function calculatePinchPreview(input) {
  if (!pinchGesture || !input) {
    return null;
  }
  return viewportReducer.previewPinch(terminalViewportState, pinchGesture, input);
}

function schedulePinchPreview(input) {
  pendingPinchInput = input;
  if (pinchPreviewFrame) {
    return;
  }
  pinchPreviewFrame = requestAnimationFrame(() => {
    pinchPreviewFrame = 0;
    const latestInput = pendingPinchInput;
    pendingPinchInput = null;
    const preview = calculatePinchPreview(latestInput);
    applyPinchPreview(preview);
  });
}

function flushPinchPreview() {
  if (pinchPreviewFrame) {
    cancelAnimationFrame(pinchPreviewFrame);
    pinchPreviewFrame = 0;
  }
  const latestInput = pendingPinchInput;
  pendingPinchInput = null;
  if (latestInput) {
    const preview = calculatePinchPreview(latestInput);
    applyPinchPreview(preview);
  }
}

function commitPinchPreview() {
  flushPinchPreview();
  if (!pinchPreview) {
    discardPinchPreview();
    return;
  }
  const scrollLeft = pinchPreview.scrollLeft;
  const scrollTop = pinchPreview.scrollTop;
  const committed = viewportReducer.commitPinch(terminalViewportState, pinchPreview);
  if (!committed) {
    discardPinchPreview();
    return;
  }
  pinchPreview = null;
  clearPinchPreviewStyles();
  applyTerminalViewport();
  elements.terminalViewport.scrollLeft = scrollLeft;
  elements.terminalViewport.scrollTop = scrollTop;
}

function clearViewportGestures() {
  discardPinchPreview();
  viewportPointers.clear();
  pinchGesture = null;
  hidePinchScale();
}

function fitTerminalViewport() {
  clearViewportGestures();
  if (!viewportReducer.layout(terminalViewportState)) {
    pendingTerminalFit = true;
    if (geometryAccepted) {
      viewportMeasureAttempts = 0;
      setTerminalDisplayPending("Opening terminal…");
      scheduleTerminalMeasurement();
    }
    return;
  }
  pendingTerminalFit = false;
  viewportReducer.fit(terminalViewportState);
  applyTerminalViewport();
}

function isCoarsePointer() {
  return Boolean(globalThis.matchMedia?.("(any-pointer: coarse)").matches);
}

function isCoarsePointerEvent(event) {
  if (event.pointerType) {
    return event.pointerType === "touch" || event.pointerType === "pen";
  }
  return isCoarsePointer();
}

function setTypingMode(value) {
  typingMode = Boolean(value);
  elements.terminalView.classList.toggle("typing", typingMode);
  elements.keyboardToggle.textContent = typingMode ? "Hide keyboard" : "Keyboard";
  elements.keyboardToggle.setAttribute("aria-pressed", String(typingMode));
  terminal.textarea?.setAttribute("inputmode", typingMode ? "text" : "none");
  if (!terminalMounted) {
    return;
  }
  if (typingMode) {
    terminal.blur();
    terminal.focus();
  } else {
    terminal.blur();
    terminal.textarea?.blur();
  }
}

function updateVisualViewport() {
  const height = globalThis.visualViewport?.height || globalThis.innerHeight;
  const width = globalThis.visualViewport?.width || globalThis.innerWidth;
  const offsetTop = globalThis.visualViewport?.offsetTop || 0;
  const offsetLeft = globalThis.visualViewport?.offsetLeft || 0;
  document.documentElement.style.setProperty("--mirror-viewport-height", `${height}px`);
  document.documentElement.style.setProperty("--mirror-viewport-width", `${width}px`);
  document.documentElement.style.setProperty("--mirror-viewport-top", `${offsetTop}px`);
  document.documentElement.style.setProperty("--mirror-viewport-left", `${offsetLeft}px`);
  elements.terminalView.classList.toggle("compact", height < 360);
  refitTerminalViewport();
}

function scheduleTerminalMeasurement() {
  cancelTerminalMeasurement();
  viewportMeasureFrame = requestAnimationFrame(() => {
    viewportMeasureFrame = 0;
    if (
      !geometryAccepted ||
      elements.terminalView.classList.contains("hidden")
    ) {
      return;
    }
    const screen = terminal.element?.querySelector(".xterm-screen");
    const rectangle = screen?.getBoundingClientRect();
    if (
      !rectangle ||
      rectangle.width <= 0 ||
      rectangle.height <= 0 ||
      elements.terminalViewport.clientWidth <= 0
    ) {
      viewportMeasureAttempts += 1;
      if (viewportMeasureAttempts >= MAX_VIEWPORT_MEASURE_ATTEMPTS) {
        showTerminalDisplayError();
        return;
      }
      scheduleTerminalMeasurement();
      return;
    }
    const style = getComputedStyle(elements.terminal);
    const scrollbar = terminal.element?.querySelector(".xterm-viewport");
    const scrollbarWidth = scrollbar
      ? Math.max(0, scrollbar.offsetWidth - scrollbar.clientWidth)
      : 0;
    const horizontalPadding = (Number.parseFloat(style.paddingLeft) || 0) +
      (Number.parseFloat(style.paddingRight) || 0);
    const verticalPadding = (Number.parseFloat(style.paddingTop) || 0) +
      (Number.parseFloat(style.paddingBottom) || 0);
    const surface = {
      width: Math.ceil(rectangle.width),
      height: Math.ceil(rectangle.height),
      fixedWidth: Math.ceil(horizontalPadding + scrollbarWidth),
      fixedHeight: Math.ceil(verticalPadding),
      fixedLeft: Number.parseFloat(style.paddingLeft) || 0,
      fixedTop: Number.parseFloat(style.paddingTop) || 0,
    };
    viewportReducer.measure(
      terminalViewportState,
      surface,
      elements.terminalViewport.clientWidth,
    );
    if (pendingTerminalFit) {
      pendingTerminalFit = false;
      viewportReducer.fit(terminalViewportState);
    } else {
      viewportReducer.readable(terminalViewportState);
    }
    applyTerminalViewport();
    revealTerminalDisplay();
  });
}

function refitTerminalViewport() {
  if (pinchGesture) {
    clearViewportGestures();
  }
  const viewportWidth = elements.terminalViewport.clientWidth;
  if (
    elements.terminalView.classList.contains("hidden") ||
    viewportWidth <= 0 ||
    !viewportReducer.layout(terminalViewportState)
  ) {
    return;
  }
  viewportReducer.resize(
    terminalViewportState,
    viewportWidth,
  );
  applyTerminalViewport();
}

function renderSessions(nextSessions) {
  viewerState.sessions = nextSessions;
  const sessions = viewerState.sessions;
  elements.sessionList.replaceChildren();
  elements.sessionCount.textContent = String(sessions.length);
  if (sessions.length === 0) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "The host is not sharing any terminals.";
    elements.sessionList.append(empty);
  }
  for (const session of sessions) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "session";
    const name = document.createElement("span");
    name.className = "session-name";
    name.textContent = session.name;
    const meta = document.createElement("span");
    meta.className = "session-meta";
    meta.textContent = session.kind || "terminal";
    const badges = document.createElement("span");
    badges.className = "badges";
    if (session.bell) {
      badges.append(makeBadge("Bell"));
    }
    if (session.activity) {
      badges.append(makeBadge("New"));
    }
    button.append(name, meta, badges);
    button.addEventListener("click", () => openSession(session));
    elements.sessionList.append(button);
  }
  viewerState.current = null;
  geometryAccepted = false;
  resetTerminalViewport();
  setOnline();
  showOnly("list");
}

const viewerEffects = Object.freeze({
  tags: TAG,
  parseJSON,
  validateSessionList,
  render: (nextSessions) => renderSessions(nextSessions),
  ended: () => {
    geometryAccepted = false;
    resetTerminalViewport();
    showMessage("Terminal ended", "The host stopped sharing that terminal.", "Encrypted");
    setTimeout(() => renderSessions(viewerState.sessions), 700);
  },
  error: (problem, target) => {
    geometryAccepted = false;
    resetTerminalViewport();
    if (problem.retry === false) {
      stopped = true;
    }
    showMessage("Terminal unavailable", String(problem.message || "The host rejected the operation."));
    if (problem.retry === false) {
      target.socket.close();
    } else {
      setTimeout(() => renderSessions(viewerState.sessions), 700);
    }
  },
});

function makeBadge(label) {
  const badge = document.createElement("span");
  badge.className = "badge";
  badge.textContent = label;
  return badge;
}

function parseJSON(payload) {
  return JSON.parse(decoder.decode(payload));
}

function validateSessionList(value) {
  if (!value || !Array.isArray(value.sessions)) {
    throw new Error("invalid session list");
  }
  for (const session of value.sessions) {
    if (
      !session ||
      typeof session.id !== "string" ||
      typeof session.generation !== "string" ||
      typeof session.name !== "string" ||
      typeof session.kind !== "string" ||
      typeof session.bell !== "boolean" ||
      typeof session.activity !== "boolean"
    ) {
      throw new Error("invalid session");
    }
  }
  return value.sessions;
}

function validateOpened(value) {
  if (
    !value ||
    typeof value.id !== "string" ||
    typeof value.generation !== "string" ||
    !Number.isInteger(value.columns) ||
    value.columns < 2 ||
    value.columns > 500 ||
    !Number.isInteger(value.rows) ||
    value.rows < 2 ||
    value.rows > 300
  ) {
    throw new Error("invalid opened terminal");
  }
  return value;
}

function focusTerminalForPhysicalKeyboard() {
  if (
    viewerState.current &&
    globalThis.matchMedia?.("(pointer: fine)").matches
  ) {
    setTypingMode(true);
  }
}

function openSession(session) {
  if (!connection?.authenticated || !closeState.open(viewerState, session)) {
    return;
  }
  geometryAccepted = false;
  resetTerminalViewport();
  setTypingMode(false);
  elements.terminalTitle.textContent = session.name;
  setTerminalDisplayPending("Opening terminal…");
  showOnly("terminal");
  try {
    ensureTerminalMounted();
    terminal.reset();
    restoreBaseTerminalMetrics();
  } catch {
    closeState.reset(viewerState);
    showMessage(
      "Terminal display unavailable",
      "Reload this page to retry. Code: terminal_display_unavailable",
      "Encrypted",
    );
    setTimeout(() => renderSessions(viewerState.sessions), 1200);
    return;
  }
  sendJSON(TAG.open, {
    id: session.id,
    generation: session.generation,
  });
}

function closeSession() {
  if (connection?.authenticated && closeState.beginClose(viewerState)) {
    geometryAccepted = false;
    cancelTerminalMeasurement();
    queueFrame(TAG.close, new Uint8Array());
    showMessage("Closing terminal…", "Waiting for the encrypted host acknowledgement.", "Encrypted");
  }
}

function sendJSON(tag, value) {
  queueFrame(tag, encoder.encode(JSON.stringify(value)));
}

function queueFrame(tag, payload) {
  const target = connection;
  if (!target || target.poisoned || target.socket.readyState !== WebSocket.OPEN) {
    return;
  }
  target.sendChain = target.sendChain.then(async () => {
    if (target.poisoned) {
      return;
    }
    const encrypted = await encryptFrame(target.sendKey, target.sendCounter, tag, payload);
    target.sendCounter += 1n;
    target.socket.send(encrypted);
  }).catch(() => poison(target, false));
}

async function receiveMessage(target, data) {
  if (target.poisoned || !(data instanceof ArrayBuffer)) {
    throw new Error("invalid socket message");
  }
  const bytes = new Uint8Array(data);
  if (!target.sendKey) {
    if (bytes.length !== 16) {
      throw new Error("invalid server nonce");
    }
    const serverNonce = bytes.slice();
    const clientNonce = crypto.getRandomValues(new Uint8Array(16));
    target.sendKey = await deriveDirectionalKey(
      secret,
      serverNonce,
      clientNonce,
      "wrap-mirror/v2/c2s",
    );
    target.receiveKey = await deriveDirectionalKey(
      secret,
      serverNonce,
      clientNonce,
      "wrap-mirror/v2/s2c",
    );
    target.socket.send(clientNonce);
    await target.sendChain;
    queueFrame(TAG.hello, encoder.encode('{"version":2}'));
    serverNonce.fill(0);
    clientNonce.fill(0);
    return;
  }
  const frame = await decryptFrame(target.receiveKey, target.receiveCounter, bytes);
  target.receiveCounter += 1n;
  if (closeState.receiveMessage(viewerState, target, frame, viewerEffects)) {
    return;
  }
  switch (frame.tag) {
    case TAG.list:
      if (target.authenticated) {
        throw new Error("duplicate initial list");
      }
      target.authenticated = true;
      reconnectAttempt = 0;
      renderSessions(validateSessionList(parseJSON(frame.payload)));
      break;
    case TAG.opened: {
      if (!target.authenticated || geometryAccepted) {
        throw new Error("unexpected opened terminal");
      }
      const opened = validateOpened(parseJSON(frame.payload));
      const requested = viewerState.current || viewerState.closing;
      if (
        !requested ||
        opened.id !== requested.id ||
        opened.generation !== requested.generation
      ) {
        throw new Error("opened terminal identity mismatch");
      }
      if (viewerState.closing) {
        geometryAccepted = false;
        cancelTerminalMeasurement();
        break;
      }
      resizeTerminalToHostGeometry(opened);
      viewportReducer.open(terminalViewportState, opened);
      geometryAccepted = true;
      scheduleTerminalMeasurement();
      focusTerminalForPhysicalKeyboard();
      break;
    }
    case TAG.output:
      if (!target.authenticated) {
        throw new Error("output without open terminal");
      }
      if (viewerState.closing) {
        break;
      }
      if (!viewerState.current || !geometryAccepted) {
        throw new Error("output without open terminal");
      }
      terminal.write(frame.payload);
      break;
    case TAG.shutdown: {
      const shutdown = parseJSON(frame.payload);
      if (typeof shutdown.retry !== "boolean") {
        throw new Error("invalid shutdown");
      }
      if (!shutdown.retry) {
        stopped = true;
        sessionStorage.removeItem(STORAGE_KEY);
        secret?.fill(0);
        secret = null;
      }
      showMessage(
        shutdown.retry ? "Connection interrupted" : "Pairing ended",
        shutdown.retry ? "The tunnel stopped. Reconnecting if it returns…" : "Scan the new QR code on the host.",
      );
      target.socket.close();
      break;
    }
    default:
      throw new Error("unexpected server frame");
  }
}

function poison(target, retry) {
  if (target.poisoned) {
    return;
  }
  target.poisoned = true;
  if (!retry) {
    stopped = true;
  }
  target.socket.close();
}

function scheduleReconnect() {
  if (stopped || reconnectTimer) {
    return;
  }
  const base = Math.min(5000, 250 * (2 ** reconnectAttempt));
  reconnectAttempt += 1;
  const jittered = Math.round(base * (0.8 + Math.random() * 0.4));
  elements.connection.textContent = "Reconnecting";
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = 0;
    connect();
  }, jittered);
}

function connect() {
  if (!secret || stopped) {
    return;
  }
  showMessage("Connecting…", "Authenticating the encrypted terminal channel.", "Connecting");
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${location.host}/ws`);
  socket.binaryType = "arraybuffer";
  const target = {
    socket,
    authenticated: false,
    poisoned: false,
    sendKey: null,
    receiveKey: null,
    sendCounter: 0n,
    receiveCounter: 0n,
    sendChain: Promise.resolve(),
    receiveChain: Promise.resolve(),
  };
  connection = target;
  socket.addEventListener("message", (event) => {
    target.receiveChain = target.receiveChain
      .then(() => receiveMessage(target, event.data))
      .catch(() => poison(target, false));
  });
  socket.addEventListener("close", (event) => {
    if (connection !== target) {
      return;
    }
    connection = null;
    closeState.reset(viewerState);
    geometryAccepted = false;
    resetTerminalViewport();
    if (event.code === 1008) {
      stopped = true;
      sessionStorage.removeItem(STORAGE_KEY);
      secret?.fill(0);
      secret = null;
      showMessage("Pairing rejected", "Scan the current QR code shown by wrap.");
      return;
    }
    if (!stopped) {
      showMessage("Connection lost", "The terminal channel closed. Reconnecting…", "Reconnecting");
      scheduleReconnect();
    }
  });
  socket.addEventListener("error", () => {
    socket.close();
  });
}

function applyControl(data) {
  if (!controlSticky || data.length !== 1) {
    return data;
  }
  const code = data.toUpperCase().charCodeAt(0);
  setControlSticky(false);
  if (code >= 64 && code <= 95) {
    return String.fromCharCode(code & 31);
  }
  return data;
}

function setControlSticky(value) {
  controlSticky = value;
  const button = elements.toolbar.querySelector('[data-key="control"]');
  button.setAttribute("aria-pressed", String(value));
}

function sendInput(data) {
  const payload = encoder.encode(applyControl(data));
  for (let offset = 0; offset < payload.length; offset += MAX_FRAME_PAYLOAD) {
    queueFrame(
      TAG.input,
      payload.subarray(offset, offset + MAX_FRAME_PAYLOAD),
    );
  }
}

const toolbarData = Object.freeze({
  enter: "\r",
  escape: "\u001b",
  tab: "\t",
  "shift-tab": "\u001b[Z",
  up: "\u001b[A",
  down: "\u001b[B",
  right: "\u001b[C",
  left: "\u001b[D",
  "ctrl-c": "\u0003",
  "ctrl-d": "\u0004",
  "ctrl-l": "\u000c",
  "ctrl-z": "\u001a",
});

terminal.onData((data) => {
  if (viewerState.current && connection?.authenticated) {
    sendInput(data);
  }
});
elements.toolbar.addEventListener("pointerdown", (event) => event.preventDefault());
elements.toolbar.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-key]");
  if (!button || !viewerState.current) {
    return;
  }
  const key = button.dataset.key;
  if (key === "control") {
    setControlSticky(!controlSticky);
    return;
  }
  const data = toolbarData[key];
  if (data) {
    sendInput(data);
    terminal.focus();
  }
});
elements.keyboardToggle.addEventListener("pointerdown", (event) => event.preventDefault());
elements.keyboardToggle.addEventListener("click", () => {
  if (viewerState.current) {
    setTypingMode(!typingMode);
  }
});
elements.fit.addEventListener("pointerdown", (event) => event.preventDefault());
elements.fit.addEventListener("click", fitTerminalViewport);

function currentPinchInput() {
  if (viewportPointers.size !== 2) {
    return null;
  }
  const [first, second] = [...viewportPointers.values()];
  const rectangle = elements.terminalViewport.getBoundingClientRect();
  return {
    distance: Math.hypot(second.x - first.x, second.y - first.y),
    midpointX: (first.x + second.x) / 2 - rectangle.left,
    midpointY: (first.y + second.y) / 2 - rectangle.top,
  };
}

function beginPinchGesture() {
  const input = currentPinchInput();
  if (!input) {
    return;
  }
  pinchGesture = viewportReducer.beginPinch(terminalViewportState, {
    ...input,
    scrollLeft: elements.terminalViewport.scrollLeft,
    scrollTop: elements.terminalViewport.scrollTop,
  });
  if (pinchGesture) {
    for (const pointer of viewportPointers.values()) {
      pointer.moved = true;
    }
    showPinchScale(terminalViewportState.scale);
  }
}

elements.terminalViewport.addEventListener("pointerdown", (event) => {
  if (!isCoarsePointerEvent(event) || viewportPointers.size >= 2) {
    return;
  }
  if (event.cancelable) {
    event.preventDefault();
  }
  viewportPointers.set(event.pointerId, {
    x: event.clientX,
    y: event.clientY,
    startX: event.clientX,
    startY: event.clientY,
    scrollLeft: elements.terminalViewport.scrollLeft,
    scrollTop: elements.terminalViewport.scrollTop,
    scrollLineOffset: 0,
    scrollTargetViewportY: terminal.buffer.active.viewportY,
    moved: false,
  });
  if (viewportPointers.size === 2) {
    beginPinchGesture();
  }
});
elements.terminalViewport.addEventListener("pointermove", (event) => {
  const pointer = viewportPointers.get(event.pointerId);
  if (!pointer) {
    return;
  }
  pointer.x = event.clientX;
  pointer.y = event.clientY;
  if (Math.hypot(pointer.x - pointer.startX, pointer.y - pointer.startY) > 10) {
    pointer.moved = true;
  }
  if (viewportPointers.size === 1) {
    if (event.cancelable) {
      event.preventDefault();
    }
    if (pointer.moved) {
      elements.terminalViewport.scrollLeft =
        pointer.scrollLeft - (pointer.x - pointer.startX);
      const vertical = viewportReducer.panVertical(terminalViewportState, {
        scrollTop: pointer.scrollTop,
        scrollHeight: elements.terminalViewport.scrollHeight,
        clientHeight: elements.terminalViewport.clientHeight,
        deltaY: pointer.y - pointer.startY,
      });
      if (vertical) {
        elements.terminalViewport.scrollTop = vertical.scrollTop;
        const lineDelta = vertical.lineOffset - pointer.scrollLineOffset;
        if (lineDelta !== 0) {
          const activeBuffer = terminal.buffer.active;
          const tracked = viewportReducer.trackLineOffset(
            pointer.scrollLineOffset,
            pointer.scrollTargetViewportY,
            activeBuffer.baseY,
            lineDelta,
          );
          terminal.scrollLines(lineDelta);
          pointer.scrollLineOffset = tracked.lineOffset;
          pointer.scrollTargetViewportY = tracked.viewportY;
        }
      }
    }
    return;
  }
  if (viewportPointers.size !== 2) {
    return;
  }
  if (!pinchGesture) {
    beginPinchGesture();
  }
  const input = currentPinchInput();
  if (!pinchGesture || !input) {
    return;
  }
  if (event.cancelable) {
    event.preventDefault();
  }
  for (const activePointer of viewportPointers.values()) {
    activePointer.moved = true;
  }
  schedulePinchPreview(input);
});
elements.terminalViewport.addEventListener("pointerup", (event) => {
  const pointer = viewportPointers.get(event.pointerId);
  if (!pointer) {
    return;
  }
  const wasPinching = Boolean(pinchGesture) || viewportPointers.size > 1;
  viewportPointers.delete(event.pointerId);
  if (wasPinching) {
    commitPinchPreview();
    pinchGesture = null;
    for (const activePointer of viewportPointers.values()) {
      activePointer.startX = activePointer.x;
      activePointer.startY = activePointer.y;
      activePointer.scrollLeft = elements.terminalViewport.scrollLeft;
      activePointer.scrollTop = elements.terminalViewport.scrollTop;
      activePointer.scrollLineOffset = 0;
      activePointer.scrollTargetViewportY = terminal.buffer.active.viewportY;
      activePointer.moved = true;
    }
    finishPinchScale();
  }
  const enterTyping = !wasPinching && !pointer.moved && !typingMode && viewerState.current;
  if (enterTyping) {
    setTypingMode(true);
  }
});
elements.terminalViewport.addEventListener("pointercancel", (event) => {
  if (!viewportPointers.has(event.pointerId)) {
    return;
  }
  const wasPinching = Boolean(pinchGesture) || viewportPointers.size > 1;
  viewportPointers.delete(event.pointerId);
  if (wasPinching) {
    commitPinchPreview();
    pinchGesture = null;
    for (const activePointer of viewportPointers.values()) {
      activePointer.startX = activePointer.x;
      activePointer.startY = activePointer.y;
      activePointer.scrollLeft = elements.terminalViewport.scrollLeft;
      activePointer.scrollTop = elements.terminalViewport.scrollTop;
      activePointer.scrollLineOffset = 0;
      activePointer.scrollTargetViewportY = terminal.buffer.active.viewportY;
      activePointer.moved = true;
    }
    finishPinchScale();
  }
});
elements.back.addEventListener("click", closeSession);
elements.close.addEventListener("click", closeSession);
window.addEventListener("resize", updateVisualViewport);
globalThis.visualViewport?.addEventListener("resize", updateVisualViewport);
globalThis.visualViewport?.addEventListener("scroll", updateVisualViewport);
setTypingMode(false);
updateVisualViewport();

if (!secret) {
  if (incompatibleBrowser) {
    showMessage(
      "Incompatible browser",
      "A secure browser context with working WebCrypto support is required.",
    );
  } else {
    showMessage(
      "Pairing key missing",
      "Scan the QR code shown by wrap. This page will not connect without its URL fragment.",
    );
  }
} else {
  connect();
}
