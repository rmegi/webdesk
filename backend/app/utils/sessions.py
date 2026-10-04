"""The live sessions, by id.

An upload arrives as its own HTTP request, so it has to find the SSH
connection it belongs to. The id is handed to one browser when its session
opens and is never written down anywhere else.
"""

from typing import Dict, Optional

_live: Dict[str, object] = {}


def register(session_id: str, handler: object) -> None:
    _live[session_id] = handler


def unregister(session_id: str) -> None:
    _live.pop(session_id, None)


def get(session_id: str) -> Optional[object]:
    return _live.get(session_id)
