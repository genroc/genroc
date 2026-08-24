package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"genroc/internal/numeric"
	"math"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// Inline cutoff for a context value-slot; larger values externalize to the object store.
// ~2 KiB aligns with Postgres TOAST_TUPLE_THRESHOLD (above it a claim TOAST-fetches the
// inline value anyway) and keeps SQLite rows off overflow pages. One value, both engines.
const contextObjectThreshold = 2 * 1024

// logForeverMillis marks a log-referenced object that must never be GC'd — used when
// log retention is disabled (logs are kept forever, so their objects must be too).
const logForeverMillis = math.MaxInt64

// SetObjectRetention sets the retention window so a log-referenced object outlives its
// log; the engine passes the same window it uses for audit-log retention.
func (db *DB) SetObjectRetention(d time.Duration) { db.objectRetentionMs.Store(d.Milliseconds()) }

// SetObjectGrace sets how long a released object stays fetchable. It is the contract a client
// relies on: a reference it has been handed resolves for this long whatever happens to the data
// that produced it. specs/object-store.md.
func (db *DB) SetObjectGrace(d time.Duration) { db.objectGraceMs.Store(d.Milliseconds()) }

// pendingObject is a content object an encode step wants written. Hash is the
// content address of Content (see hashContent); it is the object's id and the
// change-detection key.
type pendingObject struct {
	Hash    string
	Content string
	Size    int64
}

// hashContent is an object's content address: the first 16 bytes of the sha256, hex
// (32 chars). Deterministic, so byte-identical content dedups to one row; 128 bits
// stays collision-free at any real scale. Truncation is safe — the hash is only
// produced and compared here, never reconstructed from the full digest.
func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// encodeContextValue: small values stay inline ({data:v}); large ones become a reference
// with the bytes as a pendingObject to persist+pin in the same transaction. An unchanged
// *model.ObjectRef re-emits as-is, so an untouched big slot avoids any rewrite.
func encodeContextValue(v any) (model.Envelope, *pendingObject, error) {
	if ref, ok := v.(*model.ObjectRef); ok {
		return model.Envelope{Refs: []*model.ObjectRef{ref}}, nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return model.Envelope{}, nil, fmt.Errorf("marshal value for externalization: %w", err)
	}
	if len(b) <= contextObjectThreshold {
		return model.Envelope{Data: v}, nil, nil
	}
	h := hashContent(b)
	return model.Envelope{Refs: []*model.ObjectRef{{Ref: h, Size: int64(len(b))}}},
		&pendingObject{Hash: h, Content: string(b), Size: int64(len(b))}, nil
}

// decodeEnvelope turns a stored envelope into an in-memory value: inline values as-is,
// externalized ones into an *model.ObjectRef marker the engine resolves lazily.
func decodeEnvelope(env model.Envelope) any {
	if env.IsRef() {
		return env.Refs[0]
	}
	return env.Data
}

func (db *DB) loadObjectValue(ctx context.Context, hash string) (any, error) {
	row, err := db.q.GetObject(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("load object %s: %w", hash, err)
	}
	var v any
	if err := numeric.Decode([]byte(row.Content), &v); err != nil {
		return nil, fmt.Errorf("decode object %s: %w", hash, err)
	}
	return v, nil
}

// ResolveObject loads an externalized value. Addressed by content hash and nothing else: the
// owner is not a parameter because it would not be consulted, and a signature that accepts one
// it ignores is a lie the next reader has to disprove. specs/object-store.md.
func (db *DB) ResolveObject(ctx context.Context, ref *model.ObjectRef) (any, error) {
	return db.loadObjectValue(ctx, ref.Ref)
}

// GetObjectContent returns an object's raw content and size for the read endpoint.
func (db *DB) GetObjectContent(hash string) (string, int64, error) {
	row, err := db.q.GetObject(context.Background(), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("object %q: %w", hash, ErrNotFound)
	}
	if err != nil {
		return "", 0, err
	}
	return row.Content, row.Size, nil
}

// applyContextObjectDiff, inside the caller's transaction: content for every pending object is
// written once (globally, deduped by hash) and this instance claims it; hashes it loaded but no
// longer references have that claim released.
//
// Releasing is never a delete. Another owner may hold the same bytes -- that is the point of one
// global store -- and even when none does, a client may be holding a reference it was handed
// moments ago. So a release leaves a grace claim and the sweep collects later.
func (db *DB) applyContextObjectDiff(ctx context.Context, qtx *dbgen.Queries, instanceID string, pending []*pendingObject, loaded, referenced map[string]struct{}, now int64) error {
	for _, obj := range pending {
		if err := qtx.PutObject(ctx, dbgen.PutObjectParams{
			Hash:      obj.Hash,
			Content:   obj.Content,
			Size:      obj.Size,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("write object %s: %w", obj.Hash, err)
		}
		if err := qtx.PutObjectRef(ctx, dbgen.PutObjectRefParams{
			Hash:      obj.Hash,
			OwnerKind: string(model.ObjectOwnerInstance),
			OwnerID:   instanceID,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("claim object %s: %w", obj.Hash, err)
		}
	}
	for h := range loaded {
		if _, stillRef := referenced[h]; stillRef {
			continue
		}
		if err := qtx.DropObjectRef(ctx, dbgen.DropObjectRefParams{
			Hash:      h,
			OwnerKind: string(model.ObjectOwnerInstance),
			OwnerID:   instanceID,
		}); err != nil {
			return fmt.Errorf("release object %s: %w", h, err)
		}
		// Unconditional: no "was that the last claim" check, because a redundant grace claim on
		// an object someone else holds simply lapses unnoticed, and the check would be one more
		// thing to get wrong under concurrency.
		if err := qtx.PutObjectRef(ctx, dbgen.PutObjectRefParams{
			Hash:      h,
			OwnerKind: string(model.ObjectOwnerGrace),
			OwnerID:   model.GraceOwnerID,
			ExpiresAt: nullInt64(now + db.objectGraceMs.Load()),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("grace object %s: %w", h, err)
		}
	}
	return nil
}

// WriteLogObject records a (pre-redacted) log payload too large to keep inline and returns a
// reference. The log's claim carries the retention horizon so the object outlives its log row;
// content that collides with an existing object shares it, and only the claim is added.
func (db *DB) WriteLogObject(instanceID, content string) (*model.ObjectRef, error) {
	ctx := context.Background()
	h := hashContent([]byte(content))
	logUntil := int64(logForeverMillis)
	if retention := db.objectRetentionMs.Load(); retention > 0 {
		logUntil = nowMillis() + retention
	}
	now := nowMillis()
	if err := db.q.PutObject(ctx, dbgen.PutObjectParams{
		Hash: h, Content: content, Size: int64(len(content)), CreatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("write log object: %w", err)
	}
	if err := db.q.PutObjectRef(ctx, dbgen.PutObjectRefParams{
		Hash:      h,
		OwnerKind: string(model.ObjectOwnerLog),
		OwnerID:   instanceID,
		ExpiresAt: nullInt64(logUntil),
		CreatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("claim log object: %w", err)
	}
	return &model.ObjectRef{Ref: h, Size: int64(len(content))}, nil
}

// CollectObjects is the sweep, in the order the two questions must be asked: retire claims whose
// horizon has passed (a log past retention, a grace window elapsed), then delete content nothing
// claims any more. Returns how many objects went.
//
// It never STAMPS a grace claim -- only an owner releasing one does. That is what keeps an
// expiring grace claim from earning itself another window forever.
func (db *DB) CollectObjects(now int64) (int64, error) {
	ctx := context.Background()
	if _, err := db.q.DropExpiredObjectRefs(ctx, nullInt64(now)); err != nil {
		return 0, fmt.Errorf("retire expired object claims: %w", err)
	}
	return db.q.CollectUnreferencedObjects(ctx)
}

// CountObjectRefs reports how many owners hold an object. The cross-instance sharing this store
// exists for is invisible without it.
func (db *DB) CountObjectRefs(hash string) (int64, error) {
	return db.q.CountObjectRefs(context.Background(), hash)
}
