import json
from typing import Optional

import asyncssh

from utils.config import KNOWN_HOSTS_FILE


class KnownHosts:
    """Pins each machine's host key on first use, like ssh's known_hosts.

    The key is kept in OpenSSH format so asyncssh can check it during the
    handshake, rather than after credentials have already been sent.
    """

    def __init__(self, path=KNOWN_HOSTS_FILE):
        self.path = path

    def _read(self) -> dict:
        try:
            with open(self.path, encoding="utf-8") as handle:
                return json.load(handle)
        except (OSError, ValueError):
            return {}

    def get(self, host_id: str) -> Optional[str]:
        """The pinned key for host:port, in OpenSSH format, if there is one."""
        entry = self._read().get(host_id)
        if isinstance(entry, dict):
            return entry.get("key")
        return None

    def remember(self, host_id: str, key: asyncssh.SSHKey) -> None:
        hosts = self._read()
        hosts[host_id] = {
            "key": key.export_public_key("openssh").decode().strip(),
            "fingerprint": key.get_fingerprint(),
        }
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with open(self.path, "w", encoding="utf-8") as handle:
            json.dump(hosts, handle, indent=2)
            handle.write("\n")

    @staticmethod
    def as_known_hosts(host: str, port: int, key: str) -> bytes:
        """One known_hosts line for this machine, which asyncssh matches against."""
        pattern = host if port == 22 else f"[{host}]:{port}"
        return f"{pattern} {key}\n".encode()
