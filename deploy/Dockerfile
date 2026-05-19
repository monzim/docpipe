# ---------- build stage ----------
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache module downloads in a dedicated layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /out/docpipe ./cmd/docpipe

# ---------- runtime stage ----------
# Why chromedp/headless-shell instead of alpine + chromium:
#
# Alpine 3.20's chromium 131 packaging is broken — chrome_crashpad_handler
# is launched unconditionally and aborts with "--database is required"
# before any flag we pass is honoured. Alpine 3.19 (chromium 121) shows
# the same failure. The chromedp/headless-shell image ships a curated
# headless chromium that's known-good with chromedp and is what the upstream
# project recommends for production.
#
# We add tini (PID 1 zombie reaper — non-negotiable per spec §2/§14),
# Noto fonts for non-Latin scripts, and the binary on top.
FROM chromedp/headless-shell:latest

RUN apt-get update && apt-get install -y --no-install-recommends \
      tini \
      tzdata \
      ca-certificates \
      wget \
      fonts-noto \
      fonts-noto-cjk \
      fonts-noto-color-emoji \
      fonts-noto-extra \
    && rm -rf /var/lib/apt/lists/*

# Non-root user with a stable UID for volume-mount compatibility.
RUN groupadd -g 10001 docpipe \
    && useradd -u 10001 -g docpipe -M -s /usr/sbin/nologin docpipe

WORKDIR /app

COPY --from=builder /out/docpipe /app/docpipe

# Data directory owned by the app user so the snapshot ticker can write.
RUN mkdir -p /app/data/daily && chown -R docpipe:docpipe /app

USER docpipe

ENV DOCPIPE_CHROME_PATH=/headless-shell/headless-shell \
    DOCPIPE_DATA_DIR=/app/data \
    DOCPIPE_HOST=0.0.0.0 \
    DOCPIPE_PORT=8080

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1

# tini as PID 1 — reaps Chrome's grandchild processes. The load-bearing fix
# for the v1 hang (alongside the long-lived allocator).
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/app/docpipe"]
