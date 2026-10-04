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

## D11 — Entries can be backdated to when they happened (2026-09-22)

- **Problem.** Money is usually reported *after the fact*: "last night at 23:31 I
  bought Ipan a data package, QRIS BRImo 1.200", typed the next morning. The ledger
  stamped the moment of entry, so the entry landed on the wrong day and daily
  reports shifted by one day. Schedules already had `entry_date`; manual entries
  had nothing.
- **Decision.** `POST /v1/transactions` and `POST /v1/transfers` accept an optional
  `date` (`YYYY-MM-DD`), surfaced in the CLI as `spend --day` / `transfer --day`.
  `created_at = COALESCE($n::date, now())`, so the default behaviour is untouched.
- **Future dates are refused** (`400`), as is a loose format (`21-09-2026`): the
  ledger records what happened, not what will. Guessing a date the user never
  mentioned is worse than asking.
- **Balance is independent of the date.** Only the day it is reported under changes;
  money still leaves the pocket. Fixing a mis-dated entry is `txn-rm` + re-record
  with `--day`, verified end to end on the Ipan entry.
- **Rejected:** editing `created_at` in place (PATCH stays metadata-only — the
  amount, pocket and direction remain immutable, and a date is part of *when*, not
  *what*), and a free-form "yesterday" string on the API (the client resolves
  relative words, the service only accepts a date).

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

## D9 — Recurring plans are reminders, not auto-writes (2026-09-20)

- **The ledger only records what a human confirms.** His bank already moves the
  money (1jt to Tabungan Jago on the 1st, 100rb each to Kurban/Umroh/Pulang
  Kampung on the 2nd, payroll into BRImo on the 1st). A cron that booked those by
  itself would write down transfers that may never have happened — an autodebit
  can fail — and the ledger's promise ("the balance matches the bank app") would
  quietly break. So the plan lives in the ledger, the reminder is a clock, and the
  entry appears only after he says the money moved.
- **One code path.** Running a plan creates an ordinary transfer or income entry
  through the same insert as a manual one, so balance maths has one implementation
  and a booked plan is indistinguishable from a typed entry except by provenance
  (`source='scheduled'`, and the run row that points at it).
- **Booked once per month, enforced by the database** (`UNIQUE(schedule_id,period)`),
  not by the caller: the failure mode to fear is a recurring plan counted twice.
  `force` re-books deliberately; deleting the booked entry un-books the month via
  `ON DELETE CASCADE`, so a mistaken entry never leaves a plan stuck on "already
  booked" with nothing to show for it.
- **Dated when the money moved, not when it was confirmed.** Confirming on the 3rd
  books the 1st (`entry_date` overrides it). A day the month lacks (the 31st in
  April) falls back to the month's last day rather than being skipped.
- **`mark_only` exists for history**: a month already inside a pocket's opening
  balance is marked settled without inventing an entry.
- **A plan cannot overdraw silently**: if the source pocket would go negative the
  entry is still recorded (it mirrors reality) and the response says so plainly.
- **Some monthly moves have no fixed figure** (what he sends from BRImo to Jago
  depends on what he keeps back; what he sends on to Pipit's SeaBank is "sometimes
  6jt, sometimes 7jt"). Such a plan stores no amount (`amount NULL`) and is shown
  as "nominal menyusul": booking it asks for the real number instead of inventing
  one, and the override is accepted only together with a single plan id so a
  figure can never be applied to the wrong row. A plan may also pay another
  member's *shared* pocket (the monthly transfer to his wife); if that pocket is
  unshared later, the plan skips rather than writing into a pocket he cannot see.
- **Reminder is deterministic** (no LLM, empty output sends nothing) so it cannot
  become daily noise, and plans are read-only in the dashboard.

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

## D12 — A recurring plan can pay someone outside the household, and can start later (2026-09-26)

Context: rent is paid monthly to a landlord and was agreed before it begins —
"1,8jt tiap tgl 2, tapi skip Oktober, langsung November".

- **`kind='expense'`, with a `payee` and no destination pocket.** A transfer moves
  money inside the ledger; an expense leaves it. Modelling rent as a transfer to a
  fake pocket would put a pocket in the books that holds nothing and receives
  nothing, and every balance would then need an explanation.
- **Booking an expense writes an ordinary transaction** (source `scheduled`) from
  the pocket that pays, so the balance maths still has exactly one implementation.
- **`starts_on date` marks when a plan begins.** A month before the start reports
  status `not_started`, and the pending filter — the one the reminder asks about —
  excludes it, so a November plan stays quiet all through October. Clearing
  `starts_on` re-enables the plan for every month.
- **The payee is required** by the endpoint and by a CHECK constraint: a reminder
  that asks "sudah dibayar?" without naming who gets the money is not actionable.
- **The paying pocket must be the planner's own**, and an expense that carries a
  destination pocket is refused rather than silently reinterpreted.

Consequences: the Jadwal tab renders `X → bayar <payee>` with a `mulai <date>`
pill (and `belum mulai` instead of `menunggu` before the start month), and the
reminder cron names the payee.

## D13 — A plan can end, and closes itself when its last month is booked (2026-09-26)

Context: the monthly savings were raided to settle the rent, and October's salary
is meant to put them back — a plan with a beginning and an end.

- **`ends_on date`** is the mirror of `starts_on`. A month after the end reports
  status `finished` and is excluded from the pending filter, so a one-off
  replacement never asks for money in November.
- **Booking the final month deactivates the plan** and says so (`finished: true`
  in the run response). A temporary plan that keeps asking forever is noise, and
  noise is how people learn to ignore reminders. The plan stays visible as
  "sudah berakhir" instead of silently disappearing.
- **Clearing `ends_on` reopens it** — "berhenti dulu" is not "hapus", and the
  booked months stay booked either way.
- **A window that ends before it starts is refused**, and `run` still refuses
  future dates: money that has not moved yet is not an entry.

Consequences: the savings replacement is plan [12] (1jt monthly savings keeps
running as [2]).

## D14 — One entry can be secret, and only its owner sees it (2026-10-03)

Context: an anniversary gift (hairdryer, Rp135.203) was booked into BRImo, the
pocket Pipit can read. Pocket sharing is the whole point of the household view,
and it would have handed her the note — "kado anniversary: hairdryer buat
istri" — plus the amount. Relabelling the note is a band-aid: the category alone
("hadiah") already gives it away.

- **`expense.transactions.visibility`** is `normal` (the default) or `secret`. It
  is a property of the *entry*, never of the pocket: hiding a whole pocket would
  mean lying about where the money lives, which is worse than the leak.
- **A secret line never leaves its owner's view.** The ledger read that serves a
  shared pocket filters `visibility='normal' OR user_id=<caller>`, so a partner's
  read of that pocket is simply missing the line — no note, no amount, not even
  a placeholder that hints at one.
- **The balance stays truthful.** The pocket still counts the secret entry, so
  the household sees that money left (it really did) without seeing what it
  bought. Masking the balance too would corrupt the one number everyone trusts.
- **Fetch-by-id and updates stay owner-scoped** (they always were), so a secret
  entry can be neither read nor touched by anyone else.
- **`--secret` on `spend`, `--secret` / `--normal` on `txn-edit`.** The owner's
  own view marks it with a lock, because the failure mode of a secret is
  forgetting to un-secret it after the surprise.

Consequences: hide-then-restore is two commands and both are reversible; the
surprise entry stays in the books, so no month is ever missing money. Transfers
are not covered yet — a surprise that is a *move* between pockets would still be
visible.

## D15 — Privacy mode: money is hidden before it leaves the server (2026-10-03)

Context: the dashboard was about to appear on a TikTok live. Balances and
per-category totals on screen are the household's business, and one frame on
stream (or a screenshot) would have published them.

- **The masking happens inside inemdash, on the way out** — in `writeJSON` and in
  the passthrough that forwards inemd's body. A hidden figure never reaches the
  browser, so neither the page source nor dev tools can reveal it. Masking in the
  page would have left every number one dev-tools panel away from the audience.
- **The rule is structural, not a list**: every money field in this API ends in
  `_idr`, so those values become `null` and the page draws `••••••`. Names,
  categories and note text still render, so the screen still looks like the
  product rather than an empty page.
- **Balances are hidden too, and that is the difference from D14.** There the
  problem was a partner reading a shared pocket, and the balance had to stay
  truthful. Here the audience is the problem: on a live, "Rp21jt" is the
  sensitive fact, not which pocket holds it.
- **The switch is a route** (`GET/POST /api/privacy`, behind the normal login) and
  its state lives in the service's `StateDirectory`, so a restart in the middle of
  a live cannot un-hide the page. `INEM_DASH_PRIVACY=1` forces it on at boot.
- **Note text is left alone.** A note that spells out an amount ("sisakan 1,4jt")
  stays readable: masking free text would gut the page, so the trade-off is
  written down instead of quietly taken.

Consequences: one click before going live, one click after. Two tests pin it —
the passthrough and the pocket-detail merge, which builds its own JSON and would
otherwise have missed the mask.

## D16 — A plan can be priced in dollars, and the rate is taken once a day (2026-10-04)

Context: the AI subscription is billed to a card as $10 + tax = $11, so the
rupiah figure is not knowable when the plan is written — and the bank's rate is
not the market's: the first charge (Rp186.568 on 4 Oct) worked out around
16.960/USD, a few percent off the market's 17.889. Which is the argument for
recording the rupiah that actually left, and for quoting the rate it came from.

- **`schedules.amount_usd`** carries the dollar price; `amount` stays NULL for
  such a plan, and a CHECK keeps the two mutually exclusive. A price is one
  number, never two.
- **One rate per day, cached in `expense.fx_rates`.** Two free providers are tried
  in order (open.er-api.com, then floatrates) and the answer is stored with the
  day it belongs to, so the reminder, the booking and the dashboard cannot quote
  three different figures for the same charge.
- **When no provider answers, the last stored rate is reused and flagged
  `stale`** — the reminder prints it as "kurs lama" and the entry's note says so.
  An old number a human can correct beats an invented one nobody can.
- **Booking needs no figure from the human**: `run` converts the price with the
  day's rate and writes the conversion into the note ("USD 11 × kurs 17889"), so
  the entry explains itself a month later. An explicit amount still wins: the
  bank's figure is the truth, the estimate is only a forecast.
- **The estimate is rounded to the nearest 100 rupiah** so the same number shows
  up everywhere, and `GET /v1/fx` + the CLI's `fx` let the agent answer "kurs
  hari ini berapa?" without a web search.

Consequences: plan [14] is the OpenCode subscription (USD 11, day 4, from eCard).
October was marked settled — the real charge was already recorded as an ordinary
transaction — so the plan only starts asking from 4 November, quoting the
morning's rate each month.

## D17 — The best rate is the bank's, and the bank's page may be unreachable (2026-10-04)

Context: the subscription is charged to a Jago card, so Jago's own rate is the
honest one to quote. Jago does publish it — "Kurs Mata Uang Asing Nasabah Jago"
on `jago.com/id/jago/digital/pocket/foreign-currency`, with a timestamp, e.g.
4 Oct 19:30 WIB: USD **Nasabah Beli 17.892 / Nasabah Jual 17.872** (buy when you
buy dollars, sell when you sell).

- **That page cannot be read from this host.** Cloudflare answers 403 to curl and
  to headless Chrome alike (datacenter IP, the same wall that blocked the profx
  retrieval vendor), and the free text relays tried (r.jina.ai, codetabs,
  allorigins, corsproxy) are blocked or useless too. The rule: never build a
  dependency on a page this host cannot fetch.
- **So the rate can be pushed in instead.** `POST /v1/fx` records a rate for a
  pair and a day, and a day that already has a stored rate is never re-fetched:
  whoever *can* read the bank's page (the agent, through its own fetcher) stores
  the number, and the rest of the system reads it from the cache. The provider
  fetch stays as the fallback, so a missing push degrades to a market rate rather
  than to nothing.
- **Jago's spread is tiny**: 17.872/17.892 against a market 17.889 — the estimate
  moves by ~Rp20 on a Rp196.800 charge. Which is why this stays a manual/agent
  push instead of a daily model call: the accuracy bought is smaller than the
  machinery.
- **The published FX rate is not the card rate.** The first charge (Rp186.568)
  works out to ~16.961/USD at $11, i.e. neither Jago's counter rate nor the
  market's — the receipt is the truth and the estimate is a forecast. The booking
  flow already lets the real figure win.

Consequences: `inem.py fx --set 17892 --source jago` records the bank's rate for
the day; the reminder then quotes Jago's number, and the falling back stays
automatic.
