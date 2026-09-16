-- Seed the two known users. Identity is keyed by Telegram user id, so the
-- second user is a row, not a migration — add her id when she has one.
-- `users.telegram_user_id` is NOT NULL UNIQUE, so her row needs an id here.
INSERT INTO inem_auth.users (telegram_user_id, display_name)
VALUES
  (8629427424, 'Dani')
ON CONFLICT (telegram_user_id) DO NOTHING;
