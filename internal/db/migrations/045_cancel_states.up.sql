-- Reintroduce the cancellation states, as the terminal stop specs/pause-resume.md left room
-- for rather than as the pre-022 verb: 'cancelled' is settled beside 'failed' and is NOT
-- revived by RetryProcess, which is what conflated the two operations before.
--
-- Only the runnable index changes. 'cancelling' rejoins it for exactly the reason 'pausing'
-- is there and 'cancelled' is not: a draining row is LEASED, so a worker that dies holding
-- one leaves it settleable only by a reclaim, and a row outside this index is never scanned
-- by ClaimInstances. Migration 022 dropped it when it renamed the states away.
DROP INDEX IF EXISTS idx_instances_runnable;
CREATE INDEX idx_instances_runnable ON process_instances (created_at)
    WHERE status IN ('running', 'failing', 'pausing', 'cancelling') AND wait_state <> 'waiting';
