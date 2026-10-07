-- OAuth credentials live only as long as the user keeps them connected, so the
-- table goes with the feature rather than lingering as an empty shell.

-- Dropped explicitly rather than relying on CASCADE: the dependent view does not
-- exist yet, and if a later migration adds one this would fail loudly instead of
-- dropping it silently.
DROP VIEW IF EXISTS oauth_connections;

DROP TABLE IF EXISTS oauth_tokens;