-- OAuth 2.1 server and MCP connections (plan 8.6).

-- name: CreateOAuthClient :one
INSERT INTO oauth_clients (id, client_name, redirect_uris, auth_method, secret_hash)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetOAuthClient :one
SELECT * FROM oauth_clients WHERE id = $1;

-- name: CreateOAuthRequest :one
INSERT INTO oauth_requests (client_id, redirect_uri, scopes, state, code_challenge, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: GetOAuthRequest :one
SELECT r.id, r.client_id, r.redirect_uri, r.scopes, r.state, r.code_challenge, r.expires_at,
       c.client_name
FROM oauth_requests r
JOIN oauth_clients c ON c.id = r.client_id
WHERE r.id = $1 AND r.expires_at > $2;

-- name: DeleteOAuthRequest :one
DELETE FROM oauth_requests WHERE id = $1 AND expires_at > $2
RETURNING id, client_id, redirect_uri, scopes, state, code_challenge;

-- name: UpsertOAuthGrant :one
INSERT INTO oauth_grants (user_id, client_id, scopes)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, client_id) DO UPDATE SET scopes = EXCLUDED.scopes
RETURNING id;

-- name: CreateOAuthCode :exec
INSERT INTO oauth_codes (code_hash, grant_id, redirect_uri, code_challenge, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UseOAuthCode :one
-- Marks the code used and returns it with its grant. used_at was already set
-- when the code is presented a second time; the caller then revokes.
WITH prev AS (
    SELECT oc.code_hash AS h, oc.used_at AS prev_used_at FROM oauth_codes oc WHERE oc.code_hash = $1 FOR UPDATE
)
UPDATE oauth_codes c SET used_at = COALESCE(c.used_at, $2)
FROM prev, oauth_grants g
WHERE c.code_hash = prev.h AND g.id = c.grant_id
RETURNING c.grant_id, c.redirect_uri, c.code_challenge, c.scopes, c.expires_at,
          prev.prev_used_at, g.user_id, g.client_id;

-- name: CreateOAuthToken :exec
INSERT INTO oauth_tokens (token_hash, grant_id, kind, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: RotateOAuthRefreshToken :one
-- Marks a refresh token rotated. rotated_at was already set when it is
-- presented again (reuse); the caller then revokes the grant.
WITH prev AS (
    SELECT ot.token_hash AS h, ot.rotated_at AS prev_rotated_at FROM oauth_tokens ot
    WHERE ot.token_hash = $1 AND ot.kind = 'refresh' FOR UPDATE
)
UPDATE oauth_tokens t SET rotated_at = COALESCE(t.rotated_at, $2)
FROM prev, oauth_grants g
WHERE t.token_hash = prev.h AND g.id = t.grant_id
RETURNING t.grant_id, t.scopes, t.expires_at, prev.prev_rotated_at, g.user_id, g.client_id;

-- name: GetOAuthAccessToken :one
SELECT t.grant_id, t.scopes, t.expires_at, g.user_id, g.client_id
FROM oauth_tokens t
JOIN oauth_grants g ON g.id = t.grant_id
WHERE t.token_hash = $1 AND t.kind = 'access' AND t.expires_at > $2;

-- name: GetOAuthTokenGrant :one
SELECT g.id, g.client_id FROM oauth_tokens t
JOIN oauth_grants g ON g.id = t.grant_id
WHERE t.token_hash = $1;

-- name: DeleteOAuthGrant :exec
DELETE FROM oauth_grants WHERE id = $1;

-- name: DeleteOAuthGrantForUser :execrows
DELETE FROM oauth_grants WHERE id = $1 AND user_id = $2;

-- name: TouchOAuthGrant :exec
UPDATE oauth_grants SET last_used_at = $2 WHERE id = $1;

-- name: ListOAuthGrantsForUser :many
SELECT g.id, g.client_id, g.scopes, g.created_at, g.last_used_at, c.client_name
FROM oauth_grants g
JOIN oauth_clients c ON c.id = g.client_id
WHERE g.user_id = $1
ORDER BY g.created_at DESC;

-- name: CountMCPCall :one
-- Adds one call to the grant's current hour and returns the calls of that
-- hour and of the last 24 hours, this one included.
WITH up AS (
    INSERT INTO mcp_calls (grant_id, hour, count) VALUES ($1, $2::timestamptz, 1)
    ON CONFLICT (grant_id, hour) DO UPDATE SET count = mcp_calls.count + 1
    RETURNING count
)
SELECT (SELECT count FROM up)::int AS hour_count,
       ((SELECT count FROM up) + COALESCE((
           SELECT SUM(m.count) FROM mcp_calls m
           WHERE m.grant_id = $1 AND m.hour > $2::timestamptz - INTERVAL '24 hours' AND m.hour < $2::timestamptz
       ), 0))::int AS day_count;

-- name: CleanupOAuthRequests :execrows
DELETE FROM oauth_requests WHERE expires_at < $1;

-- name: CleanupOAuthCodes :execrows
DELETE FROM oauth_codes WHERE expires_at < $1;

-- name: CleanupOAuthTokens :execrows
DELETE FROM oauth_tokens WHERE expires_at < $1;

-- name: CleanupOAuthClients :execrows
-- Clients nobody connected within a day of registering.
DELETE FROM oauth_clients c
WHERE c.created_at < $1
  AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM oauth_requests r WHERE r.client_id = c.id);

-- name: CleanupMCPCalls :execrows
DELETE FROM mcp_calls WHERE hour < $1;
