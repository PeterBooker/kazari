-- Prefix of the API key that approved the grant, so the grant dies with the
-- key; '' after an identity-provider sign-in.
ALTER TABLE oauth_code  ADD COLUMN key_prefix TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_token ADD COLUMN key_prefix TEXT NOT NULL DEFAULT '';
