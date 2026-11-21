-- The audit log filtered by who acted: a person's page in the dashboard and
-- the account page list one actor's events, newest first.
CREATE INDEX audit_event_actor ON audit_event(actor, at);
