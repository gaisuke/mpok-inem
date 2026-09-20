-- Recurring plans (2026-09-20): a transfer or an income that is expected every
-- month on a given day. The bank (or payroll) does the actual moving; the ledger
-- only records it once a human confirms it happened, so a failed autodebit can
-- never enter the books as if it had gone through.
CREATE TABLE IF NOT EXISTS expense.schedules (
  id           bigserial PRIMARY KEY,
  user_id      int NOT NULL REFERENCES inem_auth.users(id),
  kind         text NOT NULL DEFAULT 'transfer' CHECK (kind IN ('transfer','income')),
  from_pocket  int REFERENCES expense.pockets(id),
  to_pocket    int NOT NULL REFERENCES expense.pockets(id),
  amount       bigint NOT NULL CHECK (amount > 0),
  day_of_month int NOT NULL CHECK (day_of_month BETWEEN 1 AND 31),
  note         text,
  active       boolean NOT NULL DEFAULT true,
  created_at   timestamptz NOT NULL DEFAULT now(),
  -- a transfer needs both ends, an income only a destination
  CONSTRAINT schedules_shape CHECK (
    (kind = 'transfer' AND from_pocket IS NOT NULL AND from_pocket <> to_pocket)
    OR (kind = 'income' AND from_pocket IS NULL)
  )
);

-- One row per (schedule, month) that was recorded — the guard that makes a
-- recurring plan impossible to book twice in the same month.
CREATE TABLE IF NOT EXISTS expense.schedule_runs (
  id          bigserial PRIMARY KEY,
  schedule_id int NOT NULL REFERENCES expense.schedules(id) ON DELETE CASCADE,
  period      text NOT NULL,
  -- deleting a booked entry un-books its month (the plan becomes pending again),
  -- so the run row must go with the entry rather than linger with a null reference
  transfer_id bigint REFERENCES expense.transfers(id) ON DELETE CASCADE,
  txn_id      bigint REFERENCES expense.transactions(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (schedule_id, period)
);

-- CREATE INDEX IF NOT EXISTS still demands table ownership before it checks
-- whether the index is there, so guard on the catalog instead: a role that does
-- not own the tables (the test harness) must be able to apply this file cleanly.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname='expense' AND indexname='schedules_user_idx') THEN
    EXECUTE 'CREATE INDEX schedules_user_idx ON expense.schedules(user_id)';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname='expense' AND indexname='schedule_runs_period_idx') THEN
    EXECUTE 'CREATE INDEX schedule_runs_period_idx ON expense.schedule_runs(period)';
  END IF;
END $$;

-- scheduled entries get their own provenance, so a row created by a plan is
-- distinguishable from one typed by hand or parsed from a sentence
ALTER TABLE expense.transactions DROP CONSTRAINT IF EXISTS transactions_source_check;
ALTER TABLE expense.transactions ADD CONSTRAINT transactions_source_check
  CHECK (source IN ('manual','ai_parsed','transfer','scheduled'));

-- The service connects as the restricted 'inem' role, which owns nothing it did
-- not create itself, so it needs explicit grants. Skipped when this file is
-- applied by a role that does not own the tables (e.g. the test harness connects
-- as 'inem' against tables an admin created) — otherwise the whole migration
-- would fail and the DB-backed tests would silently skip.
DO $$
BEGIN
  GRANT SELECT, INSERT, UPDATE, DELETE ON expense.schedules, expense.schedule_runs TO inem;
  GRANT USAGE, SELECT ON SEQUENCE expense.schedules_id_seq, expense.schedule_runs_id_seq TO inem;
EXCEPTION WHEN insufficient_privilege THEN
  RAISE NOTICE 'grants skipped: % does not own the schedule tables', current_user;
END $$;

-- existing databases: replace SET NULL with CASCADE (see the note above)
ALTER TABLE expense.schedule_runs DROP CONSTRAINT IF EXISTS schedule_runs_transfer_id_fkey;
ALTER TABLE expense.schedule_runs ADD CONSTRAINT schedule_runs_transfer_id_fkey
  FOREIGN KEY (transfer_id) REFERENCES expense.transfers(id) ON DELETE CASCADE;
ALTER TABLE expense.schedule_runs DROP CONSTRAINT IF EXISTS schedule_runs_txn_id_fkey;
ALTER TABLE expense.schedule_runs ADD CONSTRAINT schedule_runs_txn_id_fkey
  FOREIGN KEY (txn_id) REFERENCES expense.transactions(id) ON DELETE CASCADE;
