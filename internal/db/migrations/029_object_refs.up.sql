-- Split content from the claims on it. specs/object-store.md.
--
-- process_objects keyed content by (instance_id, hash), so byte-identical content in ten
-- instances was ten rows: content addressing dedupes within an instance and nowhere else. It
-- also had no owner but an instance, which is why a value embedded in a DEFINITION could not be
-- externalized at all -- nothing could express "lives as long as this version".
CREATE TABLE objects (
    hash       TEXT PRIMARY KEY,   -- sha256 prefix of content; the identity, globally
    content    TEXT   NOT NULL,    -- immutable: same hash, same bytes, written once
    size       BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

-- Who holds an object, and until when. A context pin was a boolean column and a log horizon a
-- millisecond column, ORed in every predicate; both are rows here, so a further owner kind needs
-- no further clause. expires_at NULL = held until the ref is dropped.
--
-- owner_kind governs LIFETIME, not access: reads are addressed by content hash and consult no
-- ref at all. Four kinds:
--   instance   -- a live context value-slot; owner_id is the instance
--   log        -- a pre-redacted log payload; owner_id is the instance, expires at the horizon
--   definition -- a value embedded in a definition version; owner_id is 'name@version', and it
--                 never expires, because nothing deletes a definition version
--   grace      -- nobody holds this any more, but a reference was handed out recently; owner_id
--                 is '' (one per object) and it expires at --object-grace. See below.
CREATE TABLE object_refs (
    hash       TEXT NOT NULL,
    owner_kind TEXT NOT NULL,
    owner_id   TEXT NOT NULL,
    expires_at BIGINT,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (hash, owner_kind, owner_id)
);

-- The GC asks one question: has this object any ref? Indexed from the hash side.
CREATE INDEX idx_object_refs_hash ON object_refs (hash, expires_at);
-- And from the owner side, for "what does this owner still hold".
CREATE INDEX idx_object_refs_owner ON object_refs (owner_kind, owner_id);

-- Move the existing rows. Content dedupes on the way in: the same bytes under two instances
-- collapse to one object, which is the whole point of the change. MIN() picks a representative
-- of columns equal by construction (content is the hash's preimage; size follows).
INSERT INTO objects (hash, content, size, created_at)
SELECT hash, MIN(content), MIN(size), MIN(created_at) FROM process_objects GROUP BY hash;

-- A pinned row becomes an instance claim with no horizon.
INSERT INTO object_refs (hash, owner_kind, owner_id, expires_at, created_at)
SELECT hash, 'instance', instance_id, NULL, created_at FROM process_objects WHERE pinned = 1;

-- A log-referenced row becomes a log claim carrying the horizon it already had. Both may exist
-- for one (instance, hash) -- the shared secret-free row 018 describes, now two rows saying so
-- rather than two columns.
INSERT INTO object_refs (hash, owner_kind, owner_id, expires_at, created_at)
SELECT hash, 'log', instance_id, log_until, created_at FROM process_objects WHERE log_until IS NOT NULL;

DROP TABLE process_objects;
