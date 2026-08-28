-- Machine credentials: a hashed token plus the permissions it grants. specs/api-auth.md §5.
--
-- The token itself is never stored -- only sha256(token), which is what makes a database dump
-- useless as a credential store. A 256-bit random token has no guessable structure, so a slow
-- KDF would cost latency on every request and buy nothing.
--
-- perms is a JSON array of permission strings rather than a join table: a token holds a short
-- flat list, and §9's per-resource constraint will hang off this row rather than off a second
-- one. UNIQUE on hash is load-bearing for bootstrap, which inserts only when no admin exists.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT   NOT NULL PRIMARY KEY,
    hash         TEXT   NOT NULL UNIQUE,
    label        TEXT   NOT NULL DEFAULT '',
    perms        TEXT   NOT NULL,
    created_at   BIGINT NOT NULL,
    last_used_at BIGINT,
    revoked_at   BIGINT
);
