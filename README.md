# LangSpeed

A web-based typing game where players race against the clock to type English
tongue-twisters as fast and accurately as possible. Pick a difficulty, keep
3 hearts alive, climb the leaderboard.

**Stack:** Go backend · PostgreSQL (permanent data) · Redis (live game state +
leaderboard) · plain HTML/CSS/JS frontend — all orchestrated by Docker Compose.

**Current progress:** Step 1 (walking skeleton) ✅, Step 2
(`game-session-creation`) ✅, Step 3 (`tongue-twister-selection`) ✅, Step 4
(`attempts-scoring-hearts`) ✅ and Step 5 (`game-end-leaderboard`) ✅ —
Requirements 1, 2, 3, 5, 6, 7, 8, 9, 10, 11, 12, 20 and parts of 4 and 21 are
implemented and verified. See the traceability tables below.

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
        │   │                     twister selection/lookup (R4, R6, R7),
        │   │                     finished-game records (R10, R11)
        │   └── migrations/     # 0001..0005 SQL, embedded into the binary
        ├── cache/              # Redis: Game_Session store, R20 serialization,
        │   │                     attempt/end state (R7-R11), leaderboard (R12)
        └── httpserver/         # router, Request_Log, game API, game end,
                                # leaderboard endpoint (R10-R12)
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
| `GET` | `/api/games/{id}` | — | `200` session state (+`game_over`) · `404` not found or expired · `503` cache unavailable |
| `GET` | `/api/games/{id}/next` | — | `200` `{tongue_twister_id, text, difficulty, hearts, score}` — repeats the pending twister until it is attempted · `200 {"completed":true,"message":…,"game_over":true,"hearts":…,"score":…}` pool exhausted (R6.3) — **persists the finished game (R10–R12)** · `404` unknown session or no content for the difficulty · `409` game already ended (0 hearts or completed) · `503` session state could not be retrieved/updated |
| `POST` | `/api/games/{id}/attempts` | `{"text":"she sells seashells…"}` | `200` `{session_id, tongue_twister_id, correct, hearts, score, game_over}` — at 0 hearts the same request **persists the finished game (R10–R12)** · `400` malformed JSON · `422` missing/empty text · `404` unknown session · `409` game over / no pending twister / twister deleted · `503` cache unavailable |
| `GET` | `/api/leaderboard` | — | `200` `{easy:[{rank,nickname,score}…], medium:[…], hard:[…]}` — top 10 per difficulty, best score kept per nickname · `503` cache unavailable |
| `GET` | `/healthz` | — | `200` `{"status":"ok"}` |

```bash
# start a game
curl -s -X POST localhost:3000/api/games \
  -H 'Content-Type: application/json' \
  -d '{"nickname":"carlo","difficulty":"hard"}'
# -> {"session_id":"…","nickname":"carlo","difficulty":"hard","hearts":3,"score":0}

# read it back (also refreshes the 2h inactivity window; game_over turns
# true once the game has finished - lost or won)
curl -s localhost:3000/api/games/<session_id>

# fetch the next tongue-twister (recorded as shown before it is returned;
# repeated calls keep returning the same one until it is attempted)
curl -s localhost:3000/api/games/<session_id>/next
# -> {"tongue_twister_id":12,"text":"…","difficulty":"hard","hearts":3,"score":0}
# -> {"completed":true,"message":"…","game_over":true,"hearts":3,"score":390}
#    (the win: this response also wrote the Game_Record and the leaderboard)

# submit the typed text for the pending tongue-twister (at 0 hearts the
# response carries game_over:true and the result has been persisted)
curl -s -X POST localhost:3000/api/games/<session_id>/attempts \
  -H 'Content-Type: application/json' \
  -d '{"text":"she sells seashells by the seashore"}'
# -> {"session_id":"…","tongue_twister_id":12,"correct":true,"hearts":3,"score":30,"game_over":false}

# leaderboard: top 10 per difficulty, each nickname's highest score
curl -s localhost:3000/api/leaderboard
# -> {"easy":[{"rank":1,"nickname":"carlo","score":170}],"medium":[],"hard":[]}
```

Nickname: 1–32 characters (trimmed). Difficulty: exactly `easy`, `medium` or
`hard`. Sessions live in Redis under `game_session:{id}` and expire 2 hours
after the last read or write.

**Grading** (R7–R9): the typed text is compared to the pending twister
ignoring letter case and surrounding/doubled whitespace — every other
character must match. A correct attempt scores `easy` 10 / `medium` 20 /
`hard` 30; a wrong one costs one of your 3 hearts **and** consumes the
twister. At 0 hearts the game is lost; completing every twister of the
difficulty wins it. Either way the finished game is persisted — one
`game_records` row with its `attempt_details` breakdown in PostgreSQL, one
leaderboard entry in Redis — and further attempts and `/next` answer `409
the game has already ended` (Step 5).

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
| R6.3 pool exhausted → notify player, end session within 3 s | `{"completed":true,"game_over":true,…}` 200 — notification **and** the R10–R12 teardown now happen in the same request (decision 7, landed in Step 5) | ✅ verified |
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

### Step 5 — `game-end-leaderboard`

The R10–R12 requirement texts are not in the repo either (the migrations and
decision 5 cite a handful of IDs); the rows below are the design the owner
asked for, same as Step 4.

| Requirement | Where | Status |
|---|---|---|
| R10 finished games are recorded (nickname, final score, difficulty, start/end time) | `store.Records.SaveGameResult` ← `httpserver.endGame` | ✅ verified (E2E: 3 rows) |
| R10.1 exactly one Game_Record per finished game | `cache.MarkEnded` write-once end stamp → `ErrAlreadyEnded` | ✅ verified (refused requests add none) |
| R10.2/R11.2 one attempt-detail row per twister, written atomically with the record | `SaveGameResult` (single transaction, rollback on failure) | ✅ testcontainers + rollback test |
| R10 loss half: 0 hearts → record written before `game_over` is reported | `handleAttempt` → `endGame` | ✅ verified (E2E) |
| R10 win half: pool completed with hearts left → record written with the R6.3 notification (decision 7 landed) | `handleNextTwister` (`ErrAllShown` → `endGame`) | ✅ verified (E2E, 17 twisters) |
| R11 per-attempt data: `attempt_count`, `time_taken_ms`, `success` | `GameSession.Attempts` + `ServedAt`; `RecordAttempt` measures serve→grade, clamped to int4; `attempt_count` = 1 (decision 13) | ✅ verified (24 rows, all counts 1, times ≥ 0) |
| R11.1 twisters deleted mid-game are skipped, the record still lands | `existingTwisterIDs` filter inside the transaction | ✅ testcontainers |
| R12 leaderboard lives in the Cache, score-ordered | `leaderboard:{difficulty}` sorted sets | ✅ verified (redis-cli) |
| R12.6 highest score per nickname | `ZADD … GT` (`SaveLeaderboardScore`) | ✅ verified (second run at 0 kept the 30) |
| R12 read API | `GET /api/leaderboard` → 3 boards, top 10, 1-based ranks | ✅ verified |
| Game-end guards: play and re-persistence are refused after the end | 409 `the game has already ended` on attempts + `/next` (both outcomes) | ✅ verified (E2E) |
| Finished state visible to a reloading client | `game_over` in `GET /api/games/{id}` and in the completion response | ✅ verified |
| R6.3 budget: win path persists within the same request | one cache round-trips + one local PostgreSQL transaction | ✅ (E2E responses immediate) |
| R21.5 game-end log lines without the nickname | `endGame` (`game ended` INFO; failures log session ID + sanitized cause only) | ✅ leak check CLEAN, 0 ERROR lines |

**Tests:** 73 tests across `config`, `cache`, `httpserver`, `store` ·
`go vet ./...` · `go test ./... -race -count=1` green on Linux (the `store`
tests again run against a throw-away PostgreSQL via
[testcontainers-go](https://testcontainers.com/)). **E2E (compose):** loss
flow (1 correct + 3 wrong → `game_over`, then attempts and `/next` → 409),
win flow (all 17 easy twisters completed → completion response with
`game_over` + final result, then `/next` → 409), `game_records` holds 3 rows
and `attempt_details` 24 rows (all `attempt_count` 1, times ≥ 0, `ended_at >
started_at`), leaderboard shows the winner at 170 and keeps a nickname's
best score across two runs, `GET` reports `game_over` after both outcomes,
no nickname or typed text in any log line.

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
   writes a Game_Record and updates the leaderboard (R10–R12). The teardown
   landed with Step 5 — via the session's `ended_at` end stamp (decision 18)
   — while exhaustion itself is still detected statelessly at `/next`, so no
   separate "pool exhausted" session field exists.
8. **Content seeding via guarded migration (`0005`)**, not a script: keeps the
   one-command startup (R1/R2 spirit) and never overwrites a catalogue an
   admin populated later (R14). The whole batch is skipped once any row
   exists.
9. **`/next` responses carry `hearts` (and `score`)** because the request
   modifies Game_Session state (the shown list) — R9.5 requires the current
   Heart count in every such response.
10. **Win/loss model (owner):** you lose when your 3 hearts are gone, you win
    when you complete every tongue-twister of your difficulty. Step 4
    implements the loss side (0 hearts → `game_over`, then 409s); Step 5
    persists **both** outcomes (decision 18) — the loss at the grading that
    takes the last Heart, the win at the `/next` that reports "pool
    exhausted", whose response now carries the final result as well.
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
16. **One leaderboard board per difficulty (`leaderboard:{difficulty}`):**
    scores are only comparable within a difficulty — the seeded pools cap at
    170 (easy), 300 (medium) and 390 (hard) points — so a single mixed board
    would show only hard games. `GET /api/leaderboard` returns all three
    boards (top 10 each, 1-based ranks) in one payload. *Sourcing: the R12
    request text is not in the repo; revisit if it says otherwise.*
17. **A leaderboard entry is a nickname's *highest* score (`ZADD … GT`):**
    R12.6's "highest score per nickname" (quoted in decision 5) is kept, so a
    later worse run never demotes anyone; equal scores simply sit next to
    each other and nothing breaks ties (decision 5). Identical nicknames
    share one entry — the board is keyed by nickname, which is all a
    pre-auth game has.
18. **Game end runs in exactly one place (`endGame`), triggered twice:**
    loss — inside the attempt request that drops the last Heart; win — inside
    the `/next` request that finds the pool exhausted with hearts left. It
    stamps the session's `ended_at` **once** (`MarkEnded` → `ErrAlreadyEnded`
    for racing duplicates), which is both the write-once guard for
    `game_records` (R10.1) and the flag behind the `409 the game has already
    ended` guards — including on a won game, where hearts alone would say
    "still playable".
19. **Per-attempt data accumulates in the session until game end:**
    `GameSession` gained `served_at` (when the pending twister was handed
    out, R11's `time_taken_ms` source) and `attempts` (one
    `{tongue_twister_id, time_taken_ms, correct}` record per graded twister,
    appended by `RecordAttempt`). At game end they become the
    `attempt_details` rows of one transaction with the `game_records` insert —
    no placeholder rows, no schema change, and R20.1 determinism holds
    (struct fields + slice order, no maps). `attempt_count` is always 1 in
    the written rows: with one attempt per twister (decision 13) nothing else
    can occur; the column is there for a future retry mode.
20. **Game-end persistence is best effort after the end stamp:** the session
    state is authoritative — guards refuse play from `ended_at` on — so a
    failed PostgreSQL write or leaderboard update logs an ERROR (session ID
    and sanitized cause only, never the nickname: R21.4/R21.5) while the
    player still gets the `game_over` they earned, instead of a 500 whose
    retry would hit a 409 and lose the result forever. The two writes are
    independent (a leaderboard entry can survive a failed record insert), and
    a failed *stamp* on the win path heals itself: the session stays
    un-ended, so the next `/next` finds the exhausted pool and retries. The
    accepted read-modify-write race window matches Step 4's single-player
    assumption; `MarkEnded` closes the common interleavings.
21. **Two additive response fields for the finished game:** `game_over` in
    `GET /api/games/{id}` (so a reloading client stops playing — it must see
    a *won* game too, where hearts remain) and `game_over`/`hearts`/`score`
    in the R6.3 completion response (the final result arrives with the
    notification, no second request). R5.5's mandated field is untouched.
22. **Abandoned sessions produce no record:** R10 is for *finished* games —
    a session that expires after 2 h without reaching 0 hearts or the pool's
    end is simply gone (its Redis state with it). Persisting partial results
    would need a background job the requirements don't ask for.

---

## Roadmap

- [x] **Step 1** — infra, schema, seed, request logging (R1, R2, R21.1–21.4/21.10)
- [x] **Step 2** — game-session-creation (R3, R5, R20, R21.5) — *feature/game-session-creation*
- [x] **Step 3** — tongue-twister selection + next-twister endpoint (R4, R6) — *feature/tongue-twister-selection*
- [x] **Step 4** — typing attempts, scoring, hearts (R7–R9) — *feature/attempts-scoring-hearts*
- [x] **Step 5** — game end win/loss + leaderboard (R10–R12) — *feature/game-end-leaderboard*
- [ ] **Step 6** — admin JWT auth + CRUD endpoints (R13–R17)
- [ ] **Step 7** — game UI + admin panel frontend (R18, R19)
