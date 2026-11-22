-- A share link may ask for a password before it opens its page; empty for
-- one that does not. Only an argon2id hash of the password is kept.
ALTER TABLE share_link ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';
