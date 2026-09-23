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

1. The browser sends the login to the **webdesk server** (`backend/`).
2. The server logs in over SSH, uploads the **host program** (`host/`) to
   `~/.cache/webdesk/` if this build isn't there yet, and starts it as that user.
3. The host program checks what the machine is missing (ffmpeg, or what a
   virtual desktop needs). The page lists it and installs it with `sudo apt-get`
   only after you confirm.
4. The host program shows the user's X11 desktop on the machine's monitor. If
   there isn't one (no monitor, or a Wayland desktop), it starts a **virtual X11
   desktop** on Xvfb with the machine's own desktop environment. The virtual
   desktop keeps running when you disconnect; **Log out** ends it.
5. The screen is captured with ffmpeg (H.264) and streamed to the browser over
   WebRTC. Mouse and keyboard come back over a WebRTC data channel and are
   replayed with XTEST. The cursor is kept out of the video and sent as a
   picture, so the browser draws it at your pointer with no network wait. The
   clipboard travels the same channel both ways: the host program holds the
   machine's X selection on your behalf and hands the text over when something
   pastes.
6. The server only relays connection setup (offer, answer, ICE candidates)
   between browser and host, through the SSH session. Video and input go
   peer-to-peer.

## Target machine requirements

- Linux on x86_64 or arm64, reachable over SSH from the webdesk server
- `ffmpeg`. A virtual desktop also needs `Xvfb`, `dbus-launch` and a desktop
  environment (LXDE on Raspberry Pi OS, XFCE, MATE, LXQt, Openbox or GNOME)
- On Debian, Ubuntu and Raspberry Pi OS, webdesk installs missing packages from
  the page after you confirm. That needs `sudo`: passwordless, or the same
  password you logged in with.

A Wayland desktop on the monitor isn't shown directly yet; you get a virtual
X11 desktop instead.

## Project layout

```
host/      Go program that runs on the target: screen capture, WebRTC, input
backend/   Node server: SSH login, signaling relay, and its Dockerfile
frontend/  Browser client (TypeScript), with its nginx image and config
test/      Docker test machine: SSH server + XFCE on a virtual screen
```

## Running

### With Docker

Nothing to install but Docker, on http://127.0.0.1:8080:

```sh
docker compose up -d --build
```

Two services. `frontend` is nginx with the compiled web client, and it passes
the signaling WebSocket through to `backend`, which does the SSH login and
carries the host binaries it uploads to target machines. Video and input touch
neither of them: the browser talks to the target machine directly over WebRTC.

Build on each machine you run it on and the images take that machine's
architecture, arm64 on a Mac and amd64 on a PC. The host binaries are
cross-compiled for both either way, because the machines being controlled are
not the machine running the server.

Pinned host keys live in a named volume, so a rebuild doesn't ask about every
machine again. `docker compose down` stops it; add `-v` to forget the keys too.

### From source

Needs Node 24+, Go 1.27, and Docker for the test machine.

```sh
npm install
npm run dev     # builds the host and web client, serves http://127.0.0.1:8080
```

| Script                | What it does                                                |
| --------------------- | ----------------------------------------------------------- |
| `npm run dev`         | Build everything, then run the server with auto-reload      |
| `npm run docker`      | Start the test machine and the server, both in Docker       |
| `npm run docker:down` | Stop both containers                                        |
| `npm run test:target` | Build and start the Docker test machine                     |
| `npm run build`       | Build the web client and host binaries (linux amd64 + arm64)|
| `npm start`           | Build, then run the server                                  |
| `npm run typecheck`   | Type-check the server and web client                        |

CI (`.github/workflows/ci.yml`) runs the type checks, `gofmt`, `go vet`, the
full build and a Docker build on every push to `main` and on pull requests.

### Local test machine

A Docker container with an SSH server and a logged-in XFCE desktop:

```sh
npm run test:target
```

Connect to host `127.0.0.1`, port `2222`, user `desk`, password `desk`.
Leaving the password empty logs in with the test SSH key in `test/ssh/`.

From a server that is itself in Docker, the test machine answers to its service
name instead: host `target`, port `22`.

## Configuration

Server environment variables:

| Variable              | Default                        | Purpose                                                       |
| --------------------- | ------------------------------ | ------------------------------------------------------------- |
| `PORT`                | `8080`                         | HTTP port                                                     |
| `HOST`                | `127.0.0.1`                    | Listen address                                                |
| `WEBDESK_ICE_SERVERS` | Google STUN                    | JSON array of `RTCIceServer`, e.g. to add a TURN relay        |
| `WEBDESK_SSH_KEY`     | none (`test/ssh/…` in dev)     | Comma-separated private key paths tried when the password is left empty (`~/` allowed; passphrase-protected keys are skipped) |

Host program environment variables, read from the SSH session's environment:

| Variable              | Default | Purpose                                                           |
| --------------------- | ------- | ----------------------------------------------------------------- |
| `WEBDESK_FPS`         | `30`    | Capture frame rate                                                |
| `WEBDESK_VIRTUAL_SIZE` | `1920x1080` | Screen size of a virtual desktop                              |
| `WEBDESK_ICE_PORT`    | random  | Pin WebRTC to one UDP+TCP port                                    |
| `WEBDESK_ICE_HOST_IP` | none    | Advertise this IP instead of local ones (e.g. behind port mapping) |

## Security notes

- The web page has **no login of its own yet**. Anyone who can open it can use
  the server to try SSH logins, so it listens on localhost only by default.
- Host keys are pinned on first connect in `backend/data/known_hosts.json`. A
  changed key is refused.
- Passwords are used only for the SSH login. They aren't logged or stored, and
  the browser remembers only host, port and username.

## Troubleshooting

- **"Couldn't reach" a machine on your local network, with the server running
  on a Mac.** macOS blocks apps that don't have Local Network permission, and
  `ssh` or `ping` still working doesn't rule it out because built-in tools are
  exempt. Allow the app that runs the server (your terminal, or Claude) in
  System Settings → Privacy & Security → Local Network, then restart the server.
  To check, `node -e "require('net').connect(22, '<ip>').on('error', e => console.log(e.code))"`
  prints `EHOSTUNREACH` straight away while it's blocked.

## Known limitations

- A Wayland screen is replaced by a virtual X11 desktop rather than shown. The
  whole X screen is captured, so multiple monitors show as one image.
- A keyframe is sent every second, and the encoder restarts when the viewer asks
  for one, so packet loss clears quickly at the cost of a brief hiccup.
- On a Mac, Cmd is sent as Ctrl so the usual shortcuts work on the machine.
  There's no audio or file transfer yet.
- The clipboard carries plain text only, up to 1 MiB. Images and files aren't
  shared, and copying in the browser needs the page to have focus.

## Roadmap

1. ~~Local prototype~~: SSH login, screen streaming, mouse and keyboard
2. Internet: HTTPS, a login for the page itself, a TURN relay
3. Real machines: clipboard, multi-monitor, Wayland, keyframes on demand
