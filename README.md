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

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | liveness |
| POST | `/internal/v1/handle` | uniform entry contract (PRD TRD §4.2) |
| GET/POST | `/v1/pockets` | list / create pockets |
| POST | `/v1/transactions` | record a transaction |
| POST | `/v1/transfers` | move money between pockets |
| GET | `/v1/expense/summary` | balances and spend summary |
| POST/GET | `/v1/notes` | capture / list notes |
| GET | `/v1/notes/search`, `/v1/notes/{id}` | search, fetch one |
| POST | `/v1/meals`, GET `/v1/nutrition/daily` | log a meal / daily rollup |

## Deployment notes

- Binds to localhost only; never exposed.
- Postgres is local-only with `trust` auth on `127.0.0.1` (see
  `docs/DECISIONS.md` for why: SCRAM handshake failed on this box while the
  stored verifier matched — a local-only trust line was the pragmatic fix).
- Voice input will use self-hosted `whisper.cpp`; OCR via `tesseract`.

## Status

Phase 0 — schema, identity, domain service skeleton. Endpoints are live and
being verified. Next: systemd unit, then wiring the Hermes side.
