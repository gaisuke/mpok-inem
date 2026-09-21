-- Content producer (2026-09-21): the review queue for original posts.
--
-- The generator (viral-content-engine) finds trends and writes drafts; the queue
-- of record lives here, so the drafts can be reviewed from chat and the dashboard
-- like everything else in the household platform — and so the engine never has to
-- touch this database directly (it goes through the API).
CREATE SCHEMA IF NOT EXISTS content;

CREATE TABLE IF NOT EXISTS content.topics (
  id         bigserial PRIMARY KEY,
  user_id    int NOT NULL REFERENCES inem_auth.users(id),
  topic      text NOT NULL,
  niche      text NOT NULL DEFAULT 'keuangan pribadi',
  source     text NOT NULL DEFAULT 'manual',   -- manual | threads_search | x_search | trends
  score      real NOT NULL DEFAULT 0,
  status     text NOT NULL DEFAULT 'new' CHECK (status IN ('new','used','skipped')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, topic)
);

CREATE TABLE IF NOT EXISTS content.drafts (
  id           bigserial PRIMARY KEY,
  user_id      int NOT NULL REFERENCES inem_auth.users(id),
  topic_id     bigint REFERENCES content.topics(id) ON DELETE SET NULL,
  platform     text NOT NULL DEFAULT 'threads' CHECK (platform IN ('threads','x')),
  niche        text NOT NULL,
  topic        text NOT NULL,
  hook         text,
  body         text NOT NULL,
  parts        jsonb NOT NULL DEFAULT '[]'::jsonb,
  fingerprint  text NOT NULL,
  status       text NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','approved','rejected','published','failed')),
  model        text,
  run_id       bigint,
  created_at   timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz,
  UNIQUE (user_id, fingerprint)
);

-- One row per scheduled run (pagi/siang/sore), so a run that finds nothing is
-- still visible as "it ran" instead of looking like the job never fired.
CREATE TABLE IF NOT EXISTS content.runs (
  id           bigserial PRIMARY KEY,
  user_id      int NOT NULL REFERENCES inem_auth.users(id),
  slot         text NOT NULL,
  started_at   timestamptz NOT NULL DEFAULT now(),
  topics_found int NOT NULL DEFAULT 0,
  drafts_made  int NOT NULL DEFAULT 0,
  note         text
);

CREATE INDEX IF NOT EXISTS content_drafts_status_idx ON content.drafts(user_id, status);
CREATE INDEX IF NOT EXISTS content_topics_status_idx ON content.topics(user_id, status);

DO $$
BEGIN
  GRANT USAGE ON SCHEMA content TO inem;
  GRANT SELECT, INSERT, UPDATE, DELETE ON content.topics, content.drafts, content.runs TO inem;
  GRANT USAGE, SELECT ON SEQUENCE content.topics_id_seq, content.drafts_id_seq, content.runs_id_seq TO inem;
EXCEPTION WHEN insufficient_privilege THEN
  RAISE NOTICE 'grants skipped: % does not own the content objects', current_user;
END $$;
