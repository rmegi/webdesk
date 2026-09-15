// webdesk server: serves the web client and, for each viewer, logs in to the
// requested machine over SSH, starts the agent there and relays WebRTC
// signaling between browser and agent. Video and input flow peer-to-peer.
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { dirname, extname, join, normalize, sep } from "node:path";
import type { Readable } from "node:stream";
import ssh2, { type Client, type ClientChannel } from "ssh2";
import { WebSocketServer, type RawData, type WebSocket } from "ws";

const PORT = Number(process.env.PORT ?? 8080);
// Localhost only by default: the page has no login of its own yet, and anyone
// who can open it can use this server to reach machines over SSH.
const HOST = process.env.HOST ?? "127.0.0.1";
const ROOT = join(import.meta.dirname, "..");
const WEB_ROOT = join(ROOT, "web");
const AGENT_DIR = join(ROOT, "agent", "dist");
const KNOWN_HOSTS_FILE = join(ROOT, "server", "data", "known_hosts.json");
// Optional private key, used when the viewer leaves the password empty.
const SSH_KEY_FILE = process.env.WEBDESK_SSH_KEY;
const ICE_SERVERS: unknown[] = process.env.WEBDESK_ICE_SERVERS
  ? JSON.parse(process.env.WEBDESK_ICE_SERVERS)
  : [{ urls: ["stun:stun.l.google.com:19302"] }]; // pion requires urls to be an array
const RELAYED = new Set(["offer", "candidate"]);
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
  return valid ? { host: host.trim(), port, username: username.trim(), password } : undefined;
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

// Logs in, pinning each machine's host key on first use like ssh's known_hosts.
async function sshLogin(target: Target): Promise<Client> {
  const hostId = `${target.host}:${target.port}`;
  const known = (await readKnownHosts())[hostId];
  const privateKey = !target.password && SSH_KEY_FILE ? await readFile(SSH_KEY_FILE) : undefined;
  if (!target.password && !privateKey) throw new UserError("Enter a password.");

  let fingerprint = "";
  const ssh = await new Promise<Client>((resolve, reject) => {
    const client = new ssh2.Client();
    client.on("ready", () => resolve(client));
    client.on("error", reject);
    // Some servers only accept passwords through keyboard-interactive auth.
    client.on("keyboard-interactive", (_name, _instructions, _lang, prompts, finish) =>
      finish(prompts.map(() => target.password)),
    );
    client.connect({
      host: target.host,
      port: target.port,
      username: target.username,
      password: target.password || undefined,
      privateKey,
      tryKeyboard: target.password !== "",
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

// Makes sure this build of the agent is on the machine; returns its remote path.
async function installAgent(ssh: Client, status: (message: string) => void): Promise<string> {
  const probe = await run(ssh, `sh -c 'uname -m; command -v ffmpeg >/dev/null && echo has-ffmpeg'`);
  const [machine = "", ffmpeg] = probe.stdout.split("\n").map((line) => line.trim());
  const arch = ARCHES[machine];
  if (!arch) throw new UserError(`webdesk doesn't support this machine's CPU (${machine || "unknown"}) yet.`);
  if (ffmpeg !== "has-ffmpeg") {
    throw new UserError("ffmpeg isn't installed on the machine. Install it there (for example: sudo apt install ffmpeg) and connect again.");
  }

  const binary = await readFile(join(AGENT_DIR, `webdesk-agent-linux-${arch}`)).catch(() => {
    throw new UserError(`The linux/${arch} agent isn't built. Run "pnpm build:agent" on the server.`);
  });
  const name = `webdesk-agent-${createHash("sha256").update(binary).digest("hex").slice(0, 12)}`;
  const path = `${REMOTE_DIR}/${name}`;
  if ((await run(ssh, `sh -c 'test -x ${path}'`)).code === 0) return path;

  status("Installing the agent…");
  const install = await run(
    ssh,
    `sh -c 'mkdir -p ${REMOTE_DIR} && rm -f ${REMOTE_DIR}/webdesk-agent-* && cat > ${path}.tmp && chmod 755 ${path}.tmp && mv ${path}.tmp ${path}'`,
    binary,
  );
  if (install.code !== 0) {
    throw new UserError(`Couldn't install the agent: ${install.stderr.trim() || `exit code ${install.code}`}`);
  }
  return path;
}

function describeError(err: unknown, target: Target): string {
  if (err instanceof UserError) return err.message;
  const e = err as { level?: string; code?: string; message?: string };
  if (e.level === "client-authentication") return "Wrong username or password.";
  if (e.code === "ECONNREFUSED") return `Nothing is accepting SSH connections on ${target.host}:${target.port}.`;
  if (e.code === "ENOTFOUND" || e.code === "EAI_AGAIN") return `Can't find a machine called ${target.host}.`;
  if (e.code === "ETIMEDOUT" || e.code === "EHOSTUNREACH" || /timed out/i.test(e.message ?? "")) {
    return `Couldn't reach ${target.host}:${target.port}.`;
  }
  console.error(err);
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
  let agent: ClientChannel | undefined;

  const status = (message: string) => send(ws, { type: "status", message });
  const end = (error?: string) => {
    if (ended) return;
    ended = true;
    if (error) send(ws, { type: "error", message: error });
    ws.close();
    // Closing the agent's stdin tells it to stop; give it a moment to clean up
    // (and log) before dropping the SSH connection.
    const client = ssh;
    if (agent) {
      agent.once("close", () => client?.end());
      agent.end();
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
    if (agent && typeof msg.type === "string" && RELAYED.has(msg.type)) agent.write(`${JSON.stringify(msg)}\n`);
  });
  ws.on("close", () => end());

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
    const agentPath = await installAgent(client, status);
    if (ended) return;

    status("Starting the agent…");
    const channel = await exec(client, `sh -c 'exec ${agentPath}'`);
    agent = channel;
    if (ended) {
      channel.end();
      return;
    }
    channel.setEncoding("utf8");
    channel.stderr.setEncoding("utf8");
    onLines(channel.stderr, (line) => console.log(`[${label}] ${line}`));
    onLines(channel, (line) => {
      let msg: { type?: string; message?: string };
      try {
        msg = JSON.parse(line);
      } catch {
        console.log(`[${label}] ${line}`);
        return;
      }
      if (msg.type === "ready") {
        status("Opening the screen…");
        send(ws, { type: "joined", iceServers: ICE_SERVERS });
      } else if (msg.type === "error") {
        end(msg.message);
      } else {
        send(ws, msg);
      }
    });
    channel.on("close", () => end("The agent on the machine stopped."));
    channel.write(`${JSON.stringify({ type: "config", iceServers: ICE_SERVERS })}\n`);
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
