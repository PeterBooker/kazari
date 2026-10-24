-- Kazari schema. Timestamps are INTEGER unix nanoseconds (UTC).

CREATE TABLE user (
    id           INTEGER PRIMARY KEY,
    handle       TEXT    NOT NULL UNIQUE,
    email        TEXT    UNIQUE,
    display_name TEXT    NOT NULL DEFAULT '',
    role         TEXT    NOT NULL CHECK (role IN ('admin', 'member')),
    quota_bytes  INTEGER NOT NULL DEFAULT 0,
    disabled_at  INTEGER,
    created_at   INTEGER NOT NULL
);

CREATE TABLE user_identity (
    user_id        INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    issuer         TEXT    NOT NULL,
    subject        TEXT    NOT NULL,
    email_at_login TEXT    NOT NULL DEFAULT '',
    last_login_at  INTEGER NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX user_identity_user ON user_identity(user_id);

CREATE TABLE api_key (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    prefix       TEXT    NOT NULL UNIQUE,
    hash         TEXT    NOT NULL,
    label        TEXT    NOT NULL DEFAULT '',
    scopes       TEXT    NOT NULL DEFAULT '',
    last_used_at INTEGER,
    expires_at   INTEGER,
    revoked_at   INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX api_key_user ON api_key(user_id);

CREATE TABLE artifact (
    id                   INTEGER PRIMARY KEY,
    slug                 TEXT    NOT NULL UNIQUE,
    name                 TEXT    NOT NULL DEFAULT '',
    description          TEXT    NOT NULL DEFAULT '',
    owner_id             INTEGER NOT NULL REFERENCES user(id),
    visibility           TEXT    NOT NULL CHECK (visibility IN ('public', 'unlisted')),
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
CREATE INDEX artifact_owner   ON artifact(owner_id);
CREATE INDEX artifact_expires ON artifact(expires_at);
CREATE INDEX artifact_deleted ON artifact(deleted_at);
CREATE INDEX artifact_purged  ON artifact(purged_at);
CREATE INDEX artifact_updated ON artifact(updated_at, id);

CREATE TABLE version (
    id          INTEGER PRIMARY KEY,
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    label       TEXT    NOT NULL DEFAULT '',
    created_by  TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (artifact_id, number)
);

CREATE TABLE version_file (
    version_id   INTEGER NOT NULL REFERENCES version(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL,
    blob_hash    TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (version_id, path)
) WITHOUT ROWID;
CREATE INDEX version_file_blob ON version_file(blob_hash);

CREATE TABLE blob (
    hash       TEXT    PRIMARY KEY,
    size       INTEGER NOT NULL,
    created_at INTEGER NOT NULL
) WITHOUT ROWID;

-- Who created or updated an artifact, for mine=created|updated|any.
CREATE TABLE artifact_actor (
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    created     INTEGER NOT NULL DEFAULT 0,
    updated     INTEGER NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, user_id)
) WITHOUT ROWID;
CREATE INDEX artifact_actor_user ON artifact_actor(user_id, last_at);

CREATE TABLE audit_event (
    id      INTEGER PRIMARY KEY,
    at      INTEGER NOT NULL,
    actor   TEXT    NOT NULL,
    action  TEXT    NOT NULL,
    slug    TEXT    NOT NULL DEFAULT '',
    details TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_event_at   ON audit_event(at);
CREATE INDEX audit_event_slug ON audit_event(slug, at);

CREATE TABLE upload (
    id         TEXT    PRIMARY KEY,
    owner_id   INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    bytes      INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX upload_owner   ON upload(owner_id);
CREATE INDEX upload_expires ON upload(expires_at);

CREATE TABLE upload_file (
    upload_id     TEXT    NOT NULL REFERENCES upload(id) ON DELETE CASCADE,
    path          TEXT    NOT NULL,
    staging_index INTEGER NOT NULL,
    bytes         INTEGER NOT NULL DEFAULT 0,
    content_type  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (upload_id, path)
) WITHOUT ROWID;
