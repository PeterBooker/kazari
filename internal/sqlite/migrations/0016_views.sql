-- Page views of artifacts, counted in memory and added once a minute. day is
-- the start of a UTC day in unix nanoseconds; nothing finer is kept.

-- Views per artifact and day: what lists and charts read.
CREATE TABLE view_day (
    artifact_id  INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    day          INTEGER NOT NULL,
    views        INTEGER NOT NULL,
    visitors     INTEGER NOT NULL,
    member_views INTEGER NOT NULL,
    link_views   INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, day)
) WITHOUT ROWID;
CREATE INDEX view_day_day ON view_day(day);

-- Views per stored file path and day.
CREATE TABLE view_path_day (
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    day         INTEGER NOT NULL,
    path        TEXT    NOT NULL,
    views       INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, day, path)
) WITHOUT ROWID;
CREATE INDEX view_path_day_day ON view_path_day(day);

-- Members who opened a members page.
CREATE TABLE view_reader (
    artifact_id INTEGER NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    first_at    INTEGER NOT NULL,
    last_at     INTEGER NOT NULL,
    views       INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, user_id)
) WITHOUT ROWID;
