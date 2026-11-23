-- The garbage collector's query, oldest first: without it every run reads
-- the whole table. The hash, the primary key, is part of every index entry,
-- so the index also gives the query's order.
CREATE INDEX blob_created ON blob(created_at);
