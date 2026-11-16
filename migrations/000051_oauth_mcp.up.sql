-- 000051_oauth_mcp (plan 8.6): connect your own AI assistant (Claude,
-- ChatGPT) to your money through an MCP server at /mcp.
--
-- The API is its own small OAuth 2.1 authorization server (decided with the
-- user on 2026-11-16, instead of a library): authorization code with PKCE
-- S256, public clients that register themselves (RFC 7591), refresh tokens
-- that rotate. Every code and token is 32 random bytes and only its SHA-256
-- is stored, so a copy of the database opens nothing.
--
-- oauth_clients: an assistant that registered itself. Anyone can register,
-- so the cleanup job deletes clients nobody authorized within a day.
-- oauth_requests: an authorization waiting for the person to answer in the
-- app (10 minutes).
-- oauth_grants: one connection of a person with a client, what
-- /ajustes/conexiones lists. Deleting it deletes its codes and tokens.
-- oauth_codes: single use; a second use revokes the grant (RFC 9700).
-- oauth_tokens: access (1 hour) and refresh (30 days). A refresh token used
-- after it was rotated revokes the grant too.
-- mcp_calls: calls per grant per hour, for the 60 an hour / 500 a day limit.
CREATE TABLE oauth_clients (
    id            TEXT        PRIMARY KEY,
    client_name   TEXT        NOT NULL CHECK (length(client_name) BETWEEN 1 AND 100),
    redirect_uris TEXT[]      NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 5),
    -- 'none' for public clients (PKCE only); a client that asks for a secret
    -- at registration gets one, and only its SHA-256 is kept.
    auth_method   TEXT        NOT NULL DEFAULT 'none'
                  CHECK (auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
    secret_hash   BYTEA       CHECK (secret_hash IS NULL OR length(secret_hash) = 32),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((auth_method = 'none') = (secret_hash IS NULL))
);

CREATE INDEX idx_oauth_clients_created ON oauth_clients (created_at);

CREATE TABLE oauth_requests (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id      TEXT        NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    redirect_uri   TEXT        NOT NULL,
    scopes         TEXT[]      NOT NULL,
    state          TEXT        NOT NULL DEFAULT '',
    code_challenge TEXT        NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_oauth_requests_client ON oauth_requests (client_id);
CREATE INDEX idx_oauth_requests_expires ON oauth_requests (expires_at);

CREATE TABLE oauth_grants (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id    TEXT        NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    scopes       TEXT[]      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    UNIQUE (user_id, client_id)
);

CREATE INDEX idx_oauth_grants_client ON oauth_grants (client_id);

CREATE TABLE oauth_codes (
    code_hash      BYTEA       PRIMARY KEY CHECK (length(code_hash) = 32),
    grant_id       UUID        NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    redirect_uri   TEXT        NOT NULL,
    code_challenge TEXT        NOT NULL,
    scopes         TEXT[]      NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    used_at        TIMESTAMPTZ
);

CREATE INDEX idx_oauth_codes_grant ON oauth_codes (grant_id);
CREATE INDEX idx_oauth_codes_expires ON oauth_codes (expires_at);

CREATE TABLE oauth_tokens (
    token_hash BYTEA       PRIMARY KEY CHECK (length(token_hash) = 32),
    grant_id   UUID        NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL CHECK (kind IN ('access', 'refresh')),
    scopes     TEXT[]      NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    rotated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_oauth_tokens_grant ON oauth_tokens (grant_id);
CREATE INDEX idx_oauth_tokens_expires ON oauth_tokens (expires_at);

CREATE TABLE mcp_calls (
    grant_id UUID        NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    hour     TIMESTAMPTZ NOT NULL,
    count    INTEGER     NOT NULL,
    PRIMARY KEY (grant_id, hour)
);

CREATE INDEX idx_mcp_calls_hour ON mcp_calls (hour);
