# Deploy

This directory holds the artefacts you need to run DocPipe in production.

```
deploy/
├── Dockerfile                    # built by CI; published to GHCR
├── docker-compose.yml            # LOCAL dev — builds image, no Traefik
├── docker-compose.prod.yml       # PROD — uses ghcr image, Traefik labels
├── .env.production.example       # copy to .env, fill in DOMAIN + keys
└── chrome-flags.txt              # reference list of Chromium flags
```

---

## Production deploy in 5 commands

Assumes you already have a Traefik instance running with HTTPS entrypoints and a Let's Encrypt cert resolver.

```bash
# 1. Get the files
git clone https://github.com/monzim/docpipe.git
cd docpipe

# 2. Configure
cp deploy/.env.production.example deploy/.env
$EDITOR deploy/.env                     # set DOMAIN, DOCPIPE_API_KEYS, CORS

# 3. Make sure the Traefik network exists
docker network create traefik           # idempotent — ignore "already exists"

# 4. Pull and start
docker compose -f deploy/docker-compose.prod.yml --env-file deploy/.env up -d

# 5. Verify
docker compose -f deploy/docker-compose.prod.yml --env-file deploy/.env ps
docker logs docpipe -f
```

Once Traefik picks up the labels (usually <10s), the service is reachable at `https://${DOMAIN}` with a Let's Encrypt cert.

## What the compose file gives you

- **Image:** `ghcr.io/monzim/docpipe:${DOCPIPE_VERSION:-latest}`, multi-arch (amd64+arm64). Pinned via `DOCPIPE_VERSION=v2.x.x` is recommended in prod.
- **Restart policy:** `unless-stopped` — survives reboots, doesn't fight you on stop.
- **Volumes:** named `docpipe-data` for analytics state + auto-generated keys. Bind-mount to a host directory if you want easier backups.
- **Security:** non-root UID 10001 (from the image), all caps dropped, `no-new-privileges`, container-level healthcheck against `/healthz`.
- **Resource ceilings:** 2 vCPU / 2 GB by default — tune via `DOCPIPE_CPU_LIMIT` / `DOCPIPE_MEM_LIMIT`.
- **Logging:** JSON file driver with 10 MB × 5 file rotation so a runaway recycle loop can't fill the host disk.
- **Traefik labels:** see below.

## What the Traefik labels give you

| Router | Entrypoint | Action |
|---|---|---|
| `docpipe-http` | `web` (:80) | Permanent redirect to HTTPS |
| `docpipe` | `websecure` (:443) | Routes `Host(${DOMAIN})` to port 8080, TLS via cert resolver |

| Middleware | Purpose |
|---|---|
| `docpipe-https-redirect` | 308 redirect HTTP→HTTPS |
| `docpipe-security` | HSTS (1y, preload, includeSubDomains), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` |
| `docpipe-compress` | Gzip/brotli (helps JSON responses; no-op on PDFs) |

| Service config | Value |
|---|---|
| `loadbalancer.server.port` | `8080` |
| `loadbalancer.healthcheck.path` | `/healthz` |
| `loadbalancer.healthcheck.interval` | `30s` |

## Customising

| Want to change | Edit |
|---|---|
| Hostname | `DOMAIN` in `.env` |
| Image version | `DOCPIPE_VERSION` in `.env` |
| Traefik network name | `TRAEFIK_NETWORK` in `.env` |
| Cert resolver | `TRAEFIK_CERT_RESOLVER` in `.env` |
| Add basic auth, IP allow-list, rate limit | Add a Traefik middleware label, then append its name to `traefik.http.routers.docpipe.middlewares` |
| Disable HTTPS redirect | Remove the `docpipe-http` router labels |
| Disable compression | Remove `docpipe-compress` from the middlewares list |

## Upgrading

```bash
# Latest tag (rotating)
docker compose -f deploy/docker-compose.prod.yml --env-file deploy/.env pull
docker compose -f deploy/docker-compose.prod.yml --env-file deploy/.env up -d

# Pinned version
sed -i 's/^DOCPIPE_VERSION=.*/DOCPIPE_VERSION=v2.0.5/' deploy/.env
docker compose -f deploy/docker-compose.prod.yml --env-file deploy/.env up -d
```

The analytics volume is preserved across upgrades. Auto-generated API keys persist too.

## Backup

The only stateful thing is the docker volume `docpipe-data`. To back it up:

```bash
docker run --rm -v docpipe-data:/data -v $PWD:/backup alpine \
  tar czf /backup/docpipe-data-$(date +%F).tgz -C /data .
```

To restore:

```bash
docker run --rm -v docpipe-data:/data -v $PWD:/backup alpine \
  sh -c "cd /data && tar xzf /backup/docpipe-data-YYYY-MM-DD.tgz"
```

## Troubleshooting

**"acme: error: 400 :: urn:ietf:params:acme:error:dns"** — DNS for `DOMAIN` isn't pointing at Traefik's public IP yet. Wait for propagation or fix the A record.

**Traefik 404 on the right hostname** — Traefik isn't seeing the container. Check:
- `docker network inspect traefik` lists both Traefik and the DocPipe container
- Traefik's docker provider is enabled (`--providers.docker=true`)
- `traefik.enable=true` label is present (`docker inspect docpipe --format '{{.Config.Labels}}' | tr , '\n' | grep traefik`)

**Service unhealthy** — `docker logs docpipe` will show the Chromium startup error. The most common cause is the host kernel missing user-namespace support; bump to Docker 24+.

**Can't retrieve API keys** — `docker exec docpipe cat /app/data/api-keys.json`. If empty, you set `DOCPIPE_API_KEYS` explicitly; check `.env`.
