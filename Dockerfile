# syntax=docker/dockerfile:1

# The three parts are built separately and end up in one image: the server is
# the only thing that runs, and it needs the other two as files it serves and
# uploads.
#
# Build it on each machine and you get that machine's architecture: arm64 on a
# Mac, amd64 on an Ubuntu box. The host binaries are cross-compiled for both
# regardless, because target machines are not the machine running the server.

# --- frontend: TypeScript compiled to frontend/dist ---
FROM node:24-alpine AS frontend
WORKDIR /src
# TypeScript 7 ships its compiler as a platform-specific optional dependency,
# so optional packages have to stay. --ignore-scripts keeps npm from trying to
# compile the native speedups in ssh2 and ws, which need a C toolchain.
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY frontend/ ./frontend/
RUN npx tsc -p frontend

# --- host: the static binaries the server uploads to target machines ---
FROM golang:1.27-alpine AS host
WORKDIR /src
COPY host/go.mod host/go.sum ./
RUN go mod download
COPY host/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/webdesk-host-linux-amd64 . \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/webdesk-host-linux-arm64 .

# --- backend: the server, plus what the two stages above produced ---
FROM node:24-alpine AS backend
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT=8080
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --omit=optional --ignore-scripts --no-audit --no-fund \
 && npm cache clean --force
COPY backend/ ./backend/
COPY frontend/index.html frontend/style.css ./frontend/
COPY --from=frontend /src/frontend/dist ./frontend/dist
COPY --from=host /src/dist ./host/dist
# Host keys are pinned here; compose mounts a volume so they survive a rebuild.
RUN mkdir -p backend/data
EXPOSE 8080
CMD ["node", "backend/server.ts"]
