-- Passkeys: WebAuthn credentials a person signs in with in place of a
-- password. Timestamps are INTEGER unix nanoseconds.

-- webauthn_id is the user handle authenticators store with every passkey of
-- the person: random, so it says nothing about them, and the same for all
-- of their passkeys.
CREATE TABLE passkey_user (
    user_id     INTEGER PRIMARY KEY REFERENCES user(id) ON DELETE CASCADE,
    webauthn_id BLOB    NOT NULL UNIQUE
);

-- record is the credential as the passkey package keeps it: the public key,
-- the flags and the signature counter.
CREATE TABLE passkey (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    credential_id BLOB    NOT NULL UNIQUE,
    name          TEXT    NOT NULL DEFAULT '',
    record        BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
);
CREATE INDEX passkey_user_id ON passkey(user_id);
