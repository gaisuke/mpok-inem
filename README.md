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
| GET | `/v1/pockets/{id}` | one pocket with its balance |
| PATCH/DELETE | `/v1/pockets/{id}` | rename/retype/reset a pocket; delete an empty one |
| POST | `/v1/transactions` | record a transaction |
| GET | `/v1/transactions` | transaction detail (`from`, `to`, `category`, `direction`, `pocket_id`, `limit`) |
| GET/PATCH/DELETE | `/v1/transactions/{id}` | fetch, fix note/category, or drop a mistaken entry |
| POST | `/v1/transfers` | move money between pockets |
| GET | `/v1/transfers` | transfers (`month`, or `from`/`to`, `pocket_id`, `limit`) |
| DELETE | `/v1/transfers/{id}` | drop a mistaken transfer |
| GET | `/v1/expense/summary` | totals + pocket balances (`month`, or `from`/`to`) |
| POST/GET | `/v1/notes` | capture / list notes (`tag`, `limit`) |
| GET/PATCH/DELETE | `/v1/notes/{id}` | fetch, edit, delete one note |
| GET | `/v1/notes/search` | full-text search |
| POST/GET | `/v1/meals` | log a meal / list meals for a day (`day`, `limit`) |
| PATCH/DELETE | `/v1/meals/{id}` | fix or drop a meal |
| GET | `/v1/nutrition/daily` | daily macro rollup (`day`) |
| GET/PUT/DELETE | `/v1/nutrition/target` | read / set / clear a daily calorie+protein target |
| GET/POST | `/v1/admin/users` | household roster / link a member |
| DELETE | `/v1/admin/users/{id}` | unlink a member and wipe their rows |
| POST | `/v1/admin/reset` | wipe a user's data: `{"confirm":"RESET","scope":"ledger"\|"all"}` |

Destructive calls need an explicit confirmation — `?confirm=true` on DELETE, the
literal `"RESET"` body on `/v1/admin/reset` — so no stray request deletes data.
The agent drives all of this through
`~/.hermes/skills/personal/inem/scripts/inem.py` and never opens psql: every
household fix (rename a pocket, retype eCard, delete a wrong entry, add a family
member, reset test data) has an endpoint.

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

- No write path exists in the binary — only GET routes are registered, so every
  POST/PATCH/DELETE answers 405 and never reaches inemd.
- `INEM_WEB_ADDR` (default `127.0.0.1:8090`), `INEM_WEB_USER` (internal user id),
  `INEM_WEB_AUTH_PASS` (HTTP basic auth; unset = no auth, so only bind localhost
  or put TLS+auth in front). Deployed as `deploy/inemdash.service` with
  `/etc/inem-dash.env`; nginx proxies `https://danimunf.duckdns.org/inem/` to it.

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
