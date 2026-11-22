-- What a person has put away on a page of their own, such as the dashboard's
-- getting-started list: one row per person and name, kept until the person
-- goes.
CREATE TABLE user_flag (
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    name    TEXT    NOT NULL,
    set_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, name)
) WITHOUT ROWID;
