-- Attribution: who caused this row. specs/api-auth.md section 7.
--
-- Empty string is "unattributed", which every row written before this migration is and
-- always will be -- the fact is gone, not merely unrecorded, which is the argument for
-- landing this early rather than when it is next asked for.
--
-- One column, not two: it holds `source:subject` (`token:ci`, `header:ada@example.com`,
-- `none:anonymous`), so a reader can never mistake an identity a proxy ASSERTED for one
-- genroc AUTHENTICATED. Splitting them would let a query read the subject alone, which is
-- exactly the read that loses the distinction.
ALTER TABLE process_definitions ADD COLUMN actor TEXT NOT NULL DEFAULT '';

-- On a log row this is set only for operator-initiated events (a deploy, pause, resume,
-- retry, upgrade, or the instance's creation). The engine advances instances on its own
-- behalf, so its rows stay empty rather than claiming the actor who started the run.
ALTER TABLE process_logs ADD COLUMN actor TEXT NOT NULL DEFAULT '';
