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

FROM node:24-alpine AS serve
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT=8080
# ssh2 and ws ship optional native speedups; the pure JS fallbacks are fine and
# save a C toolchain in the image.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --omit=optional --ignore-scripts --no-audit --no-fund \
 && npm cache clean --force
COPY backend/ ./backend/
COPY --from=host /src/dist ./host/dist
# Host keys are pinned here; compose mounts a volume so they survive a rebuild.
RUN mkdir -p backend/data
EXPOSE 8080
CMD ["node", "backend/server.ts"]
