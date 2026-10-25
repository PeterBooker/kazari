-- Scopes are space-separated.

-- fetched_at is set for cached client ID metadata documents, NULL for DCR.
CREATE TABLE oauth_client (
    id            TEXT    PRIMARY KEY,
    name          TEXT    NOT NULL DEFAULT '',
    redirect_uris TEXT    NOT NULL DEFAULT '[]', -- JSON array
    created_at    INTEGER NOT NULL,
    fetched_at    INTEGER
);

-- user_id is bound at the callback.
CREATE TABLE oauth_request (
    id                TEXT    PRIMARY KEY,
    client_id         TEXT    NOT NULL REFERENCES oauth_client(id) ON DELETE CASCADE,
    redirect_uri      TEXT    NOT NULL,
    scopes            TEXT    NOT NULL DEFAULT '',
    code_challenge    TEXT    NOT NULL,
    resource          TEXT    NOT NULL DEFAULT '',
    client_state      TEXT    NOT NULL DEFAULT '',
    nonce             TEXT    NOT NULL DEFAULT '',
    upstream_state    TEXT    NOT NULL DEFAULT '',
    upstream_verifier TEXT    NOT NULL DEFAULT '',
    user_id           INTEGER REFERENCES user(id) ON DELETE CASCADE,
    expires_at        INTEGER NOT NULL
);
CREATE INDEX oauth_request_expires ON oauth_request(expires_at);

-- Single use: used_at is set atomically on redemption.
CREATE TABLE oauth_code (
    code_hash      TEXT    PRIMARY KEY,
    client_id      TEXT    NOT NULL REFERENCES oauth_client(id) ON DELETE CASCADE,
    user_id        INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    scopes         TEXT    NOT NULL DEFAULT '',
    redirect_uri   TEXT    NOT NULL,
    code_challenge TEXT    NOT NULL,
    resource       TEXT    NOT NULL DEFAULT '',
    expires_at     INTEGER NOT NULL,
    used_at        INTEGER
) WITHOUT ROWID;
CREATE INDEX oauth_code_expires ON oauth_code(expires_at);

-- family_id: the first refresh token of the rotation chain; replaced_by: a
-- rotated refresh token's successor.
CREATE TABLE oauth_token (
    id          INTEGER PRIMARY KEY,
    token_hash  TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL CHECK (kind IN ('access', 'refresh')),
    client_id   TEXT    NOT NULL REFERENCES oauth_client(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    scopes      TEXT    NOT NULL DEFAULT '',
    resource    TEXT    NOT NULL DEFAULT '',
    family_id   INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    revoked_at  INTEGER,
    replaced_by INTEGER REFERENCES oauth_token(id) ON DELETE SET NULL
);
CREATE INDEX oauth_token_user    ON oauth_token(user_id, kind);
CREATE INDEX oauth_token_family  ON oauth_token(family_id);
CREATE INDEX oauth_token_expires ON oauth_token(expires_at);
