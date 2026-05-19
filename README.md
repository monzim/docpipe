# DocPipe

HTML → PDF rendering service in Go, backed by a long-lived headless Chromium pool.

- Synchronous request/response (POST base64-encoded HTML → receive PDF)
- API-key authentication with per-key rate limiting
- **Zero-config API keys** — auto-generated and persisted across restarts if you don't supply them
- Built-in public analytics with persistence: `/v1/stats` + dashboard
- Single binary, no external services — config + analytics in env vars and local files
- OpenAPI 3.1 spec + Swagger UI (dev mode)
- v1 backwards-compatible shim at `/api/html-to-pdf`

The full architectural spec lives in [`project-specification.md`](./project-specification.md).

---

## Quick start

```bash
# Zero-config: image generates 10 API keys on first launch and saves them
docker run -d --name docpipe \
  -p 8080:8080 \
  -v docpipe-data:/app/data \
  -e DOCPIPE_ENABLE_SWAGGER=true \
  ghcr.io/monzim/docpipe:latest
```

That's it. The service is now live on `:8080`. On the **next** step you'll fetch the auto-generated keys.

### Or supply your own keys

```bash
./scripts/gen-apikey.sh portal internal   # prints a paste-ready DOCPIPE_API_KEYS line

docker run -d --name docpipe -p 8080:8080 \
  -v docpipe-data:/app/data \
  -e DOCPIPE_API_KEYS="portal:ak_live_...,internal:ak_live_..." \
  ghcr.io/monzim/docpipe:latest
```

When `DOCPIPE_API_KEYS` is set, the service uses *only* those keys and ignores any persisted file. This is the recommended mode for multi-host deployments — same env, same keys, regardless of which container restarts.

---

## Retrieving the auto-generated API keys

If you didn't set `DOCPIPE_API_KEYS`, DocPipe generated 10 keys (`key-01` through `key-10`) on first start and saved them to `/app/data/api-keys.json` inside the container. **Pick whichever method is easier:**

### 1. From the container logs (only printed once, at first generation)

```bash
docker logs docpipe 2>&1 | sed -n '/auto-generated API keys/,/Set DOCPIPE_API_KEYS/p'
```

The first-run banner looks like this:

```
═══════════════════════════════════════════════════════════════════════
 DocPipe auto-generated API keys (saved to /app/data/api-keys.json)
═══════════════════════════════════════════════════════════════════════
  key-01      ak_live_a1b2c3...
  key-02      ak_live_d4e5f6...
  ...
═══════════════════════════════════════════════════════════════════════
 To use: Authorization: Bearer <secret>   or   X-API-Key: <secret>
 Set DOCPIPE_API_KEYS env to override; these are persisted otherwise.
═══════════════════════════════════════════════════════════════════════
```

This banner is only printed when keys are *freshly generated* — not on subsequent restarts.

### 2. From the persisted file (always available)

```bash
# Read the keys JSON straight out of the docker volume
docker exec docpipe cat /app/data/api-keys.json
```

Or, if the container is stopped:

```bash
docker run --rm -v docpipe-data:/data alpine cat /data/api-keys.json
```

The file shape:

```json
{
  "generated_at": "2026-05-20T08:14:33Z",
  "keys": {
    "key-01": "ak_live_a1b2c3...",
    "key-02": "ak_live_d4e5f6...",
    ...
  }
}
```

It's `chmod 0600` (owner-only readable) on disk, since secrets are stored in plaintext so you can recover them.

### 3. Quick one-liner for the first key

```bash
docker exec docpipe sh -c "cat /app/data/api-keys.json" | jq -r '.keys["key-01"]'
```

### Replacing the auto-generated keys

To switch over to your own keys, just stop, set `DOCPIPE_API_KEYS`, and restart:

```bash
docker rm -f docpipe
docker run -d --name docpipe -p 8080:8080 \
  -v docpipe-data:/app/data \
  -e DOCPIPE_API_KEYS="portal:ak_live_yourkey" \
  ghcr.io/monzim/docpipe:latest
```

The persisted `api-keys.json` is left on disk but ignored. The next time you launch without the env var, those persisted keys come back.

---

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

HTML must be **base64-encoded** in the request body. Raw HTML inside JSON is fragile — every quote, newline, or backslash in a `<style>` block can break naive callers. One unambiguous encoding eliminates a whole class of bugs.

```bash
KEY=ak_live_yourkey
HTML='<!doctype html><html><body><h1>Hello</h1></body></html>'
BODY=$(printf '%s' "$HTML" | base64 -w0)

curl -X POST http://localhost:8080/v1/convert/html-to-pdf \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"html_base64\": \"$BODY\",
    \"filename\": \"hello.pdf\",
    \"options\": {
      \"format\": \"A4\",
      \"margin\": {\"top\":\"10mm\",\"bottom\":\"10mm\",\"left\":\"10mm\",\"right\":\"10mm\"},
      \"wait\": {\"strategy\":\"networkidle\",\"timeout_ms\":5000}
    }
  }" --output hello.pdf
```

To receive a JSON envelope with the base64-encoded PDF instead of raw bytes:

```bash
curl ... -H "Accept: application/json" -o response.json
# {"filename":"hello.pdf","size_bytes":...,"pdf_base64":"...","duration_ms":...,"request_id":"..."}
```

---

## Configuration

All settings come from environment variables — see [`.env.example`](./.env.example) for the full list with defaults.

| Variable | Default | Notes |
|---|---|---|
| `DOCPIPE_API_KEYS` | *(optional — auto-gen if unset)* | Comma-separated `name:secret` |
| `DOCPIPE_RENDER_CONCURRENCY` | `runtime.NumCPU()` | Cap on simultaneous renders |
| `DOCPIPE_RENDER_TIMEOUT` | `30s` | Per-request render budget |
| `DOCPIPE_RATE_LIMIT_RPS` | `5` | Per-key token bucket |
| `DOCPIPE_BROWSER_RECYCLE_AFTER` | `500` | Renders before Chrome restart |
| `DOCPIPE_DATA_DIR` | `./data` | Analytics + api-keys.json directory |
| `DOCPIPE_SNAPSHOT_INTERVAL` | `1h` | Analytics persistence cadence |
| `DOCPIPE_STATS_PUBLIC` | `true` | Toggle public stats |
| `DOCPIPE_ENABLE_SWAGGER` | `false` | Force-enable Swagger UI in production |
| `DOCPIPE_SWAGGER_SERVERS` | *(empty)* | Extra servers for the Swagger UI dropdown, prepended to the YAML defaults. Format: comma-separated `URL[\|Description]`. |

---

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

### Project layout

```
cmd/docpipe/         entrypoint
internal/
  config/            env loading + validation
  observability/     slog setup
  httpx/             chi router, middleware, error envelope
  render/            chromedp browser pool + PDF action
  analytics/         counters, histogram, RPS, persistence, public view
  auth/              API key store, persistence (api-keys.json), rate limiter
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

The legacy `/api/html-to-pdf` accepts the original `{"base64_html": "..."}` payload (note the field name swap vs v2's `html_base64`) and returns a PDF named `admit_card.pdf` for the CASCK job portal. Every call emits a `WARN` log with the key name, plus `Deprecation: true` and `Sunset` response headers. Migrate to `/v1/convert/html-to-pdf` and the path will be removed.

## Spec

The full design and rationale is in [`project-specification.md`](./project-specification.md). The OpenAPI 3.1 spec is in [`internal/webassets/openapi.yaml`](./internal/webassets/openapi.yaml).
