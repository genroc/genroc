package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"genroc/internal/numeric"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ~2 KiB matches Postgres TOAST_TUPLE_THRESHOLD (above it a claim TOAST-fetches the inline value
// anyway) and keeps SQLite rows off overflow pages.
const contextObjectThreshold = 2 * 1024

// SetObjectGrace sets how long released content stays fetchable: a reference a client was handed
// resolves for this long whatever happens to its data. specs/object-store.md.
func (db *DB) SetObjectGrace(d time.Duration) { db.objectGraceMs.Store(d.Milliseconds()) }

// pendingObject is content an encode wants written; Hash (hashContent) is its id.
type pendingObject struct {
	Hash    string
	Content string
	Size    int64
}

// hashContent is the content address: the first 16 bytes of the sha256, hex. Deterministic, so
// identical content dedups to one row.
func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// cutSlot moves the fewest, largest leaves out, so a big leaf repeated across calls hashes the
// same and is stored once. An ALREADY-reference comes back as one ref at the empty path.
// specs/object-store.md §Choosing what to externalize.
func cutSlot(v any) (stripped any, refs []*model.ObjectRef, pending []*pendingObject, err error) {
	if ref, ok := v.(*model.ObjectRef); ok {
		return nil, []*model.ObjectRef{{Ref: ref.Ref, Size: ref.Size}}, nil, nil
	}
	stripped, refs, pending, err = cutForSize(v, contextObjectThreshold)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal value for externalization: %w", err)
	}
	if len(refs) == 0 {
		return v, nil, nil, nil
	}
	return stripped, refs, pending, nil
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

// ObjectLoader is the hash -> value function model.NewContext takes. It does not retry; the
// engine wraps it (internal/engine/readretry.go).
func (db *DB) ObjectLoader() func(hash string) (any, error) {
	return func(hash string) (any, error) {
		return db.ResolveObject(context.Background(), &model.ObjectRef{Ref: hash})
	}
}

// ResolveObject loads an externalized value by content hash alone; no owner parameter, since
// none would be consulted. specs/object-store.md.
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

// claimObjects stores pending content, then claims every hash the value REFERENCES, not just the
// ones written. qtx must be the owner write's transaction: the upsert's row lock ends with its
// statement, so content claimed in a second transaction is sweepable in between.
func claimObjects(ctx context.Context, qtx *dbgen.Queries, owner model.ObjectOwner, ownerID string,
	pending []*pendingObject, referenced map[string]struct{}, now int64) error {
	for _, obj := range pending {
		if err := qtx.PutObject(ctx, dbgen.PutObjectParams{
			Hash:      obj.Hash,
			Content:   obj.Content,
			Size:      obj.Size,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("write object %s: %w", obj.Hash, err)
		}
	}
	// Idempotent: a claim is keyed (hash, kind, owner), so re-claiming what is already held is a
	// no-op and the caller need not know which hashes are new.
	for h := range referenced {
		if err := qtx.PutObjectRef(ctx, dbgen.PutObjectRefParams{
			Hash:      h,
			OwnerKind: string(owner),
			OwnerID:   ownerID,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("claim object %s: %w", h, err)
		}
	}
	return nil
}

// applyContextObjectDiff claims pending objects and releases unreferenced ones in the caller's
// transaction.
func (db *DB) applyContextObjectDiff(ctx context.Context, qtx *dbgen.Queries, instanceID string, pending []*pendingObject, loaded, referenced map[string]struct{}, now int64) error {
	if err := claimObjects(ctx, qtx, model.ObjectOwnerInstance, instanceID, pending, referenced, now); err != nil {
		return err
	}
	for h := range loaded {
		if _, stillRef := referenced[h]; stillRef {
			continue
		}
		// Drop the claim and nothing else: whether it was the LAST one, starting a grace window,
		// is not knowable here -- the sweep decides.
		if err := qtx.DropObjectRef(ctx, dbgen.DropObjectRefParams{
			Hash:      h,
			OwnerKind: string(model.ObjectOwnerInstance),
			OwnerID:   instanceID,
		}); err != nil {
			return fmt.Errorf("release object %s: %w", h, err)
		}
	}
	return nil
}

// CollectObjects is the sweep, in the order its questions must be asked: retire orphaned log
// claims, mark unclaimed content, delete what stayed marked past --object-grace.
func (db *DB) CollectObjects(now int64) (int64, error) {
	ctx := context.Background()
	if err := db.retireOrphanedLogRefs(ctx); err != nil {
		return 0, err
	}
	// Catches a claim added without re-writing content (a passed-through reference; PutObject
	// covers the rest). Before the mark, so content that regained a claim is never collected.
	if _, err := db.q.ClearObjectRelease(ctx); err != nil {
		return 0, fmt.Errorf("clear object release marks: %w", err)
	}
	if _, err := db.q.MarkObjectReleased(ctx, nullInt64(now)); err != nil {
		return 0, fmt.Errorf("mark released objects: %w", err)
	}
	cutoff := now - db.objectGraceMs.Load()
	if db.dialect != "postgres" {
		// One statement: SQLite's single writer lets nothing commit between snapshot and delete.
		return db.q.CollectUnreferencedObjects(ctx, nullInt64(cutoff))
	}
	return db.collectUnreferencedPG(ctx, cutoff)
}

// retireOrphanedLogRefs is driven by the owner being absent, not by the ids a prune removed, so a
// crash between the two is repaired by the next sweep.
func (db *DB) retireOrphanedLogRefs(ctx context.Context) error {
	orphans, err := db.q.OrphanedLogRefs(ctx)
	if err != nil {
		return fmt.Errorf("find orphaned log claims: %w", err)
	}
	if len(orphans) == 0 {
		return nil
	}
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		for _, o := range orphans {
			if err := qtx.DropObjectRef(ctx, dbgen.DropObjectRefParams{
				Hash:      o.Hash,
				OwnerKind: string(model.ObjectOwnerLog),
				OwnerID:   o.OwnerID,
			}); err != nil {
				return fmt.Errorf("release log claim %s: %w", o.Hash, err)
			}
		}
		return nil
	})
}

// collectUnreferencedPG must stay TWO statements: a single DELETE woken from a row lock re-checks
// only the target row, so its NOT EXISTS keeps the old snapshot and deletes just-claimed content.
// internal/db/CLAUDE.md, the object store, item 2.
func (db *DB) collectUnreferencedPG(ctx context.Context, cutoff int64) (int64, error) {
	tx, err := db.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin object sweep: %w", err)
	}
	defer tx.Rollback()

	const lock = `SELECT hash FROM objects o
		WHERE NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = o.hash) FOR UPDATE`
	rows, err := tx.QueryContext(ctx, lock)
	if err != nil {
		return 0, fmt.Errorf("lock unclaimed objects: %w", err)
	}
	rows.Close()

	const del = `DELETE FROM objects o
		WHERE NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = o.hash)
		  AND o.released_at IS NOT NULL AND o.released_at < $1`
	res, err := tx.ExecContext(ctx, del, cutoff)
	if err != nil {
		return 0, fmt.Errorf("collect unreferenced objects: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit object sweep: %w", err)
	}
	return n, nil
}

// CountObjectRefs reports how many owners hold an object. The cross-instance sharing this store
// exists for is invisible without it.
func (db *DB) CountObjectRefs(hash string) (int64, error) {
	return db.q.CountObjectRefs(context.Background(), hash)
}

// cutLogPayload WRITES NOTHING: the claims belong to the log ROW and are written with it. Same
// machinery as a slot, so a payload repeating externalized content shares its object.
func cutLogPayload(v any, target int64) (any, []*model.ObjectRef, []*pendingObject, map[string]struct{}, error) {
	stripped, refs, objs, err := cutForSize(v, target)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(refs) == 0 {
		return v, nil, nil, nil, nil
	}
	referenced := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		referenced[r.Ref] = struct{}{}
	}
	return stripped, refs, objs, referenced, nil
}
