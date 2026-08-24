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

// Inline cutoff for a context value-slot; larger values externalize to the object store.
// ~2 KiB aligns with Postgres TOAST_TUPLE_THRESHOLD (above it a claim TOAST-fetches the
// inline value anyway) and keeps SQLite rows off overflow pages. One value, both engines.
const contextObjectThreshold = 2 * 1024

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

// cutSlot cuts one context slot to fit: the fewest, largest leaves move to the object store and
// the rest stays inline. specs/object-store.md §Choosing what to externalize.
//
// It used to be all-or-nothing over 2 KiB, and the playground showed what that costs. The
// documented way to run a script is a child process whose INPUT carries the bundle beside the
// caller's own arguments; folding them into one object gives every call a different hash, so
// three runs of one script stored three copies of it and shared nothing. Cutting the bundle
// alone is what makes it one object with three claims.
//
// A value that is ALREADY a reference (an untouched marker) comes back as one ref at the empty
// path and no data: the caller stores nothing in the column and lists the ref.
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

// claimObjects is the ADDITION half of every object write, and the only place it is spelled:
// store the content this encode produced, then claim every hash the value REFERENCES -- not
// merely the ones just written, because a value can carry a reference it did not produce (a
// marker copied through an expression, or one that came from another instance).
//
// It takes a TRANSACTION's queries, and that is the contract rather than a convenience. The
// content upsert's row lock lasts exactly as long as its statement, so content claimed in a
// second transaction sits committed and unclaimed in between, and the sweep is entitled to take
// it. Callers either join a transaction (the instance write) or open one (CutLogValue).
//
// The owner is the only thing that varies. Removal is not shared: an instance releases when its
// value stops pointing at the object, a log claim goes when its row does. Neither has anything to
// say about the grace window -- the sweep decides that, because no owner dropping ITS claim can
// know whether it dropped the last one.
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

// applyContextObjectDiff, inside the caller's transaction: content for every pending object is
// written once (globally, deduped by hash) and this instance claims it; hashes it loaded but no
// longer references have that claim released.
//
// Releasing is never a delete. Another owner may hold the same bytes -- that is the point of one
// global store -- and even when none does, a client may be holding a reference it was handed
// moments ago. So a release leaves a grace claim and the sweep collects later.
func (db *DB) applyContextObjectDiff(ctx context.Context, qtx *dbgen.Queries, instanceID string, pending []*pendingObject, loaded, referenced map[string]struct{}, now int64) error {
	if err := claimObjects(ctx, qtx, model.ObjectOwnerInstance, instanceID, pending, referenced, now); err != nil {
		return err
	}
	for h := range loaded {
		if _, stillRef := referenced[h]; stillRef {
			continue
		}
		// Drop the claim and nothing else. Whether this was the LAST claim, and therefore
		// whether a grace window should start, is not knowable here -- the sweep decides it.
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

// CollectObjects is the sweep, in the order the two questions must be asked: retire claims whose
// horizon has passed (a log past retention, a grace window elapsed), then delete content nothing
// claims any more. Returns how many objects went.
//
// It never STAMPS a grace claim -- only an owner releasing one does. That is what keeps an
// expiring grace claim from earning itself another window forever.
func (db *DB) CollectObjects(now int64) (int64, error) {
	ctx := context.Background()
	if err := db.retireOrphanedLogRefs(ctx); err != nil {
		return 0, err
	}
	// Belt and braces: PutObject clears the mark when content is re-written, which covers a claim
	// that comes and goes between two sweeps. This catches the rest -- a claim added without
	// re-writing content, which is what a passed-through reference does. Before the mark, so an
	// object that regained a claim is never collected on a stale one.
	if _, err := db.q.ClearObjectRelease(ctx); err != nil {
		return 0, fmt.Errorf("clear object release marks: %w", err)
	}
	if _, err := db.q.MarkObjectReleased(ctx, nullInt64(now)); err != nil {
		return 0, fmt.Errorf("mark released objects: %w", err)
	}
	cutoff := now - db.objectGraceMs.Load()
	if db.dialect != "postgres" {
		// SQLite serializes writers, so no claim can be committed between this statement's
		// snapshot and its delete -- the interleaving the Postgres path guards against cannot
		// happen, and FOR UPDATE does not exist here.
		return db.q.CollectUnreferencedObjects(ctx, nullInt64(cutoff))
	}
	return db.collectUnreferencedPG(ctx, cutoff)
}

// retireOrphanedLogRefs releases the claims of log rows that no longer exist, stamping grace
// exactly as an instance release does. A log claim's owner IS the log row, so the claim needs no
// horizon: it is wanted while the row is, and this is where "the row is gone" is noticed.
//
// Driven by the owner being absent rather than by ids the prune collected, so a crash between
// deleting rows and releasing their claims is repaired by the next sweep instead of leaking.
//
// The sweep may stamp grace HERE and nowhere else, and the difference is termination: a grace
// claim retired by DropExpiredObjectRefs must never earn another window, while a log claim
// retired once has no owner left to retire again.
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

// collectUnreferencedPG deletes unclaimed content in TWO statements, and the split is the whole
// correctness argument.
//
// A single DELETE has ONE snapshot. Locking the object row (the content upsert's DO UPDATE) makes
// this wait for a writer, but on waking, Postgres re-checks only the target ROW against the newer
// version -- the NOT EXISTS subquery keeps the original snapshot and still reports no claims, so
// the delete proceeds and the writer's claim, committed while we waited, is left pointing at
// content that is gone.
//
// Read committed takes a fresh snapshot per STATEMENT, so the fix is to make the wait and the
// decision two statements: the SELECT ... FOR UPDATE blocks until every writer touching a
// candidate commits, and the DELETE that follows sees what they committed.
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

// cutLogPayload cuts a log payload the same way a value-slot is cut, and WRITES NOTHING: the
// claims belong to the log ROW and are written with it (AppendLogValue), so a claim can never
// exist without the row that owns it. The refs come back beside the value, for the row's own
// `objects` column.
//
// Same machinery as a context slot deliberately: a payload that repeats something the instance
// externalized produces the identical leaf, hashes the same, and shares that object.
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
