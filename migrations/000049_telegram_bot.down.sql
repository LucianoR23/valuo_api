ALTER TABLE users DROP COLUMN IF EXISTS ai_suspended_until;
DROP TABLE IF EXISTS telegram_drafts;
DROP TABLE IF EXISTS telegram_link_codes;
DROP TABLE IF EXISTS telegram_links;
