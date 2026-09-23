import asyncio
import socket

import asyncssh


class UserError(Exception):
    """An error whose message is written for the person at the browser."""


def last_lines(text: str, count: int = 3) -> str:
    return " ".join(text.strip().split("\n")[-count:])


def is_local_network_host(host: str) -> bool:
    """RFC 1918 and link-local IPv4 addresses, and mDNS .local names."""
    if host.endswith(".local"):
        return True
    parts = host.split(".")
    if len(parts) != 4 or not (parts[0].isdigit() and parts[1].isdigit()):
        return False
    first, second = int(parts[0]), int(parts[1])
    return (
        first == 10
        or (first == 192 and second == 168)
        or (first == 172 and 16 <= second <= 31)
        or (first == 169 and second == 254)
    )


def describe_error(error: Exception, host: str, port: int, username: str, on_mac: bool) -> str:
    """Turns whatever went wrong into a line the person at the browser can act on."""
    if isinstance(error, UserError):
        return str(error)
    if isinstance(error, asyncssh.PermissionDenied):
        return (
            f"{username}@{host} refused the login. Check the username and the password; "
            "some accounts are set up for keys only."
        )
    # ConnectionRefusedError is an OSError, so it has to be tested before one.
    if isinstance(error, ConnectionRefusedError):
        return f"Nothing is accepting SSH connections on {host}:{port}."
    if isinstance(error, socket.gaierror):
        return f"Can't find a machine called {host}."
    if isinstance(error, (asyncio.TimeoutError, TimeoutError, OSError)):
        if on_mac and is_local_network_host(host):
            # macOS answers this instantly when the app running the server lacks
            # Local Network permission, and ssh or ping still working doesn't
            # rule it out because built-in tools are exempt.
            return (
                f"Couldn't reach {host}:{port}. If it's on your local network, macOS may be blocking "
                "this server: allow the app that runs it in System Settings -> Privacy & Security -> "
                "Local Network, then restart the server."
            )
        return f"Couldn't reach {host}:{port}."
    return str(error) or repr(error)
