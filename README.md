# webdesk

**A Linux desktop in your browser.** Type a machine's address and password like
you would for SSH, and its screen appears — mouse, keyboard and clipboard
working on it.

Nothing to install on the machine you're connecting to. No VNC server to set
up, no ports to forward. If you can SSH to it, you can see it.

```
Browser ──WebSocket──▶ webdesk ──SSH──▶ your Linux machine
   ▲                                          │
   └───────── WebRTC: video + input ──────────┘
```

## Try it

```sh
docker compose up -d --build
```

Open <http://127.0.0.1:8080> and log in the way you would over SSH. That's the
whole setup — the web client, the server and the program that runs on the
target machine are all built inside the image.

## How it works

webdesk logs in over SSH and leaves a small Go binary in `~/.cache/webdesk/`.
That binary captures the X screen with ffmpeg, encodes H.264, and opens a
**WebRTC connection straight to your browser**.

The server only introduces the two sides. Once they're talking, video and input
go peer-to-peer and never touch it again.

Three things that make it feel quick:

- **The cursor is drawn locally.** It's kept out of the video and sent as a
  picture, so the pointer keeps up with your hand instead of trailing a round
  trip behind it.
- **Keys travel by position, not by letter**, so the machine's own keyboard
  layout stays in charge.
- **No desktop? It makes one.** A machine with no monitor gets a virtual X11
  desktop on Xvfb, which keeps running after you disconnect — **Log out** ends
  it.

## What the machine needs

Linux on x86_64 or arm64, reachable over SSH, with `ffmpeg`. A virtual desktop
also wants `Xvfb`, `dbus-launch` and one of LXDE, XFCE, MATE, LXQt, Openbox or
GNOME. On Debian and Ubuntu, webdesk offers to install whatever is missing from
the page, once you confirm.
