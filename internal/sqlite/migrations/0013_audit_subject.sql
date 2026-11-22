-- The person an event is about, whoever acted: an admin's reset link or key
-- for someone shows on that person's account page. Earlier events get it
-- from the details that named the person; a grant revoked by id names its
-- owner only while the grant's tokens are kept.
ALTER TABLE audit_event ADD COLUMN subject TEXT NOT NULL DEFAULT '';

UPDATE audit_event SET subject = COALESCE(json_extract(details, '$.handle'), '')
WHERE action IN ('user.create', 'user.update', 'key.issue', 'key.rotate', 'key.revoke',
                 'invite.create', 'invite.revoke', 'password.set');

UPDATE audit_event SET subject = actor
WHERE action IN ('oauth.signin', 'oauth.grant', 'dashboard.signin', 'invite.use');

UPDATE audit_event SET subject = json_extract(details, '$.user')
WHERE action IN ('session.revoke', 'oauth.revoke') AND json_extract(details, '$.user') IS NOT NULL;

UPDATE audit_event SET subject = COALESCE((
    SELECT u.handle FROM oauth_token t JOIN user u ON u.id = t.user_id
    WHERE t.family_id = CAST(json_extract(audit_event.details, '$.grant') AS INTEGER)
    LIMIT 1), '')
WHERE action = 'oauth.revoke' AND json_extract(details, '$.user') IS NULL;

CREATE INDEX audit_event_subject ON audit_event(subject, at);
