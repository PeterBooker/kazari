-- slugs: the slug patterns a key is restricted to, space-separated; '' is no
-- restriction. issued_by: the key its owner issued it with, NULL when an
-- admin did; revoking that key revokes this one.
ALTER TABLE api_key ADD COLUMN slugs TEXT NOT NULL DEFAULT '';
ALTER TABLE api_key ADD COLUMN issued_by INTEGER REFERENCES api_key(id) ON DELETE SET NULL;
CREATE INDEX api_key_issued_by ON api_key(issued_by);

-- artifacts:share was part of artifacts:write: visibility and share links.
-- Every credential that could write keeps it.
UPDATE api_key SET scopes = replace(scopes, 'artifacts:write', 'artifacts:write artifacts:share')
    WHERE instr(scopes, 'artifacts:write') > 0 AND instr(scopes, 'artifacts:share') = 0;
UPDATE oauth_request SET scopes = replace(scopes, 'artifacts:write', 'artifacts:write artifacts:share')
    WHERE instr(scopes, 'artifacts:write') > 0 AND instr(scopes, 'artifacts:share') = 0;
UPDATE oauth_code SET scopes = replace(scopes, 'artifacts:write', 'artifacts:write artifacts:share')
    WHERE instr(scopes, 'artifacts:write') > 0 AND instr(scopes, 'artifacts:share') = 0;
UPDATE oauth_token SET scopes = replace(scopes, 'artifacts:write', 'artifacts:write artifacts:share')
    WHERE instr(scopes, 'artifacts:write') > 0 AND instr(scopes, 'artifacts:share') = 0;
