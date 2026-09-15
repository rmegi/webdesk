# webdesk

A web-based remote desktop for Linux. Enter a machine's host, username and
password like you would for SSH, and you get its screen in the browser with
mouse and keyboard control.

## How it works

```
Browser ──WebSocket──▶ webdesk server ──SSH──▶ target machine
   ▲                                              │
   └────────── WebRTC: video + input ─────────────┘
```

1. The browser sends the login to the **webdesk server** (`server/`).
2. The server logs in over SSH, uploads the **agent** (`agent/`) to
   `~/.cache/webdesk/` if this build isn't there yet, and starts it as that user.
3. The agent finds the user's desktop session on the machine's monitor, captures
   it with ffmpeg (H.264) and streams it to the browser over WebRTC. Mouse and
   keyboard come back over a WebRTC data channel and are replayed with XTEST.
4. The server only relays connection setup (offer, answer, ICE candidates)
   between browser and agent, through the SSH session. Video and input go
   peer-to-peer.

## Target machine requirements

- Linux on x86_64 or arm64, reachable over SSH from the webdesk server
- `ffmpeg` installed
- The user logged in at the machine in an **X11 (Xorg) session**. Wayland isn't
  supported yet; most login screens let you pick "Xorg" or "on Xorg".

## Project layout

```
agent/    Go agent that runs on the target: screen capture, WebRTC, input
server/   Node server: serves the page, SSH login, signaling relay
web/      Browser client (TypeScript, compiled with tsc)
dev/      Docker test machine: SSH server + XFCE on a virtual screen
```

## Running

Needs Node 24+, pnpm, Go 1.27, and Docker for the test machine.

```sh
pnpm install
pnpm dev        # builds the agent and web client, serves http://127.0.0.1:8080
```

| Script            | What it does                                                    |
| ----------------- | --------------------------------------------------------------- |
| `pnpm dev`        | Build everything, then run the server with auto-reload          |
| `pnpm dev:target` | Build and start the Docker test machine                         |
| `pnpm build`      | Build the web client and agent binaries (linux amd64 + arm64)   |
| `pnpm start`      | Build, then run the server                                      |
| `pnpm typecheck`  | Type-check the server and web client                            |

CI (`.github/workflows/ci.yml`) runs the type checks, `gofmt`, `go vet` and the
full build on every push to `main` and on pull requests.

### Local test machine

A Docker container with an SSH server and a logged-in XFCE desktop:

```sh
pnpm dev:target
```

Then connect to host `127.0.0.1`, port `2222`, user `desk`, password `desk`.
Leaving the password empty logs in with the dev SSH key in `dev/ssh/`.

## Configuration

Server environment variables:

| Variable              | Default                        | Purpose                                                       |
| --------------------- | ------------------------------ | ------------------------------------------------------------- |
| `PORT`                | `8080`                         | HTTP port                                                     |
| `HOST`                | `127.0.0.1`                    | Listen address                                                |
| `WEBDESK_ICE_SERVERS` | Google STUN                    | JSON array of `RTCIceServer`, e.g. to add a TURN relay        |
| `WEBDESK_SSH_KEY`     | none (`dev/ssh/…` in `pnpm dev`) | Private key used when the password is left empty            |

Agent environment variables, read from the SSH session's environment:

| Variable              | Default | Purpose                                                           |
| --------------------- | ------- | ----------------------------------------------------------------- |
| `WEBDESK_FPS`         | `30`    | Capture frame rate                                                |
| `WEBDESK_ICE_PORT`    | random  | Pin WebRTC to one UDP+TCP port                                    |
| `WEBDESK_ICE_HOST_IP` | none    | Advertise this IP instead of local ones (e.g. behind port mapping) |

## Security notes

- The web page has **no login of its own yet**. Anyone who can open it can use
  the server to try SSH logins, so it listens on localhost only by default.
- Host keys are pinned on first connect in `server/data/known_hosts.json`. A
  changed key is refused.
- Passwords are used only for the SSH login. They aren't logged or stored, and
  the browser remembers only host, port and username.

## Known limitations

- X11 only; the whole X screen is captured, so multiple monitors show as one image.
- Keyframes come every 2 seconds, so packet loss can freeze the picture briefly.
- The remote cursor is drawn into the video, so it lags slightly behind your pointer.
- On a Mac, Cmd is sent as the Super key. There's no clipboard or audio yet.

## Roadmap

1. ~~Local prototype~~: SSH login, screen streaming, mouse and keyboard
2. Internet: HTTPS, a login for the page itself, a TURN relay
3. Real machines: clipboard, multi-monitor, Wayland, keyframes on demand
