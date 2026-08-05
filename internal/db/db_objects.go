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

// Inline cutoff for a context value-slot; larger values externalize to process_objects.
// ~2 KiB aligns with Postgres TOAST_TUPLE_THRESHOLD (above it a claim TOAST-fetches the
// inline value anyway) and keeps SQLite rows off overflow pages. One value, both engines.
const contextObjectThreshold = 2 * 1024

// logForeverMillis marks a log-referenced object that must never be GC'd — used when
// log retention is disabled (logs are kept forever, so their objects must be too).
const logForeverMillis = math.MaxInt64

// SetObjectRetention sets the retention window so a log-referenced object outlives its
// log; the engine passes the same window it uses for audit-log retention.
func (db *DB) SetObjectRetention(d time.Duration) { db.objectRetentionMs.Store(d.Milliseconds()) }

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

func (db *DB) loadObjectValue(ctx context.Context, instanceID, hash string) (any, error) {
	content, err := db.q.GetObject(ctx, dbgen.GetObjectParams{InstanceID: instanceID, Hash: hash})
	if err != nil {
		return nil, fmt.Errorf("load object %s: %w", hash, err)
	}
	var v any
	if err := numeric.Decode([]byte(content), &v); err != nil {
		return nil, fmt.Errorf("decode object %s: %w", hash, err)
	}
	return v, nil
}

func (db *DB) ResolveObject(ctx context.Context, instanceID string, ref *model.ObjectRef) (any, error) {
	return db.loadObjectValue(ctx, instanceID, ref.Ref)
}

// applyContextObjectDiff, inside the caller's transaction: pending (new) written+pinned;
// dereferenced (loaded − referenced) deleted immediately when no live log needs the row
// — replaced secrets must not linger — else unpinned for the sweep. loaded ∩ referenced untouched.
func (db *DB) applyContextObjectDiff(ctx context.Context, qtx *dbgen.Queries, instanceID string, pending []*pendingObject, loaded, referenced map[string]struct{}, now int64) error {
	for _, obj := range pending {
		if err := qtx.PinContextObject(ctx, dbgen.PinContextObjectParams{
			InstanceID: instanceID,
			Hash:       obj.Hash,
			Content:    obj.Content,
			Size:       obj.Size,
			CreatedAt:  now,
		}); err != nil {
			return fmt.Errorf("write object %s: %w", obj.Hash, err)
		}
	}
	for h := range loaded {
		if _, stillRef := referenced[h]; stillRef {
			continue
		}
		// Delete outright unless a log still needs it; then unpin the survivor.
		if err := qtx.DeleteDereferencedObject(ctx, dbgen.DeleteDereferencedObjectParams{
			InstanceID: instanceID,
			Hash:       h,
			Now:        nullInt64(now),
		}); err != nil {
			return fmt.Errorf("delete dereferenced object %s: %w", h, err)
		}
		if err := qtx.UnpinObject(ctx, dbgen.UnpinObjectParams{InstanceID: instanceID, Hash: h}); err != nil {
			return fmt.Errorf("unpin object %s: %w", h, err)
		}
	}
	return nil
}

// HydrateContext resolves every externalized value-slot in inst.ContextData in place,
// replacing *model.ObjectRef markers with their loaded values. Used by the API detail
// view, which needs the full context materialized (then redacted) rather than lazily.
func (db *DB) HydrateContext(inst *model.ProcessInstance) error {
	resolve := func(v any) (any, error) {
		ref, ok := v.(*model.ObjectRef)
		if !ok {
			return v, nil
		}
		val, err := db.loadObjectValue(context.Background(), inst.ID, ref.Ref)
		if errors.Is(err, sql.ErrNoRows) {
			// The value was superseded (and its object deleted) between reading the row
			// and hydrating it — a benign race for the detail view; show it as absent.
			return nil, nil
		}
		return val, err
	}
	for _, key := range []string{"input", "output"} {
		if v, ok := inst.ContextData[key]; ok {
			rv, err := resolve(v)
			if err != nil {
				return err
			}
			inst.ContextData[key] = rv
		}
	}
	if outs, ok := inst.ContextData["outputs"].(map[string]any); ok {
		for k, v := range outs {
			rv, err := resolve(v)
			if err != nil {
				return err
			}
			outs[k] = rv
		}
	}
	return nil
}

// WriteLogObject records a (pre-redacted) log payload too large to keep inline and
// returns a reference. The object is kept until the log-retention horizon (forever when
// disabled) so it outlives its log row. Content that collides with an existing object
// shares the row — only the log horizon is extended.
func (db *DB) WriteLogObject(instanceID, content string) (*model.ObjectRef, error) {
	h := hashContent([]byte(content))
	logUntil := int64(logForeverMillis)
	if retention := db.objectRetentionMs.Load(); retention > 0 {
		logUntil = nowMillis() + retention
	}
	if err := db.q.ReferenceLogObject(context.Background(), dbgen.ReferenceLogObjectParams{
		InstanceID: instanceID,
		Hash:       h,
		Content:    content,
		Size:       int64(len(content)),
		LogUntil:   nullInt64(logUntil),
		CreatedAt:  nowMillis(),
	}); err != nil {
		return nil, fmt.Errorf("reference log object: %w", err)
	}
	return &model.ObjectRef{Ref: h, Size: int64(len(content))}, nil
}

// GetLogObject returns a log-referenced object's raw content. Non-log-referenced
// (unredacted, context-only) objects are never served here — see the query.
func (db *DB) GetLogObject(instanceID, hash string) (string, error) {
	content, err := db.q.GetLogObject(context.Background(), dbgen.GetLogObjectParams{InstanceID: instanceID, Hash: hash})
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("log payload %q for instance %q: %w", hash, instanceID, ErrNotFound)
	}
	return content, err
}

// DeleteExpiredObjects removes objects no longer pinned by context and no longer
// needed by any log (log_until passed). Called from the log-retention sweep.
func (db *DB) DeleteExpiredObjects(before int64) (int64, error) {
	return db.q.DeleteExpiredObjects(context.Background(), nullInt64(before))
}
