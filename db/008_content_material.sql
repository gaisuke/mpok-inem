-- Bank ide (2026-09-22): a topic now carries the ANGLE and the MATERIAL it came from.
--
-- The engine collects reading material (daily.dev, dev.to, Hacker News, Lobsters,
-- GitHub Trending, RSS teknologi Indonesia) and files *ideas*, never article text:
--   angle          the personal story the post could tell — the thing Dani writes
--   material_title the source that backs the facts up
--   material_url   where to check it
--
-- The material exists so a claim can be verified, never so it can be paraphrased:
-- the post is Dani's own, and Meta's original-content rules punish accounts that
-- mostly post reworded material. See notes [9] and [10] in inem/brain.
ALTER TABLE content.topics ADD COLUMN IF NOT EXISTS angle          text;
ALTER TABLE content.topics ADD COLUMN IF NOT EXISTS material_title text;
ALTER TABLE content.topics ADD COLUMN IF NOT EXISTS material_url   text;

-- A draft keeps the same trail, so "where did this come from" is answerable even
-- after the topic row is gone.
ALTER TABLE content.drafts ADD COLUMN IF NOT EXISTS material_title text;
ALTER TABLE content.drafts ADD COLUMN IF NOT EXISTS material_url   text;
