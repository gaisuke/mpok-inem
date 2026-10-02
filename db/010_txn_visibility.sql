-- A transaction can be hidden from everyone but its owner (2026-10-03).
--
-- Pocket sharing exists so a partner can read a shared pocket's ledger, and
-- that is the point — until a surprise gift shows up in it, note and all.
--
-- So one entry, and only that entry, can be marked 'secret'. The owner keeps
-- seeing it (the record stays complete and honest), the pocket balance still
-- counts it (the money really left the account), and nobody else's view
-- contains the line at all. It is deliberately an attribute of the entry, not
-- of the pocket: hiding a whole pocket would mean lying about where the money
-- lives.

ALTER TABLE expense.transactions
  ADD COLUMN IF NOT EXISTS visibility text NOT NULL DEFAULT 'normal'
  CHECK (visibility IN ('normal','secret'));
