import { Terminal } from "/assets/vendor/xterm/xterm.mjs";
import { FitAddon } from "/assets/vendor/xterm/addon-fit.mjs";

const STORAGE_KEY = "wrap.mirror.v1.secret";
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
  resize: 0x08,
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
    "wrap-mirror/v1/c2s",
  );
  const raw = new Uint8Array(await crypto.subtle.exportKey(
    "raw",
    await crypto.subtle.deriveKey(
      {
        name: "HKDF",
        hash: "SHA-256",
        salt: concat(serverNonce, clientNonce),
        info: encoder.encode("wrap-mirror/v1/s2c"),
      },
      await crypto.subtle.importKey("raw", secretBytes, "HKDF", false, ["deriveKey"]),
      { name: "AES-GCM", length: 256 },
      true,
      ["encrypt", "decrypt"],
    ),
  ));
  if (toHex(raw) !== "7d0a505e5c8410cd4e19fa87368ef82168f2533c4757b57c56a1d6ae5b3b3120") {
    throw new Error("key derivation self-test failed");
  }
  const hello = await encryptFrame(key, 0n, TAG.hello, encoder.encode('{"version":1}'));
  if (toHex(hello) !== "b93c018983645fd740b104f3de1f1f2b17c8b24b171062cc1c7845c4f47c") {
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
  terminal: document.querySelector("#terminal"),
  back: document.querySelector("#back-button"),
  close: document.querySelector("#close-button"),
  toolbar: document.querySelector("#toolbar"),
};

const terminal = new Terminal({
  convertEol: false,
  cursorBlink: true,
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
  fontSize: 14,
  scrollback: 5000,
  theme: {
    background: "#080a09",
    foreground: "#edf2ed",
    cursor: "#b7f34a",
    selectionBackground: "#34402f",
  },
});
const fitAddon = new FitAddon();
terminal.loadAddon(fitAddon);
terminal.open(elements.terminal);

let connection = null;
let sessions = [];
let current = null;
let closing = null;
let reconnectAttempt = 0;
let reconnectTimer = 0;
let stopped = false;
let controlSticky = false;
let resizeTimer = 0;

function showOnly(view) {
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

function renderSessions(nextSessions) {
  sessions = nextSessions;
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
  current = null;
  setOnline();
  showOnly("list");
}

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

function dimensions() {
  fitAddon.fit();
  return {
    columns: Math.max(2, Math.min(500, terminal.cols)),
    rows: Math.max(2, Math.min(300, terminal.rows)),
  };
}

function openSession(session) {
  if (!connection?.authenticated || closing) {
    return;
  }
  terminal.reset();
  current = session;
  elements.terminalTitle.textContent = session.name;
  showOnly("terminal");
  requestAnimationFrame(() => {
    const size = dimensions();
    sendJSON(TAG.open, {
      id: session.id,
      generation: session.generation,
      ...size,
    });
    terminal.focus();
  });
}

function closeSession() {
  if (current && connection?.authenticated) {
    closing = current;
    queueFrame(TAG.close, new Uint8Array());
    current = null;
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
      "wrap-mirror/v1/c2s",
    );
    target.receiveKey = await deriveDirectionalKey(
      secret,
      serverNonce,
      clientNonce,
      "wrap-mirror/v1/s2c",
    );
    target.socket.send(clientNonce);
    await target.sendChain;
    queueFrame(TAG.hello, encoder.encode('{"version":1}'));
    serverNonce.fill(0);
    clientNonce.fill(0);
    return;
  }
  const frame = await decryptFrame(target.receiveKey, target.receiveCounter, bytes);
  target.receiveCounter += 1n;
  switch (frame.tag) {
    case TAG.list:
      if (target.authenticated) {
        throw new Error("duplicate initial list");
      }
      target.authenticated = true;
      reconnectAttempt = 0;
      renderSessions(validateSessionList(parseJSON(frame.payload)));
      break;
    case TAG.status:
      if (!target.authenticated) {
        throw new Error("status before authentication");
      }
      {
        const nextSessions = validateSessionList(parseJSON(frame.payload));
        if (closing) {
          closing = null;
          renderSessions(nextSessions);
        } else if (current) {
          sessions = nextSessions;
          const stillMirrored = sessions.some(
            (session) => session.id === current.id && session.generation === current.generation,
          );
          if (!stillMirrored) {
            current = null;
            showMessage("Terminal ended", "The host stopped sharing that terminal.", "Encrypted");
            setTimeout(() => renderSessions(sessions), 700);
          }
        } else {
          renderSessions(nextSessions);
        }
      }
      break;
    case TAG.output:
      if (!target.authenticated) {
        throw new Error("output without open terminal");
      }
      if (closing) {
        break;
      }
      if (!current) {
        throw new Error("output without open terminal");
      }
      terminal.write(frame.payload);
      break;
    case TAG.revoked: {
      const revoked = parseJSON(frame.payload);
      const isCurrent = current?.id === revoked.id &&
        current?.generation === revoked.generation;
      const isClosing = closing?.id === revoked.id &&
        closing?.generation === revoked.generation;
      sessions = sessions.filter(
        (session) => session.id !== revoked.id || session.generation !== revoked.generation,
      );
      if (isCurrent) {
        current = null;
        showMessage("Terminal ended", "The host stopped sharing that terminal.", "Encrypted");
        setTimeout(() => renderSessions(sessions), 700);
      } else if (isClosing) {
        closing = null;
        renderSessions(sessions);
      }
      break;
    }
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
    case TAG.error: {
      const problem = parseJSON(frame.payload);
      if (problem.retry === false) {
        stopped = true;
      }
      current = null;
      closing = null;
      showMessage("Terminal unavailable", String(problem.message || "The host rejected the operation."));
      if (problem.retry === false) {
        target.socket.close();
      } else {
        setTimeout(() => renderSessions(sessions), 700);
      }
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
    current = null;
    closing = null;
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
  if (current && connection?.authenticated) {
    sendInput(data);
  }
});
elements.toolbar.addEventListener("pointerdown", (event) => event.preventDefault());
elements.toolbar.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-key]");
  if (!button || !current) {
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
elements.back.addEventListener("click", closeSession);
elements.close.addEventListener("click", closeSession);

function scheduleResize() {
  window.clearTimeout(resizeTimer);
  resizeTimer = window.setTimeout(() => {
    if (!current || !connection?.authenticated) {
      return;
    }
    sendJSON(TAG.resize, dimensions());
  }, 100);
}

window.addEventListener("resize", scheduleResize);
globalThis.visualViewport?.addEventListener("resize", scheduleResize);

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
