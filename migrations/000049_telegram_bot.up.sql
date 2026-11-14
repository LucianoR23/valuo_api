-- 000049_telegram_bot (plan 3.6): load expenses from a Telegram bot.
--
-- A person links their Telegram chat from the app: the app asks for a one-time
-- code (10 minutes) and opens t.me/<bot>?start=<code>. Only the SHA-256 of the
-- code is stored. One chat per user and one user per chat.
--
-- Every expense read from a message waits in telegram_drafts until the person
-- taps ✅ (saved_at, expense_id) or ❌ (the row is deleted). ✏️ sets editing_at
-- and the next message replaces the draft (decided with the user on
-- 2026-11-14). A saved draft is what /deshacer undoes. Rows older than 24 h
-- are deleted by a job.
--
-- users.ai_suspended_until: the AI is off for that account until then (plan
-- §8.6); manual entry keeps working.
CREATE TABLE telegram_links (
    user_id   UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    chat_id   BIGINT      NOT NULL UNIQUE,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE telegram_link_codes (
    code_hash  BYTEA       PRIMARY KEY CHECK (length(code_hash) = 32),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_telegram_link_codes_user ON telegram_link_codes (user_id);

CREATE TABLE telegram_drafts (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chat_id    BIGINT      NOT NULL,
    message_id BIGINT,
    fields     JSONB       NOT NULL,
    editing_at TIMESTAMPTZ,
    saved_at   TIMESTAMPTZ,
    expense_id UUID        REFERENCES expenses(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_telegram_drafts_user ON telegram_drafts (user_id, created_at DESC);
-- Deleting an expense sets expense_id to NULL here.
CREATE INDEX idx_telegram_drafts_expense ON telegram_drafts (expense_id);
CREATE INDEX idx_telegram_drafts_created ON telegram_drafts (created_at);

ALTER TABLE users ADD COLUMN ai_suspended_until TIMESTAMPTZ;
