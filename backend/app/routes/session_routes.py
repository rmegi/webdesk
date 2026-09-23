import asyncio
import json
from typing import Optional

from fastapi import APIRouter, WebSocket, WebSocketDisconnect
from pydantic import BaseModel, Field, ValidationError

from handlers.session_handler import SessionHandler

router = APIRouter()


class Target(BaseModel):
    """The machine the viewer asked for, as the page sends it."""

    host: str = Field(min_length=1)
    port: int = Field(default=22, ge=1, le=65535)
    username: str = Field(min_length=1)
    password: str = ""
    # The viewer's window, used to size a new virtual desktop.
    size: Optional[str] = Field(default=None, pattern=r"^\d{3,5}x\d{3,5}$")


@router.websocket("/ws")
async def session(websocket: WebSocket) -> None:
    await websocket.accept()

    # The first message names the machine; everything after it is session traffic.
    try:
        first = json.loads(await websocket.receive_text())
        if first.get("type") != "connect":
            raise ValueError("expected connect")
        target = Target(**{key: value for key, value in first.items() if key != "type"})
    except (ValidationError, ValueError, KeyError, WebSocketDisconnect):
        await websocket.close(code=4002, reason="expected connect")
        return

    handler = SessionHandler(websocket, target.model_dump())
    started = asyncio.create_task(handler.start())
    try:
        while True:
            message = json.loads(await websocket.receive_text())
            await handler.handle_viewer_message(message)
    except (WebSocketDisconnect, ValueError, RuntimeError):
        pass
    finally:
        await handler.end()
        started.cancel()
