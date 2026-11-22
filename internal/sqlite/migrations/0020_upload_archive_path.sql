-- The path a single file sent to an archive URL is staged at, fixed when the
-- upload begins like the digest; empty for an archive of files.
ALTER TABLE upload ADD COLUMN archive_path TEXT NOT NULL DEFAULT '';
