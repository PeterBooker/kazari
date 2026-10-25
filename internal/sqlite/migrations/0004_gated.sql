-- kazari:foreign-keys-off
-- Gated visibility: the members value, share links, and the browser
-- sessions of the sign-in host. Timestamps are INTEGER unix nanoseconds.

-- SQLite cannot change a CHECK constraint in place, so artifact is rebuilt
-- to accept 'members'. The runner applies this file with foreign keys off,
-- so dropping the old table leaves version, artifact_actor and the rest
-- alone, and it checks every reference before it commits.
CREATE TABLE artifact_new (
    id                   INTEGER PRIMARY KEY,
    slug                 TEXT    NOT NULL UNIQUE,
    name                 TEXT    NOT NULL DEFAULT '',
    description          TEXT    NOT NULL DEFAULT '',
    owner_id             INTEGER NOT NULL REFERENCES user(id),
    visibility           TEXT    NOT NULL CHECK (visibility IN ('public', 'unlisted', 'members')),
    published_version_id INTEGER REFERENCES version(id) ON DELETE SET NULL,
    metadata             TEXT    NOT NULL DEFAULT '{}',
    stale_at             INTEGER,
    expires_at           INTEGER,
    unpublished_at       INTEGER,
    deleted_at           INTEGER,
    purged_at            INTEGER,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);
INSERT INTO artifact_new (id, slug, name, description, owner_id, visibility, published_version_id, metadata,
    stale_at, expires_at, unpublished_at, deleted_at, purged_at, created_at, updated_at)
SELECT id, slug, name, description, owner_id, visibility, published_version_id, metadata,
    stale_at, expires_at, unpublished_at, deleted_at, purged_at, created_at, updated_at
FROM artifact;
DROP TABLE artifact;
ALTER TABLE artifact_new RENAME TO artifact;
CREATE INDEX artifact_owner   ON artifact(owner_id);
CREATE INDEX artifact_expires ON artifact(expires_at);
CREATE INDEX artifact_deleted ON artifact(deleted_at);
CREATE INDEX artifact_purged  ON artifact(purged_at);
CREATE INDEX artifact_updated ON artifact(updated_at, id);

-- Share links open one members-only artifact without signing in. The token
-- is kzs_<prefix>_<secret>; only its SHA-256 is stored, and the prefix is
-- the link's public name.
CREATE TABLE share_link (
    id           INTEGER PRIMARY KEY,
    artifact_id  INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    prefix       TEXT    NOT NULL UNIQUE,
    token_hash   TEXT    NOT NULL UNIQUE,
    label        TEXT    NOT NULL DEFAULT '',
    created_by   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    revoked_at   INTEGER,
    last_used_at INTEGER,
    uses         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX share_link_artifact ON share_link(artifact_id, created_at);

-- A browser sign-in at the identity provider, started for an artifact host.
-- id is the upstream state; binding is the hash of the browser's sign-in
-- cookie, so the callback only completes in the browser that started it.
CREATE TABLE signin_request (
    id                TEXT    PRIMARY KEY,
    binding           TEXT    NOT NULL,
    nonce             TEXT    NOT NULL,
    upstream_verifier TEXT    NOT NULL,
    host              TEXT    NOT NULL,
    path              TEXT    NOT NULL,
    nonce_hash        TEXT    NOT NULL,
    expires_at        INTEGER NOT NULL
);
CREATE INDEX signin_request_expires ON signin_request(expires_at);

-- Who signed in in a browser, on the MCP host. key_prefix names the API key
-- that signed in when no identity provider is configured; the session ends
-- with that key.
CREATE TABLE signin_session (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT    NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    key_prefix TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX signin_session_user    ON signin_session(user_id);
CREATE INDEX signin_session_expires ON signin_session(expires_at);

-- Single-use codes that carry a sign-in session to one artifact host, bound
-- to the nonce cookie the host set before it redirected.
CREATE TABLE signin_code (
    code_hash  TEXT    PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES signin_session(id) ON DELETE CASCADE,
    host       TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    nonce_hash TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
) WITHOUT ROWID;
CREATE INDEX signin_code_expires ON signin_code(expires_at);

-- A sign-in session's cookie on one artifact host (host is the label,
-- <slug> or <slug>--v<N>). Ending the sign-in session ends all of them.
CREATE TABLE view_session (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT    NOT NULL UNIQUE,
    session_id INTEGER NOT NULL REFERENCES signin_session(id) ON DELETE CASCADE,
    host       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX view_session_session ON view_session(session_id);
CREATE INDEX view_session_expires ON view_session(expires_at);
