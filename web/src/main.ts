// webdesk viewer: logs in to a Linux machine through the webdesk server,
// receives its screen over WebRTC and sends mouse + keyboard back.

interface Target {
  host: string;
  port: number;
  username: string;
  password: string;
}

type Signal =
  | { type: "status"; message: string }
  | { type: "setup"; tools: string; packages: string[]; virtual: boolean }
  | { type: "joined"; iceServers: RTCIceServer[]; virtual: boolean; warning?: string }
  | { type: "bye"; message: string }
  | { type: "answer"; sdp: RTCSessionDescriptionInit }
  | { type: "candidate"; candidate: RTCIceCandidateInit }
  | { type: "error"; message: string };

const $ = <T extends HTMLElement = HTMLElement>(selector: string) => document.querySelector(selector) as T;

const loginEl = $("#login");
const form = $<HTMLFormElement>("#login-form");
const loginError = $("#login-error");
const loginNote = $("#login-note");
const sessionEl = $("#session");
const titleEl = $("#title");
const statusEl = $("#status");
const hudEl = $("#hud");
const screenEl = $("#screen");
const overlayEl = $("#overlay");
const video = $<HTMLVideoElement>("#video");
const setupEl = $("#setup");
const setupText = $("#setup-text");
const setupCommand = $("#setup-command");
const logoutBtn = $<HTMLButtonElement>("#logout");

const SAVED_TARGET = "webdesk:last-target";
const MODIFIERS = new Set([
  "ShiftLeft", "ShiftRight", "ControlLeft", "ControlRight",
  "AltLeft", "AltRight", "MetaLeft", "MetaRight", "CapsLock",
]);

let target: Target | null = null;
let ws: WebSocket | null = null;
let pc: RTCPeerConnection | null = null;
let dc: RTCDataChannel | null = null;
let pendingCandidates: RTCIceCandidateInit[] = [];
let signalQueue = Promise.resolve();
let statsTimer = 0;
let streaming = false; // the video connected at least once in this attempt
let ended = false; // an error was already shown for this attempt
let warning = ""; // something the machine told us about this desktop

// ---------- login ----------

function field(name: string) {
  return form.elements.namedItem(name) as HTMLInputElement;
}

function showLogin(error?: string, note?: string) {
  closeSession();
  sessionEl.hidden = true;
  loginEl.hidden = false;
  loginError.textContent = error ?? "";
  loginError.hidden = !error;
  loginNote.textContent = note ?? "";
  loginNote.hidden = !note;
  (field("host").value ? field("password") : field("host")).focus();
}

function restoreLogin() {
  try {
    const saved = JSON.parse(localStorage.getItem(SAVED_TARGET) ?? "null");
    if (saved) {
      field("host").value = saved.host ?? "";
      field("port").value = String(saved.port ?? 22);
      field("username").value = saved.username ?? "";
    }
  } catch {
    // Storage unavailable (private window); start blank.
  }
}

form.addEventListener("submit", (ev) => {
  ev.preventDefault();
  const next: Target = {
    host: field("host").value.trim(),
    port: Number(field("port").value) || 22,
    username: field("username").value.trim(),
    password: field("password").value,
  };
  try {
    localStorage.setItem(SAVED_TARGET, JSON.stringify({ host: next.host, port: next.port, username: next.username }));
  } catch {
    // Not remembering the form is fine.
  }
  openSession(next);
});

// ---------- session + signaling ----------

function setStatus(text: string, overlay: boolean) {
  statusEl.textContent = text;
  overlayEl.textContent = text;
  overlayEl.hidden = !overlay;
}

function signal(msg: object) {
  if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
}

function openSession(next: Target) {
  closeSession();
  target = next;
  streaming = false;
  ended = false;
  warning = "";
  loginEl.hidden = true;
  sessionEl.hidden = false;
  titleEl.textContent = `${next.username}@${next.host}`;
  hudEl.textContent = "";
  setStatus("Connecting…", true);

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const sock = new WebSocket(`${proto}://${location.host}/ws`);
  ws = sock;
  sock.onopen = () => sock.send(JSON.stringify({ type: "connect", ...next, size: desiredSize() }));
  sock.onmessage = (ev) => {
    if (ws !== sock) return;
    const msg = JSON.parse(ev.data) as Signal;
    // Handle signals strictly in order: candidates must not race the answer.
    signalQueue = signalQueue.then(() => onSignal(msg)).catch((err) => console.error("signal", err));
  };
  sock.onclose = () => {
    if (ws !== sock || ended) return;
    ended = true;
    if (streaming) setStatus("Disconnected from the webdesk server.", true);
    else showLogin("Lost the connection to the webdesk server.");
  };
}

function closeSession() {
  const sock = ws;
  ws = null;
  sock?.close();
  clearInterval(statsTimer);
  releaseKeys(true);
  dc?.close();
  pc?.close();
  dc = null;
  pc = null;
  pendingCandidates = [];
  video.srcObject = null;
  setupEl.hidden = true;
  logoutBtn.hidden = true;
  screenEl.style.cursor = "";
  cursorCache.clear();
}

// A new virtual desktop is made to fit the viewer's window. Both sides are
// scaled together so the desktop keeps the window's proportions instead of
// being stretched into an odd shape by the limits.
function desiredSize(): string {
  const scale = Math.min(window.devicePixelRatio || 1, 2);
  let width = (screenEl.clientWidth || window.innerWidth) * scale;
  let height = (screenEl.clientHeight || window.innerHeight - 40) * scale;
  const grow = Math.max(800 / width, 600 / height, 1);
  width *= grow;
  height *= grow;
  const shrink = Math.min(3840 / width, 2160 / height, 1);
  width *= shrink;
  height *= shrink;
  return `${Math.round(width) & ~1}x${Math.round(height) & ~1}`;
}

// The agent sends the remote cursor's shape, which the browser draws at the
// local pointer: no round trip, so it keeps up with the mouse.
const cursorCache = new Map<number, string>();

function applyCursor(msg: { serial: number; w?: number; h?: number; xhot?: number; yhot?: number; png?: string }) {
  if (msg.png) {
    // Browsers ignore cursors bigger than 128px and then show nothing.
    const tooBig = (msg.w ?? 0) > 128 || (msg.h ?? 0) > 128;
    const style = tooBig ? "default" : `url("data:image/png;base64,${msg.png}") ${msg.xhot ?? 0} ${msg.yhot ?? 0}, auto`;
    cursorCache.set(msg.serial, style);
  }
  screenEl.style.cursor = cursorCache.get(msg.serial) ?? "default";
}

async function onSignal(msg: Signal) {
  switch (msg.type) {
    case "status":
      if (!streaming) setStatus(msg.message, true);
      break;
    case "setup":
      setStatus("Setup needed", false);
      setupText.textContent = msg.virtual
        ? `There's no X11 desktop on this machine's screen, so webdesk will start a virtual desktop. First it needs ${msg.tools}, which webdesk can install now:`
        : `This machine needs ${msg.tools}, which webdesk can install now:`;
      setupCommand.textContent = `sudo apt-get install ${msg.packages.join(" ")}`;
      setupEl.hidden = false;
      break;
    case "joined":
      logoutBtn.hidden = !msg.virtual;
      if (msg.virtual) titleEl.textContent += " · virtual desktop";
      warning = msg.warning ?? "";
      await startPeer(msg.iceServers);
      break;
    case "bye":
      ended = true;
      showLogin(undefined, msg.message);
      break;
    case "answer":
      if (!pc) return;
      await pc.setRemoteDescription(msg.sdp);
      for (const candidate of pendingCandidates) await pc.addIceCandidate(candidate);
      pendingCandidates = [];
      break;
    case "candidate":
      if (pc?.remoteDescription) await pc.addIceCandidate(msg.candidate);
      else pendingCandidates.push(msg.candidate);
      break;
    case "error":
      ended = true;
      if (streaming) setStatus(msg.message, true);
      else showLogin(msg.message);
      break;
  }
}

async function startPeer(iceServers: RTCIceServer[]) {
  const peer = new RTCPeerConnection({ iceServers });
  pc = peer;
  peer.addTransceiver("video", { direction: "recvonly" });
  const channel = peer.createDataChannel("input", { ordered: true });
  dc = channel;
  channel.onmessage = (ev) => {
    try {
      const msg = JSON.parse(ev.data);
      if (msg.t === "cursor") applyCursor(msg);
    } catch {
      // Ignore anything we don't understand.
    }
  };

  peer.ontrack = (ev) => {
    // Ask the browser to render frames as soon as they arrive instead of buffering.
    const receiver = ev.receiver as RTCRtpReceiver & { jitterBufferTarget?: number; playoutDelayHint?: number };
    receiver.jitterBufferTarget = 0;
    receiver.playoutDelayHint = 0;
    video.srcObject = ev.streams[0] ?? new MediaStream([ev.track]);
  };
  peer.onicecandidate = (ev) => {
    if (ev.candidate) signal({ type: "candidate", candidate: ev.candidate.toJSON() });
  };
  peer.onconnectionstatechange = () => {
    if (pc !== peer) return;
    switch (peer.connectionState) {
      case "connected":
        streaming = true;
        setStatus(warning || "Connected", false);
        screenEl.focus();
        startStats(peer);
        break;
      case "disconnected":
        setStatus("Connection interrupted…", true);
        break;
      case "failed":
        setStatus(
          streaming
            ? "The connection dropped. Try Reconnect."
            : "Logged in, but couldn't open a video connection to the machine. It may need a TURN relay.",
          true,
        );
        break;
    }
  };

  await peer.setLocalDescription(await peer.createOffer());
  signal({ type: "offer", sdp: peer.localDescription });
}

function startStats(peer: RTCPeerConnection) {
  clearInterval(statsTimer);
  let lastBytes = 0;
  let lastTime = 0;
  statsTimer = setInterval(async () => {
    const report = await peer.getStats();
    const parts: string[] = [];
    let selectedPair = "";
    report.forEach((s) => {
      if (s.type === "transport" && s.selectedCandidatePairId) selectedPair = s.selectedCandidatePairId;
    });
    report.forEach((s) => {
      if (s.type === "inbound-rtp" && s.kind === "video") {
        if (s.frameWidth) parts.push(`${s.frameWidth}×${s.frameHeight}`);
        parts.push(`${Math.round(s.framesPerSecond ?? 0)} fps`);
        if (lastTime) parts.push(`${Math.round(((s.bytesReceived - lastBytes) * 8) / (s.timestamp - lastTime))} kbps`);
        lastBytes = s.bytesReceived;
        lastTime = s.timestamp;
      }
    });
    const pair = report.get(selectedPair);
    if (pair) {
      if (pair.currentRoundTripTime !== undefined) parts.push(`${Math.round(pair.currentRoundTripTime * 1000)} ms`);
      const local = report.get(pair.localCandidateId);
      if (local) parts.push(`${local.protocol} ${local.candidateType}`);
    }
    hudEl.textContent = parts.join(" · ");
  }, 1000);
}

// ---------- input ----------

function sendInput(msg: object) {
  if (dc?.readyState === "open") dc.send(JSON.stringify(msg));
}

const clamp01 = (v: number) => Math.min(1, Math.max(0, v));

// Map a pointer position to 0..1 coordinates on the remote screen,
// accounting for letterboxing from object-fit: contain.
function screenCoords(ev: MouseEvent): { x: number; y: number } | null {
  const { videoWidth: vw, videoHeight: vh } = video;
  if (!vw || !vh) return null;
  const rect = video.getBoundingClientRect();
  const scale = Math.min(rect.width / vw, rect.height / vh);
  const width = vw * scale;
  const height = vh * scale;
  const left = rect.left + (rect.width - width) / 2;
  const top = rect.top + (rect.height - height) / 2;
  return { x: clamp01((ev.clientX - left) / width), y: clamp01((ev.clientY - top) / height) };
}

let pendingMove: { x: number; y: number } | null = null;

function flushMove() {
  if (pendingMove) sendInput({ t: "move", ...pendingMove });
  pendingMove = null;
}

screenEl.addEventListener("pointermove", (ev) => {
  const pos = screenCoords(ev);
  if (!pos) return;
  // Coalesce moves to one per frame.
  if (!pendingMove) requestAnimationFrame(flushMove);
  pendingMove = pos;
});

screenEl.addEventListener("pointerdown", (ev) => {
  ev.preventDefault();
  screenEl.focus();
  screenEl.setPointerCapture(ev.pointerId);
  pendingMove = screenCoords(ev);
  flushMove();
  sendInput({ t: "button", b: ev.button, down: true });
});

screenEl.addEventListener("pointerup", (ev) => {
  ev.preventDefault();
  pendingMove = screenCoords(ev);
  flushMove();
  sendInput({ t: "button", b: ev.button, down: false });
});

screenEl.addEventListener("contextmenu", (ev) => ev.preventDefault());

let wheelX = 0;
let wheelY = 0;
screenEl.addEventListener(
  "wheel",
  (ev) => {
    ev.preventDefault();
    // Convert to wheel "notches": ~100px or 3 lines per notch.
    // A notch is a page, 3 lines, or ~100px from a wheel and ~40px from a trackpad.
    const pixels = Math.max(Math.abs(ev.deltaX), Math.abs(ev.deltaY)) >= 80 ? 100 : 40;
    const unit = ev.deltaMode === WheelEvent.DOM_DELTA_PAGE ? 1 : ev.deltaMode === WheelEvent.DOM_DELTA_LINE ? 1 / 3 : 1 / pixels;
    wheelX += ev.deltaX * unit;
    wheelY += ev.deltaY * unit;
    const dx = Math.trunc(wheelX);
    const dy = Math.trunc(wheelY);
    if (!dx && !dy) return;
    wheelX -= dx;
    wheelY -= dy;
    sendInput({ t: "wheel", dx, dy });
  },
  { passive: false },
);

const KEY_TO_CODE: Record<string, string> = {
  " ": "Space", "-": "Minus", "=": "Equal", "[": "BracketLeft", "]": "BracketRight",
  "\\": "Backslash", ";": "Semicolon", "'": "Quote", "`": "Backquote", ",": "Comma",
  ".": "Period", "/": "Slash", Shift: "ShiftLeft", Control: "ControlLeft", Alt: "AltLeft", Meta: "MetaLeft",
};

// Some synthetic and on-screen keyboards leave `code` empty; derive it from `key`.
function codeFor(ev: KeyboardEvent): string {
  if (ev.code) return ev.code;
  if (/^[a-z]$/i.test(ev.key)) return `Key${ev.key.toUpperCase()}`;
  if (/^[0-9]$/.test(ev.key)) return `Digit${ev.key}`;
  return KEY_TO_CODE[ev.key] ?? ev.key; // named keys (Enter, ArrowUp, F5…) share their code name
}

// On a Mac, Cmd sits where Ctrl is on a PC keyboard, so send it as Ctrl and the
// usual shortcuts (copy, paste, select all) work on the remote machine.
const IS_MAC = /Mac/i.test(navigator.userAgent);

function remoteCode(code: string): string {
  if (!IS_MAC) return code;
  if (code === "MetaLeft") return "ControlLeft";
  if (code === "MetaRight") return "ControlRight";
  return code;
}

const pressedKeys = new Set<string>();

function releaseKeys(includeModifiers: boolean) {
  for (const code of pressedKeys) {
    if (!includeModifiers && MODIFIERS.has(code)) continue;
    sendInput({ t: "key", code, down: false });
    pressedKeys.delete(code);
  }
}

screenEl.addEventListener("keydown", (ev) => {
  ev.preventDefault();
  const code = remoteCode(codeFor(ev));
  // The remote X server auto-repeats held keys itself.
  if (ev.repeat || !code) return;
  pressedKeys.add(code);
  sendInput({ t: "key", code, down: true });
});

screenEl.addEventListener("keyup", (ev) => {
  ev.preventDefault();
  const pressed = codeFor(ev);
  const code = remoteCode(pressed);
  if (!code) return;
  pressedKeys.delete(code);
  sendInput({ t: "key", code, down: false });
  // macOS never fires keyup for keys released while Cmd is held.
  if (pressed === "MetaLeft" || pressed === "MetaRight") releaseKeys(false);
});

screenEl.addEventListener("blur", () => releaseKeys(true));

// ---------- toolbar ----------

$("#disconnect").onclick = () => showLogin();

$("#setup-install").onclick = () => {
  setupEl.hidden = true;
  setStatus("Installing…", true);
  signal({ type: "install" });
};

$("#setup-cancel").onclick = () => showLogin();

logoutBtn.onclick = () => {
  setStatus("Logging out…", true);
  signal({ type: "logout" });
};

$("#reconnect").onclick = () => {
  if (target) openSession(target);
};

$("#fullscreen").onclick = async () => {
  await screenEl.requestFullscreen();
  // Chromium only: capture Esc and system shortcuts while fullscreen.
  const nav = navigator as Navigator & { keyboard?: { lock?: () => Promise<void> } };
  await nav.keyboard?.lock?.().catch(() => {});
  screenEl.focus();
};

restoreLogin();
showLogin();
