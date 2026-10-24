-- The API key that approved a grant on the consent page (only possible
-- without an identity provider): its prefix, carried from the code to every
-- token of the family. '' for a grant approved after an identity-provider
-- sign-in. A grant with a key lives only as long as that key.
ALTER TABLE oauth_code  ADD COLUMN key_prefix TEXT NOT NULL DEFAULT '';
ALTER TABLE oauth_token ADD COLUMN key_prefix TEXT NOT NULL DEFAULT '';
