# Mpok Inem — Architecture Decisions (v1.0, 2026-09-15)

Supersedes the corresponding sections of PRD/TRD v0.2 where noted.
Decisions D1–D3 made by Dani after a live verification pass against the VPS.

## D1 — Hybrid routing (amends PRD §2)
The Telegram entry point and the "router" is the **Hermes gateway itself**
(sessions, memory, personas, profile routing, guardrails-by-prompt).
Go services are thin deterministic domain APIs:

- `expense`   — pockets, transactions, transfers, investment ledger, SQL rollups
- `brain`     — notes CRUD, tags, links, markdown export (pgvector later)
- `nutrition` — meal logging, rollups, photo refs

The Hermes agent (default profile today; per-service profiles via
`profile_routes` later) parses natural language, calls the service REST API
via curl/skills, and confirms before persisting anything above confidence/
amount thresholds. The PRD's uniform `/internal/v1/handle` contract is kept
as the write surface for each service so a future standalone router can drop
in without touching service internals.

Rationale: Hermes already provides chat state, memory, allowlists, media
handling, and multi-profile isolation. Rebuilding it in Go was net-negative.

## D2 — Classification: regex first, paid model second (amends TRD §4.5)
OpenCode Zen free tier is DEAD for programmatic use (verified 2026-09-15:
`zen/v1` rejects GO keys and anonymous calls alike: "OpenCode's free tier can
only be used in OpenCode"; `opencode-free` keyless path returns 401
"Model free is not supported"). It was the source of yesterday's bot auth bug.

Instead:
- Fast path: slash commands + regex (e.g. `spent <amt> on <what> [from <pocket>]`) — free, in-skill rules.
- Ambiguous free-form text: handled by the primary agent model (opencode-go, paid) — negligible cost at household scale.
- Circuit breaker intent preserved: degrade to command-only mode if opencode-go is unreachable.
- Redaction-before-classify principle (§4.5 rec #1) stays relevant if an
  external cheap model is ever introduced; not needed while classification
  is done by the same agent that sees the full text anyway.

## D3 — apt-native deployment (amends TRD §4.1)
No Docker on this box (2 vCPU / 1.9 GB RAM / 2 GB swap). Stack:
- PostgreSQL 16 + pgvector (apt), single cluster, schemas: `inem_auth`, `expense`, `brain`, `nutrition`
- Go services as systemd units (Go 1.22 apt), bind 127.0.0.1 only
- Tesseract 5 (+ind) apt for receipt OCR; whisper.cpp source build for STT (Phase 1+)
- Object storage: plain disk at `/home/ubuntu/inem-data/` (photos), served
  to agents via local paths; MinIO only if S3 API is ever needed.

## Verified runtime facts (2026-09-15)
- opencode-go direct REST: `POST https://opencode.ai/zen/go/v1/chat/completions`,
  `Authorization: Bearer <OPENCODE_GO_API_KEY>` + **required header
  `x-opencode-session: <name>`** (401 MissingSessionID otherwise). Tested OK
  with `deepseek-v4.1-flash`.
- opencode-go has NO embeddings endpoint (`/embeddings` returns HTML).
  Phase 5 RAG needs a local embedder (fastembed ONNX in Go, or Python
  sentence-transformers sidecar).
- Telegram bot `@mpok_inem_bot` live; user Dani = telegram id 8629427424,
  allowlisted; profile photo upload to Telegram blocked by network path
  (multipart uploads to api.telegram.org stall) — cosmetic only, set manually.
- rtk 0.49.0 installed; `rtk init --agent hermes` applied (plugin rtk-rewrite)
  — compresses git/test/lint output in future build sessions.

## Money conventions
IDR primary (50k = 50000; agents normalize). Amounts stored as BIGINT
(scalar rupiah). "k" suffix parsing lives in the skill/prompt layer, never
in the DB.

## Multi-user
`users(id, telegram_user_id, display_name)` in `inem_auth` schema from day
one; every domain table carries `user_id`. Adding Dani's wife = one INSERT +
one Telegram allowlist entry.

## D4 — Dashboard is a separate, GET-only binary; tests run against a real Postgres (2026-09-20)

Two calls made after the CRUD/admin surface landed:

- **`inemdash` (service/dashboard) instead of a UI inside inemd.** D1 keeps the
  Go services as thin deterministic domain APIs; a browser UI in the same binary
  would need its own auth model (browsers can't send `X-User-ID`) and would put
  a write path next to a read-only page. The dashboard registers *only* GET
  routes and proxies to inemd with a fixed internal user id, so it is
  structurally incapable of writing: any POST/PATCH/DELETE answers 405 before
  reaching inemd. It binds 127.0.0.1 and nginx terminates TLS + proxies
  `/inem/`; basic auth lives in the dashboard env, not in nginx.
- **Tests hit a real Postgres (`inem_test`), not mocks.** Balance Maths lives in
  `balanceSQL`, and constraints (NOT NULL tags, pocket name uniqueness, FK
  cascade on meal_items, note_links without cascade) are part of the contract;
  a mock would let the schema and the service drift apart. `cover_test.go` is a
  coverage *gate*: it fails the build if any route in `allRoutes()` was never
  exercised, so a new endpoint cannot ship untested. Writing the suite found two
  real bugs: a tags-less note POST violated NOT NULL, and `scope=all` reset
  rewound id sequences while another member still had rows (colliding ids).

## D5 — Identity is Telegram's, and the bot token stays out of the public process (2026-09-20)

Login is a Telegram Mini App instead of a shared password. A telegram id on its
own proves nothing (anyone can type it), so the dashboard trusts only the HMAC
Telegram puts in `initData`, checked against the bot token.

That check needs the token, and the token can read every bot update and
impersonate the bot — so it lives in **inemgate**, which binds 127.0.0.1 and is
never proxied. The internet-facing dashboard holds no secret: it asks the gate to
verify a session token, then serves that member's data. A bug in the public
process therefore leaks household data, not the bot. Both public-facing units run
with `ProtectHome=yes` so they cannot read Hermes' own `.env` either.

Consequences worth keeping:

- Identity decides data: the verified telegram id becomes the `X-User-ID` sent to
  inemd, so the wife sees her pockets and Dani sees his. Unknown telegram ids are
  refused until they exist in the roster (one `/start` + one roster row).
- `auth_date` freshness (default 5 min) is what stops a leaked `initData` being
  replayed; sessions are separate signed tokens with a 30-day life.
- The page needs no external SDK: Telegram passes the payload in the URL hash, so
  the dashboard still works when telegram.org is unreachable.
- Per-pocket sharing (`shared` vs `private`) is still to come; when it lands it
  must be read-only for non-owners and default to private.

## D6 — Pocket sharing is opt-in, read-only, and masks private counterparties (2026-09-20)

`expense.pockets.visibility` is `private` by default; the owner shares a pocket
explicitly. The rules that keep this from leaking:

- **Read-only, always.** A shared pocket is readable by the other members
  (`GET /v1/pockets/{id}`, its ledger, its moves); every write path stays scoped
  by `user_id`, so a non-owner gets `404` on rename/retype/unshare/delete and
  `400` when spending from it. Deleting checks ownership *before* counting
  entries, so the error cannot even reveal whether the pocket has rows.
- **A shared ledger must still add up.** Entries and transfers of a shared pocket
  are fully listed — hiding a movement would make the balance look wrong — but a
  counterparty pocket the reader cannot see is masked as `(pribadi)`. The money
  moves are transparent; private pocket names are not.
- **Private stays private everywhere:** the single-pocket read, the entry lists
  (`403`) and `/v1/household` (which returns shared pockets only, per member).
- `GET /v1/transactions?pocket_id=` scopes by pocket *after* the access check
  rather than by `user_id`, because a shared pocket's rows belong to its owner.

## D8 — Money moves between members, not just between pockets (2026-09-20)

- **The three cases are one operation.** Cash withdrawal (BRImo → Cash), moving
  money between his own pockets, and giving money to his wife are all a transfer:
  no `spend` entry, because a transfer is not an expense and recording it as one
  would double-count it. `POST /v1/transfers` takes any two pockets the caller can
  reach, so there is one code path to reason about and one place to get the
  balance maths right.
- **The giver spends their own money, and only their own.** `from_pocket` must
  belong to the caller; `to_pocket` may belong to another member *if the caller
  can see it* (shared). Giving money away needs nobody's permission — taking it
  does, so a member can never move money out of someone else's pocket.
- **A destination the caller cannot see answers like one that does not exist**
  (same status, same message), so private pocket names cannot be probed by trying
  to send money into them.
- **The receiver must see it.** `GET /v1/transfers` now lists every transfer that
  touches one of the caller's pockets, whoever created the row, and each row
  carries `from_user`/`to_user`. A balance that grew because money arrived from
  another member has an explanation in the same view (the dashboard's Transaksi
  tab lists transfers under "Pindah uang"). Transparent money, still-private
  pocket names: a counterparty pocket the reader cannot see stays `(pribadi)`.
- **Only the giver can undo it** (`DELETE` is scoped to the row's creator), and
  undoing moves both balances back — verified end to end, including a real
  transfer between his account and hers created and then reversed.
- **An internal transfer changes no household total** — asserted in the tests, so
  a bug that double-books a move would be caught.

## D7 — Member scope decides which features a member gets (2026-09-20)

- **Scope lives on the member, not in the browser.** `inem_auth.users.scope`
  (`full` | `finance`) is readable by the member themselves (`GET /v1/me`) and by
  the household roster; the dashboard asks inemd who the caller is and hides the
  notes and nutrition tabs for a `finance` member. Nothing the page sends is
  trusted for this — the same rule as every other authorization decision.
- **Why a column and not a config flag:** his wife's account is money-only *for
  now* ("belum punya fitur itu"), and that is a fact about her membership, not
  about one UI. When her scope changes it is one `UPDATE`, and her agent profile
  (finance-only skill, finance-only SOUL) matches it.
- **Hidden is not forbidden.** The tabs disappear, but the underlying endpoints
  stay authorized by `user_id` as always — the UI is a convenience, never the
  boundary. A finance member who curls `/v1/notes` still only ever sees their own.
- **Fail visible, not silent.** If `/v1/me` cannot be reached the dashboard falls
  back to showing every tab rather than guessing `finance`, so a broken lookup
  cannot quietly hide features from the wrong person.
- **Pitfall recorded:** an author CSS `display` rule (e.g. `#login { display:flex }`)
  beats the browser's `[hidden]` rule, so `hidden = true` silently does nothing.
  Any element hidden via the attribute now carries an explicit `[hidden]` rule,
  and verification checks the *computed* style — the earlier check read the
  attribute and passed while the overlay covered the page.
