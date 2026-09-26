-- Expense schedules and late-starting plans (2026-09-26).
--
-- Until now a plan could only move money between his own pockets ('transfer') or
-- announce money arriving ('income'). Rent paid to a landlord is neither: the
-- money leaves the household and goes to a person. Modelling that as a transfer
-- to a fake pocket would put a pocket in the books that holds nothing, so the
-- kind is explicit instead, with the payee named and no destination pocket.
--
-- starts_on exists because a plan is often agreed before it begins: rent from
-- November must not ask "sudah dibayar?" on 2 October.

ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_kind_check;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_kind_check
  CHECK (kind IN ('transfer','income','expense'));

-- an expense has no destination pocket: it leaves the ledger, it does not move inside it
ALTER TABLE expense.schedules ALTER COLUMN to_pocket DROP NOT NULL;

ALTER TABLE expense.schedules ADD COLUMN IF NOT EXISTS payee text;
ALTER TABLE expense.schedules ADD COLUMN IF NOT EXISTS category text;
ALTER TABLE expense.schedules ADD COLUMN IF NOT EXISTS starts_on date;

ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_shape;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_shape CHECK (
  (kind = 'transfer' AND from_pocket IS NOT NULL AND to_pocket IS NOT NULL AND from_pocket <> to_pocket)
  OR (kind = 'income' AND from_pocket IS NULL AND to_pocket IS NOT NULL)
  OR (kind = 'expense' AND from_pocket IS NOT NULL AND to_pocket IS NULL AND payee IS NOT NULL)
);

-- The payee is who the money is for; an expense plan must say it, otherwise the
-- reminder asks for a payment nobody can identify.
ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_payee_check;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_payee_check
  CHECK (kind <> 'expense' OR (payee IS NOT NULL AND length(btrim(payee)) > 0));
