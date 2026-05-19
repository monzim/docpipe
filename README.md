# DocPipe

HTML → PDF rendering service in Go, backed by a long-lived headless Chromium pool.

- Synchronous request/response (POST HTML → receive PDF)
- API-key authentication with per-key rate limiting
- Built-in public analytics with persistence: `/v1/stats` + dashboard
- Single binary, no external services — config + analytics in env vars and local files
- OpenAPI 3.1 spec + Swagger UI (dev mode)
- v1 backwards-compatible shim at `/api/html-to-pdf`

The full architectural spec lives in [`project-specification.md`](./project-specification.md).

## Quick start

```bash
# Generate API keys
./scripts/gen-apikey.sh portal internal

# Copy .env.example to .env and paste the DOCPIPE_API_KEYS line
cp .env.example .env

# Run via docker compose
docker compose -f deploy/docker-compose.yml up --build

# Or build the image manually
make docker
docker run -d -p 8080:8080 -e DOCPIPE_API_KEYS=portal:ak_live_... docpipe:latest
```

The service listens on `:8080`.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/convert/html-to-pdf` | Bearer / `X-API-Key` | Convert HTML → PDF |
| `POST` | `/api/html-to-pdf` | Bearer / `X-API-Key` | v1 compat shim (deprecated) |
| `GET`  | `/v1/stats` | Public¹ | Redacted analytics snapshot |
| `GET`  | `/v1/stats/dashboard` | Public¹ | Live HTML dashboard |
| `GET`  | `/healthz` | None | Liveness |
| `GET`  | `/readyz` | None | Readiness (browser health) |
| `GET`  | `/swagger/` | None² | Swagger UI |

¹ Set `DOCPIPE_STATS_PUBLIC=false` to require auth on stats.
² Disabled in production unless `DOCPIPE_ENABLE_SWAGGER=true` or `DOCPIPE_ENV=development`.

### Convert example

```bash
curl -X POST http://localhost:8080/v1/convert/html-to-pdf \
  -H "Authorization: Bearer ak_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "html": "<!doctype html><html><body><h1>Hello</h1></body></html>",
    "filename": "hello.pdf",
    "options": {
      "format": "A4",
      "margin": {"top":"10mm","bottom":"10mm","left":"10mm","right":"10mm"},
      "wait": {"strategy":"networkidle","timeout_ms":5000}
    }
  }' --output hello.pdf
```

Request `application/json` instead of the raw PDF:

```bash
curl ... -H "Accept: application/json" -o response.json
# { "filename":"hello.pdf", "size_bytes":..., "pdf_base64":"...", "duration_ms":..., "request_id":"..." }
```

## Configuration

All settings come from environment variables — see [`.env.example`](./.env.example) for the full list with defaults. The handful that matter most:

| Variable | Default | Notes |
|---|---|---|
| `DOCPIPE_API_KEYS` | *(required)* | Comma-separated `name:secret` |
| `DOCPIPE_RENDER_CONCURRENCY` | `runtime.NumCPU()` | Cap on simultaneous renders |
| `DOCPIPE_RENDER_TIMEOUT` | `30s` | Per-request render budget |
| `DOCPIPE_RATE_LIMIT_RPS` | `5` | Per-key token bucket |
| `DOCPIPE_BROWSER_RECYCLE_AFTER` | `500` | Renders before Chrome restart |
| `DOCPIPE_DATA_DIR` | `./data` | Analytics snapshot directory |
| `DOCPIPE_SNAPSHOT_INTERVAL` | `1h` | Persistence cadence |
| `DOCPIPE_STATS_PUBLIC` | `true` | Toggle public stats |

## Development

```bash
make build         # local binary into bin/docpipe
make test          # unit tests
make test-integration   # requires chromium-browser on PATH
make vet
make docker        # build the Docker image
make docker-up     # docker compose up --build
make soak          # 1000-seq + parallel waves, asserts no zombies (uses Docker)
make openapi-validate   # spectral lint (if installed)
```

The renderer integration tests are gated behind `-tags=integration` and require a local Chromium-class binary. Production-style soak runs via Docker.

### Project layout

```
cmd/docpipe/         entrypoint
internal/
  config/            env loading + validation
  observability/     slog setup
  httpx/             chi router, middleware, error envelope
  render/            chromedp browser pool + PDF action
  analytics/         counters, histogram, RPS, persistence, public view
  auth/              API key store + rate limiter
  handlers/          HTTP handlers (convert, legacy, stats, dashboard, swagger)
  webassets/         go:embed for dashboard.html + openapi.yaml
deploy/              Dockerfile, docker-compose, chrome-flags reference
scripts/             gen-apikey.sh, soak.sh
testdata/            fixtures: minimal, Bangla, CJK, page rules, broken
```

## Soak / regression gate

```bash
make soak                                # 10-minute default
SOAK_SEQ=100 SOAK_DURATION_S=30 make soak # quick smoke
```

Asserts at the end of every run:

- Browser still ready (`/readyz` returns 200)
- Zero zombie processes inside the container
- RSS within 100 MB of baseline (no leak)
- `/v1/stats.totals` reflects all sent requests

## v1 → v2 migration

The legacy `/api/html-to-pdf` accepts the original `{"base64_html": "..."}` payload and returns a PDF named `admit_card.pdf` for the CASCK job portal. Every call emits a `WARN` log with the key name, plus `Deprecation: true` and `Sunset` response headers. Migrate to `/v1/convert/html-to-pdf` and the path will be removed.

## Spec

The full design and rationale is in [`project-specification.md`](./project-specification.md). The OpenAPI 3.1 spec is in [`internal/webassets/openapi.yaml`](./internal/webassets/openapi.yaml).
