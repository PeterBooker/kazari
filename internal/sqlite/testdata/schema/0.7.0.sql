-- The schema of a Kazari 0.7.0 database, for TestMigrateFromEveryRelease.
-- Written with -dump-schema when the version was released; never edited.

CREATE TABLE schema_migration (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	);

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
, slugs TEXT NOT NULL DEFAULT '', issued_by INTEGER REFERENCES api_key(id) ON DELETE SET NULL);

CREATE INDEX api_key_user ON api_key(user_id);

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
, subject TEXT NOT NULL DEFAULT '');

CREATE INDEX audit_event_at   ON audit_event(at);

CREATE INDEX audit_event_slug ON audit_event(slug, at);

CREATE TABLE upload (
    id         TEXT    PRIMARY KEY,
    owner_id   INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    bytes      INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
, archive_token_hash TEXT NOT NULL DEFAULT '', archive_sha256 TEXT NOT NULL DEFAULT '', archive_bytes INTEGER NOT NULL DEFAULT 0, archive_attempts INTEGER NOT NULL DEFAULT 0, archive_received_at INTEGER);

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

CREATE TABLE oauth_client (
    id            TEXT    PRIMARY KEY,
    name          TEXT    NOT NULL DEFAULT '',
    redirect_uris TEXT    NOT NULL DEFAULT '[]', -- JSON array
    created_at    INTEGER NOT NULL,
    fetched_at    INTEGER
, granted_at INTEGER);

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
, key_prefix TEXT NOT NULL DEFAULT '', family_id INTEGER) WITHOUT ROWID;

CREATE INDEX oauth_code_expires ON oauth_code(expires_at);

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
, key_prefix TEXT NOT NULL DEFAULT '');

CREATE INDEX oauth_token_user    ON oauth_token(user_id, kind);

CREATE INDEX oauth_token_family  ON oauth_token(family_id);

CREATE INDEX oauth_token_expires ON oauth_token(expires_at);

CREATE TABLE "artifact" (
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

CREATE INDEX artifact_owner   ON artifact(owner_id);

CREATE INDEX artifact_expires ON artifact(expires_at);

CREATE INDEX artifact_deleted ON artifact(deleted_at);

CREATE INDEX artifact_purged  ON artifact(purged_at);

CREATE INDEX artifact_updated ON artifact(updated_at, id);

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

CREATE TABLE signin_session (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT    NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    key_prefix TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
, browser TEXT NOT NULL DEFAULT '');

CREATE INDEX signin_session_user    ON signin_session(user_id);

CREATE INDEX signin_session_expires ON signin_session(expires_at);

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

CREATE INDEX signin_session_browser ON signin_session(browser);

CREATE INDEX api_key_issued_by ON api_key(issued_by);

CREATE TABLE user_password (
    user_id       INTEGER PRIMARY KEY REFERENCES user(id) ON DELETE CASCADE,
    hash          TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
);

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

CREATE INDEX audit_event_actor ON audit_event(actor, at);

CREATE INDEX audit_event_action ON audit_event(action, at);

CREATE INDEX audit_event_subject ON audit_event(subject, at);

CREATE TABLE recovery_code (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    code_hash  TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    used_at    INTEGER,
    UNIQUE (user_id, code_hash)
);

CREATE TABLE passkey_user (
    user_id     INTEGER PRIMARY KEY REFERENCES user(id) ON DELETE CASCADE,
    webauthn_id BLOB    NOT NULL UNIQUE
);

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

CREATE TABLE view_day (
    artifact_id  INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    day          INTEGER NOT NULL,
    views        INTEGER NOT NULL,
    visitors     INTEGER NOT NULL,
    member_views INTEGER NOT NULL,
    link_views   INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, day)
) WITHOUT ROWID;

CREATE INDEX view_day_day ON view_day(day);

CREATE TABLE view_path_day (
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    day         INTEGER NOT NULL,
    path        TEXT    NOT NULL,
    views       INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, day, path)
) WITHOUT ROWID;

CREATE INDEX view_path_day_day ON view_path_day(day);

CREATE TABLE view_reader (
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    first_at    INTEGER NOT NULL,
    last_at     INTEGER NOT NULL,
    views       INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, user_id)
) WITHOUT ROWID;

INSERT INTO schema_migration (version, name, applied_at) VALUES (1, '0001_init.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (2, '0002_oauth.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (3, '0003_oauth_key.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (4, '0004_gated.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (5, '0005_oauth_client_grant.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (6, '0006_oauth_code_family.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (7, '0007_upload_archive.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (8, '0008_signin_browser.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (9, '0009_key_restriction.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (10, '0010_free_reserved_handles.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (11, '0011_accounts.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (12, '0012_audit_indexes.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (13, '0013_audit_subject.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (14, '0014_recovery_codes.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (15, '0015_passkeys.sql', 0);
INSERT INTO schema_migration (version, name, applied_at) VALUES (16, '0016_views.sql', 0);
