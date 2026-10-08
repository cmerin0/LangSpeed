# LangSpeed

A web-based typing game where players race against the clock to type English
tongue-twisters as fast and accurately as possible. Pick a difficulty, keep
3 hearts alive, climb the leaderboard.

**Stack:** Go backend · PostgreSQL (permanent data) · Redis (live game state +
leaderboard) · plain HTML/CSS/JS frontend — all orchestrated by Docker Compose.

**Current progress:** Step 1 (walking skeleton) ✅ and Step 2
(`game-session-creation`) ✅ — Requirements 1, 2, 3, 5, 20 and parts of 21 are
implemented and verified. See the traceability tables below.

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
    ├── cmd/server/main.go      # startup: config → wait → migrate → seed → listen
    └── internal/
        ├── config/             # env loading + validation (R1.3, R1.4)
        ├── logging/            # structured JSON logger (R21.10)
        ├── store/              # TCP health checks, migrations, Seed_Script
        │   └── migrations/     # 0001..0004 SQL, embedded into the binary
        ├── cache/              # Redis: Game_Session store + R20 serialization
        └── httpserver/         # router, Request_Log middleware, game API
```

---

## Quick start

```bash
cp .env.example .env        # optional - everything has safe dev defaults
docker compose up --build
```

Then open <http://localhost:3000> (placeholder frontend) or
<http://localhost:3000/healthz> (backend health probe).

**Admin seed credentials (dev defaults):** `admin` / `admin12345` — set
`ADMIN_USERNAME` / `ADMIN_PASSWORD` in `.env` before first start to change them.

---

## API

| Method | Path | Body | Responses |
|---|---|---|---|
| `POST` | `/api/games` | `{"nickname":"carlo","difficulty":"easy"}` | `201` `{session_id, nickname, difficulty, hearts, score}` · `400` malformed JSON · `422` invalid/missing field(s) · `503` cache unavailable |
| `GET` | `/api/games/{id}` | — | `200` session state · `404` not found or expired · `503` cache unavailable |
| `GET` | `/healthz` | — | `200` `{"status":"ok"}` |

```bash
# start a game
curl -s -X POST localhost:3000/api/games \
  -H 'Content-Type: application/json' \
  -d '{"nickname":"carlo","difficulty":"hard"}'
# -> {"session_id":"…","nickname":"carlo","difficulty":"hard","hearts":3,"score":0}

# read it back (also refreshes the 2h inactivity window)
curl -s localhost:3000/api/games/<session_id>
```

Nickname: 1–32 characters (trimmed). Difficulty: exactly `easy`, `medium` or
`hard`. Sessions live in Redis under `game_session:{id}` and expire 2 hours
after the last read or write.

---

## Configuration (R1.3)

The server refuses to start when a required variable is missing and logs
**every** missing name in one error line before exiting with status 1 (R1.4).

| Variable | Required | Used by | Default |
|---|---|---|---|
| `SERVER_PORT` | ✅ | server + compose host port | `3000` (compose, R1.5) |
| `DATABASE_URL` | ✅ | server | built by compose from `POSTGRES_*` |
| `REDIS_ADDR` | ✅ | server (Game_Session store) | `cache:6379` (compose) |
| `JWT_SECRET` | ✅ | server (R13, later) | dev placeholder — **change it** |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | seed only | Seed_Script (R2.8) | `admin` / `admin12345` |
| `STATIC_DIR` | ➖ | server (frontend dir) | `./web`, then `../web` |
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | ➖ | database container | `langspeed` |

> Avoid `@`, `:` and `#` in the dev database password — it is embedded in
> `DATABASE_URL`. To run the server outside Docker, add `ports:` entries for
> `database`/`cache` in `docker-compose.yml` and point `DATABASE_URL` /
> `REDIS_ADDR` at `localhost` (see `.env.example`).

---

## Traceability

### Step 1 — infrastructure, schema, seed

| Requirement | Where | Status |
|---|---|---|
| R1.1 exactly 3 services | `docker-compose.yml` | ✅ |
| R1.2 wait for healthy DB/Cache: TCP, ≤5 attempts, ≤30 s | `store.WaitReady` + `depends_on: service_healthy` | ✅ verified |
| R1.3 config from env vars | `config.Load` | ✅ |
| R1.4 missing var → non-zero exit naming each var | `config.Load` + `main` | ✅ verified |
| R1.5 host port from env, default 3000 | compose `ports` | ✅ |
| R1.6 named volume for the database | `volumes: pgdata` | ✅ |
| R2.1/R2.2 ordered migrations, halt + name on failure | `store.Migrate` | ✅ |
| R2.3–R2.6 the four tables | `store/migrations/*.sql` | ✅ |
| R2.7 `role` only `admin`/`moderator` | CHECK in `0002` | ✅ |
| R2.8–R2.10 Seed_Script (bcrypt 12, skip if exists, abort on missing vars) | `store.SeedAdmin` | ✅ verified |
| R21.1 Request_Log: method, path, status, latency | `httpserver.withRequestLog` | ✅ |
| R21.2/R21.3 severity by status | `severityFor` | ✅ (503→WARN, see note 2) |
| R21.4 no SQL/stack traces in logs | `dbFailure` + client-safe `writeError` | ✅ |

### Step 2 — `game-session-creation`

| Requirement | Where | Status |
|---|---|---|
| R3.1 start-game endpoint (nickname 1–32, difficulty enum) | `POST /api/games` | ✅ |
| R3.2 empty nickname → 422 "nickname is required" | `validateStartRequest` | ✅ verified |
| R3.3 >32 chars → 422 naming the limit | `validateStartRequest` | ✅ verified |
| R3.4 bad difficulty → 422 listing all three values | `validateStartRequest` | ✅ verified |
| R3.5 creates Game_Session in Cache, returns Session_ID | `cache.Store.Create` | ✅ verified (201) |
| R3.6 cache unavailable → 503 | `cache.ErrUnavailable` | ✅ verified (answered in 2.5 s) |
| R5.1 all 7 fields initialized (3 hearts, 0 score, empty list, timestamp) | `cache.NewGameSession` | ✅ unit + Redis inspection |
| R5.2 exactly 2 h TTL, reset on every read **and** write | `SET NX EX` + `GETEX` + `Save` | ✅ verified (TTL 92 → 7200) |
| R5.3 unknown Session_ID → error response | `GET /api/games/{id}` → 404 | ✅ verified |
| R5.4 exactly one session per Session_ID | one Redis key per UUID | ✅ |
| R5.5 Session_ID returned in the response body | 201 body | ✅ |
| R5.6 invalid/missing fields → 422, **no partial session** | validation before any Redis call | ✅ tests assert 0 keys |
| R5.7 concurrent creates → only one session stored | `SET NX` + ID regeneration | ✅ unit test |
| R20.1 deterministic serialization (identical bytes) | struct-only JSON | ✅ byte-equality test |
| R20.2/R20.3 lossless round-trip of every field | `Serialize`/`Deserialize` | ✅ field-by-field test |
| R20.4/R20.5 no partial write, no partial object | nil bytes / nil object on error | ✅ tests |
| R21.5 session-created INFO log without the nickname | `handleStartGame` | ✅ leak check CLEAN |

**Tests:** `cd server && go test ./...` (23 tests across `config`, `cache`,
`httpserver`) · `go vet ./...`

---

## Decisions & open questions

1. **Seed runs inside server boot** (after migrations, before listen) to keep
   `docker compose up` a single command.
2. **503 log level conflict:** R21.2 says *5xx → ERROR* but R21.3 explicitly
   lists `503` among *WARN* codes → we log 503 as WARN, other 5xx as ERROR.
3. **Session API statuses not in the spec:** success is `201 Created`
   (REST convention), a syntactically malformed JSON body is `400`; every
   spec-defined validation failure uses `422` as written.
4. **Whitespace-only nicknames** are treated as empty (422) and valid
   nicknames are stored trimmed — the glossary requires a *non-empty* nickname.
5. **R12.6 + R12.8 (future):** "highest score per nickname" vs "earlier entry
   wins ties" doesn't match Redis sorted-set defaults — design before building
   the leaderboard.
6. **R7.3 (future):** mentions a "remaining Attempts count" that no requirement
   defines (only the 3-heart limit) — clarify.

---

## Roadmap

- [x] **Step 1** — infra, schema, seed, request logging (R1, R2, R21.1–21.4/21.10)
- [x] **Step 2** — game-session-creation (R3, R5, R20, R21.5) — *feature/game-session-creation*
- [ ] **Step 3** — tongue-twister selection + next-twister endpoint (R4, R6)
- [ ] **Step 4** — typing attempts, scoring, hearts (R7–R9)
- [ ] **Step 5** — game end win/loss + leaderboard (R10–R12)
- [ ] **Step 6** — admin JWT auth + CRUD endpoints (R13–R17)
- [ ] **Step 7** — game UI + admin panel frontend (R18, R19)
