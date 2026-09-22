// webdesk server: serves the web client and, for each viewer, logs in to the
// requested machine over SSH, starts the host program there and relays WebRTC
// signaling between browser and host. Video and input flow peer-to-peer.
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { homedir } from "node:os";
import { dirname, extname, join, normalize, sep } from "node:path";
import type { Readable } from "node:stream";
import ssh2, {
  type AuthenticationType,
  type AuthHandlerMiddleware,
  type Client,
  type ClientChannel,
  type KeyboardInteractiveAuthMethod,
  type PasswordAuthMethod,
  type PublicKeyAuthMethod,
} from "ssh2";
import { WebSocketServer, type RawData, type WebSocket } from "ws";

const PORT = Number(process.env.PORT ?? 8080);
// Localhost only by default: the page has no login of its own yet, and anyone
// who can open it can use this server to reach machines over SSH.
const HOST = process.env.HOST ?? "127.0.0.1";
const ROOT = join(import.meta.dirname, "..");
const WEB_ROOT = join(ROOT, "web");
const HOST_DIR = join(ROOT, "host", "dist");
const KNOWN_HOSTS_FILE = join(ROOT, "server", "data", "known_hosts.json");
// Optional private keys (comma-separated paths), tried when the viewer leaves the password empty.
const SSH_KEY_FILES = (process.env.WEBDESK_SSH_KEY ?? "")
  .split(",")
  .map((path) => path.trim())
  .filter(Boolean);
const ICE_SERVERS: unknown[] = process.env.WEBDESK_ICE_SERVERS
  ? JSON.parse(process.env.WEBDESK_ICE_SERVERS)
  : [{ urls: ["stun:stun.l.google.com:19302"] }]; // pion requires urls to be an array
const RELAYED = new Set(["offer", "candidate", "logout"]);
const PACKAGE_NAME = /^[a-z0-9][a-z0-9+.-]*$/;
const SCREEN_SIZE = /^\d{3,5}x\d{3,5}$/;
const ARCHES: Record<string, string> = { x86_64: "amd64", aarch64: "arm64", arm64: "arm64" };
const REMOTE_DIR = `"$HOME"/.cache/webdesk`;
const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".map": "application/json",
};

interface Target {
  host: string;
  port: number;
  username: string;
  password: string;
  size?: string; // the viewer's window, used for a new virtual desktop
}

/** What `webdesk-host check` reports about a machine. */
interface MachineCheck {
  virtual: boolean; // no X11 desktop on the monitor, so a virtual one will be used
  missing: string[] | null; // commands that aren't installed ("desktop" = no desktop environment)
  packages: string[] | null; // apt packages providing them
  sudo?: "nopasswd" | "password" | "none";
}

/** An error whose message is written for the person at the browser. */
class UserError extends Error {}

const alive = new WeakSet<WebSocket>();

function send(ws: WebSocket, msg: object) {
  if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(msg));
}

async function handleHttp(req: IncomingMessage, res: ServerResponse) {
  const { pathname } = new URL(req.url ?? "/", "http://localhost");
  const file = normalize(join(WEB_ROOT, pathname === "/" ? "index.html" : pathname));
  if (!file.startsWith(WEB_ROOT + sep)) {
    res.writeHead(403).end();
    return;
  }
  try {
    const body = await readFile(file);
    res
      .writeHead(200, { "content-type": MIME[extname(file)] ?? "application/octet-stream", "cache-control": "no-cache" })
      .end(body);
  } catch {
    res.writeHead(404).end("not found");
  }
}

function parseTarget(msg: Record<string, unknown>): Target | undefined {
  const { host, username, password } = msg;
  const port = msg.port === undefined ? 22 : Number(msg.port);
  const valid =
    msg.type === "connect" &&
    typeof host === "string" &&
    host.trim() !== "" &&
    typeof username === "string" &&
    username.trim() !== "" &&
    typeof password === "string" &&
    Number.isInteger(port) &&
    port >= 1 &&
    port <= 65535;
  const size = typeof msg.size === "string" && SCREEN_SIZE.test(msg.size) ? msg.size : undefined;
  return valid ? { host: host.trim(), port, username: username.trim(), password, size } : undefined;
}

// ---------- SSH ----------

async function readKnownHosts(): Promise<Record<string, string>> {
  try {
    return JSON.parse(await readFile(KNOWN_HOSTS_FILE, "utf8"));
  } catch {
    return {};
  }
}

async function rememberHost(hostId: string, fingerprint: string) {
  const hosts = await readKnownHosts();
  hosts[hostId] = fingerprint;
  await mkdir(dirname(KNOWN_HOSTS_FILE), { recursive: true });
  await writeFile(KNOWN_HOSTS_FILE, `${JSON.stringify(hosts, null, 2)}\n`);
}

type SshAuth = PasswordAuthMethod | KeyboardInteractiveAuthMethod | PublicKeyAuthMethod;

function passwordAuth({ username, password }: Target): SshAuth[] {
  return [
    { type: "password", username, password },
    // Some servers only accept passwords through keyboard-interactive auth.
    {
      type: "keyboard-interactive",
      username,
      prompt: (_name, _instructions, _lang, prompts, finish) => finish(prompts.map(() => password)),
    },
  ];
}

// The server's configured keys, skipping missing files and passphrase-protected keys.
async function keyAuth(username: string): Promise<SshAuth[]> {
  const methods: SshAuth[] = [];
  for (const path of SSH_KEY_FILES) {
    const key = await readFile(path.startsWith("~/") ? join(homedir(), path.slice(2)) : path).catch(() => undefined);
    if (key && !(ssh2.utils.parseKey(key) instanceof Error)) methods.push({ type: "publickey", username, key });
  }
  return methods;
}

// Logs in, pinning each machine's host key on first use like ssh's known_hosts.
async function sshLogin(target: Target): Promise<Client> {
  const hostId = `${target.host}:${target.port}`;
  const known = (await readKnownHosts())[hostId];
  // With a password, still fall back to the configured keys if it is refused.
  const keys = await keyAuth(target.username);
  const auth = target.password ? [...passwordAuth(target), ...keys] : keys;
  if (auth.length === 0) throw new UserError("Enter a password.");

  let fingerprint = "";
  let offered: AuthenticationType[] = [];
  const queue = [...auth];
  // Walk our methods in order, skipping any the machine doesn't accept.
  const authHandler: AuthHandlerMiddleware = (authsLeft, _partialSuccess, next) => {
    if (authsLeft?.length) offered = authsLeft;
    while (queue.length > 0 && offered.length > 0 && !offered.includes(queue[0].type)) queue.shift();
    const method = queue.shift();
    next(method ?? (false as unknown as AuthenticationType));
  };

  const ssh = await new Promise<Client>((resolve, reject) => {
    const client = new ssh2.Client();
    client.on("ready", () => resolve(client));
    client.on("error", reject);
    client.connect({
      host: target.host,
      port: target.port,
      username: target.username,
      authHandler,
      readyTimeout: 20_000,
      keepaliveInterval: 15_000,
      hostVerifier: (key: Buffer) => {
        fingerprint = `SHA256:${createHash("sha256").update(key).digest("base64").replace(/=+$/, "")}`;
        return !known || known === fingerprint;
      },
    });
  }).catch((err: unknown) => {
    if (known && fingerprint && known !== fingerprint) {
      throw new UserError(
        `${hostId} presented a different host key than last time (${fingerprint}). ` +
          `If the machine was reinstalled, remove its entry from server/data/known_hosts.json.`,
      );
    }
    if ((err as { level?: string }).level === "client-authentication") {
      const methods = offered.length > 0 ? offered.join(", ") : "none it would tell us about";
      throw new UserError(
        `${target.username}@${target.host} refused the login. The machine accepts: ${methods}. ` +
          (target.password
            ? "Check the username and the password; some accounts are set up for keys only."
            : "No password was given, and none of the server's SSH keys were accepted."),
      );
    }
    throw err;
  });

  if (!known) await rememberHost(hostId, fingerprint);
  return ssh;
}

function exec(ssh: Client, command: string): Promise<ClientChannel> {
  return new Promise((resolve, reject) => ssh.exec(command, (err, channel) => (err ? reject(err) : resolve(channel))));
}

async function run(ssh: Client, command: string, stdin?: Buffer) {
  const channel = await exec(ssh, command);
  return new Promise<{ code: number | null; stdout: string; stderr: string }>((resolve) => {
    let stdout = "";
    let stderr = "";
    let code: number | null = null;
    channel.setEncoding("utf8");
    channel.stderr.setEncoding("utf8");
    channel.on("data", (chunk: string) => (stdout += chunk));
    channel.stderr.on("data", (chunk: string) => (stderr += chunk));
    channel.on("exit", (exitCode: number | null) => (code = exitCode));
    channel.on("close", () => resolve({ code, stdout, stderr }));
    if (stdin) channel.end(stdin);
    else channel.end();
  });
}

function onLines(stream: Readable, handler: (line: string) => void) {
  let buffered = "";
  stream.on("data", (chunk: string) => {
    buffered += chunk;
    let newline: number;
    while ((newline = buffered.indexOf("\n")) >= 0) {
      const line = buffered.slice(0, newline).trim();
      buffered = buffered.slice(newline + 1);
      if (line) handler(line);
    }
  });
}

// Makes sure this build of the host program is on the machine; returns its remote path.
async function installHost(ssh: Client, status: (message: string) => void): Promise<string> {
  const machine = (await run(ssh, "uname -m")).stdout.trim();
  const arch = ARCHES[machine];
  if (!arch) throw new UserError(`webdesk doesn't support this machine's CPU (${machine || "unknown"}) yet.`);

  const binary = await readFile(join(HOST_DIR, `webdesk-host-linux-${arch}`)).catch(() => {
    throw new UserError(`The linux/${arch} host program isn't built. Run "pnpm build:host" on the server.`);
  });
  const name = `webdesk-host-${createHash("sha256").update(binary).digest("hex").slice(0, 12)}`;
  const path = `${REMOTE_DIR}/${name}`;
  if ((await run(ssh, `sh -c 'test -x ${path}'`)).code === 0) return path;

  status("Installing webdesk on the machine…");
  const install = await run(
    ssh,
    `sh -c 'mkdir -p ${REMOTE_DIR} && rm -f ${REMOTE_DIR}/webdesk-* && cat > ${path}.tmp && chmod 755 ${path}.tmp && mv ${path}.tmp ${path}'`,
    binary,
  );
  if (install.code !== 0) {
    throw new UserError(`Couldn't install webdesk on the machine: ${install.stderr.trim() || `exit code ${install.code}`}`);
  }
  return path;
}

async function checkMachine(ssh: Client, hostPath: string): Promise<MachineCheck> {
  const result = await run(ssh, `sh -c '${hostPath} check'`);
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new UserError(`Couldn't check the machine: ${lastLines(result.stderr) || "webdesk gave no answer"}`);
  }
}

// Turns apt's noise into something worth reading.
function describeAptFailure(output: string): string {
  const text = output.trim();
  if (/Could not get lock|lock-frontend|dpkg frontend lock/i.test(text)) {
    return "The machine is busy installing something else, so apt is locked — automatic updates, usually. Try again in a minute.";
  }
  if (/No space left on device/i.test(text)) return "The machine has run out of disk space for the install.";
  if (/Unable to locate package|has no installation candidate/i.test(text)) {
    return `The machine's package lists don't offer what's needed: ${lastLines(text, 1)}`;
  }
  if (/incorrect password|Sorry, try again/i.test(text)) return "sudo refused the password on that machine.";
  return `Installing failed: ${lastLines(text)}`;
}

function lastLines(text: string, count = 3): string {
  return text.trim().split("\n").slice(-count).join(" ");
}

// RFC 1918 and link-local IPv4 addresses, and mDNS .local names.
function isLocalNetworkHost(host: string): boolean {
  return /^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|169\.254\.)/.test(host) || host.endsWith(".local");
}

function describeError(err: unknown, target: Target): string {
  if (err instanceof UserError) return err.message;
  const e = err as { level?: string; code?: string; message?: string };
  console.error(`session failed for ${target.username}@${target.host}:${target.port}: ${e.code ?? e.level ?? ""} ${e.message ?? err}`);
  if (e.level === "client-authentication") return "Wrong username or password.";
  if (e.code === "ECONNREFUSED") return `Nothing is accepting SSH connections on ${target.host}:${target.port}.`;
  if (e.code === "ENOTFOUND" || e.code === "EAI_AGAIN") return `Can't find a machine called ${target.host}.`;
  if (e.code === "EHOSTUNREACH" && process.platform === "darwin" && isLocalNetworkHost(target.host)) {
    // macOS answers this instantly when the app running the server lacks Local Network permission.
    return (
      `Couldn't reach ${target.host}:${target.port}. If it's on your local network, macOS may be blocking this server: ` +
      "allow the app that runs it in System Settings → Privacy & Security → Local Network, then restart the server."
    );
  }
  if (e.code === "ETIMEDOUT" || e.code === "EHOSTUNREACH" || /timed out/i.test(e.message ?? "")) {
    return `Couldn't reach ${target.host}:${target.port}.`;
  }
  return e.message ?? String(err);
}

// ---------- viewer sessions ----------

const server = createServer((req, res) => void handleHttp(req, res));
const wss = new WebSocketServer({ server, path: "/ws" });

wss.on("connection", (ws) => {
  alive.add(ws);
  ws.on("pong", () => alive.add(ws));

  let started = false;
  let ended = false;
  let ssh: Client | undefined;
  let hostChannel: ClientChannel | undefined;
  let approveInstall: (() => void) | undefined;

  const status = (message: string) => send(ws, { type: "status", message });
  const end = (error?: string) => {
    if (ended) return;
    ended = true;
    if (error) send(ws, { type: "error", message: error });
    ws.close();
    // Closing the host program's stdin tells it to stop; give it a moment to clean up
    // (and log) before dropping the SSH connection.
    const client = ssh;
    if (hostChannel) {
      hostChannel.once("close", () => client?.end());
      hostChannel.end();
      setTimeout(() => client?.end(), 5_000).unref();
    } else {
      client?.end();
    }
  };

  ws.on("message", (data: RawData) => {
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(String(data));
    } catch {
      return;
    }
    if (!started) {
      const target = parseTarget(msg);
      if (!target) {
        ws.close(4002, "expected connect");
        return;
      }
      started = true;
      startSession(target).catch((err) => end(describeError(err, target)));
      return;
    }
    if (msg.type === "install") {
      approveInstall?.();
      return;
    }
    if (hostChannel && typeof msg.type === "string" && RELAYED.has(msg.type)) hostChannel.write(`${JSON.stringify(msg)}\n`);
  });
  ws.on("close", () => {
    end();
    approveInstall?.(); // a session waiting on setup notices it ended and stops
  });

  async function startSession(target: Target) {
    const label = `${target.username}@${target.host}:${target.port}`;
    status(`Connecting to ${target.host}…`);
    const client = await sshLogin(target);
    if (ended) {
      client.end();
      return;
    }
    ssh = client;
    client.on("error", (err) => end(`SSH connection lost: ${err.message}`));
    client.on("close", () => end("The SSH connection closed."));
    console.log(`logged in: ${label}`);

    status("Checking the machine…");
    const hostPath = await installHost(client, status);
    let check = await checkMachine(client, hostPath);
    if (check.missing?.length) {
      await setUpMachine(client, target, check);
      if (ended) return;
      check = await checkMachine(client, hostPath);
      if (check.missing?.length) throw new UserError(`Still missing after installing: ${check.missing.join(", ")}.`);
    }
    if (ended) return;

    status("Starting webdesk on the machine…");
    const channel = await exec(client, `sh -c 'exec ${hostPath}'`);
    hostChannel = channel;
    if (ended) {
      channel.end();
      return;
    }
    channel.setEncoding("utf8");
    channel.stderr.setEncoding("utf8");
    onLines(channel.stderr, (line) => console.log(`[${label}] ${line}`));
    onLines(channel, (line) => {
      let msg: { type?: string; message?: string; virtual?: boolean; warning?: string };
      try {
        msg = JSON.parse(line);
      } catch {
        console.log(`[${label}] ${line}`);
        return;
      }
      if (msg.type === "ready") {
        status("Opening the screen…");
        send(ws, { type: "joined", iceServers: ICE_SERVERS, virtual: msg.virtual === true, warning: msg.warning });
      } else if (msg.type === "error") {
        end(msg.message);
      } else if (msg.type === "bye") {
        send(ws, msg);
        end();
      } else {
        send(ws, msg); // status, answer, candidate
      }
    });
    channel.on("close", () => end("webdesk stopped on the machine."));
    channel.write(`${JSON.stringify({ type: "config", iceServers: ICE_SERVERS, virtualSize: target.size })}\n`);
  }

  // Installs what the machine is missing, once the viewer approves in the page.
  async function setUpMachine(client: Client, target: Target, check: MachineCheck) {
    const tools = (check.missing ?? []).map((name) => (name === "desktop" ? "a desktop environment" : name)).join(", ");
    const packages = check.packages ?? [];
    if (packages.length === 0) throw new UserError(`This machine needs ${tools}. Install them there and connect again.`);
    if (!packages.every((name) => PACKAGE_NAME.test(name))) throw new UserError("The machine reported unexpected package names.");
    const canSudo = check.sudo === "nopasswd" || (check.sudo === "password" && target.password !== "");
    if (!canSudo) {
      throw new UserError(
        `This machine needs ${tools}. Run this there, then connect again: sudo apt-get install --no-install-recommends ${packages.join(" ")}`,
      );
    }

    send(ws, { type: "setup", tools, packages, virtual: check.virtual });
    await new Promise<void>((resolve) => (approveInstall = resolve));
    approveInstall = undefined;
    if (ended) return;

    status(`Installing ${packages.join(", ")}… This can take a few minutes.`);
    // Wait for any other apt on the machine — automatic updates, usually —
    // instead of falling over on its lock.
    const apt = "DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=180";
    const script = `${apt} update -qq && ${apt} install -y -qq --no-install-recommends ${packages.join(" ")}`;
    const result =
      check.sudo === "nopasswd"
        ? await run(client, `sudo -n sh -c '${script}'`)
        : await run(client, `sudo -S -p "" sh -c '${script}'`, Buffer.from(`${target.password}\n`));
    if (result.code !== 0) throw new UserError(describeAptFailure(result.stderr || result.stdout));
    console.log(`installed on ${target.host}: ${packages.join(" ")}`);
  }
});

// Drop connections that stop answering pings (laptop sleep, dead NAT mapping).
setInterval(() => {
  for (const ws of wss.clients) {
    if (!alive.has(ws)) {
      ws.terminate();
      continue;
    }
    alive.delete(ws);
    ws.ping();
  }
}, 15_000);

server.listen(PORT, HOST, () => console.log(`webdesk server on http://${HOST}:${PORT}`));
