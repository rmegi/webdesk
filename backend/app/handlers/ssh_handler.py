import asyncio
import hashlib
import logging
import re
from pathlib import Path
from typing import Awaitable, Callable, List, Optional

import asyncssh

from utils.config import ARCHES, HOST_DIR, REMOTE_DIR, SSH_KEY_FILES
from utils.errors import UserError, last_lines
from utils.known_hosts import KnownHosts

log = logging.getLogger("webdesk")

PACKAGE_NAME = re.compile(r"^[a-z0-9][a-z0-9+.-]*$")


async def offered_auth_methods(host: str, port: int, username: str, timeout: float = 5.0) -> List[str]:
    """Which methods the machine says it accepts, for the refused-login message.

    Best effort: a second connection that offers nothing and is thrown away, so
    the real one is never disturbed. asyncssh keeps the server's list on a
    private attribute and pops from it as methods are tried, so it has to be
    read while auth is still running.
    """
    found: List[str] = []

    class Probe(asyncssh.SSHClient):
        def connection_made(self, conn):
            self._conn = conn

        def _snapshot(self):
            methods = getattr(self._conn, "_auth_methods", None) or []
            found.extend(m.decode() if isinstance(m, bytes) else str(m) for m in methods)

        def public_key_auth_requested(self):
            self._snapshot()
            return None

        def password_auth_requested(self):
            self._snapshot()
            return None

        def kbdint_auth_requested(self):
            self._snapshot()
            return None

    try:
        conn = await asyncio.wait_for(
            asyncssh.connect(
                host=host, port=port, username=username, known_hosts=None, client_keys=[], client_factory=Probe
            ),
            timeout,
        )
        conn.close()
    except Exception:  # noqa: BLE001 - the probe only ever improves a message
        pass

    ordered: List[str] = []
    for method in found:
        if method not in ordered:
            ordered.append(method)
    return ordered


async def server_fingerprint(host: str, port: int, timeout: float = 5.0) -> Optional[str]:
    """The key this machine is presenting now, so a mismatch can be compared."""
    try:
        key = await asyncio.wait_for(asyncssh.get_server_host_key(host=host, port=port), timeout)
    except Exception:  # noqa: BLE001 - the message is still useful without it
        return None
    return key.get_fingerprint() if key is not None else None


class SshHandler:
    """One machine, over SSH: logging in, putting the host program there and
    installing what it still needs."""

    def __init__(self, host: str, port: int, username: str, password: str):
        self.host = host
        self.port = port
        self.username = username
        self.password = password
        self.host_id = f"{host}:{port}"
        self.known_hosts = KnownHosts()
        self.conn: Optional[asyncssh.SSHClientConnection] = None

    # ---------- connecting ----------

    def _client_keys(self) -> List[str]:
        """The server's configured keys, skipping ones that aren't readable."""
        keys = []
        for path in SSH_KEY_FILES:
            expanded = Path(path).expanduser()
            if expanded.is_file():
                keys.append(str(expanded))
        return keys

    async def connect(self) -> None:
        keys = self._client_keys()
        if not self.password and not keys:
            raise UserError("Enter a password.")

        pinned = self.known_hosts.get(self.host_id)
        options = {
            "host": self.host,
            "port": self.port,
            "username": self.username,
            "client_keys": keys,
            "keepalive_interval": 15,
            "connect_timeout": 20,
        }
        # asyncssh answers keyboard-interactive with the password too, which is
        # what some servers insist on. With a password we still fall back to the
        # configured keys if it is refused.
        if self.password:
            options["password"] = self.password

        if pinned:
            options["known_hosts"] = KnownHosts.as_known_hosts(self.host, self.port, pinned)
        else:
            options["known_hosts"] = None  # nothing to check against yet

        try:
            self.conn = await asyncssh.connect(**options)
        except asyncssh.HostKeyNotVerifiable as error:
            fingerprint = await server_fingerprint(self.host, self.port)
            presented = f" ({fingerprint})" if fingerprint else ""
            raise UserError(
                f"{self.host_id} presented a different host key than last time{presented}. If the "
                f"machine was reinstalled, remove its entry from backend/data/known_hosts.json."
            ) from error
        except asyncssh.PermissionDenied as error:
            methods = await offered_auth_methods(self.host, self.port, self.username)
            accepts = ", ".join(methods) if methods else "none it would tell us about"
            detail = (
                "Check the username and the password; some accounts are set up for keys only."
                if self.password
                else "No password was given, and none of the server's SSH keys were accepted."
            )
            raise UserError(
                f"{self.username}@{self.host} refused the login. The machine accepts: {accepts}. {detail}"
            ) from error

        if not pinned:
            key = self.conn.get_server_host_key()
            if key is not None:
                self.known_hosts.remember(self.host_id, key)
        log.info("logged in: %s@%s", self.username, self.host_id)

    async def close(self) -> None:
        if self.conn is not None:
            self.conn.close()

    # ---------- running things there ----------

    async def run(self, command: str, stdin: Optional[bytes] = None):
        assert self.conn is not None
        if stdin is None:
            return await self.conn.run(command, check=False)
        return await self.conn.run(command, input=stdin, encoding=None, check=False)

    async def install_host(self, status: Callable[[str], Awaitable[None]]) -> str:
        """Makes sure this build of the host program is on the machine; returns its remote path."""
        machine = (await self.run("uname -m")).stdout.strip()
        arch = ARCHES.get(machine)
        if not arch:
            raise UserError(f"webdesk doesn't support this machine's CPU ({machine or 'unknown'}) yet.")

        binary_path = HOST_DIR / f"webdesk-host-linux-{arch}"
        try:
            binary = binary_path.read_bytes()
        except OSError as error:
            raise UserError(
                f"The linux/{arch} host program isn't built. Run \"npm run build\" on the server."
            ) from error

        name = f"webdesk-host-{hashlib.sha256(binary).hexdigest()[:12]}"
        path = f"{REMOTE_DIR}/{name}"
        if (await self.run(f"sh -c 'test -x {path}'")).exit_status == 0:
            return path

        await status("Installing webdesk on the machine...")
        result = await self.run(
            f"sh -c 'mkdir -p {REMOTE_DIR} && rm -f {REMOTE_DIR}/webdesk-* && cat > {path}.tmp "
            f"&& chmod 755 {path}.tmp && mv {path}.tmp {path}'",
            stdin=binary,
        )
        if result.exit_status != 0:
            detail = (result.stderr or b"").decode(errors="replace").strip() or f"exit code {result.exit_status}"
            raise UserError(f"Couldn't install webdesk on the machine: {detail}")
        return path

    async def check_machine(self, host_path: str) -> dict:
        """What `webdesk-host check` reports about this machine."""
        import json

        result = await self.run(f"sh -c '{host_path} check'")
        try:
            return json.loads(result.stdout)
        except ValueError as error:
            detail = last_lines(result.stderr or "") or "webdesk gave no answer"
            raise UserError(f"Couldn't check the machine: {detail}") from error

    async def install_packages(self, check: dict, status: Callable[[str], Awaitable[None]]) -> None:
        """Installs what the machine is missing. The viewer has already approved."""
        tools = ", ".join("a desktop environment" if name == "desktop" else name for name in check.get("missing") or [])
        packages = check.get("packages") or []
        if not packages:
            raise UserError(f"This machine needs {tools}. Install them there and connect again.")
        if not all(PACKAGE_NAME.match(name) for name in packages):
            raise UserError("The machine reported unexpected package names.")

        await status(f"Installing {', '.join(packages)}... This can take a few minutes.")
        # Wait for any other apt on the machine -- automatic updates, usually --
        # instead of falling over on its lock.
        apt = "DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=180"
        script = f"{apt} update -qq && {apt} install -y -qq --no-install-recommends {' '.join(packages)}"
        if check.get("sudo") == "nopasswd":
            result = await self.run(f"sudo -n sh -c '{script}'")
        else:
            result = await self.run(f'sudo -S -p "" sh -c \'{script}\'', stdin=f"{self.password}\n".encode())

        if result.exit_status != 0:
            output = result.stderr or result.stdout or b""
            if isinstance(output, bytes):
                output = output.decode(errors="replace")
            raise UserError(describe_apt_failure(output))
        log.info("installed on %s: %s", self.host, " ".join(packages))

    def can_sudo(self, check: dict) -> bool:
        return check.get("sudo") == "nopasswd" or (check.get("sudo") == "password" and self.password != "")


def describe_apt_failure(output: str) -> str:
    """Turns apt's noise into something worth reading."""
    text = output.strip()
    if re.search(r"Could not get lock|lock-frontend|dpkg frontend lock", text, re.I):
        return (
            "The machine is busy installing something else, so apt is locked -- automatic updates, "
            "usually. Try again in a minute."
        )
    if re.search(r"No space left on device", text, re.I):
        return "The machine has run out of disk space for the install."
    if re.search(r"Unable to locate package|has no installation candidate", text, re.I):
        return f"The machine's package lists don't offer what's needed: {last_lines(text, 1)}"
    if re.search(r"incorrect password|Sorry, try again", text, re.I):
        return "sudo refused the password on that machine."
    return f"Installing failed: {last_lines(text)}"
