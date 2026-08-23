-- A buffered signal carries an OUTCOME, not a result. Migration 021 renamed this column
-- payload -> result on the grounds that "the value delivered to an external task IS its
-- result"; the error channel (specs/external-task-queue.md) makes that half the story, and a
-- column named for one branch cannot hold the other.
--
-- The stored shape is the request body's: {"result": <value>} or {"error": {code, message,
-- data?}}, discriminated by key presence so a null result stays distinct from a failure.
ALTER TABLE process_signals RENAME COLUMN result TO outcome;

-- Existing rows hold a bare result, so wrap them into the envelope. String concatenation
-- rather than a JSON builder: json_object is spelled differently on SQLite and PostgreSQL,
-- and the stored text is already valid JSON, so `||` is exact on both.
UPDATE process_signals SET outcome = '{"result":' || outcome || '}';
