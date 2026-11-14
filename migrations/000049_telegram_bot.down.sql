UPDATE ai_usage SET status = 'ok' WHERE status = 'off_topic';
UPDATE ai_usage SET status = 'disabled' WHERE status = 'suspended';
ALTER TABLE ai_usage DROP CONSTRAINT ai_usage_status_check;
ALTER TABLE ai_usage ADD CONSTRAINT ai_usage_status_check CHECK (status IN (
    'ok', 'invalid_output', 'provider_error', 'timeout', 'disabled',
    'rate_limited', 'user_budget', 'global_budget'));
ALTER TABLE users DROP COLUMN IF EXISTS ai_suspended_until;
DROP TABLE IF EXISTS telegram_drafts;
DROP TABLE IF EXISTS telegram_link_codes;
DROP TABLE IF EXISTS telegram_links;
