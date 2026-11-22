-- The audit log filtered by who acted: a person's page in the dashboard and
-- the account page list one actor's events, newest first.
CREATE INDEX audit_event_actor ON audit_event(actor, at);

-- And by action: the newest event of one kind, such as the last backup, is
-- asked for on a page load, and must not walk the log when there is none.
CREATE INDEX audit_event_action ON audit_event(action, at);
