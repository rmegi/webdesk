# syntax=docker/dockerfile:1

# The web client: compiled to static files and served by nginx, which also
# passes the signaling WebSocket through to the backend.

FROM node:24-alpine AS build
WORKDIR /src
# TypeScript 7 ships its compiler as a platform-specific optional dependency,
# so optional packages have to stay. --ignore-scripts keeps npm from trying to
# compile the native speedups in ssh2 and ws, which need a C toolchain.
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY frontend/ ./frontend/
RUN npx tsc -p frontend

FROM nginx:alpine AS serve
COPY frontend/nginx.conf /etc/nginx/conf.d/default.conf
COPY frontend/index.html frontend/style.css /usr/share/nginx/html/
COPY --from=build /src/frontend/dist /usr/share/nginx/html/dist
EXPOSE 80
