-- Built-in accounts: a password per person, and the single-use links that
-- set one. Timestamps are INTEGER unix nanoseconds.

-- hash is a PHC string ($argon2id$v=19$m=...,t=...,p=...$salt$key), so the
-- parameters can change and a hash is renewed at the next sign-in.
CREATE TABLE user_password (
    user_id       INTEGER PRIMARY KEY REFERENCES user(id) ON DELETE CASCADE,
    hash          TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
);

-- An invite, a password reset or the first-run setup link. Only the SHA-256
-- of the token is stored. user_id is NULL for setup, whose use creates the
-- first user. revoke_all: using a reset link also revokes the person's keys
-- and OAuth grants.
CREATE TABLE invite (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER REFERENCES user(id) ON DELETE CASCADE,
    purpose    TEXT    NOT NULL CHECK (purpose IN ('invite', 'reset', 'setup')),
    token_hash TEXT    NOT NULL UNIQUE,
    revoke_all INTEGER NOT NULL DEFAULT 0,
    created_by TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    revoked_at INTEGER
);
CREATE INDEX invite_user    ON invite(user_id, purpose);
CREATE INDEX invite_expires ON invite(expires_at);
