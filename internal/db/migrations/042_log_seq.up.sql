-- A trail's order, stored instead of smuggled through the id's rendering.
--
-- created_at is millisecond-granular, so two rows written in one advance routinely share it
-- and something else has to break the tie. That used to be the id, which worked only while ids
-- were fixed-width and time-led -- the cost of which was `00004000000001` where `2-1` would do.
-- seq is the minting counter beside the id: it rises within a process, which is the only place
-- the comparison is ever made (a trail's rows are written by whichever worker holds the lease).
--
-- id stays in the sort key after it, for two reasons: it is what makes the key unique, without
-- which the keyset cursor can skip or repeat a row; and rows written before this migration all
-- carry seq 0, so they fall through to the UUIDv7 ids they were ordered by all along.
ALTER TABLE process_logs ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE process_signals ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;

-- The sort key has to be covered end to end or the page is sorted rather than read in order,
-- which is the difference migration 040 measured at 23 buffers against 18,616.
DROP INDEX idx_process_logs_instance;
CREATE INDEX idx_process_logs_instance ON process_logs (instance_id, created_at, seq, id);
DROP INDEX idx_process_logs_root;
CREATE INDEX idx_process_logs_root ON process_logs (root_id, created_at, seq, id);

-- The signal FIFO reads oldest-first for one (instance, task) and needs the same tie-break:
-- two outcomes buffered in the same millisecond are ordered by arrival, not by chance.
DROP INDEX idx_signals_fifo;
CREATE INDEX idx_signals_fifo ON process_signals (instance_id, task_id, created_at, seq, id);
