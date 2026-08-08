# Telegram Gateway

A single point of communication between your server and an external Telegram
user, running in Docker with a Postgres backend.

The gateway long-polls the Telegram Bot API for **inbound** messages (from a
whitelisted chat/user) and stores them in Postgres. Your backend sends
**outbound** messages by inserting into the `outbox` table or calling the REST
API; the gateway delivers them to Telegram with retries and backoff.

---

## Table of contents

- [Architecture](#architecture)
- [Repository layout](#repository-layout)
- [Prerequisites](#prerequisites)
- [Local development](#local-development)
- [Configuration reference](#configuration-reference)
- [Ports](#ports)
- [REST API](#rest-api)
- [Database contract](#database-contract)
- [Observability](#observability)
- [Deploying to a server](#deploying-to-a-server)
  - [1. Provision the server](#1-provision-the-server)
  - [2. Install Docker + compose](#2-install-docker--compose)
  - [3. Clone and configure](#3-clone-and-configure)
  - [4. Generate TLS certs for Postgres](#4-generate-tls-certs-for-postgres)
  - [5. Start the stack](#5-start-the-stack)
  - [6. Verify the deployment](#6-verify-the-deployment)
  - [7. Production secrets (optional)](#7-production-secrets-optional)
- [Operations](#operations)
  - [Updating](#updating)
  - [Backups](#backups)
  - [Restore](#restore)
  - [Rotating secrets](#rotating-secrets)
  - [Troubleshooting](#troubleshooting)
- [Security notes](#security-notes)

---

## Architecture

```
External Telegram user
        │
        ▼
   Bot API (long polling)
        │ getUpdates / sendMessage
        ▼
┌─────────────────────┐
│  Gateway (Go, TLS)  │──► Postgres (TLS, isolated network)
│  - poll loop        │       ├─ incoming_messages
│  - outbox worker    │       └─ outbox (LISTEN/NOTIFY)
└────────┬────────────┘
         │
    ┌────┴─────────┐
    │ REST API     │  /api/v1/*  (X-API-Key)
    └──────────────┘
         │
    ┌────┴─────────┐
    │ /metrics     │  Prometheus (separate port)
    └──────────────┘
```

- **Inbound**: long-polled from Telegram, whitelist-checked, stored in
  `incoming_messages`. Read via the API or SQL.
- **Outbound**: your server inserts into `outbox` (or calls `POST /api/v1/send`).
  The gateway claims, delivers, and marks `sent`/`failed`/`dead` with retries.

---

## Repository layout

```
├── cmd/gateway/main.go        # entry point (errgroup wiring, JSON logger)
├── internal/
│   ├── api/                   # REST API + middleware (auth, rate limit, logging)
│   ├── config/                # env config, secrets, validation
│   ├── gateway/               # poll loop + outbox worker
│   ├── metrics/               # Prometheus metrics
│   ├── store/                 # Postgres access, schema, pool config
│   ├── telegram/              # Telegram Bot API client
│   └── testdb/                # test-only Postgres schema isolation
├── db/postgresql.conf         # TLS config for the compose Postgres
├── db-certs/                  # generated TLS certs (gitignored)
├── scripts/gen-db-certs.sh    # one-time cert generation
├── docker-compose.yml         # dev stack (db + gateway)
├── docker-compose.prod.yml    # production overlay (Docker secrets)
├── Dockerfile                 # multi-stage, non-root
└── .env                       # configuration (gitignored)
```

---

## Prerequisites

- **Docker** 24+ and **Docker Compose v2** (`docker compose version`)
- **A Telegram bot**: create one with [@BotFather](https://t.me/BotFather) and
  copy the token
- **Your chat id**: message the bot once, then find the id (the gateway logs
  `chat_id` for any message, or use [@userinfobot](https://t.me/userinfobot))
- **OpenSSL** (only for generating the DB TLS certs)

---

## Local development

```bash
# 1. Configure
# Create .env from the Configuration reference below (all variables listed).
# Minimum: TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_IDS, GATEWAY_API_KEY.

# 2. Generate Postgres TLS certs (once)
./scripts/gen-db-certs.sh

# 3. Build and start
docker compose up --build

# 4. Check
curl http://localhost:8090/healthz        # → {"status":"ok"}
curl http://localhost:9100/metrics        # → Prometheus metrics
```

Without Docker:

```bash
go run ./cmd/gateway
```

Requires `DATABASE_URL` pointing at a reachable Postgres.

---

## Configuration reference

All configuration lives in `.env` (loaded via `env_file`). Secrets can also be
supplied via `_FILE` env vars (see [Production secrets](#7-production-secrets-optional)).

| Variable | Required | Default | Description |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | — | Bot token from @BotFather (format `<bot_id>:<token>`) |
| `TELEGRAM_CHAT_IDS` | yes | — | Comma-separated whitelist of allowed chat/user ids |
| `GATEWAY_API_KEY` | yes | — | API key for `X-API-Key` header. **≥16 chars**, not a weak value |
| `GATEWAY_API_KEY_LEGACY` | no | — | Optional previous API key, accepted during key rotation |
| `DATABASE_URL` | yes | — | Postgres connection string. Non-local hosts must use `sslmode=require` or `verify-full` |
| `POSTGRES_DB` | no | `gateway` | Name of the database created by the compose `db` service |
| `POSTGRES_USER` | no | `gateway` | Database user created by the compose `db` service |
| `POSTGRES_PASSWORD` | **yes** | — | Password for the compose `db` service. **Required** — compose fails to start without it. Use `openssl rand -hex 24` |
| `GATEWAY_API_PORT` | no | `8080` | Host port for the REST API (compose) |
| `METRICS_PORT` | no | `9100` | Host port for Prometheus metrics (compose) |
| `GATEWAY_API_ADDRESS` | no | `:8080` | API listen address when running **outside** compose (compose derives this from `GATEWAY_API_PORT`) |
| `METRICS_ADDRESS` | no | `:9100` | Metrics listen address outside compose; set empty to serve `/metrics` on the API port |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, or `error` |
| `RATE_LIMIT_RPS` | no | `50` | API rate limit (requests/sec); `0` = disabled |
| `RATE_LIMIT_BURST` | no | `100` | Rate-limit burst size |
| `DB_POOL_MIN_CONNS` | no | `1` | pgx pool min connections |
| `DB_POOL_MAX_CONNS` | no | `10` | pgx pool max connections |
| `DB_POOL_MAX_CONN_LIFETIME` | no | `30m` | Pool connection max lifetime |
| `DB_POOL_MAX_CONN_IDLE_TIME` | no | `5m` | Pool connection idle timeout |
| `ALLOW_INSECURE_DB_TLS` | no | `false` | Dev-only: allow `sslmode=disable` for non-local DB hosts |
| `POLL_INTERVAL_SECONDS` | no | `5` | Telegram long-poll interval |
| `RETRY_INTERVAL_SECONDS` | no | `10` | Outbox sweep interval |
| `MAX_RETRIES` | no | `5` | Delivery attempts before a message becomes `dead` |
| `RETRY_BACKOFF_SECONDS` | no | `30` | Base retry backoff (scaled by attempt) |

### Example `.env`

```bash
TELEGRAM_BOT_TOKEN=123456:ABCdef...GHI
TELEGRAM_CHAT_IDS=779839848
GATEWAY_API_KEY=openssl-rand-hex-32-bytes

# Database (compose `db` service)
POSTGRES_DB=gateway
POSTGRES_USER=gateway
POSTGRES_PASSWORD=change-me-strong-password

# Ports — adjust to what is free on your server
GATEWAY_API_PORT=8090
METRICS_PORT=9100

# Optional tuning
LOG_LEVEL=info
RATE_LIMIT_RPS=20
RATE_LIMIT_BURST=40
MAX_RETRIES=5
RETRY_BACKOFF_SECONDS=30
```

Generate a strong API key with:

```bash
openssl rand -hex 32
```

---

## Ports

All externally reachable ports are configurable from `.env` so the same codebase
deploys on any server without conflicts.

| Service | Env var | Default | Inside container | Notes |
|---|---|---|---|---|
| REST API | `GATEWAY_API_PORT` | `8080` | same port | Compose publishes `HOST:CONTAINER` at the same number and derives `GATEWAY_API_ADDRESS` |
| Metrics | `METRICS_PORT` | `9100` | same port | Compose publishes at the same number and derives `METRICS_ADDRESS` |
| Postgres | — | `5432` | `5432` | **Not published** to the host by default — only reachable on the internal `gateway-net` network |

If a port is already in use on the target server, change the value in `.env`
(e.g. `GATEWAY_API_PORT=9090`) and `docker compose up -d`.

---

## REST API

All endpoints require `X-API-Key: <GATEWAY_API_KEY>` except `/healthz`. The
`/metrics` endpoint is **also protected** when it shares the API port (set
`METRICS_ADDRESS` to an empty value to serve it there; otherwise it runs on a
separate port). Every response includes an `X-Request-ID` for log correlation.

### `GET /healthz`

Liveness + DB connectivity.

```bash
curl http://localhost:8090/healthz
# {"status":"ok"}
```

### `GET /api/v1/messages?limit=50`

Recent incoming messages, newest first. `limit` ∈ [1, 500].

```bash
curl -H "X-API-Key: $GATEWAY_API_KEY" \
  "http://localhost:8090/api/v1/messages?limit=10"
```

### `POST /api/v1/messages`

Filtered query. All fields optional.

```json
{
  "chat_id": 123,
  "from_id": 456,
  "from_name": "Ali",
  "after": "2025-06-01T00:00:00Z",
  "before": "2025-06-30T23:59:59Z",
  "limit": 100
}
```

### `POST /api/v1/send`

Enqueue an outbound message to a whitelisted chat.

```bash
curl -X POST http://localhost:8090/api/v1/send \
  -H "X-API-Key: $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"chat_id": 779839848, "text": "hello from the server"}'
# 202 {"id": 1, "status": "queued", "chat_id": 779839848}
```

### `GET /api/v1/outbox/{id}`

Delivery status of a queued message.

```bash
curl -H "X-API-Key: $GATEWAY_API_KEY" http://localhost:8090/api/v1/outbox/1
# {"id":1,"chat_id":779839848,"text":"hello","status":"sent",
#  "attempts":1,"error_message":"","created_at":"...","updated_at":"..."}
```

`status` is one of `pending`, `processing`, `sent`, `failed` (retryable), `dead`.

---

## Database contract

The schema is applied automatically on startup (idempotent, advisory-locked).
The compose Postgres runs with **TLS enabled** on an isolated network, with a
**read-only root filesystem**, a configurable database name, user, and
password (`POSTGRES_DB`/`POSTGRES_USER`/`POSTGRES_PASSWORD` in `.env`), and
**scram-sha-256 auth for all connections** (no `trust` — a hardened
`pg_hba.conf` is mounted).

- `incoming_messages` — messages received from Telegram
  (`id, chat_id, from_id, from_name, text, status, created_at`)
- `outbox` — outbound queue
  (`id, chat_id, text, status, attempts, error_message, source, locked_until, created_at, updated_at`)
  - `source` records the origin of the message (`api` from the REST API,
    `unknown` for legacy/direct rows) so forged sends can be distinguished.
  - Inserting fires `pg_notify('outbox_channel')`.

Send without the REST API:

```sql
INSERT INTO outbox (chat_id, text) VALUES (779839848, 'hello');
```

---

## Observability

**Logs**: structured JSON to stdout/stderr via Docker. Control verbosity with
`LOG_LEVEL`.

```bash
docker compose logs -f gateway
```

**Metrics** (`http://localhost:9100/metrics` by default):

- `http_requests_total{route,method,status}`, `http_request_duration_seconds`
- `telegram_api_requests_total{method,result}`, `telegram_api_errors_total{method}`
- `outbox_messages_total{status}`, `outbox_delivery_attempts_total`,
  `outbox_backlog{status}`
- `db_pool_*` (max/open/in-use/idle connections, wait count & duration)

Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: telegram-gateway
    metrics_path: /metrics
    static_configs: [{ targets: ["<SERVER_IP>:9100"] }]
```

---

## Deploying to a server

### 1. Provision the server

Any Linux box with Docker works (Ubuntu/Debian recommended). Requirements:

- 1 vCPU, 1 GB RAM minimum (the gateway is light; Postgres is the heavier part)
- Outbound HTTPS access to `api.telegram.org`
- An open (or firewalled-but-accessible) port for the REST API, e.g. `8090`

### 2. Install Docker + compose

```bash
# Ubuntu/Debian example
curl -fsSL https://get.docker.com | sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"   # re-login after this
docker compose version            # verify compose v2
```

### 3. Clone and configure

```bash
git clone git@github.com:nayakashwin/telegram-gateway.git
cd telegram-gateway

cp .env .env.bak 2>/dev/null; : > .env   # start fresh
# ... or create .env from the Configuration reference above
```

Set at minimum:

```bash
TELEGRAM_BOT_TOKEN=<your bot token>
TELEGRAM_CHAT_IDS=<your chat id>
GATEWAY_API_KEY=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 16)
GATEWAY_API_PORT=8090        # change if 8090 is taken
METRICS_PORT=9100            # change if 9100 is taken
```

### 4. Generate TLS certs for Postgres

```bash
./scripts/gen-db-certs.sh
```

This writes a **CA** (`ca.crt`) and a **server certificate** (`server.crt`,
`signed by the CA, CN=db with SAN`) to `db-certs/` (gitignored). The certs are
mounted into the `db` container, chowned to the postgres user at startup, and
Postgres runs with `ssl=on`. The gateway connects with
**`sslmode=verify-full`** and pins `ca.crt` as the trust root — the DB server
is authenticated, not just encrypted, so a MITM cannot impersonate it.

> Rotate the certs by deleting `db-certs/` and re-running the script, then
> `docker compose up -d --force-recreate db gateway`.

### 5. Start the stack

```bash
docker compose up -d --build
docker compose ps          # both services healthy/up
```

### 6. Verify the deployment

```bash
# API health (DB reachable over TLS)
curl http://localhost:8090/healthz          # {"status":"ok"}

# Metrics endpoint
curl http://localhost:9100/metrics | head

# Send a real message to your Telegram
curl -X POST http://localhost:8090/api/v1/send \
  -H "X-API-Key: $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"chat_id": 779839848, "text": "deployment test"}'

# Confirm delivery status flips to sent
sleep 12
curl -H "X-API-Key: $GATEWAY_API_KEY" http://localhost:8090/api/v1/outbox/1

# Inbound: send a message to your bot on Telegram, then
curl -H "X-API-Key: $GATEWAY_API_KEY" http://localhost:8090/api/v1/messages
```

### 7. Production secrets (optional)

Instead of putting secrets in `.env`, mount them as Docker secrets. Create
`./secrets/` (gitignored) and the prod overlay:

```bash
mkdir -p secrets
printf '%s' "$TELEGRAM_BOT_TOKEN" > secrets/telegram_bot_token
printf '%s' "$GATEWAY_API_KEY"     > secrets/gateway_api_key
printf '%s' "$DATABASE_URL"        > secrets/database_url   # sslmode=require+

docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

When a `_FILE` var is set it takes precedence over `.env`. See
`docker-compose.prod.yml`.

---

## Operations

### Updating

```bash
git pull
docker compose up -d --build
```

### Backups

Nightly dump of the `gateway` database (cron):

```bash
0 2 * * * cd /opt/telegram-gateway && docker compose exec -T db pg_dump -U gateway gateway | gzip > /backups/gateway-$(date +\%F).sql.gz
```

Keep several days of backups and copy them off-box.

### Restore

```bash
gunzip -c /backups/gateway-2026-08-08.sql.gz | \
  docker compose exec -T db psql -U gateway -d gateway
```

### Rotating secrets

1. Update the value in `.env` (or the secret file).
2. Recreate: `docker compose up -d` (or `--force-recreate gateway`).
3. Verify `/healthz` and a test send.
4. Revoke the old Telegram token via @BotFather; rotate the DB password by
   updating `POSTGRES_PASSWORD` and the `DATABASE_URL` accordingly.

### Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Gateway restarts, `dial tcp ... connection refused` | DB not ready / `pgdata` stale | `docker compose down` then `up -d`; check `docker compose logs db` |
| `DATABASE_URL uses sslmode=disable for non-local host` | Config validation | Use `sslmode=require` or set `ALLOW_INSECURE_DB_TLS=true` (dev only) |
| `GATEWAY_API_KEY is too weak` | Key < 16 chars or blocklisted | `openssl rand -hex 32` |
| `telegram http 401` | Bad bot token | Check `TELEGRAM_BOT_TOKEN` / secret file |
| Port already allocated | Port in use on host | Change `GATEWAY_API_PORT` / `METRICS_PORT` in `.env` |
| DB won't start, cert errors | Certs missing or wrong ownership | Re-run `scripts/gen-db-certs.sh`, `docker compose down -v` only if you accept data loss |
| Messages stuck `failed`/`dead` | Telegram errors, e.g. chat not started | Check `error_message` via `/api/v1/outbox/{id}` |

---

## Security notes

- The API key is checked with a constant-time compare; weak keys are rejected
  at startup.
- The gateway container runs as a non-root user with a read-only root filesystem,
  `cap_drop: ALL`, `no-new-privileges`, and memory/CPU limits.
- Postgres is not exposed on the host — only reachable on the internal Docker
  network, over TLS.
- The compose Postgres uses a CA-signed certificate with the gateway pinning
  the CA (`sslmode=verify-full`); rotate the CA/certs via
  `scripts/gen-db-certs.sh`.
- Inbound messages are accepted **only** from `TELEGRAM_CHAT_IDS`.
- Secrets (`secrets/`, `db-certs/`, `.env`) are gitignored.
- Rate limiting is on by default (50 rps / 100 burst); set `RATE_LIMIT_RPS=0` to disable.

## Testing

```bash
# unit + integration (integration tests need a Postgres on TEST_DATABASE_URL)
TEST_DATABASE_URL=postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable \
  go test ./... -race -count=1
```

The suite covers config validation, Telegram client, store (retries, backoff,
dead-letter, notify), REST API (auth, filters, rate limit, metrics, outbox
lookup), and the gateway poll/delivery loops.
