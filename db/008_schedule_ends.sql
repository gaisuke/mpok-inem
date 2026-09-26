-- Plans can end, not only start (2026-09-26).
--
-- The monthly savings pocket was raided to settle the rent, and October's salary
-- is meant to put it back. That is a plan with a beginning AND an end: without
-- ends_on it would keep asking for 2jt every month forever, and a "temporary"
-- plan that never stops is worse than no plan at all.

ALTER TABLE expense.schedules ADD COLUMN IF NOT EXISTS ends_on date;

ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_window_check;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_window_check
  CHECK (starts_on IS NULL OR ends_on IS NULL OR ends_on >= starts_on);
