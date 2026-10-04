-- A plan can be priced in dollars (2026-10-04).
--
-- The AI subscription is billed as $10 + tax = $11, so the rupiah figure is only
-- known on the day the card is charged — and the bank's rate is not the market's.
-- Storing the dollar price on the plan, and the day's rate in a table, keeps one
-- number for the whole day: the reminder and the booking quote the same rupiah
-- amount, and the ledger records which rate produced it.
ALTER TABLE expense.schedules ADD COLUMN IF NOT EXISTS amount_usd numeric(12,2);

ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_amount_check;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_amount_check
  CHECK (amount IS NULL OR amount_usd IS NULL); -- one price, never two

CREATE TABLE IF NOT EXISTS expense.fx_rates (
  day        date          NOT NULL,
  pair       text          NOT NULL,
  rate       numeric(18,6) NOT NULL,
  source     text          NOT NULL DEFAULT '',
  fetched_at timestamptz   NOT NULL DEFAULT now(),
  PRIMARY KEY (day, pair)
);

CREATE INDEX IF NOT EXISTS fx_rates_recent_idx ON expense.fx_rates(pair, day DESC);
