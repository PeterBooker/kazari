-- Handles admin and scheduler name the operator and the scheduler, but users
-- created before 0.5.0 can still hold them, and an artifact the operator
-- changed then gained an actor row for the user called admin. Drop the rows
-- that only the operator's own events explain; a row with any other event
-- behind it, or from an audit trail that does not say, stays.
DELETE FROM artifact_actor
WHERE created = 0
  AND user_id IN (SELECT id FROM user WHERE handle = 'admin')
  AND EXISTS (
    SELECT 1 FROM artifact a
    WHERE a.id = artifact_actor.artifact_id
      AND EXISTS (SELECT 1 FROM audit_event e
        WHERE e.slug = a.slug AND e.actor = 'admin'
          AND json_extract(e.details, '$.credential') IN ('admin_token', 'local'))
      AND NOT EXISTS (SELECT 1 FROM audit_event e
        WHERE e.slug = a.slug AND e.actor = 'admin'
          AND COALESCE(json_extract(e.details, '$.credential'), '') NOT IN ('admin_token', 'local')));

-- Rename those users to the first free suffix, as provisioning would, and
-- carry the handle to the rows that store it as text. The audit log keeps
-- what it recorded, and records the rename so the two names can be joined.
CREATE TEMP TABLE handle_rename AS
SELECT u.id, u.handle AS old_handle, (
    WITH RECURSIVE n(i) AS (SELECT 2 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
    SELECT u.handle || '-' || i FROM n
    WHERE NOT EXISTS (SELECT 1 FROM user t WHERE t.handle = u.handle || '-' || i)
    ORDER BY i LIMIT 1
  ) AS new_handle
FROM user u WHERE u.handle IN ('admin', 'scheduler');

UPDATE version SET created_by = (SELECT new_handle FROM handle_rename WHERE old_handle = created_by)
WHERE created_by IN (SELECT old_handle FROM handle_rename);
UPDATE share_link SET created_by = (SELECT new_handle FROM handle_rename WHERE old_handle = created_by)
WHERE created_by IN (SELECT old_handle FROM handle_rename);
UPDATE user SET handle = (SELECT new_handle FROM handle_rename WHERE handle_rename.id = user.id)
WHERE id IN (SELECT id FROM handle_rename);

INSERT INTO audit_event (at, actor, action, details)
SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000000000, 'admin', 'user.update',
       json_object('handle', new_handle, 'renamed_from', old_handle)
FROM handle_rename;

DROP TABLE handle_rename;
