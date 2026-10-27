# syntax=docker/dockerfile:1
# Build the frontend and binary from this checkout; no published image is required.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS frontend
# Optional heap cap for small build hosts, e.g. --build-arg NODE_OPTIONS=--max-old-space-size=1536
ARG NODE_OPTIONS=
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
COPY locale/definitions.json /src/locale/definitions.json
RUN npm run build:static

FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS backend
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/out/ ./server/webui/dist/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags embedui -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o /scopeweaver ./cmd/artex

FROM python:3.12-slim-bookworm
# Runtime tools and Node 20 for Playwright. Browser installation occurs at build time.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
# Claude Code CLI for the claude-code provider. Sign in once with your own Claude
# subscription: docker compose exec -it scopeweaver claude, then /login. The
# sign-in lives in the claude_home volume; the version is pinned and self-update is off.
ARG CLAUDE_CODE_VERSION=2.1.311
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && claude --version
ENV CLAUDE_CONFIG_DIR=/app/claude-home \
    DISABLE_AUTOUPDATER=1
WORKDIR /app
COPY --from=backend /scopeweaver /app/scopeweaver
# The supervisor forwards SIGTERM and restarts after a self-update.
COPY start.sh /app/start.sh
RUN chmod +x /app/scopeweaver /app/start.sh
COPY skills/ /app/skills/
COPY LICENSE /app/LICENSE
COPY THIRD_PARTY_NOTICES.txt /app/THIRD_PARTY_NOTICES.txt
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
