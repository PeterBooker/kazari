-- Who first put a file's content at its path in the artifact, carried with
-- the content from version to version. It was found by looking for the
-- earliest version with the same content, which deleting versions would
-- change; existing files take that answer.
ALTER TABLE version_file ADD COLUMN uploaded_by TEXT NOT NULL DEFAULT '';

UPDATE version_file SET uploaded_by = COALESCE((
    SELECT v.created_by
    FROM version cv
    JOIN version v ON v.artifact_id = cv.artifact_id
    JOIN version_file vf ON vf.version_id = v.id AND vf.path = version_file.path AND vf.blob_hash = version_file.blob_hash
    WHERE cv.id = version_file.version_id
    ORDER BY v.number
    LIMIT 1), '');
