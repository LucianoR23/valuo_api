-- ===================== Telegram bot (plan 3.6) =====================

-- name: DeleteTelegramLinkCodes :exec
DELETE FROM telegram_link_codes WHERE user_id = $1;

-- name: CreateTelegramLinkCode :exec
INSERT INTO telegram_link_codes (code_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: TakeTelegramLinkCode :one
-- A code works once, before it expires, for an account that still exists.
DELETE FROM telegram_link_codes c
USING users u
WHERE c.code_hash = $1 AND c.expires_at > sqlc.arg(now)::timestamptz
  AND u.id = c.user_id AND u.deleted_at IS NULL
RETURNING c.user_id;

-- name: UnlinkTelegramChat :exec
-- Linking a chat already linked to someone else moves it.
DELETE FROM telegram_links WHERE chat_id = $1;

-- name: LinkTelegram :exec
INSERT INTO telegram_links (user_id, chat_id) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET chat_id = EXCLUDED.chat_id, linked_at = NOW();

-- name: GetTelegramLink :one
SELECT * FROM telegram_links WHERE user_id = $1;

-- name: TelegramUserByChat :one
SELECT l.user_id FROM telegram_links l
JOIN users u ON u.id = l.user_id
WHERE l.chat_id = $1 AND u.deleted_at IS NULL;

-- name: DeleteTelegramLink :one
DELETE FROM telegram_links WHERE user_id = $1 RETURNING chat_id;

-- name: CreateTelegramDraft :one
INSERT INTO telegram_drafts (user_id, chat_id, fields) VALUES ($1, $2, $3) RETURNING *;

-- name: SetTelegramDraftMessage :exec
UPDATE telegram_drafts SET message_id = $2 WHERE id = $1;

-- name: GetTelegramDraft :one
SELECT * FROM telegram_drafts WHERE id = $1 AND user_id = $2;

-- name: UpdateTelegramDraftFields :exec
UPDATE telegram_drafts SET fields = $3 WHERE id = $1 AND user_id = $2 AND saved_at IS NULL;

-- name: EditTelegramDraft :one
-- ✏️: the next message replaces this draft. Only one draft waits at a time.
UPDATE telegram_drafts SET editing_at = sqlc.arg(now)::timestamptz
WHERE id = $1 AND user_id = $2 AND saved_at IS NULL
RETURNING *;

-- name: ClearTelegramEditing :exec
UPDATE telegram_drafts SET editing_at = NULL
WHERE user_id = $1 AND editing_at IS NOT NULL AND id <> sqlc.arg(keep)::uuid;

-- name: TelegramDraftBeingEdited :one
SELECT * FROM telegram_drafts
WHERE user_id = $1 AND saved_at IS NULL AND editing_at >= sqlc.arg(since)::timestamptz
ORDER BY editing_at DESC
LIMIT 1;

-- name: ClaimTelegramDraft :one
-- ✅: only the first tap saves.
UPDATE telegram_drafts SET saved_at = sqlc.arg(now)::timestamptz
WHERE id = $1 AND user_id = $2 AND saved_at IS NULL
RETURNING *;

-- name: UnclaimTelegramDraft :exec
UPDATE telegram_drafts SET saved_at = NULL WHERE id = $1 AND expense_id IS NULL;

-- name: SetTelegramDraftExpense :exec
UPDATE telegram_drafts SET expense_id = $2, editing_at = NULL WHERE id = $1;

-- name: DeleteTelegramDraft :one
DELETE FROM telegram_drafts WHERE id = $1 AND user_id = $2 AND saved_at IS NULL RETURNING *;

-- name: LastTelegramExpense :one
-- /deshacer: the last expense saved from Telegram in the last 24 h.
SELECT * FROM telegram_drafts
WHERE user_id = $1 AND expense_id IS NOT NULL AND saved_at >= sqlc.arg(since)::timestamptz
ORDER BY saved_at DESC, created_at DESC
LIMIT 1;

-- name: ForgetTelegramDraft :exec
DELETE FROM telegram_drafts WHERE id = $1;

-- name: DeleteOldTelegramRows :one
WITH codes AS (
    DELETE FROM telegram_link_codes WHERE expires_at < sqlc.arg(now)::timestamptz RETURNING 1
), drafts AS (
    DELETE FROM telegram_drafts WHERE created_at < sqlc.arg(drafts_before)::timestamptz RETURNING 1
)
SELECT ((SELECT count(*) FROM codes) + (SELECT count(*) FROM drafts))::bigint AS deleted;

-- name: TelegramMethods :many
-- The person's active methods, most used in the last 90 days first.
SELECT pm.id, pm.name
FROM payment_methods pm
LEFT JOIN expenses e ON e.payment_method_id = pm.id AND e.created_by = pm.owner_user_id
     AND e.created_at >= sqlc.arg(since)::timestamptz
WHERE pm.owner_user_id = $1 AND pm.is_active
GROUP BY pm.id, pm.name
ORDER BY count(e.id) DESC, pm.name ASC;

-- name: LatestWeeklyReview :one
-- /resumen: the last weekly review of the space.
SELECT title, body, insight_date FROM daily_insights
WHERE household_id = $1 AND user_id IS NULL AND insight_type = 'weekly_review'
ORDER BY insight_date DESC
LIMIT 1;
