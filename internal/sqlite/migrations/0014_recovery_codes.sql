-- Recovery codes: single-use codes a person keeps to set a new password
-- without an admin. Only the SHA-256 of a code is stored. Making a new set
-- deletes the old one.
CREATE TABLE recovery_code (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    code_hash  TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    used_at    INTEGER,
    UNIQUE (user_id, code_hash)
);
