import logging

from fastapi import APIRouter, HTTPException, Query, Request

from utils import sessions
from utils.config import MAX_UPLOAD_BYTES
from utils.errors import UserError

log = logging.getLogger("webdesk")

router = APIRouter()


@router.post("/upload/{session_id}")
async def upload(session_id: str, request: Request, name: str = Query(min_length=1)) -> dict:
    """Writes a dropped file to the machine this session is connected to.

    The body is streamed straight through to SFTP, so the file is never held
    here. The session id is the authority: it is handed to one browser when its
    session opens, and dies with it.
    """
    handler = sessions.get(session_id)
    if handler is None:
        raise HTTPException(status_code=404, detail="That session isn't open any more.")

    declared = request.headers.get("content-length")
    if declared is not None and declared.isdigit() and int(declared) > MAX_UPLOAD_BYTES:
        raise HTTPException(
            status_code=413, detail=f"That file is larger than the {MAX_UPLOAD_BYTES // 1024**3} GB limit."
        )

    try:
        path = await handler.ssh.upload(name, request.stream())
    except UserError as error:
        raise HTTPException(status_code=400, detail=str(error)) from error
    except Exception as error:  # noqa: BLE001 - the viewer gets a line either way
        log.info("upload failed: %r", error)
        raise HTTPException(status_code=500, detail="Couldn't put that file on the machine.") from error

    return {"path": path}
