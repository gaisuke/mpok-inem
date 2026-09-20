-- Pocket sharing (2026-09-20): a pocket is private by default and becomes
-- visible to the other household members only when its owner shares it.
-- Sharing is read-only: only the owner may write to the pocket.
ALTER TABLE expense.pockets
  ADD COLUMN IF NOT EXISTS visibility text NOT NULL DEFAULT 'private'
  CHECK (visibility IN ('private','shared'));
