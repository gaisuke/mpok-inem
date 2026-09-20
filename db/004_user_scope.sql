-- Member scope (2026-09-20): what a member's account covers. 'finance' means the
-- household money features only — the dashboard hides the notes and nutrition
-- tabs for them, matching the finance-only agent profile they run on.
ALTER TABLE inem_auth.users
  ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'full'
  CHECK (scope IN ('full','finance'));
