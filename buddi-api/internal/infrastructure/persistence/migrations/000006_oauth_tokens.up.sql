-- Stores each user's provider credentials, encrypted at rest.
--
-- One row per user and provider, so a reconnect replaces the credential rather than
-- accumulating rows the user would have to think about revoking. The unique index is
-- what makes that a fact the database enforces rather than a convention the
-- application has to remember.
--
-- Both token columns hold base64-encoded AES-256-GCM ciphertext. They are NOT hashed:
-- a hash is the right storage for a credential the server only verifies, but an OAuth
-- refresh token has to be presented to the provider to obtain a new access token, so
-- the server must be able to recover it. See domain.OAuthToken.
CREATE TABLE oauth_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider text NOT NULL,

    access_token_ciphertext text NOT NULL,
    refresh_token_ciphertext text NOT NULL,
    scopes text NOT NULL DEFAULT '',

    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,

    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,

    CONSTRAINT oauth_tokens_user_provider_key UNIQUE (user_id, provider),

    -- A provider is part of the code's vocabulary, so an unknown value is a bug
    -- rather than a new integration. Without this, a typo creates a credential no
    -- code path will ever read and the user is left believing they are connected.
    CONSTRAINT oauth_tokens_provider_check CHECK (provider IN ('google_calendar')),

    -- A revoked credential keeps its ciphertext so the disconnect is auditable, but
    -- the two must never be both revoked and unexpiring: that combination would be
    -- read as expired-and-refreshable by a caller that checks the two fields
    -- separately.
    CONSTRAINT oauth_tokens_revoked_expires_check CHECK (revoked_at IS NULL OR expires_at IS NOT NULL)
);

-- The lookup is always by user and provider together, which is exactly the unique
-- index's key, so this index serves Get as well as the constraint.
CREATE INDEX oauth_tokens_user_provider_idx ON oauth_tokens (user_id, provider);