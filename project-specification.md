# DocPipe v2 — Project Specification (rev. 2)

A dedicated HTML → PDF rendering service in Go, backed by a long-lived headless Chromium pool. v1 works for small bursts but hangs under sustained load because every request spawns a fresh Chrome process and leaks resources. v2 fixes that, adds auth, observability, an embedded analytics module with public stats, and the operational polish needed for a service you can leave running.

**No external dependencies.** No Postgres, no Redis, no S3. Everything (config, keys, analytics) lives in env vars or local files. Analytics persist via hourly snapshots to disk.

---

## 1. Goals and non-goals

**Goals**
- Convert HTML (base64 or raw) to PDF with predictable latency.
- Stay healthy under sustained load — no Chrome zombies, no FD exhaustion, no hangs.
- API-key authenticated, multi-tenant friendly (per-key limits, per-key logging).
- Self-documenting via Swagger/OpenAPI.
- **Public analytics endpoint** with totals, latencies, RPS, uptime — no auth required.
- Containerised, single binary, no external services.

**Non-goals (for v2)**
- Async job queue / webhook callbacks. v2 stays synchronous.
- DOCX/XLSX/image conversion. Scope stays HTML→PDF.
- Template engine on the server. Caller sends final rendered HTML.
- External databases or message brokers of any kind.

---

## 2. Root cause of the v1 hang

Three things compound:

1. **Per-request `chromedp.NewContext(context.Background())`** with no parent allocator creates a brand-new Chrome process every call. Under sustained traffic, processes don't always exit cleanly — `cancel()` returns before Chrome's child processes (renderer, GPU, zygote) are fully reaped. Zombies pile up until `fork()` starts failing or FDs are exhausted, and the next request blocks forever waiting on a Chrome that will never start.
2. **No init system in the container.** Your v1 Dockerfile runs the Go binary as PID 1. Linux gives PID 1 special responsibilities — reaping orphaned children — and the Go binary doesn't do that. Even if Chrome exits cleanly, its grandchild processes become orphans that never get reaped. Combined with #1, this is fatal.
3. **No concurrency cap and a fixed 10 s sleep.** 50 concurrent requests = 50 Chromes = OOM. Each holds for 10 s minimum, multiplying the pressure.

v2 fixes all three: one long-lived allocator, a tab pool with a hard concurrency cap, event-based waiting, and `tini` as PID 1.

---

## 3. Architecture overview

```
┌─────────────┐    ┌──────────────────────────────────────────┐
│   Client    │──▶│  HTTP server (chi/gorilla)                │
└─────────────┘    │   ├─ middleware: reqID, log, recover,    │
                   │   │   CORS, auth, rate-limit, body-size, │
                   │   │   analytics-record                   │
                   │   ├─ /healthz, /readyz                   │
                   │   ├─ /v1/convert/html-to-pdf  (auth)     │
                   │   ├─ /v1/stats                (public)   │
                   │   ├─ /v1/stats/dashboard      (public)   │
                   │   └─ /swagger/*                          │
                   └────────────────┬─────────────────────────┘
                                    │
                  ┌─────────────────┼─────────────────┐
                  ▼                 ▼                 ▼
        ┌─────────────────┐ ┌────────────────┐ ┌──────────────┐
        │ Renderer pool   │ │ Analytics      │ │ API key store│
        │ (1 Chrome, N    │ │ (in-memory +   │ │ (env-loaded, │
        │  tabs, sem)     │ │  hourly flush) │ │  in-memory)  │
        └─────────────────┘ └────────┬───────┘ └──────────────┘
                                     │
                          ┌──────────▼──────────┐
                          │ ./data/             │
                          │   state.json        │ ← snapshot, atomic write
                          │   daily/2026-05-19.json
                          │   daily/2026-05-18.json
                          └─────────────────────┘
```

Single Chrome process, many tabs. A semaphore bounds concurrency. A supervisor goroutine restarts Chrome if it wedges. Analytics are computed in-memory and snapshotted to JSON every hour with atomic rename. On startup, the snapshot is replayed.

---

## 4. Project layout

```
docpipe/
├── cmd/
│   └── docpipe/
│       └── main.go              # entrypoint
├── internal/
│   ├── config/
│   │   └── config.go            # env loading + validation
│   ├── auth/
│   │   └── apikey.go            # API key middleware + store
│   ├── httpx/
│   │   ├── server.go            # router, middleware, graceful shutdown
│   │   ├── middleware.go        # reqID, logging, recover, body-limit, rate-limit
│   │   ├── errors.go            # uniform JSON error envelope
│   │   └── response.go
│   ├── render/
│   │   ├── browser.go           # ExecAllocator lifecycle, supervisor
│   │   ├── pool.go              # tab semaphore
│   │   ├── pdf.go               # HTML → PDF action
│   │   └── options.go           # PDF option mapping
│   ├── analytics/
│   │   ├── recorder.go          # atomic counters, histograms, ring buffers
│   │   ├── snapshot.go          # JSON marshal/unmarshal of state
│   │   ├── store.go             # disk I/O, atomic writes, rotation
│   │   ├── rps.go               # sliding window RPS tracker
│   │   └── public.go            # public-facing view (redaction)
│   ├── handlers/
│   │   ├── convert.go           # /v1/convert/html-to-pdf
│   │   └── stats.go             # /v1/stats, /v1/stats/dashboard
│   └── observability/
│       └── logger.go            # slog setup
├── web/
│   └── dashboard.html           # embedded via go:embed
├── api/
│   └── openapi.yaml             # source of truth
├── docs/                         # served via Swagger UI
├── deploy/
│   ├── Dockerfile               # Alpine, multi-stage
│   ├── docker-compose.yml
│   └── chrome-flags.txt
├── data/                         # gitignored — runtime state
│   ├── state.json
│   └── daily/
├── scripts/
│   └── gen-apikey.sh
├── .env.example
├── Makefile
├── go.mod
└── README.md
```

---

## 5. Configuration (env-driven)

| Variable | Default | Notes |
|---|---|---|
| `DOCPIPE_PORT` | `8080` | HTTP listen port |
| `DOCPIPE_HOST` | `0.0.0.0` | Bind address |
| `DOCPIPE_ENV` | `production` | `development` enables verbose logs + Swagger UI |
| `DOCPIPE_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `DOCPIPE_LOG_FORMAT` | `json` | `json\|console` |
| `DOCPIPE_API_KEYS` | *(required)* | Comma-separated `name:key` pairs |
| `DOCPIPE_CORS_ORIGINS` | `` | Comma-separated exact-match origins |
| `DOCPIPE_MAX_BODY_BYTES` | `10485760` | 10 MB |
| `DOCPIPE_RENDER_TIMEOUT` | `30s` | Per-request render timeout |
| `DOCPIPE_RENDER_CONCURRENCY` | `runtime.NumCPU()` | Max simultaneous renders |
| `DOCPIPE_RATE_LIMIT_RPS` | `5` | Per API key, token bucket |
| `DOCPIPE_RATE_LIMIT_BURST` | `10` | |
| `DOCPIPE_CHROME_PATH` | `/usr/bin/chromium-browser` | Override Chrome binary |
| `DOCPIPE_BROWSER_RECYCLE_AFTER` | `500` | Renders before allocator restart |
| `DOCPIPE_BROWSER_HEALTHCHECK_INTERVAL` | `30s` | Supervisor probe interval |
| `DOCPIPE_DATA_DIR` | `./data` | Analytics persistence directory |
| `DOCPIPE_SNAPSHOT_INTERVAL` | `1h` | Analytics flush cadence |
| `DOCPIPE_DAILY_RETENTION_DAYS` | `30` | Days of per-day history kept on disk |
| `DOCPIPE_ENABLE_SWAGGER` | `false` | Force-enable Swagger in production |
| `DOCPIPE_STATS_PUBLIC` | `true` | Toggle public visibility of stats |

API keys: load as `name:secret`. The `name` attributes logs/metrics without leaking the secret. Generate with `openssl rand -hex 32` prefixed (`ak_live_…`).

---

## 6. HTTP API

### `POST /v1/convert/html-to-pdf` — auth required

**Headers**
- `Authorization: Bearer <api-key>` *(or `X-API-Key: <api-key>`)*
- `Content-Type: application/json`
- `Accept: application/pdf` *(default)* or `application/json` for base64-wrapped response
- `X-Request-ID: <id>` *(optional; server generates if absent)*

**Request body**
```json
{
  "html": "<!doctype html>...",
  "html_base64": "PCFkb2N0eXBlIGh0bWw+...",
  "filename": "report.pdf",
  "options": {
    "format": "A4",
    "landscape": false,
    "scale": 1.0,
    "print_background": true,
    "prefer_css_page_size": false,
    "margin": { "top": "10mm", "right": "10mm", "bottom": "10mm", "left": "10mm" },
    "header_template": "<div style='font-size:8px'></div>",
    "footer_template": "<div style='font-size:8px'>Page <span class='pageNumber'></span> / <span class='totalPages'></span></div>",
    "display_header_footer": false,
    "page_ranges": "",
    "wait": {
      "strategy": "networkidle",
      "timeout_ms": 15000,
      "selector": null
    }
  }
}
```

Either `html` or `html_base64` — never both. `wait.strategy` is one of `load|networkidle|selector|none`.

**Default response** — `200 OK`, `Content-Type: application/pdf`, raw bytes, `Content-Disposition: attachment; filename="<filename>"`.

**Alternate response** when `Accept: application/json`:
```json
{
  "filename": "report.pdf",
  "size_bytes": 48213,
  "pdf_base64": "JVBERi0xLjQK...",
  "duration_ms": 842,
  "request_id": "01HV..."
}
```

### `GET /v1/stats` — public, no auth

Returns the public-safe analytics snapshot. Schema:

```json
{
  "service": {
    "name": "DocPipe",
    "version": "2.0.0",
    "started_at": "2026-05-18T09:14:33Z",
    "uptime_seconds": 92847,
    "status": "healthy"
  },
  "totals": {
    "requests": 184729,
    "pdfs_generated": 182104,
    "failures": 2625,
    "success_rate": 0.9858,
    "bytes_in": 4823917284,
    "bytes_out": 7193847261,
    "pdf_pages_estimated": 642301
  },
  "latency_ms": {
    "p50": 412,
    "p90": 1140,
    "p95": 1620,
    "p99": 3210,
    "mean": 587,
    "max": 28910
  },
  "throughput": {
    "current_rps": 3.2,
    "peak_rps_1m": 18.7,
    "peak_rps_5m": 14.2,
    "peak_rps_all_time": 42.1,
    "peak_rps_at": "2026-05-19T03:42:18Z"
  },
  "concurrency": {
    "max_configured": 4,
    "current_in_flight": 1,
    "peak_in_flight": 4
  },
  "browser": {
    "healthy": true,
    "restarts": 3,
    "last_restart_at": "2026-05-19T01:08:42Z",
    "last_restart_reason": "scheduled_recycle"
  },
  "windows": {
    "last_1h": { "requests": 4218, "pdfs": 4194, "failures": 24, "p95_ms": 1450 },
    "last_24h": { "requests": 92047, "pdfs": 91204, "failures": 843, "p95_ms": 1680 }
  },
  "failures_by_reason": {
    "timeout": 412,
    "chrome_crash": 18,
    "invalid_html": 1894,
    "invalid_request": 301
  },
  "snapshot": {
    "last_persisted_at": "2026-05-19T08:00:00Z",
    "next_persist_at": "2026-05-19T09:00:00Z"
  }
}
```

**Not exposed publicly:**
- API key names
- Per-key counts
- Request IDs
- Client IPs
- HTML content or sizes per request
- Server hostname / internal addresses

If `DOCPIPE_STATS_PUBLIC=false`, this endpoint requires the same API key auth as `/v1/convert/*`.

### `GET /v1/stats/dashboard` — public HTML dashboard

Serves an embedded `web/dashboard.html` (loaded via `go:embed`) that polls `/v1/stats` every 5 s and renders the numbers with simple charts. No build step, no JS framework — vanilla JS + a small chart library inlined.

Visual style: monospace numbers, dark mode by default, clean. One page, no routing.

### `GET /healthz`
Liveness. `200 {"status":"ok"}`.

### `GET /readyz`
Readiness. `200` if browser allocator is healthy, `503` otherwise.

### `GET /swagger/` and `GET /swagger/openapi.yaml`
Swagger UI + raw spec. Disabled in production unless `DOCPIPE_ENABLE_SWAGGER=true`.

---

## 7. Analytics module (the new piece)

### 7.1 Design principles

- **Zero blocking on the request path.** Recording an event is `atomic.Add` and an enqueue to a buffered channel. If the channel is full, drop the latency sample (counters still increment) — never block the renderer to write analytics.
- **No external storage.** State lives in `./data/state.json`, snapshotted hourly. Daily roll-ups live in `./data/daily/YYYY-MM-DD.json`.
- **Survive restarts.** Replay `state.json` on startup; if missing or corrupt, start fresh and log a warning.
- **Bounded memory.** Histograms have fixed buckets. RPS uses a fixed-size ring buffer. Per-key counters are bounded by the number of configured keys (small).
- **Atomic writes.** Write to `state.json.tmp`, `fsync`, then `rename` → POSIX-atomic. Never leave a partial file on crash.

### 7.2 In-memory state

```go
type Recorder struct {
    startedAt    time.Time
    version      string

    // Counters (atomic.Int64)
    totalRequests atomic.Int64
    totalPDFs     atomic.Int64
    totalFailures atomic.Int64
    bytesIn       atomic.Int64
    bytesOut      atomic.Int64

    // Failure breakdown (sync.Map[string]*atomic.Int64)
    failuresByReason sync.Map

    // Per-key counters (private, never exposed publicly)
    byKey sync.Map // key name → *KeyStats

    // Latency histogram (fixed buckets, atomic)
    latencyHist *Histogram
    pdfSizeHist *Histogram

    // RPS tracking
    rps *SlidingWindow

    // In-flight gauge
    inFlight     atomic.Int64
    peakInFlight atomic.Int64

    // Latency extremes
    maxLatencyMs atomic.Int64

    // Browser events
    browserRestarts atomic.Int64
    lastRestartAt   atomic.Pointer[restartEvent]

    // Rolling windows
    rolling *RollingWindows

    // Snapshot tracking
    lastSnapshotAt atomic.Pointer[time.Time]
}
```

### 7.3 Histogram

Fixed-bucket histogram, lock-free observation:

```go
var latencyBuckets = []int64{
    5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000, // ms
}

type Histogram struct {
    buckets []atomic.Int64
    sum     atomic.Int64
    count   atomic.Int64
    max     atomic.Int64
}
```

`Observe(v int64)` does one binary search, one atomic increment, plus updates to sum/count/max. Percentiles are computed at read time from the bucket distribution — approximate but cheap and bounded. Bucket granularity is sufficient for dashboard purposes; swap in t-digest later if you need exact.

**Histograms persist across restarts.** Bucket counts + sum + count + max are serialised into `state.json` and reloaded on startup. This means a process restart doesn't blow away your p95/p99 view, and your "all-time max latency" stays accurate. The bucket boundaries are fixed in code — if you ever change them, bump the snapshot `schema_version` (see 7.6) and write a migration that re-buckets old data conservatively or starts fresh with a logged warning.

### 7.4 Sliding-window RPS

A 60-second ring buffer of 1-second slots:

```go
type SlidingWindow struct {
    slots       [60]atomic.Int64
    currentSlot atomic.Int64
    mu          sync.Mutex
    startNs     int64
}
```

Background goroutine ticks every second, advances the current slot index, zeros the slot rotating out. `CurrentRPS()` reads the previous slot; `Peak1m()` is max across all 60 slots; `Peak5m()` and all-time peaks are tracked separately as `atomic.Int64` updated via the ticker.

### 7.5 Rolling windows (1h, 24h)

For 1h and 24h aggregates without storing raw events: a coarser ring buffer with 1-minute granularity, 1440 slots (24h × 60min). Each slot holds `{requests, pdfs, failures, latencySum, latencyCount}`. Cheap to read, bounded memory (~50 KB).

### 7.6 Persistence

Background goroutine runs on `DOCPIPE_SNAPSHOT_INTERVAL` ticker (default 1h):

```
1. Take read-side snapshot of all atomic counters, histogram bucket counts,
   peak trackers, and last-restart event.
2. Marshal to JSON with a schema_version field.
3. Write to ./data/state.json.tmp.
4. fsync the file.
5. rename to ./data/state.json (atomic on POSIX).
6. If clock has crossed midnight, roll up the day's counters and write
   ./data/daily/YYYY-MM-DD.json. Then prune files older than DAILY_RETENTION_DAYS.
```

Also snapshot on graceful shutdown (`SIGTERM`/`SIGINT`).

**`state.json` schema** (versioned for forward compatibility):

```json
{
  "schema_version": 1,
  "saved_at": "2026-05-19T08:00:00Z",
  "service": {
    "started_at_first_run": "2026-05-12T14:22:01Z",
    "version": "2.0.0"
  },
  "totals": {
    "requests": 184729,
    "pdfs": 182104,
    "failures": 2625,
    "bytes_in": 4823917284,
    "bytes_out": 7193847261
  },
  "failures_by_reason": {
    "timeout": 412,
    "chrome_crash": 18,
    "invalid_html": 1894,
    "invalid_request": 301
  },
  "latency_histogram": {
    "buckets_ms": [5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000],
    "counts":     [12, 84, 1203, 8421, 24917, 68214, 41203, 28104, 12041, 4218, 1102, 218, 47],
    "sum_ms":     108234918,
    "count":      184729,
    "max_ms":     28910
  },
  "pdf_size_histogram": {
    "buckets_bytes": [1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216],
    "counts":        [201, 8410, 41203, 92841, 28104, 9012, 2014, 304, 15],
    "sum_bytes":     7193847261,
    "count":         182104,
    "max_bytes":     14921384
  },
  "peaks": {
    "rps_all_time": 42.1,
    "rps_all_time_at": "2026-05-19T03:42:18Z",
    "concurrency": 4,
    "max_latency_ms": 28910
  },
  "browser": {
    "restarts": 3,
    "last_restart_at": "2026-05-19T01:08:42Z",
    "last_restart_reason": "scheduled_recycle"
  }
}
```

Notice the histogram blob includes `buckets_ms` — this is defensive. If a future release changes the bucket boundaries, the reader can detect the mismatch and either re-bucket conservatively (collapse adjacent buckets into the new boundaries) or log a warning and start histograms fresh. The buckets array is small (~100 bytes); cost of redundancy is negligible.

Note the histogram count is one element longer than the bucket array — the last entry counts observations above the highest bucket boundary. Standard Prometheus convention.

**On startup:**

```
1. If ./data/state.json exists, decode it.
   a. If schema_version is unknown (> current), log error, refuse to start.
      Better to fail loudly than silently corrupt data.
   b. If schema_version is older, run the migration for that version.
2. Load totals into atomic counters. Counters are cumulative — they survive
   restarts and accumulate forever (Int64 won't wrap in any realistic timeline).
3. Load histogram bucket counts. If the saved buckets array doesn't match the
   current code's buckets, log a warning and either re-bucket or reset.
   Recommend: reset to be safe, log clearly.
4. Load peak trackers and the last-restart event.
5. Rolling windows (1h, 24h) do NOT survive restart — they describe recent
   activity and a restart resets that view. They start empty.
6. If state.json is corrupt or unreadable, archive it to
   ./data/state.json.broken.<ts>, log a loud warning, start fresh.
   Don't crash — the service is more valuable up with empty stats than down.
```

What this means for the public stats endpoint:
- `totals.*` keep climbing across restarts.
- `latency_ms.p50/p95/p99/max` reflect lifetime distribution, not just since-restart.
- `throughput.peak_rps_all_time` is genuine all-time.
- `windows.last_1h` and `windows.last_24h` start empty after a restart and fill in over time. You may want to expose a `windows.last_24h.coverage_seconds` field so the dashboard can show "based on 0h 12m of data since restart" when relevant — that prevents misleadingly low numbers right after a deploy.

**Restart survival summary:**

| Field | Survives restart? |
|---|---|
| `totals.*` (requests, pdfs, failures, bytes) | ✅ yes |
| `failures_by_reason` | ✅ yes |
| `latency_ms.p50/p95/p99/max/mean` | ✅ yes (via persisted histogram) |
| `pdf size histogram` | ✅ yes |
| `peaks.rps_all_time`, `peaks.max_latency_ms` | ✅ yes |
| `browser.restarts`, `browser.last_restart_at` | ✅ yes |
| `service.started_at` (current process) | ❌ no (resets — that's the point) |
| `service.uptime_seconds` | ❌ no (resets) |
| `throughput.current_rps` | ❌ no (60s window) |
| `throughput.peak_rps_1m`, `peak_rps_5m` | ❌ no (rolling) |
| `concurrency.current_in_flight`, `peak_in_flight` | ❌ no (process-local) |
| `windows.last_1h`, `windows.last_24h` | ❌ no (rolling — fills back in) |

### 7.7 Daily files

```
./data/daily/2026-05-19.json
{
  "date": "2026-05-19",
  "requests": 89421,
  "pdfs": 88102,
  "failures": 1319,
  "bytes_in": 2384917284,
  "bytes_out": 3593847261,
  "latency_p50_ms": 410,
  "latency_p95_ms": 1620,
  "latency_p99_ms": 3210,
  "peak_rps": 38.2,
  "peak_concurrency": 4,
  "browser_restarts": 2
}
```

One file per day, finalised after midnight. Useful for historical drill-down if you want to add a `/v1/stats/history?days=7` endpoint later.

### 7.8 What gets recorded where

| Event | Recorded by |
|---|---|
| Request started | `analytics-record` middleware (after auth) |
| Request finished | same middleware (defer) with status + duration |
| PDF rendered (size) | `render.Browser.Render` callback |
| Render failure (reason) | `render.Browser.Render` error path |
| Browser restart | `render.Browser.supervisor` |

The middleware is mounted after auth and rate-limit, so 401/429 traffic doesn't pollute success metrics. Separate counters track auth rejections and rate limits.

---

## 8. Renderer design

### 8.1 Browser lifecycle

One `chromedp.ExecAllocator` lives for the life of the process. From it, each request derives a fresh tab context, runs the render, cancels the tab — but never the allocator.

```go
type Browser struct {
    allocCtx     context.Context
    allocCancel  context.CancelFunc
    parentCtx    context.Context
    parentCancel context.CancelFunc
    sem          chan struct{}
    renderCount  atomic.Int64
    recycleAt    int64
    healthy      atomic.Bool
    recorder     *analytics.Recorder
}

func (b *Browser) Render(ctx context.Context, html string, opts PDFOptions) ([]byte, error) {
    b.recorder.IncInFlight()
    defer b.recorder.DecInFlight()

    select {
    case b.sem <- struct{}{}:
        defer func() { <-b.sem }()
    case <-ctx.Done():
        return nil, ctx.Err()
    }

    tabCtx, cancel := chromedp.NewContext(b.parentCtx)
    defer cancel()

    tabCtx, cancelTimeout := context.WithTimeout(tabCtx, opts.Timeout)
    defer cancelTimeout()

    // ... actions ...

    if b.renderCount.Add(1) >= atomic.LoadInt64(&b.recycleAt) {
        go b.recycle("scheduled_recycle")
    }
    return pdf, nil
}
```

The **first call** to `chromedp.NewContext(allocCtx)` starts the browser. Subsequent calls reuse it. Do this once at startup with a no-op `chromedp.Run(parentCtx)` to force launch, then derive tab contexts from `parentCtx` for every render.

### 8.2 Chrome flags

In Alpine container, with the `chromium` package installed at `/usr/bin/chromium-browser`:

```
--headless=new
--no-sandbox                       # required in Alpine container, ok with non-root user
--disable-gpu
--disable-dev-shm-usage            # critical — /dev/shm is tiny, causes crashes
--disable-software-rasterizer
--disable-background-networking
--disable-background-timer-throttling
--disable-backgrounding-occluded-windows
--disable-breakpad
--disable-extensions
--disable-features=TranslateUI,IsolateOrigins,site-per-process
--disable-ipc-flooding-protection
--disable-renderer-backgrounding
--disable-sync
--enable-features=NetworkService
--force-color-profile=srgb
--hide-scrollbars
--metrics-recording-only
--mute-audio
--no-default-browser-check
--no-first-run
--password-store=basic
--use-mock-keychain
--font-render-hinting=none
```

`--disable-dev-shm-usage` alone fixes a large fraction of "Chrome crashes in Docker" reports.

### 8.3 Loading HTML

The v1 trick of `document.body.innerHTML = htmlContent` is wrong because it discards `<head>`, the doctype, and `<link rel=stylesheet>` in head. Use **`Page.setDocumentContent`**: get the frame tree, take the root frame ID, call `Page.setDocumentContent(frameId, html)`. No size limit, no encoding overhead.

Fallback to data URL `data:text/html;base64,...` only for emergencies.

### 8.4 Waiting strategy

Replace `Sleep(10s)`:

```
load        → wait for Page.loadEventFired
networkidle → load + no in-flight requests for 500 ms
selector    → load + wait for CSS selector
none        → no wait beyond initial navigation
```

Default `networkidle`, 15 s ceiling. Implement via `chromedp.ListenTarget` listening for `network.EventRequestWillBeSent` / `network.EventLoadingFinished`, debounced 500 ms.

### 8.5 Supervisor

Goroutine ticks `BROWSER_HEALTHCHECK_INTERVAL` (default 30 s):
1. Run `chromedp.Run(parentCtx, chromedp.Evaluate("1+1", nil))` with 5 s timeout.
2. On failure: mark unhealthy, cancel allocator, rebuild, record `browser_restart{reason: "healthcheck_failed"}`.
3. Recycle proactively after `BROWSER_RECYCLE_AFTER` renders.

---

## 9. Middleware chain

```
recover → requestID → logger → bodyLimit → CORS → auth → rateLimit → analytics → timeout → handler
```

- **recover** — catch panics, log with stack, return 500 envelope, record failure.
- **requestID** — accept incoming `X-Request-ID` or generate ULID.
- **logger** — structured log of method, path, status, duration, key name, request id, bytes.
- **bodyLimit** — `http.MaxBytesReader` at `MAX_BODY_BYTES`.
- **CORS** — exact-origin match from env list.
- **auth** — constant-time compare of bearer token against in-memory key map.
- **rateLimit** — `golang.org/x/time/rate` per key name.
- **analytics** — record request count, latency, bytes, and outcome. Skipped for `/healthz`, `/readyz`, `/v1/stats`, `/swagger/*` to avoid recording observability traffic in its own metrics.
- **timeout** — `http.TimeoutHandler` at `RENDER_TIMEOUT + 5s`.

---

## 10. Public dashboard

`web/dashboard.html` is a single self-contained file embedded into the binary via `go:embed`. Renders client-side from `/v1/stats`.

Layout: header (service name, uptime, status pill), big-number grid (total requests, PDFs, success %, current RPS), latency panel (p50/p95/p99 cards), throughput panel (current/peak RPS), failure breakdown table, browser health (last restart time, restart count).

No frameworks. Vanilla JS + minimal CSS. Polls every 5 s. Looks intentional — monospace numbers, generous whitespace, dark by default.

```
┌─────────────────────────────────────────────────────────────┐
│ DocPipe                              ● healthy   up 1d 2h   │
├─────────────────────────────────────────────────────────────┤
│  184,729             182,104           98.6%        3.2 r/s │
│  REQUESTS            PDFS              SUCCESS      NOW     │
├─────────────────────────────────────────────────────────────┤
│  LATENCY                  THROUGHPUT                        │
│  p50  412 ms              now      3.2 r/s                  │
│  p95  1620 ms             peak 1m  18.7 r/s                 │
│  p99  3210 ms             peak     42.1 r/s (May 19 03:42)  │
├─────────────────────────────────────────────────────────────┤
│  LAST 24H                 FAILURES                          │
│  92,047 req               invalid_html    1,894             │
│  843 failures             timeout         412               │
│  p95  1680 ms             invalid_request 301               │
│                           chrome_crash    18                │
└─────────────────────────────────────────────────────────────┘
```

No external CDN dependencies if you can avoid it — embed everything.

---

## 11. Error model

Uniform JSON envelope for all 4xx/5xx:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "exactly one of `html` or `html_base64` must be provided",
    "request_id": "01HV...",
    "details": { "field": "html" }
  }
}
```

| Code | HTTP | When |
|---|---|---|
| `invalid_request` | 400 | Missing/malformed fields, bad options |
| `invalid_base64` | 400 | `html_base64` won't decode |
| `unauthorized` | 401 | Missing/malformed key |
| `forbidden` | 403 | Valid format, unknown key |
| `payload_too_large` | 413 | Over `MAX_BODY_BYTES` |
| `rate_limited` | 429 | Token bucket empty (with `Retry-After`) |
| `render_timeout` | 504 | Render exceeded timeout |
| `render_failed` | 500 | Chrome reported an error |
| `service_unavailable` | 503 | Browser unhealthy |
| `internal_error` | 500 | Anything else |

---

## 12. Security

- API keys required on `/v1/convert/*`. Optional on `/v1/stats` (toggled by `DOCPIPE_STATS_PUBLIC`).
- Keys in env at startup, loaded into a `map[string]string` keyed by hash for constant-time lookup. Raw values never logged.
- TLS terminated upstream (Cloudflare, reverse proxy).
- CORS exact-match only. No wildcard subdomains.
- Body capped 10 MB default, hard ceiling 50 MB.
- Run Chrome with `--no-sandbox` as a non-root user inside the container. Container's seccomp default profile keeps it contained.
- The stats endpoint redacts: per-key counts, key names, request IDs, client IPs, hostnames. Redaction happens in `analytics/public.go` — never return the internal `Recorder` struct directly.
- The dashboard endpoint is read-only HTML; no auth, no state mutation.

---

## 13. Swagger / OpenAPI

Hand-written `api/openapi.yaml` (OpenAPI 3.1) is the source of truth. Reasons: small surface, generated specs drift, hand-written is faster to lint with `spectral`.

Served via `swaggo/http-swagger` or `flowchartsman/swaggerui` pointed at the static YAML. Conditional on `DOCPIPE_ENABLE_SWAGGER=true` or `ENV=development`.

Includes:
- `securitySchemes` for both `bearerAuth` and `apiKeyHeader`.
- Schemas for request, options, all error codes, public stats payload.
- Example requests for A4 portrait, Letter landscape, base64 response.

---

## 14. Dockerfile (Alpine, fixed)

The v1 Dockerfile has three problems: no init system (zombie reaping), root user, and missing fonts (Bangla/CJK won't render). v2:

```dockerfile
# ---------- build stage ----------
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Embed version info from git
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /out/docpipe ./cmd/docpipe

# ---------- runtime stage ----------
FROM alpine:3.20

# Chromium + minimum fonts. Add CJK/Bangla packages as needed.
RUN apk add --no-cache \
      chromium \
      nss \
      freetype \
      freetype-dev \
      harfbuzz \
      ca-certificates \
      ttf-freefont \
      ttf-dejavu \
      font-noto \
      font-noto-cjk \
      font-noto-emoji \
      tini \
      tzdata \
    && rm -rf /var/cache/apk/* /tmp/*

# Non-root user, gid/uid stable for volume mounts
RUN addgroup -g 10001 -S docpipe \
    && adduser -u 10001 -S -G docpipe -H -s /sbin/nologin docpipe

WORKDIR /app
COPY --from=builder /out/docpipe /app/docpipe

# Data dir owned by app user
RUN mkdir -p /app/data/daily && chown -R docpipe:docpipe /app

USER docpipe

ENV DOCPIPE_CHROME_PATH=/usr/bin/chromium-browser \
    DOCPIPE_DATA_DIR=/app/data \
    DOCPIPE_HOST=0.0.0.0 \
    DOCPIPE_PORT=8080

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1

# tini as PID 1 — reaps Chrome zombies. This is the critical fix.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/docpipe"]
```

**Key changes from v1:**
- `tini` as PID 1 — reaps Chrome's orphaned children. Without this, the long-lived allocator alone won't fully solve the hang.
- Non-root user (`docpipe`, UID 10001).
- Font packages for non-Latin scripts (Noto CJK + emoji). Verify Bangla coverage with a test render — `font-noto` should cover it but check.
- `tzdata` so timestamps in logs match container TZ if you set one.
- Healthcheck against `/healthz`.
- Volume on `/app/data` so the snapshot survives container restarts. **Bind-mount this in production** or the daily history is lost when the container is replaced.
- Build flags: `-trimpath -ldflags="-s -w"` strips paths and debug info, embeds version/commit.

**Image size**: ~250–300 MB (Chromium dominates). Acceptable.

**docker-compose.yml** for local dev:

```yaml
services:
  docpipe:
    build: .
    ports: ["8080:8080"]
    env_file: .env
    volumes:
      - ./data:/app/data
    restart: unless-stopped
    shm_size: 1gb            # belt-and-braces; we also pass --disable-dev-shm-usage
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
```

`shm_size: 1gb` is harmless when combined with `--disable-dev-shm-usage` but provides headroom if you ever drop that flag.

---

## 15. Testing

- **Unit**: config parsing, option mapping, base64 decoding, error envelopes, middleware in isolation, histogram math, RPS window math, analytics snapshot round-trip.
- **Integration**: real renderer against fixture HTML; assert PDF magic bytes (`%PDF-`), page count, basic text extraction; analytics counters move correctly.
- **Soak (the test v1 fails)**: `make soak` fires 1000 sequential + 50 parallel for 10 minutes against a docker-compose'd instance. Assert: browser still healthy, memory flat, no zombie processes (`ps -eo state,pid,comm | grep '^Z'` is empty), final `/v1/stats` matches expected totals.
- **Fixtures directory**: minimal HTML, web-fonts, CSS page rules, images via data URIs, 50-page document, broken HTML, Bangla text, CJK text.
- **Analytics persistence**: write snapshot, restart, verify totals restored.

---

## 16. Migration from v1

v1 exposes `POST /api/html-to-pdf` with `{ "base64_html": "..." }` and returns a PDF named `admit_card.pdf`. The CASCK job portal depends on this.

Recommend a **compat shim**: keep `/api/html-to-pdf` as an alias for `/v1/convert/html-to-pdf` that accepts the old payload (`base64_html` → `html_base64`), uses defaults, returns the legacy filename. Mark deprecated in OpenAPI, log a warning per call, remove in v3.

Auth on the shim: also bearer / `X-API-Key`. If you can't update the portal in lockstep, allow a per-key opt-in to "no auth on legacy path" — but it's cleaner to update both at once. The shim is one extra handler and zero coordination cost.

---

## 17. Implementation milestones

Independently shippable steps:

1. **Skeleton** — `cmd/docpipe`, config, logger, `/healthz`, `/readyz`, graceful shutdown.
2. **Renderer** — `Browser` with allocator + tab pool + supervisor; standalone benchmark proving stability for 1000+ renders.
3. **HTTP surface** — `/v1/convert/html-to-pdf` with full options + error envelope + request ID.
4. **Auth + middleware** — API keys, rate limit, body limit, CORS, recover.
5. **Analytics** — recorder, histogram, RPS window, hourly snapshot, replay on startup.
6. **Public stats endpoint + dashboard** — `/v1/stats`, `/v1/stats/dashboard`, embedded HTML.
7. **OpenAPI + Swagger UI** — hand-written spec, conditional serving.
8. **Docker + compose** — multi-stage Alpine build, tini, fonts, healthcheck.
9. **v1 compat shim + portal cutover.**
10. **Soak test + tuning** — find the right `RENDER_CONCURRENCY` for the target host.

Days 1–3 fix the v1 hang. Days 4–6 give you the analytics dashboard. The rest is operational polish.

---

## 18. Quick wins for v1 if v2 has to wait

Three changes that would buy the most stability on the existing code, in order of impact:

1. **Add `tini` to the v1 Dockerfile** as `ENTRYPOINT ["/sbin/tini", "--"]`. One line. Stops zombie accumulation. This alone may make v1 stable enough to run for weeks.
2. **Long-lived allocator** — move `chromedp.NewExecAllocator` + the first `chromedp.NewContext` to `main()` and reuse for every request. Derive per-request tabs.
3. **Concurrency cap** — wrap the handler in `chan struct{}{}` of size `runtime.NumCPU()` so you can't have more renders in flight than tabs you're willing to run.

Replace `Sleep(10s)` if you have time — `chromedp.WaitReady("body")` or listen for `Page.loadEventFired`.

But these are band-aids. The v2 spec above is what you actually want running.

---

## 19. Things deliberately deferred

- Async job queue with status polling — synchronous is fine until SLA gets tight.
- Template rendering on server — keep callers in charge of HTML.
- DOCX / XLSX — different toolchain (LibreOffice or Gotenberg). Run alongside, don't absorb.
- Multi-region / horizontal scaling — single-instance saturates one host's CPU first.
- t-digest for exact percentiles — fixed buckets are sufficient for the dashboard. If you ever want exact percentiles, swap the histogram implementation behind the same `Observe(v int64)` interface and bump `schema_version`.
- Per-key dashboard view — totals only on the public dashboard; per-key would need auth.
