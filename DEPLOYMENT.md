# Deployment Guide — Telegram Gateway

A machine-executable deployment document. Every step is a command an AI agent
(or human) can run. Follow the steps **in order**. The document assumes a Linux
host with Docker installed. All commands are idempotent unless noted.

---

## 1. Overview

This is a Go + Docker Telegram gateway:

- `gateway` service — REST API (send/query/lookup), Telegram long-polling,
  outbox delivery, Prometheus metrics. Runs as non-root in a hardened container.
- `db` service — Postgres 16 (TLS + scram-sha-256 auth), isolated on an
  internal Docker network, **not published** to the host.

**Config sources**: `.env` (in the repo root) is used both by docker-compose
for interpolation (`${VAR}`) and by the gateway container via `env_file`.
Secrets can alternatively be supplied as Docker secrets (see `docker-compose.prod.yml`).

---

## 2. Prerequisites

```bash
# Verify tooling exists. If any check fails, install Docker first.
docker --version
docker compose version        # must be v2
openssl version
curl --version
```

Minimum resources: 1 vCPU / 1 GB RAM. Outbound HTTPS to `api.telegram.org`
must be allowed.

---

## 3. Obtain inputs (human step)

The agent needs three secrets **before** deploying:

| Input | Where to get it |
|---|---|
| `TELEGRAM_BOT_TOKEN` | Create a bot with [@BotFather](https://t.me/BotFather) → `/newbot` → copy the token (`<digits>:<alnum>`). |
| `TELEGRAM_CHAT_IDS` | Message the bot once, then find your numeric chat id (e.g. [@userinfobot](https://t.me/userinfobot)). This is the whitelist — only these chats may talk to the gateway. |
| A strong API key | Generate locally: `openssl rand -hex 32` |

If these are not provided, **stop** and ask for them. Do not invent or reuse
secrets from another deployment.

---

## 4. Clone and check for free ports

```bash
cd /tmp
rm -rf telegram-gateway
git clone git@github.com:nayakashwin/telegram-gateway.git
cd telegram-gateway
```

### 4.1 Determine free ports

The app needs **two free TCP ports** on the host:
- `GATEWAY_API_PORT` — REST API (default 8080)
- `METRICS_PORT` — Prometheus metrics (default 9100)

Check each candidate port; if it is in use, pick the next free one:

```bash
# For each candidate port P, run:
ss -tlnp | grep -E ":(8080|9100)\b" && echo "PORT IN USE" || echo "PORT FREE"

# Example: find two free ports programmatically
API_PORT=8080
METRICS_PORT=9100
while ss -tln | grep -q ":$API_PORT "; do API_PORT=$((API_PORT+1)); done
while ss -tln | grep -q ":$METRICS_PORT "; do METRICS_PORT=$((METRICS_PORT+1)); done
echo "API_PORT=$API_PORT METRICS_PORT=$METRICS_PORT"
```

Record the chosen values; they go into `.env`. Note: the DB (5432) is **not**
published to the host, so no host port is needed for Postgres.

---

## 5. Create `.env`

Generate strong random values and write `.env`. **Never commit or share this
file** (it is already gitignored).

```bash
cat > .env <<'EOF'
# Telegram bot token from @BotFather (format <digits>:<alnum>)
TELEGRAM_BOT_TOKEN=<BOT_TOKEN>

# Comma-separated whitelist of chat/user IDs
TELEGRAM_CHAT_IDS=<CHAT_IDS>

# --- Postgres (compose `db` service) ---
POSTGRES_DB=gateway
POSTGRES_USER=gateway
POSTGRES_PASSWORD=<GENERATE: openssl rand -hex 24>

# Postgres connection string (used when running outside docker-compose)
DATABASE_URL=postgres://gateway:<POSTGRES_PASSWORD>@localhost:5432/gateway?sslmode=disable

# Shared secret for the REST API (X-API-Key header)
GATEWAY_API_KEY=<GENERATE: openssl rand -hex 32>

# Host ports (from step 4.1 — must be free)
GATEWAY_API_PORT=<API_PORT>
METRICS_PORT=<METRICS_PORT>

# API listen address (compose derives this from GATEWAY_API_PORT)
GATEWAY_API_ADDRESS=:<API_PORT>

# Optional tuning
LOG_LEVEL=info
RATE_LIMIT_RPS=50
RATE_LIMIT_BURST=100
MAX_CONCURRENT_REQUESTS=8
POLL_INTERVAL_SECONDS=5
RETRY_INTERVAL_SECONDS=10
MAX_RETRIES=5
RETRY_BACKOFF_SECONDS=30
EOF
```

Reference implementation with placeholders resolved:

```bash
BOT_TOKEN="<BOT_TOKEN>"
CHAT_IDS="<CHAT_IDS>"
DB_PASS=$(openssl rand -hex 24)
API_KEY=$(openssl rand -hex 32)
API_PORT=${API_PORT:-8080}
METRICS_PORT=${METRICS_PORT:-9100}

cat > .env <<EOF
TELEGRAM_BOT_TOKEN=$BOT_TOKEN
TELEGRAM_CHAT_IDS=$CHAT_IDS
POSTGRES_DB=gateway
POSTGRES_USER=gateway
POSTGRES_PASSWORD=$DB_PASS
DATABASE_URL=postgres://gateway:$DB_PASS@localhost:5432/gateway?sslmode=disable
GATEWAY_API_KEY=$API_KEY
GATEWAY_API_PORT=$API_PORT
METRICS_PORT=$METRICS_PORT
GATEWAY_API_ADDRESS=:$API_PORT
LOG_LEVEL=info
RATE_LIMIT_RPS=50
RATE_LIMIT_BURST=100
MAX_CONCURRENT_REQUESTS=8
POLL_INTERVAL_SECONDS=5
RETRY_INTERVAL_SECONDS=10
MAX_RETRIES=5
RETRY_BACKOFF_SECONDS=30
EOF
```

### 5.1 Validate `.env`

```bash
# Compose fails fast on a missing POSTGRES_PASSWORD or bad ports.
docker compose config --quiet && echo "CONFIG OK"
```

---

## 6. Generate the Postgres TLS certificates

The DB runs TLS and the gateway pins the CA (`sslmode=verify-full`). Generate
once (certificates land in `db-certs/`, gitignored):

```bash
./scripts/gen-db-certs.sh
# Verify the chain:
openssl verify -CAfile db-certs/ca.crt db-certs/server.crt
```

---

## 7. Build and start

```bash
docker compose up -d --build
```

Wait for both services to be healthy (up to ~60s):

```bash
for i in $(seq 1 30); do
  status=$(docker inspect --format '{{.State.Health.Status}}' $(docker compose ps -q gateway) 2>/dev/null)
  [ "$status" = "healthy" ] && break
  sleep 2
done
docker compose ps
# Expect: db (healthy) and gateway (healthy or starting -> healthy)
```

If `gateway` shows `Restarting`, inspect logs and fix before continuing:

```bash
docker compose logs gateway | tail -40
```

Common startup failures:
- `TELEGRAM_BOT_TOKEN is malformed` → token must match `<digits>:<alnum>`.
- `GATEWAY_API_KEY is too weak` → use ≥16 random chars.
- `sslrootcert`/`verify certificate` errors → re-run step 6 and
  `docker compose up -d --force-recreate db gateway`.
- `connection refused` to `db` → Postgres is still initializing; wait, then
  `docker compose up -d`.

---

## 8. Post-deployment feature tests

Run every test. Record pass/fail. If any fail, investigate (see §9).

### 8.1 Liveness + DB connectivity

```bash
curl -sf http://localhost:$API_PORT/healthz
# Expect: {"status":"ok"}
```

### 8.2 Auth enforcement

```bash
# Without key -> 401
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:$API_PORT/api/v1/messages
# Expect: 401

# With correct key -> 200
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/messages
# Expect: 200

# Wrong key -> 401
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "X-API-Key: wrong-key" http://localhost:$API_PORT/api/v1/messages
# Expect: 401
```

### 8.3 Outbound (server → Telegram)

Send a message to the whitelisted chat; expect `202` with `"status":"queued"`:

```bash
curl -s -X POST http://localhost:$API_PORT/api/v1/send \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"chat_id\": $CHAT_IDS, \"text\": \"deployment test $(date -u +%s)\"}"
```

Track delivery — within ~15s the status must become `sent` and `sent_at` must
be set. Capture the `id` from the send response and poll the outbox endpoint:

```bash
ID=<id from the send response above>
for i in $(seq 1 15); do
  st=$(curl -s -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/outbox/$ID \
    | grep -o '"status":"[a-z]*"' | cut -d: -f2 | tr -d '"')
  [ "$st" = "sent" ] && break
  sleep 2
done
curl -s -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/outbox/$ID
# Expect eventually: "status":"sent", "sent_at":"...", "source":"api"
```

**Human confirmation**: the message should appear in the Telegram chat.

### 8.4 Inbound (Telegram → server)

This requires a human (or an agent with Telegram access) to **send a message to
the bot** from the whitelisted account. After doing so, wait a few seconds for
the poll loop, then:

```bash
curl -s -H "X-API-Key: $API_KEY" "http://localhost:$API_PORT/api/v1/messages?limit=5"
# Expect: the message with "status":"received" and a non-zero "received_at"
```

**Human confirmation**: the sent message appears in the list above.

If no human is available, verify the inbound pipeline with a simulated insert
and confirm it is readable via the API (proves the DB→API path). Load the
secrets into shell variables first (anchored greps avoid matching comments):

```bash
API_KEY=$(grep -E '^GATEWAY_API_KEY=' .env | cut -d= -f2)
CHAT_IDS=$(grep -E '^TELEGRAM_CHAT_IDS=' .env | cut -d= -f2)
DB_PASS=$(grep -E '^POSTGRES_PASSWORD=' .env | cut -d= -f2)

docker compose exec -T db psql \
  "postgres://gateway:$DB_PASS@127.0.0.1:5432/gateway?sslmode=disable" \
  -c "INSERT INTO incoming_messages (chat_id, from_id, from_name, text, status) \
      VALUES ($CHAT_IDS, $CHAT_IDS, 'deploy-test', 'inbound pipeline check', 'received') RETURNING id, received_at;"
curl -s -H "X-API-Key: $API_KEY" "http://localhost:$API_PORT/api/v1/messages?limit=1"
# Expect: the inserted row with "received_at" populated
```

### 8.5 Whitelist enforcement

Sending to a **non-whitelisted** chat must be rejected with `403`:

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:$API_PORT/api/v1/send \
  -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
  -d '{"chat_id": 999999999, "text": "nope"}'
# Expect: 403
```

### 8.6 Message query (filters)

```bash
curl -s -X POST http://localhost:$API_PORT/api/v1/messages \
  -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
  -d "{\"chat_id\": $CHAT_IDS, \"limit\": 10}"
# Expect: 200 with the messages array
```

### 8.7 Outbox lookup by id

Use the id returned by `POST /api/v1/send` in 8.3:

```bash
curl -s -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/outbox/<ID>
# Expect: 200 with status, attempts, error_message, source, sent_at, created_at, updated_at
# For a missing id: expect 404
curl -s -o /dev/null -w "%{http_code}\n" -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/outbox/99999999
# Expect: 404
```

### 8.8 Metrics

```bash
# Separate metrics port (unauthenticated by design — firewalled, not published to internet):
curl -s http://localhost:$METRICS_PORT/metrics | grep -E "^http_requests_total|^db_pool_|^outbox_backlog"
# Expect: metric lines
```

### 8.9 Rate limiting

With defaults (50 rps / 100 burst) a short burst is absorbed. To prove the
limiter works, set a low limit temporarily and expect 429s. If `RATE_LIMIT_*`
is not already in `.env`, append it (defaults apply when absent):

```bash
# (optional) temporarily set a low limit:
echo "RATE_LIMIT_RPS=2"  >> .env
echo "RATE_LIMIT_BURST=3" >> .env
docker compose up -d --force-recreate gateway
# wait for healthz: curl -sf http://localhost:$API_PORT/healthz

for i in $(seq 1 6); do
  curl -s -o /dev/null -w "%{http_code} " -H "X-API-Key: $API_KEY" http://localhost:$API_PORT/api/v1/messages
done
echo
# Expect: 200 200 200 429 429 429

# Restore: remove the two lines, or set back to defaults, then recreate:
sed -i '/^RATE_LIMIT_RPS=/d; /^RATE_LIMIT_BURST=/d' .env
docker compose up -d --force-recreate gateway
```

### 8.10 Request correlation

```bash
curl -s -D - -o /dev/null -H "X-Request-ID: my-test-123" http://localhost:$API_PORT/healthz | grep -i x-request-id
# Expect: X-Request-Id: my-test-123
```

---

## 9. Test results summary (fill in)

| Test | Expected | Result |
|---|---|---|
| 8.1 healthz | `{"status":"ok"}` | ☐ |
| 8.2 auth (no/wrong/correct key) | 401/401/200 | ☐ |
| 8.3 outbound send → sent | 202 → sent + sent_at | ☐ |
| 8.4 inbound receive | message with received_at | ☐ |
| 8.5 whitelist 403 | 403 | ☐ |
| 8.6 message query | 200 + messages | ☐ |
| 8.7 outbox lookup / 404 | 200 / 404 | ☐ |
| 8.8 metrics | metric lines | ☐ |
| 8.9 rate limit (if tested) | 200 200 200 429 429 429 | ☐ |
| 8.10 request id echo | header echoed | ☐ |

---

## 10. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `gateway` restarting, `dial tcp ... connection refused` | DB not ready / stale volume | `docker compose up -d`; check `docker compose logs db` |
| `GET /api/v1/messages` → 401 | Wrong/absent `X-API-Key` | Use `GATEWAY_API_KEY` from `.env` |
| `POST /api/v1/send` → 403 | `chat_id` not in `TELEGRAM_CHAT_IDS` | Add the id to the whitelist |
| `telegram http 401` in gateway logs | Invalid bot token | Fix `TELEGRAM_BOT_TOKEN`; recreate gateway |
| `sent_at` stays NULL, status stuck `failed`/`dead` | Telegram rejected the send | `GET /api/v1/outbox/<ID>` and read `error_message` |
| Healthz `db_unreachable` | DB down | `docker compose ps db`; `docker compose logs db` |
| Port already allocated at startup | Chosen port in use | Re-pick free ports (step 4.1), update `.env`, recreate |

---

## 11. Operations reference

```bash
# Logs
docker compose logs -f gateway
docker compose logs db

# Restart gateway (e.g. after .env change)
docker compose up -d --force-recreate gateway

# Full stack down (keeps the pgdata volume)
docker compose down

# Full stack down + wipe data (IRREVERSIBLE)
docker compose down -v

# Backup
docker compose exec -T db pg_dump -U gateway gateway | gzip > backup-$(date +%F).sql.gz

# Restore
gunzip -c backup-*.sql.gz | docker compose exec -T db psql -U gateway -d gateway

# Rotate the API key (zero-downtime): set old key as GATEWAY_API_KEY_LEGACY,
# new key as GATEWAY_API_KEY, recreate, migrate consumers, then remove LEGACY.
```

## 12. Security notes for the agent

- `.env`, `db-certs/`, and `secrets/` contain secrets — never print or commit them.
- The metrics port is unauthenticated by design; keep it firewalled or bind it
  to a private interface in production.
- The DB is not exposed to the host; only the API and metrics ports are published.
- Inbound messages are accepted only from `TELEGRAM_CHAT_IDS`.
- Rotate the bot token and API key if either is ever exposed.
