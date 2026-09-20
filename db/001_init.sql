-- Mpok Inem — Phase 0 schema (2026-09-15)
-- One Postgres cluster, per-service schemas (D3: apt-native).
CREATE EXTENSION IF NOT EXISTS vector;

CREATE SCHEMA IF NOT EXISTS inem_auth;
CREATE SCHEMA IF NOT EXISTS expense;
CREATE SCHEMA IF NOT EXISTS brain;
CREATE SCHEMA IF NOT EXISTS nutrition;

-- Identity resolution seam (PRD §3.1): every domain table carries user_id.
CREATE TABLE IF NOT EXISTS inem_auth.users (
  id                serial PRIMARY KEY,
  telegram_user_id  bigint UNIQUE NOT NULL,
  display_name      text NOT NULL,
  scope             text NOT NULL DEFAULT 'full'
                    CHECK (scope IN ('full','finance')),
  created_at        timestamptz NOT NULL DEFAULT now()
);

-- ============ expense ============
CREATE TABLE IF NOT EXISTS expense.pockets (
  id              serial PRIMARY KEY,
  user_id         int NOT NULL REFERENCES inem_auth.users(id),
  name            text NOT NULL,
  type            text NOT NULL CHECK (type IN ('cash','savings','investment')),
  opening_balance bigint NOT NULL DEFAULT 0,           -- IDR whole rupiah
  -- private unless the owner shares it; sharing is read-only (see 003)
  visibility      text NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','shared')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);

CREATE TABLE IF NOT EXISTS expense.transactions (
  id          bigserial PRIMARY KEY,
  user_id     int NOT NULL REFERENCES inem_auth.users(id),
  pocket_id   int NOT NULL REFERENCES expense.pockets(id),
  direction   text NOT NULL CHECK (direction IN ('in','out')),
  amount      bigint NOT NULL CHECK (amount > 0),      -- IDR
  category    text NOT NULL DEFAULT 'general',
  note        text,
  source      text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','ai_parsed','transfer')),
  confidence  real,                                    -- for ai_parsed audit (§5)
  raw_input   text,                                    -- exactly what the model saw
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS txn_user_time ON expense.transactions (user_id, created_at);
CREATE INDEX IF NOT EXISTS txn_pocket    ON expense.transactions (pocket_id);

CREATE TABLE IF NOT EXISTS expense.transfers (
  id            bigserial PRIMARY KEY,
  user_id       int NOT NULL REFERENCES inem_auth.users(id),
  from_pocket   int NOT NULL REFERENCES expense.pockets(id),
  to_pocket     int NOT NULL REFERENCES expense.pockets(id),
  amount        bigint NOT NULL CHECK (amount > 0),
  note          text,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- ============ brain ============
CREATE TABLE IF NOT EXISTS brain.notes (
  id          bigserial PRIMARY KEY,
  user_id     int NOT NULL REFERENCES inem_auth.users(id),
  title       text NOT NULL,
  body_md     text NOT NULL DEFAULT '',
  tags        text[] NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notes_tags  ON brain.notes USING gin (tags);
CREATE INDEX IF NOT EXISTS notes_fts   ON brain.notes USING gin (to_tsvector('simple', title || ' ' || body_md));
-- Phase 5: brain.note_embeddings (pgvector). Extension already enabled above.

CREATE TABLE IF NOT EXISTS brain.note_links (
  from_note bigint NOT NULL REFERENCES brain.notes(id),
  to_note   bigint NOT NULL REFERENCES brain.notes(id),
  PRIMARY KEY (from_note, to_note)
);

-- ============ nutrition ============
CREATE TABLE IF NOT EXISTS nutrition.meals (
  id         bigserial PRIMARY KEY,
  user_id    int NOT NULL REFERENCES inem_auth.users(id),
  photo_ref  text,                                     -- disk path under /home/ubuntu/inem-data/
  source     text NOT NULL DEFAULT 'manual' CHECK (source IN ('photo','manual')),
  note       text,
  logged_at  date NOT NULL DEFAULT CURRENT_DATE,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nutrition.meal_items (
  id         bigserial PRIMARY KEY,
  meal_id    bigint NOT NULL REFERENCES nutrition.meals(id) ON DELETE CASCADE,
  name       text NOT NULL,
  est_grams  int,
  calories   int,
  protein    numeric(6,1),
  carbs      numeric(6,1),
  fat        numeric(6,1),
  confidence real
);

CREATE TABLE IF NOT EXISTS nutrition.daily_targets (
  user_id        int NOT NULL REFERENCES inem_auth.users(id),
  day            date NOT NULL,
  calorie_target int,
  protein_target numeric(6,1),
  PRIMARY KEY (user_id, day)
);
