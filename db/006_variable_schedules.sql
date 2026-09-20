-- Variable-amount plans (2026-09-20). Two of his monthly moves are not fixed:
-- what he sends from BRImo to Jago depends on what he leaves behind, and what he
-- sends on to Pipit's SeaBank is "sometimes 6jt, sometimes 7jt". Such a plan is a
-- checklist item with an expected amount, not a contract: amount NULL means the
-- figure is supplied at booking time, after he says what actually moved.
ALTER TABLE expense.schedules ALTER COLUMN amount DROP NOT NULL;
ALTER TABLE expense.schedules DROP CONSTRAINT IF EXISTS schedules_amount_check;
ALTER TABLE expense.schedules ADD CONSTRAINT schedules_amount_check
  CHECK (amount IS NULL OR amount > 0);
