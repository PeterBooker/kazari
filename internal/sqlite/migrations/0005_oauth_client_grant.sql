-- When a client first completed a grant; NULL until then. Anyone can
-- register a client, so the cleanup job forgets those that never do.
-- Existing clients count as granted at creation: a grant whose tokens have
-- since expired and been deleted leaves no other trace.
ALTER TABLE oauth_client ADD COLUMN granted_at INTEGER;
UPDATE oauth_client SET granted_at = COALESCE(
    (SELECT MIN(t.created_at) FROM oauth_token t WHERE t.client_id = oauth_client.id), created_at);
