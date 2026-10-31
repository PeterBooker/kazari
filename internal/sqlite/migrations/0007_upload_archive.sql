-- An upload may accept one archive at a URL without a credential: the hash
-- of the token in that URL, the digest and size the body must have, how
-- many bodies missed them, and when the archive arrived (NULL until then).
ALTER TABLE upload ADD COLUMN archive_token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE upload ADD COLUMN archive_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE upload ADD COLUMN archive_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upload ADD COLUMN archive_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upload ADD COLUMN archive_received_at INTEGER;
