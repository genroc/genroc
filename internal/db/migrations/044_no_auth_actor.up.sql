-- `none:` renamed to `no-auth:` in Principal.Actor(). Beside `startup:` and `cli:` (043) the
-- old source read as a missing value rather than as the statement it is: auth was off.
-- Rewritten rather than left mixed, so a reader never has to know which build wrote a row.
UPDATE process_definitions SET actor = 'no-auth:anonymous' WHERE actor = 'none:anonymous';
UPDATE process_channels    SET actor = 'no-auth:anonymous' WHERE actor = 'none:anonymous';
UPDATE process_logs        SET actor = 'no-auth:anonymous' WHERE actor = 'none:anonymous';
