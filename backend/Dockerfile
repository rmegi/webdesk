# syntax=docker/dockerfile:1

# The server: SSH login, machine setup and signaling. It carries the host
# program, which it uploads to each target machine over SSH, so that is built
# here rather than being a service of its own.
#
# Build the image on each machine you run it on and it takes that machine's
# architecture. The host binaries are cross-compiled for both either way,
# because the machines being controlled are not the machine running this.

FROM golang:1.27-alpine AS host
WORKDIR /src
COPY host/go.mod host/go.sum ./
RUN go mod download
COPY host/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/webdesk-host-linux-amd64 . \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/webdesk-host-linux-arm64 .

# Slim rather than Alpine: asyncssh pulls in cryptography, which has manylinux
# wheels but would need a Rust toolchain to build against musl.
FROM python:3.13-slim AS serve
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOST=0.0.0.0 PORT=8080
COPY backend/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt && rm /tmp/requirements.txt
COPY backend/app/ /app/backend/app/
COPY --from=host /src/dist /app/host/dist
# Host keys are pinned here; compose mounts a volume so they survive a rebuild.
RUN mkdir -p /app/backend/data
# app/ is the import root: modules refer to each other as routes.x, handlers.x,
# utils.x, so that is where uvicorn has to run from.
WORKDIR /app/backend/app
EXPOSE 8080
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8080", "--log-level", "warning"]
