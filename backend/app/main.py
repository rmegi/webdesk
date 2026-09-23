# webdesk server: for each viewer, logs in to the requested machine over SSH,
# starts the host program there and relays WebRTC signaling between browser and
# host. Video and input flow peer-to-peer.
import logging

from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from routes.session_routes import router as session_router

from utils.config import FRONTEND_ROOT, HOST, PORT

logging.basicConfig(level=logging.INFO, format="%(message)s")
# asyncssh logs every channel it opens and closes at INFO, which buries the
# lines the host program sends us.
logging.getLogger("asyncssh").setLevel(logging.WARNING)

app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
app.include_router(session_router)

# In Docker nginx serves the page and this never gets used. Running from source
# there is no nginx, so serve it here as well.
if FRONTEND_ROOT.is_dir():
    app.mount("/", StaticFiles(directory=str(FRONTEND_ROOT), html=True), name="frontend")


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=HOST, port=PORT, log_level="warning")
