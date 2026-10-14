# LangSpeed

A web-based typing game where players race against the clock to type English
tongue-twisters as fast and accurately as possible. Pick a difficulty, keep
3 hearts alive, climb the leaderboard.

**Stack:** Go backend · PostgreSQL (permanent data) · Redis (live game state +
leaderboard) · plain HTML/CSS/JS frontend — all orchestrated by Docker Compose.

**Current progress:** Step 1 (walking skeleton) ✅, Step 2
(`game-session-creation`) ✅, Step 3 (`tongue-twister-selection`) ✅ and
Step 4 (`attempts-scoring-hearts`) ✅ — Requirements 1, 2, 3, 5, 6, 7, 8, 9,
20 and parts of 4 and 21 are implemented and verified. See the traceability
tables below.

---

## Layout

```
LangSpeed/
├── docker-compose.yml          # exactly 3 services: server, database, cache
├── .env.example                # all configuration (copy to .env)
├── .dockerignore / .gitignore
├── .github/workflows/ci.yml    # PR gate: go vet + go test -race
├── web/                        # static frontend (placeholder page for now)
└── server/                     # Go module
    ├── Dockerfile              # multi-stage build (context = repo root)
    ├── go.mod / go.sum
    ├── cmd/server/main.go      # startup: config → wait → migrate → seed → listen
    └── internal/
        ├── config/             # env loading + validation (R1.3, R1.4)
        ├── logging/            # structured JSON logger (R21.10)
        ├── store/              # TCP health checks, migrations, Seed_Script,
        │   │                     tongue-twister selection + lookup (R4, R6, R7)
        │   └── migrations/     # 0001..0005 SQL, embedded into the binary
        ├── cache/              # Redis: Game_Session store, R20 serialization,
        │                         attempt state transitions (R7-R9)
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
| `GET` | `/api/games/{id}/next` | — | `200` `{tongue_twister_id, text, difficulty, hearts, score}` — repeats the pending twister until it is attempted · `200 {"completed":true,"message":…}` pool exhausted (R6.3) · `404` unknown session or no content for the difficulty · `409` game already over (0 hearts) · `503` session state could not be retrieved/updated |
| `POST` | `/api/games/{id}/attempts` | `{"text":"she sells seashells…"}` | `200` `{session_id, tongue_twister_id, correct, hearts, score, game_over}` · `400` malformed JSON · `422` missing/empty text · `404` unknown session · `409` game over / no pending twister / twister deleted · `503` cache unavailable |
| `GET` | `/healthz` | — | `200` `{"status":"ok"}` |

```bash
# start a game
curl -s -X POST localhost:3000/api/games \
  -H 'Content-Type: application/json' \
  -d '{"nickname":"carlo","difficulty":"hard"}'
# -> {"session_id":"…","nickname":"carlo","difficulty":"hard","hearts":3,"score":0}

# read it back (also refreshes the 2h inactivity window)
curl -s localhost:3000/api/games/<session_id>

# fetch the next tongue-twister (recorded as shown before it is returned;
# repeated calls keep returning the same one until it is attempted)
curl -s localhost:3000/api/games/<session_id>/next
# -> {"tongue_twister_id":12,"text":"…","difficulty":"hard","hearts":3,"score":0}
# -> {"completed":true,"message":"all tongue-twisters for the current difficulty have been completed"}

# submit the typed text for the pending tongue-twister
curl -s -X POST localhost:3000/api/games/<session_id>/attempts \
  -H 'Content-Type: application/json' \
  -d '{"text":"she sells seashells by the seashore"}'
# -> {"session_id":"…","tongue_twister_id":12,"correct":true,"hearts":3,"score":30,"game_over":false}
```

Nickname: 1–32 characters (trimmed). Difficulty: exactly `easy`, `medium` or
`hard`. Sessions live in Redis under `game_session:{id}` and expire 2 hours
after the last read or write.

**Grading** (R7–R9): the typed text is compared to the pending twister
ignoring letter case and surrounding/doubled whitespace — every other
character must match. A correct attempt scores `easy` 10 / `medium` 20 /
`hard` 30; a wrong one costs one of your 3 hearts **and** consumes the
twister. At 0 hearts the game is over: further attempts and `/next` answer
`409` until Step 5 adds the game-end handling.

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

## Continuous integration (`.github/workflows/ci.yml`)

Every pull request runs the same checks a reviewer would otherwise have to run
locally, so a broken build or a failing test is caught **before** merge, not
after.

| Aspect | Value | Why |
|---|---|---|
| Trigger | `on: pull_request` | the gate lives on PRs; pushes to `master`/`develop` are already covered by the merged PRs themselves |
| Permissions | `contents: read` | least privilege — the workflow only inspects the code, it never writes |
| Concurrency | one run per PR, `cancel-in-progress: true` | pushing again supersedes the previous run; no queue backlog, no stale red ✗ |
| Runner | `ubuntu-latest`, `timeout-minutes: 10` | bounded cost — a hung test fails instead of burning minutes |
| Working dir | `server/` | the Go module lives one level down (`docker-compose.yml` stays at the root) |
| Go version | `go-version-file: server/go.mod` | CI and local toolchains stay in lockstep automatically when the module's Go version changes |
| Module cache | keyed on `server/go.sum` | dependencies are downloaded once per `go.sum` change, not on every run |

**Steps, in order:**

1. **Checkout** — `actions/checkout@v7`.
2. **Set up Go** — `actions/setup-go@v7`, version read from `go.mod` (currently
   1.27), with the build cache keyed on `go.sum`.
3. **`go vet ./...`** — static analysis: the cheap, catchable mistakes (bad
   `Printf` verbs, unreachable code, malformed struct tags) fail the run before
   any test executes.
4. **`go test ./... -race -count=1`** — the full suite with the race detector
   and a forced re-run (`-count=1` disables the test cache, so CI can never
   report a stale "cached ok").

**Notes:**

- The job's status check is named **`go test`** — make that the required check
  in branch protection so nothing merges around it.
- The `store` package's tests use
  [testcontainers-go](https://testcontainers.com/) to spin up a throw-away
  PostgreSQL; Docker is available on GitHub's runners, so they run there and
  only skip on machines without Docker.
- No Docker images are built or pushed by CI yet — `docker compose up --build`
  is still a manual/local verification step (see Step 1's E2E notes).

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

### Step 3 — `tongue-twister-selection`

| Requirement | Where | Status |
|---|---|---|
| R4.4 selection limited to the session's difficulty | `Content.NextTwister` (`difficulty = $1`) | ✅ verified |
| R4.5 inactive tongue-twisters never selected | `AND active` | ✅ verified |
| R4.6 no active content → error for the difficulty | `ErrNoContent` → 404 | ✅ verified (content deactivated live) |
| R6.1 already-shown IDs excluded | `NOT (id = ANY($2))` + `AddShownTwister` | ✅ verified (12/12 unique) |
| R6.2 random from the remaining pool | `ORDER BY random() LIMIT 1` | ✅ |
| R6.3 pool exhausted → notify player, end session within 3 s | `{"completed":true,…}` 200 — notification now, session teardown with R10 in Step 5 (decision 7) | ✅ verified |
| R6.4 record shown ID **before** returning the twister | `handleNextTwister` marks shown first | ✅ verified (Redis `shown_ids`) |
| R6.5 Cache read fails → error, state preserved | `503 the session state could not be retrieved` | ✅ verified (cache stopped) |
| R6.6 Cache write fails → error, **no twister** in the response | `503 the session state could not be updated` | ✅ unit test |
| R21.5 `/next` log lines without the nickname | request log carries path only | ✅ leak check CLEAN |
| Playable catalogue on first start (prerequisite) | `0005_seed_tongue_twisters.sql`, guarded | ✅ verified (12 easy / 10 medium / 8 hard) |

**Tests:** 37 tests across `config`, `cache`, `httpserver`, `store` ·
`go vet ./...` · `go test ./... -race -count=1` green on Linux. The
`store` tests run against a real throw-away PostgreSQL via
[testcontainers-go](https://testcontainers.com/) and skip cleanly where
Docker is unavailable. **E2E (compose):** full 12-twister pool drained with no
repeats, completion notification, R4.6 and R6.5 responses, `shown_ids` in
Redis, no nickname in logs, 3/3 services healthy.

### Step 4 — `attempts-scoring-hearts`

| Requirement | Where | Status |
|---|---|---|
| R7 one attempt per served twister (submit the pending text) | `POST /api/games/{id}/attempts` | ✅ |
| R7.1 attempt graded against the twister the session is typing | `GameSession.CurrentTwisterID` + `Content.TwisterByID` | ✅ verified (E2E) |
| R7.2 one attempt per twister: a resubmission is a 409, never a re-grade | `RecordAttempt` clears the pending ID | ✅ verified |
| R7.3 remaining attempts = remaining Hearts (3, 2 or 1; 0 = lost) | `RecordAttempt` (hearts bottom out at 0) | ✅ verified (see decision 10) |
| R7.4 wrong attempt costs one Heart and consumes the twister | `RecordAttempt(correct=false)` | ✅ verified |
| R8.1 correct attempt adds points: easy 10 / medium 20 / hard 30 | `scoreFor` | ✅ verified (E2E) |
| R8.2 wrong attempt awards nothing | `RecordAttempt` | ✅ verified |
| R8.3 case/whitespace-insensitive grading, characters exact | `normalizeForComparison` | ✅ table test |
| R9.1 game starts with 3 Hearts | `cache.InitialHearts` (unchanged from Step 2) | ✅ |
| R9.5 every attempt response carries the Heart count | `attemptResponse.Hearts` | ✅ verified |
| R9.6 at 0 Hearts the game is over: attempts and `/next` → 409 | `handleAttempt` / `handleNextTwister` guards | ✅ verified (E2E) |
| R7.5 no partial state: rejected requests never mutate the session | validation before any Cache write | ✅ tests assert the pending twister survives |
| R7.6 cache read/write failure → 503, state preserved | `cache.ErrUnavailable` | ✅ verified |
| R21.5 graded-attempt log without nickname or typed text | `handleAttempt` | ✅ leak check CLEAN |

**Tests:** 56 tests across `config`, `cache`, `httpserver`, `store` ·
`go vet ./...` · `go test ./... -race -count=1` green on Linux.
**E2E (compose):** full loop verified — correct attempt (+10/+30), wrong
attempt (−1 heart), duplicate submit → 409, `/next` idempotent while a
twister is pending, third wrong attempt → `game_over:true`, attempts and
`/next` → 409 afterwards, `current_twister_id` cleared and TTL refreshed in
Redis, no nickname or typed text in any log line.

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
5. **R12.6 + R12.8 (resolved by owner):** the leaderboard has **no
   tie-break** — ordering is by score alone. Combined with the outcome model
   (decision 10), ties simply coexist; nothing extra is computed.
6. **R7.3 (resolved by owner):** the "remaining Attempts count" **is** the
   remaining Heart count — a player has 3, 2 or 1 attempts left, and 0 Hearts
   means the game is lost. There is no separate attempts counter.
7. **R6.3 split across Steps 3 and 5:** pool exhaustion returns the completion
   notification immediately (well within 3 s), but *ending* the Game_Session
   writes a Game_Record and updates the leaderboard (R10–R11). The teardown
   lands with Step 5; exhaustion is detected statelessly, so no intermediate
   session field is needed.
8. **Content seeding via guarded migration (`0005`)**, not a script: keeps the
   one-command startup (R1/R2 spirit) and never overwrites a catalogue an
   admin populated later (R14). The whole batch is skipped once any row
   exists.
9. **`/next` responses carry `hearts` (and `score`)** because the request
   modifies Game_Session state (the shown list) — R9.5 requires the current
   Heart count in every such response.
10. **Win/loss model (owner):** you lose when your 3 hearts are gone, you win
    when you complete every tongue-twister of your difficulty. Step 4
    implements the loss side (0 hearts → `game_over`, then 409s); the win
    side's persistence (Game_Record, leaderboard) is Step 5's R10–R12, and
    the R6.3 completion notification already reports "pool exhausted".
11. **Grading is case- and whitespace-insensitive, character-exact
    otherwise:** `strings.EqualFold` on whitespace-collapsed text. A typo in
    any character fails the attempt; " SHE  SELLS…" never does.
12. **Fixed points per difficulty (10/20/30), no speed bonus yet:** the
    requirements text for R8 isn't in the repo; difficulty weighting is the
    only signal the Game_Session carries today. A speed or accuracy bonus can
    be layered on without breaking anything — the response already reports
    `score`.
13. **A wrong attempt consumes the twister** (you then fetch the next one)
    rather than allowing retries: with attempts = hearts, retrying the same
    twister would make the last heart unloseable and trivialise the game.
14. **`GameSession` gained `current_twister_id` (R5.1's seven fields are
    untouched):** grading needs to know which twister an attempt belongs to;
    without it a resubmission could double-score. It is set by `/next`,
    cleared by grading, and `0` means "no attempt pending" → 409.
15. **`/next` is idempotent while an attempt is pending:** it re-serves the
    same twister instead of selecting a new one. Selecting would let a player
    skip hard twisters for free, and a page reload would lose the text.

---

## Roadmap

- [x] **Step 1** — infra, schema, seed, request logging (R1, R2, R21.1–21.4/21.10)
- [x] **Step 2** — game-session-creation (R3, R5, R20, R21.5) — *feature/game-session-creation*
- [x] **Step 3** — tongue-twister selection + next-twister endpoint (R4, R6) — *feature/tongue-twister-selection*
- [x] **Step 4** — typing attempts, scoring, hearts (R7–R9) — *feature/attempts-scoring-hearts*
- [ ] **Step 5** — game end win/loss + leaderboard (R10–R12)
- [ ] **Step 6** — admin JWT auth + CRUD endpoints (R13–R17)
- [ ] **Step 7** — game UI + admin panel frontend (R18, R19)
