-- The browser a sign-in session belongs to: the hash of its sign-in cookie,
-- so that sign-out ends every session of one browser. '' for a session from
-- before this column, which stands alone.
ALTER TABLE signin_session ADD COLUMN browser TEXT NOT NULL DEFAULT '';
CREATE INDEX signin_session_browser ON signin_session(browser);
