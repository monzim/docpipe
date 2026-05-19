# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

This is **DocPipe v2** — a Go HTML→PDF service backed by a long-lived headless Chromium pool. The v1 single-file implementation was replaced wholesale in the v2 build-out; the design rationale and the v1 hang post-mortem live in `project-specification.md` (the spec is still the source of truth for env vars, schemas, error codes, and what was deliberately deferred).

Module path: `github.com/monzim/docpipe`.

## Common commands

```bash
make build               # local binary into bin/docpipe
make test                # unit tests
make test-integration    # requires chromium-browser in PATH; runs render package E2E
make vet
make docker              # build the Docker image
make docker-up           # docker compose up --build
make soak                # docker-based load test asserting no zombies
make openapi-validate    # spectral lint (optional)
./scripts/gen-apikey.sh portal internal   # generate API keys to paste into .env
```

Single-package tests: `go test ./internal/analytics/...`. Integration tests are tagged: `go test -tags=integration ./internal/render/...`.

## Architecture invariants

These shape every design decision in the codebase. Don't relitigate them without re-reading the spec section noted in each.

- **Single Chrome, many tabs (spec §8).** One `chromedp.ExecAllocator` lives for the process lifetime. `Browser.parentCtx` is the `chromedp.NewContext(allocCtx)` that owns the browser process. Each render derives a tab via `chromedp.NewContext(parentCtx)` and cancels the tab — never the allocator. Concurrency is capped by `cfg.Concurrency` via a semaphore in `Browser.sem`. A supervisor goroutine probes every `HealthCheckInterval` and triggers `recycle()` on failure or after `RecycleAfter` renders.
- **The initial chromedp.Run must use parentCtx directly, not a child of it.** chromedp binds the browser process lifetime to whichever context first runs against it. If you wrap `parentCtx` with `context.WithTimeout` and pass that to the launch probe, the deferred cancel will kill the browser. `Browser.spawn` uses a `select` with `time.After` instead — preserve that pattern.
- **No external dependencies (spec §2/§7).** No Postgres, Redis, S3, brokers. Config and API keys in env vars. Analytics persist to `./data/state.json` via atomic-rename snapshots (`internal/analytics/store.go`). Daily rollups in `./data/daily/YYYY-MM-DD.json`.
- **Non-blocking analytics on hot path (spec §7.1).** Recording is `atomic.Add` and channel-buffered. Drop latency samples on backpressure rather than block the renderer.
- **Atomic, versioned snapshot writes.** `state.json.tmp` → `fsync` → `rename`. `SchemaVersion` in `internal/analytics/snapshot.go`; bump it when the on-disk shape changes, otherwise add fields with safe defaults.
- **Histograms survive restarts; rolling 1h/24h windows do not (spec §7.6).** Lifetime totals, max latency, all-time peak RPS keep climbing across restarts. The `windows.last_*` views refill after a restart — `BuildPublicView` exposes `coverage_seconds` so the dashboard can show "based on X minutes of data."
- **Load HTML via `Page.setDocumentContent`** on the root frame (`internal/render/pdf.go` → `loadHTML`). Do NOT load by assigning to `document.body`'s inner HTML — that pattern drops `<head>`, doctype, and stylesheet links. v1 made this mistake.
- **Wait-strategy listeners must attach BEFORE the action that fires the event.** `setDocumentContent` fires `Page.loadEventFired` synchronously inside the call. `runRender` in `pdf.go` installs the listener via `startWait` *before* calling `loadHTML` for this reason — keep that ordering.
- **Public stats redact (spec §6, §12).** `internal/analytics/public.go` is the single redaction boundary. Never serialise `*Recorder` to the network — only the `PublicView` struct. No per-key counts, key names, request IDs, client IPs, or hostnames cross that line.
- **Middleware chain order (spec §9).** `recover → requestID → logger → bodyLimit → CORS → auth → rateLimit → analytics → timeout → handler`. Analytics sits *after* auth/rate-limit so 401/429 traffic doesn't pollute success metrics. `/healthz`, `/readyz`, `/v1/stats`, `/v1/stats/dashboard` are skipped via the analytics-middleware skip set.
- **Commits must be authored as the user with no `Co-Authored-By` trailer.** Standing instruction from the user — overrides the default Claude Code commit guidance.

## Container constraints

- **`tini` as PID 1 is non-negotiable.** It reaps Chrome's grandchild processes. Without it, the long-lived allocator alone does not fully solve the v1 hang. Spec §8/§14.
- **Runtime base is `chromedp/headless-shell`, not Alpine + chromium.** Alpine 3.19 and 3.20's chromium packages have a broken `chrome_crashpad_handler` that aborts on every invocation. The `deploy/Dockerfile` documents this. Don't switch back to alpine + chromium without verifying the crashpad issue is resolved.
- **Non-Latin scripts** (Bangla, CJK, emoji) require the Noto font packages. The Dockerfile installs `fonts-noto fonts-noto-cjk fonts-noto-color-emoji fonts-noto-extra`. Verify with the fixtures in `testdata/`.
- **`--disable-dev-shm-usage`** is mandatory for containerized Chromium — `/dev/shm` is tiny in containers. Other required flags listed in `internal/render/browser.go` and mirrored in `deploy/chrome-flags.txt`.

## API contract (v2)

`POST /v1/convert/html-to-pdf` requires `html_base64` — a base64-encoded HTML string. **Raw HTML is not accepted.** The `html` field was dropped in the second v2 iteration because inline CSS quotes/newlines/backslashes break naive JSON callers; base64 eliminates the encoding ambiguity entirely. If you add another input format, mirror the lesson — pick one unambiguous wire format per field.

## API key lifecycle

Keys come from one of two sources, in order:

1. **`DOCPIPE_API_KEYS` env var** (explicit). When set, this is authoritative and the persisted file is ignored entirely. Use this for multi-host / auto-scaled deployments where every instance needs to honor the same key set regardless of volume state.
2. **`${DOCPIPE_DATA_DIR}/api-keys.json`** (auto-generated, persisted). When the env is unset, the service generates `AutoGenCount` (10) keys on first startup, writes them to disk at `chmod 0600`, and reuses them on every subsequent restart. A first-run banner prints the secrets to stderr exactly once so an operator can grab them. The persisted file is the only place plaintext secrets live; `auth.Store` only ever holds SHA-256 hashes.

Implementation: `internal/auth/persist.go` (`LoadOrGenerate`). Wired in `cmd/docpipe/main.go` before any handler is constructed. If the persisted file is corrupt, the service archives it as `api-keys.json.broken.<ts>` and regenerates — keys reset, but the service stays up.

## v1 compat shim

`POST /api/html-to-pdf` is preserved for the CASCK job portal (`apply.casckjobs.org`). It accepts the v1 `{"base64_html": "..."}` payload (note the field-name difference vs v2's `html_base64`) and returns a PDF named `admit_card.pdf`. Every call:

- Emits a `WARN` log (`legacy_endpoint_used`) with the key name and remote address.
- Sets `Deprecation: true`, `Sunset`, and `Link: rel="successor-version"` response headers.
- Goes through the same auth + rate-limit + analytics chain as `/v1/convert/*`.

Don't remove this until the portal is cut over. The handler lives in `internal/handlers/legacy.go`.

## Files of note

- `internal/render/browser.go` — Chromium allocator + supervisor lifecycle. The hang fix lives here.
- `internal/render/pdf.go` — `loadHTML` + wait-strategy + `PrintToPDF`. Read before changing render mechanics.
- `internal/analytics/store.go` — disk persistence with atomic write + corrupt-archive recovery.
- `internal/analytics/public.go` — redacted public view. The redaction boundary.
- `internal/handlers/legacy.go` — v1 compat shim with deprecation headers.
- `internal/webassets/dashboard.html` — embedded vanilla-JS dashboard. No frameworks, no CDN.
- `deploy/Dockerfile` — `tini` + chromedp/headless-shell + Noto fonts.
- `scripts/soak.sh` — load test that asserts no zombies, no leaks, `/readyz` still ok.
- `project-specification.md` — the source of truth for env vars, error codes, OpenAPI shape, snapshot schema, milestones, and what was deliberately deferred.
