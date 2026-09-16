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
