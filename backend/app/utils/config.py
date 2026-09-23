import json
import os
from pathlib import Path

PORT = int(os.getenv("PORT", "8080"))
# Localhost only by default: the page has no login of its own yet, and anyone
# who can open it can use this server to reach machines over SSH.
HOST = os.getenv("HOST", "127.0.0.1")

ROOT = Path(__file__).resolve().parents[2]
FRONTEND_ROOT = ROOT.parent / "frontend"
HOST_DIR = ROOT.parent / "host" / "dist"
KNOWN_HOSTS_FILE = ROOT / "data" / "known_hosts.json"

# Private keys tried when the viewer leaves the password empty.
SSH_KEY_FILES = [path.strip() for path in os.getenv("WEBDESK_SSH_KEY", "").split(",") if path.strip()]

ICE_SERVERS = (
    json.loads(os.environ["WEBDESK_ICE_SERVERS"])
    if os.getenv("WEBDESK_ICE_SERVERS")
    else [{"urls": ["stun:stun.l.google.com:19302"]}]  # pion requires urls to be a list
)

# Messages the viewer may pass straight through to the host program.
RELAYED = {"offer", "candidate", "logout"}
REMOTE_DIR = '"$HOME"/.cache/webdesk'
ARCHES = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
