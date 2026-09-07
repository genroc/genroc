-- A trail's order, stored rather than smuggled through the id's rendering. created_at is
-- millisecond-granular, so rows written in one advance share it and something has to break the
-- tie; ids stopped sorting when they stopped being fixed-width (internal/idgen).
--
-- id stays after seq in the sort key: it is what makes the key unique, without which the keyset
-- cursor can skip a row, and rows written before this carry seq 0 and fall through to the
-- UUIDs they were ordered by then.
ALTER TABLE process_logs ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE process_signals ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;

-- Each index covers its sort key end to end, or the page is sorted rather than read in order.
DROP INDEX idx_process_logs_instance;
CREATE INDEX idx_process_logs_instance ON process_logs (instance_id, created_at, seq, id);
DROP INDEX idx_process_logs_root;
CREATE INDEX idx_process_logs_root ON process_logs (root_id, created_at, seq, id);
DROP INDEX idx_signals_fifo;
CREATE INDEX idx_signals_fifo ON process_signals (instance_id, task_id, created_at, seq, id);
