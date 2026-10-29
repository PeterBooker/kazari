-- The token family a code was exchanged for, so that a second presentation
-- of the code revokes what the first obtained; NULL until the exchange.
ALTER TABLE oauth_code ADD COLUMN family_id INTEGER;
