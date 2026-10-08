# LangSpeed

A web-based typing game where players race against the clock to type English
tongue-twisters as fast and accurately as possible. Pick a difficulty, keep
3 hearts alive, climb the leaderboard.

**Stack:** Go backend · PostgreSQL (permanent data) · Redis (live game state +
leaderboard) · plain HTML/CSS/JS frontend — all orchestrated by Docker Compose.

This repository currently contains **Step 1 (walking skeleton)** of the
roadmap: the full infrastructure, database schema and admin seed from
`LangSpeed_Requirements_English_Only.md` (Requirements 1, 2 and the request
logging parts of 21).

---

## Layout

```
LangSpeed/
├── docker-compose.yml          # exactly 3 services: server, database, cache
├── .env.example                # all configuration (copy to .env)
├── .dockerignore / .gitignore
├── web/                        # static frontend (placeholder page for now)
└── server/                     # Go module
    ├── Dockerfile              # multi-stage build (context = repo root)
    ├── go.mod / go.sum
    ├── cmd/server/main.go      # startup sequence (config → wait → migrate → seed → listen)
    └── internal/
        ├── config/             # env loading + validation (R1.3, R1.4)
        ├── logging/            # structured JSON logger, levels DEBUG..ERROR (R21)
        ├── store/              # TCP health checks, migrations, Seed_Script
        │   └── migrations/     # 0001..0004 SQL, embedded into the binary
        └── httpserver/         # router, static files, Request_Log middleware
```

---

## Quick start

```bash
cp .env.example .env        # optional - everything has safe dev defaults
docker compose up --build
```

Then open:

| URL | What |
|---|---|
| <http://localhost:3000> | placeholder frontend |
| <http://localhost:3000/healthz> | backend health probe |

First start pulls the images, waits for PostgreSQL + Redis to become healthy,
applies the 4 migrations, creates the first admin user and only then starts
accepting HTTP traffic.

**Admin seed credentials (dev defaults):** `admin` / `admin12345` — set
`ADMIN_USERNAME` / `ADMIN_PASSWORD` in `.env` before first start to change them.
If an admin already exists, the seed is a no-op (R2.10).

Inspect data:

```bash
docker compose exec database psql -U langspeed -d langspeed -c '\dt'
docker compose exec database psql -U langspeed -d langspeed -c 'SELECT username, role FROM users;'
docker compose exec cache redis-cli
docker compose logs -f server        # structured JSON request logs
```

Stop: `docker compose down` (add `-v` to also wipe the `pgdata` volume).

---

## Configuration (R1.3)

All runtime configuration comes from environment variables. The server refuses
to start when a required one is missing and logs **every** missing name in a
single error line before exiting with status 1 (R1.4).

| Variable | Required | Used by | Default |
|---|---|---|---|
| `SERVER_PORT` | ✅ | server + compose host port | `3000` (compose, R1.5) |
| `DATABASE_URL` | ✅ | server | built by compose from `POSTGRES_*` |
| `REDIS_ADDR` | ✅ | server | `cache:6379` (compose) |
| `JWT_SECRET` | ✅ | server (R13) | dev placeholder — **change it** |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | seed only | Seed_Script (R2.8) | `admin` / `admin12345` |
| `STATIC_DIR` | ➖ | server (frontend dir) | `./web`, then `../web` |
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | ➖ | database container | `langspeed` |

> Avoid `@`, `:` and `#` in the dev database password — it is embedded in
> `DATABASE_URL`.

### Running the server outside Docker

The database/cache ports are intentionally **not published** to the host. For
local `go run` development either add `ports:` entries to `database`/`cache` in
`docker-compose.yml`, or run your own local instances, then:

```bash
cd server
$env:SERVER_PORT="3000"             # PowerShell — see .env.example for all four
$env:DATABASE_URL="postgres://langspeed:langspeed@localhost:5432/langspeed?sslmode=disable"
$env:REDIS_ADDR="localhost:6379"
$env:JWT_SECRET="dev"
go run ./cmd/server
```

---

## What step 1 implements (traceability)

| Requirement | Where | Status |
|---|---|---|
| R1.1 exactly 3 services | `docker-compose.yml` | ✅ |
| R1.2 wait for healthy DB/Cache: TCP, ≤5 attempts, ≤30 s | `store.WaitReady` + compose `depends_on: service_healthy` | ✅ verified (5 WARN lines, aborts < 30 s, exit 1) |
| R1.3 config from env vars | `config.Load` | ✅ |
| R1.4 missing var → non-zero exit naming each var | `config.Load` + `main` | ✅ verified |
| R1.5 host port from env, default 3000 | compose `ports: "${SERVER_PORT:-3000}:…"` | ✅ |
| R1.6 named volume for the database | `volumes: pgdata` | ✅ |
| R2.1 migrations applied in order before serving | `store.Migrate` (embedded, ordered, own tx each) | ✅ |
| R2.2 failed migration → halt, name it, no later migrations | `store.Migrate` | ✅ |
| R2.3–R2.6 the four tables | `server/internal/store/migrations/*.sql` | ✅ |
| R2.7 `role` accepts only `admin`/`moderator` | CHECK constraint in `0002` | ✅ |
| R2.8 seed creates exactly one admin | `store.SeedAdmin` (bcrypt cost 12, R13.7) | ✅ |
| R2.9 missing credentials → abort naming them, no insert | `store.SeedAdmin` | ✅ |
| R2.10 admin exists → skip + INFO log | `store.SeedAdmin` | ✅ |
| R21.1 Request_Log: method, path, status, latency | `httpserver.withRequestLog` | ✅ |
| R21.2/R21.3 log severity by status | `severityFor` | ✅ (see note 2 below) |
| R21.10 structured JSON lines | `logging.Logger` | ✅ |
| R21.4 no SQL/stack traces in logs | `dbFailure` + client-safe `writeError` | ✅ |

**Tests:** `cd server && go test ./...` (env validation) ·
`go vet ./...`

---

## Decisions & open questions

1. **Seed runs inside server boot** (after migrations, before listen). The
   glossary calls it a "script that runs on first startup"; making it a startup
   step keeps `docker compose up` a true single command. A dedicated compose
   job would also work if you prefer separation.
2. **503 log level conflict:** R21.2 says *5xx → ERROR* but R21.3 explicitly
   lists `503` among *WARN* codes. We follow the explicit mention → `503` logs
   as WARN, other 5xx as ERROR. Flag this to whoever wrote the spec.
3. **R12.6 + R12.8 (future):** "keep only the highest score per nickname" and
   "equal scores → earlier entry ranks higher" don't match Redis sorted-set
   defaults (lexicographic tie-break). Needs an encoding scheme — design it
   before implementing the leaderboard.
4. **R7.3 (future):** mentions a "remaining Attempts count" but no maximum
   attempts is defined anywhere — only the 3-heart limit. Clarify.
5. **DATABASE_URL errors are never echoed** (they contain the password);
   database failures log the server message + SQLSTATE only, never SQL text.

---

## Roadmap

- [x] **Step 1** — infra, schema, seed, request logging (R1, R2, R21.1/10)
- [ ] **Step 2** — game session + tongue-twister selection (R3–R6, R20)
- [ ] **Step 3** — typing attempts, scoring, hearts (R7–R9)
- [ ] **Step 4** — game end win/loss + leaderboard (R10–R12)
- [ ] **Step 5** — admin JWT auth + CRUD endpoints (R13–R17)
- [ ] **Step 6** — game UI + admin panel frontend (R18, R19), rest of R21
