import asyncio
import json
import logging
import sys
from typing import Optional

import asyncssh
from fastapi import WebSocket

from handlers.ssh_handler import SshHandler
from utils.config import ICE_SERVERS, RELAYED
from utils.errors import UserError, describe_error

log = logging.getLogger("webdesk")


class SessionHandler:
    """One viewer: logs in to the machine they asked for, starts the host program
    there and relays signaling between the two.

    Video and input never come through here. Once the browser and the machine
    have been introduced they talk to each other directly over WebRTC.
    """

    def __init__(self, websocket: WebSocket, target: dict):
        self.ws = websocket
        self.host = target["host"]
        self.port = target["port"]
        self.username = target["username"]
        self.password = target["password"]
        self.size = target.get("size")
        self.label = f"{self.username}@{self.host}:{self.port}"

        self.ssh = SshHandler(self.host, self.port, self.username, self.password)
        self.process: Optional[asyncssh.SSHClientProcess] = None
        self.approved = asyncio.Event()  # the viewer pressed "Install and connect"
        self.ended = False

    # ---------- talking to the viewer ----------

    async def send(self, message: dict) -> None:
        try:
            await self.ws.send_text(json.dumps(message))
        except Exception:  # the viewer is gone; nothing left to tell them
            pass

    async def status(self, message: str) -> None:
        await self.send({"type": "status", "message": message})

    async def end(self, error: Optional[str] = None) -> None:
        if self.ended:
            return
        self.ended = True
        self.approved.set()  # a session waiting on setup notices it ended and stops
        if error:
            await self.send({"type": "error", "message": error})
        # Closing the host program's stdin tells it to stop; give it a moment to
        # clean up and log before dropping the SSH connection.
        if self.process is not None:
            try:
                self.process.stdin.write_eof()
                await asyncio.wait_for(self.process.wait_closed(), timeout=5)
            except Exception:
                pass
        await self.ssh.close()

    # ---------- the session ----------

    async def start(self) -> None:
        try:
            await self.status(f"Connecting to {self.host}...")
            await self.ssh.connect()
            if self.ended:
                return

            await self.status("Checking the machine...")
            host_path = await self.ssh.install_host(self.status)
            check = await self.ssh.check_machine(host_path)

            if check.get("missing"):
                await self.set_up_machine(check)
                if self.ended:
                    return
                check = await self.ssh.check_machine(host_path)
                if check.get("missing"):
                    raise UserError(f"Still missing after installing: {', '.join(check['missing'])}.")
            if self.ended:
                return

            await self.status("Starting webdesk on the machine...")
            assert self.ssh.conn is not None
            self.process = await self.ssh.conn.create_process(f"sh -c 'exec {host_path}'")
            await self.write_to_host({"type": "config", "iceServers": ICE_SERVERS, "virtualSize": self.size})
            await asyncio.gather(self.read_host_stdout(), self.read_host_stderr())
        except Exception as error:  # noqa: BLE001 - every failure becomes a line for the viewer
            if not self.ended:
                log.info("session failed for %s: %r", self.label, error)
                await self.end(describe_error(error, self.host, self.port, self.username, sys.platform == "darwin"))

    async def set_up_machine(self, check: dict) -> None:
        """Asks the viewer before installing anything, then installs it."""
        tools = ", ".join(
            "a desktop environment" if name == "desktop" else name for name in check.get("missing") or []
        )
        packages = check.get("packages") or []
        if not packages:
            raise UserError(f"This machine needs {tools}. Install them there and connect again.")
        if not self.ssh.can_sudo(check):
            joined = " ".join(packages)
            raise UserError(
                f"This machine needs {tools}. Run this there, then connect again: "
                f"sudo apt-get install --no-install-recommends {joined}"
            )

        await self.send({"type": "setup", "tools": tools, "packages": packages, "virtual": check.get("virtual", False)})
        await self.approved.wait()
        if self.ended:
            return
        await self.ssh.install_packages(check, self.status)

    # ---------- relaying ----------

    async def write_to_host(self, message: dict) -> None:
        if self.process is not None:
            self.process.stdin.write(json.dumps(message) + "\n")

    async def handle_viewer_message(self, message: dict) -> None:
        if message.get("type") == "install":
            self.approved.set()
            return
        if message.get("type") in RELAYED:
            await self.write_to_host(message)

    async def read_host_stdout(self) -> None:
        """The host program answers in newline-delimited JSON."""
        assert self.process is not None
        async for line in self.process.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except ValueError:
                log.info("[%s] %s", self.label, line)
                continue

            kind = message.get("type")
            if kind == "ready":
                await self.status("Opening the screen...")
                await self.send(
                    {
                        "type": "joined",
                        "iceServers": ICE_SERVERS,
                        "virtual": message.get("virtual") is True,
                        "warning": message.get("warning"),
                    }
                )
            elif kind == "error":
                await self.end(message.get("message"))
                return
            elif kind == "bye":
                await self.send(message)
                await self.end()
                return
            else:
                await self.send(message)  # status, answer, candidate

        if not self.ended:
            await self.end("webdesk stopped on the machine.")

    async def read_host_stderr(self) -> None:
        assert self.process is not None
        async for line in self.process.stderr:
            line = line.strip()
            if line:
                log.info("[%s] %s", self.label, line)
