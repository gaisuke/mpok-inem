-- A published draft should be able to point at what it became.
--
-- Without this, "published" is a claim nobody can check: the queue says it went
-- out, but not where. The engine writes the platform's post id back here after
-- a successful publish, so the audit trail ends at a permalink.
ALTER TABLE content.drafts ADD COLUMN IF NOT EXISTS post_id text;
