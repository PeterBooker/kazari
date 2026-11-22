-- A staged version was added without being published, and stays off the
-- artifact's address until it is. Every version before this migration was
-- published when it was added.
ALTER TABLE version ADD COLUMN staged INTEGER NOT NULL DEFAULT 0;
