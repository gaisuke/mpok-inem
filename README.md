# Mpok Inem

Personal AI platform — one Telegram chat in front of three household services:
**expense tracking**, a **second brain** (notes), and a **nutrition logger**.

Owner: Dani. Private project, running on a single VPS.

## Architecture (as built)

Hybrid, not the pure-microservice version from the original PRD.

- **Hermes gateway** stays the Telegram entrypoint and the brain: sessions,
  memory, persona, intent routing.
- **`inemd`** (Go, this repo) provides the deterministic domain layer —
  persistence, validation, aggregation — behind plain HTTP on localhost.

Why: Hermes already does transport, sessions and LLM calls well; reimplementing
that in Go would have been pure maintenance cost. The Go service owns what an
LLM should never own: money math, data integrity, structured queries.

Full rationale and the amendments to the original PRD/TRD live in
[`docs/DECISIONS.md`](docs/DECISIONS.md).

## Layout

    db/001_init.sql     Postgres schema: inem_auth, expense, brain, nutrition
    service/main.go     inemd — all domain endpoints in one binary
    docs/DECISIONS.md   architecture decisions, hardware constraints, findings
    bin/                build output (gitignored)

## Data model

- `inem_auth.users` — identity, keyed by Telegram user id. Two users from day
  one (Dani + wife), so adding the second is a row, not a migration.
- `expense.pockets` / `transactions` / `transfers` — ledger only. Investment
  tracking is logged value, no live pricing.
- `brain.notes` / `note_links` — markdown notes; `pgvector` extension is
  installed for the Phase 5 embedding work.
- `nutrition.meals` / `meal_items` / `daily_targets` — meals with per-item
  macro estimates and per-day targets.

## Running it

Requirements: Go 1.22+, PostgreSQL 16.

    sudo -u postgres createdb inem
    psql -h 127.0.0.1 -U inem -d inem -f db/001_init.sql

    cd service
    go build -o ../bin/inemd .
    INEM_DSN="postgres://inem@127.0.0.1:5432/inem?sslmode=disable" ../bin/inemd

Listens on `127.0.0.1:8777` (override with `INEM_ADDR`). Health check:

    curl http://127.0.0.1:8777/healthz

Domain routes require an `X-User-ID` header (the caller — Hermes — resolves the
Telegram user to a `inem_auth.users.id` first); `/internal/v1/handle` takes
`user_id` in the body instead.

### Deploying as a service

    sudo cp deploy/inemd.service /etc/systemd/system/
    sudo cp deploy/inem.env.example /etc/inem.env   # then edit the DSN
    sudo chmod 600 /etc/inem.env
    sudo systemctl daemon-reload && sudo systemctl enable --now inemd
    journalctl -u inemd -f

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | liveness |
| POST | `/internal/v1/handle` | uniform entry contract (PRD TRD §4.2) |
| GET/POST | `/v1/pockets` | list / create pockets |
| GET | `/v1/pockets/{id}` | one pocket with its balance (own, or shared with you) |
| PATCH/DELETE | `/v1/pockets/{id}` | rename/retype/share a pocket; delete an empty one |
| GET | `/v1/household` | every member's *shared* pockets + household total |
| POST | `/v1/transactions` | record a transaction |
| GET | `/v1/transactions` | transaction detail (`from`, `to`, `category`, `direction`, `pocket_id`, `limit`) |
| GET/PATCH/DELETE | `/v1/transactions/{id}` | fetch, fix note/category, or drop a mistaken entry |
| POST | `/v1/transfers` | move money between pockets (`from_pocket_id`, `to_pocket_id`, `amount_idr`, `note`); destination may be another member's shared pocket |
| GET | `/v1/transfers` | transfers (`month`, or `from`/`to`, `pocket_id`, `limit`) — includes moves into/out of your pockets made by the other member |
| DELETE | `/v1/transfers/{id}` | drop a mistaken transfer |
| GET/POST | `/v1/schedules` | recurring plans (`?date=`, `?pending=true`) / add one (`kind`, `from_pocket_id`, `to_pocket_id`, `amount_idr` omitted = figure varies, `day_of_month`); destination may be another member's shared pocket |
| PATCH/DELETE | `/v1/schedules/{id}` | change amount/day/note or pause it / delete a plan |
| POST | `/v1/schedules/run` | book the confirmed plans once per month (`date`, `entry_date`, `ids`, `amount_idr` for one variable plan, `force`, `mark_only`) |
| GET | `/v1/expense/summary` | totals + pocket balances (`month`, or `from`/`to`) |
| POST/GET | `/v1/notes` | capture / list notes (`tag`, `limit`) |
| GET/PATCH/DELETE | `/v1/notes/{id}` | fetch, edit, delete one note |
| GET | `/v1/notes/search` | full-text search |
| POST/GET | `/v1/meals` | log a meal / list meals for a day (`day`, `limit`) |
| PATCH/DELETE | `/v1/meals/{id}` | fix or drop a meal |
| GET | `/v1/nutrition/daily` | daily macro rollup (`day`) |
| GET/PUT/DELETE | `/v1/nutrition/target` | read / set / clear a daily calorie+protein target |
| GET/POST | `/v1/admin/users` | household roster / link a member (`scope`: full\|finance) |
| GET | `/v1/me` | who am I: id, display name, scope |
| DELETE | `/v1/admin/users/{id}` | unlink a member and wipe their rows |
| POST | `/v1/admin/reset` | wipe a user's data: `{"confirm":"RESET","scope":"ledger"\|"all"}` |

Destructive calls need an explicit confirmation — `?confirm=true` on DELETE, the
literal `"RESET"` body on `/v1/admin/reset` — so no stray request deletes data.
The agent drives all of this through
`~/.hermes/skills/personal/inem/scripts/inem.py` and never opens psql: every
household fix (rename a pocket, retype eCard, delete a wrong entry, add a family
member, reset test data) has an endpoint.

### Pocket sharing

A pocket is `private` until its owner shares it (`visibility: shared`). Sharing is
**read-only**: the other members can open the pocket, its ledger and its moves,
but every write stays scoped to the owner (`POST /v1/transactions`,
`PATCH/DELETE /v1/pockets/{id}` → 404 for anyone else).

- A shared pocket's ledger is complete — the balance must still add up — but a
  counterparty pocket the reader may not see is masked as `(pribadi)` in
  `GET /v1/transfers`, so a private pocket's name never leaks.
- `/v1/household` returns only shared pockets, grouped per member, plus the total.
- Private pockets are invisible in every read path (`404` / `403`), including the
  household view.

## Tests

`go test ./...` covers every endpoint in the route table and fails if one is
never exercised (`service/cover_test.go` is a coverage gate, not a percentage
target). The suite runs against a real Postgres so the SQL, constraints and
balance maths are exercised for real:

```
sudo -u postgres psql -c "CREATE DATABASE inem_test OWNER inem"
sudo -u postgres psql -d inem_test -c "CREATE EXTENSION IF NOT EXISTS vector"
cd service && go test ./... -cover
```

`INEM_TEST_DSN` overrides the default `postgres://inem@127.0.0.1:5432/inem_test`;
if the database is unreachable the DB-backed tests skip instead of failing.
The suite truncates its tables between tests and never touches the `inem`
database. It also guards the bugs found while writing it: a `tags`-less note
POST used to violate NOT NULL, and a reset used to rewind a table's id sequence
while other members still had rows (colliding ids on the next INSERT).

## Read-only dashboard (inemdash)

`service/dashboard` serves one static page + a GET-only JSON passthrough to
inemd: ringkasan (period totals, category bars, pocket balances), transaksi,
notes with search/tag filter, and meals with macros + daily target. Tapping a
pocket opens that pocket's own ledger (`/api/pocket`, which merges the pocket,
its entries and the transfers in/out of it into one response).

Login is a Telegram Mini App: the bot's menu button opens the page with
Telegram's signed `initData` in the URL hash, inemdash forwards it to inemgate,
and the verified telegram id decides *whose* data is served (no shared login, no
password). `GET /api/whoami`, `POST /auth/telegram`, `POST /auth/password`
(break-glass, only when configured) and `GET /auth/logout` are the only
non-GET routes — none of them can reach inemd, so the data path stays read-only.

- No write path exists in the binary — only GET routes are registered, so every
  POST/PATCH/DELETE answers 405 and never reaches inemd.
- `INEM_WEB_ADDR` (default `127.0.0.1:8090`), `INEM_GATE_BASE`
  (default `http://127.0.0.1:8778`), `INEM_WEB_USER` + `INEM_WEB_AUTH_USER` /
  `INEM_WEB_AUTH_PASS` for the break-glass password. Deployed as
  `deploy/inemdash.service` with `/etc/inem-dash.env`; nginx proxies
  `https://danimunf.duckdns.org/inem/` to it.

## Telegram identity (inemgate)

`service/inemgate` is the only process holding `TELEGRAM_BOT_TOKEN`. It verifies
the Mini App `initData` HMAC, rejects stale payloads, maps the telegram id to an
inemd member, and mints a signed session token that inemdash asks it to verify.

```
sudo -u postgres psql -c "CREATE DATABASE inem_test OWNER inem"   # tests
# secrets live in /etc/inem-gate.env (root, 600): bot token + session secret
```

- Binds `127.0.0.1:8778` and is never proxied: a bug in the internet-facing
  dashboard leaks data, not the bot token (which can read every bot update and
  impersonate the bot).
- Unknown telegram ids are refused (`403`) and a broken roster fails closed.
- Sessions last `INEM_GATE_TTL_HOURS` (default 720h); `INEM_GATE_MAX_SKEW_SEC`
  (default 300) bounds how old an `initData` may be.

## Deployment notes

- inemd binds to localhost only; never exposed directly. The dashboard is the
  only off-box surface and it is read-only + basic auth over TLS.
- Postgres is local-only with `trust` auth on `127.0.0.1` (see
  `docs/DECISIONS.md` for why: SCRAM handshake failed on this box while the
  stored verifier matched — a local-only trust line was the pragmatic fix).
- Voice input will use self-hosted `whisper.cpp`; OCR via `tesseract`.

## Status

Phase 0 — schema, identity, domain services, full REST surface (CRUD + admin),
test suite with a route-coverage gate, read-only dashboard behind nginx. Next:
the Hermes side in daily use, then voice/OCR input.
