package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ── Public types ──────────────────────────────────────────────────────────────

type DependencyRow struct {
	ParentName    string
	ParentVersion int
	TaskID        string
	ChildKey      string
	ChildName     string
	ChildVersion  int
}

type StaleRefRow struct {
	ParentName     string
	ParentVersion  int
	TaskID         string
	ChildName      string
	BakedVersion   int
	ChannelVersion int
}

type VersionedDef struct {
	Version   int
	Def       *model.ProcessDefinition
	CreatedAt time.Time // when this version was registered; the default listing sort
}

// ── Process Definitions ───────────────────────────────────────────────────────

// DefinitionWrite is one definition's outcome in a batch apply, as decided before any of
// them is written. Def nil means the content already exists at Version and only the
// channel pointers move; otherwise Version is inserted with Deps.
//
// Channels are listed rather than derived because whether the default channel needs
// setting is a question about state *before* the batch — answering it mid-commit would
// read rows the same commit is writing.
type DefinitionWrite struct {
	Def      *model.ProcessDefinition
	Name     string
	Version  int
	Deps     []DependencyRow
	Hash     string
	Channels []string
}

// ApplyDefinitions commits a whole planned batch in one transaction: either every
// definition, dependency and channel pointer lands, or none does.
//
// A batch is one logical change — an apply that half-succeeds leaves a registry whose
// parents point at children that were never written. Callers must therefore decide and
// validate every entry first; nothing here judges a definition, it only writes what it
// is given.
func (db *DB) ApplyDefinitions(writes []DefinitionWrite) error {
	if len(writes) == 0 {
		return nil
	}
	type marshalled struct {
		w    DefinitionWrite
		data string
	}
	// Marshal before opening the transaction: a failure here is the caller's data, and
	// holding a write transaction open across avoidable work costs the single SQLite
	// writer for no reason.
	rows := make([]marshalled, 0, len(writes))
	for _, w := range writes {
		data := ""
		if w.Def != nil {
			b, err := json.Marshal(w.Def)
			if err != nil {
				return fmt.Errorf("marshal %s: %w", w.Name, err)
			}
			data = string(b)
		}
		rows = append(rows, marshalled{w: w, data: data})
	}

	ctx := context.Background()
	now := nowMillis()
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		for _, r := range rows {
			if r.w.Def != nil {
				if err := qtx.InsertDefinition(ctx, dbgen.InsertDefinitionParams{
					Name:        r.w.Name,
					Version:     int64(r.w.Version),
					Definition:  r.data,
					ContentHash: r.w.Hash,
					CreatedAt:   now,
				}); err != nil {
					return fmt.Errorf("insert %s@v%d: %w", r.w.Name, r.w.Version, err)
				}
				if err := qtx.DeleteDependencies(ctx, dbgen.DeleteDependenciesParams{
					ParentName:    r.w.Name,
					ParentVersion: int64(r.w.Version),
				}); err != nil {
					return fmt.Errorf("clear deps %s@v%d: %w", r.w.Name, r.w.Version, err)
				}
				for _, d := range r.w.Deps {
					if err := qtx.InsertDependency(ctx, dbgen.InsertDependencyParams{
						ParentName:    d.ParentName,
						ParentVersion: int64(d.ParentVersion),
						TaskID:        d.TaskID,
						ChildKey:      d.ChildKey,
						ChildName:     d.ChildName,
						ChildVersion:  int64(d.ChildVersion),
					}); err != nil {
						return fmt.Errorf("dep %s -> %s: %w", r.w.Name, d.ChildName, err)
					}
				}
			}
			for _, channel := range r.w.Channels {
				if channel == "" {
					continue
				}
				if err := qtx.UpsertChannel(ctx, dbgen.UpsertChannelParams{
					Name:      r.w.Name,
					Channel:   channel,
					Version:   int64(r.w.Version),
					UpdatedAt: now,
				}); err != nil {
					return fmt.Errorf("channel %s@%s: %w", r.w.Name, channel, err)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// Only after the commit: InsertDefinition upserts, so a re-registered (name, version)
	// can carry different content than a reader cached. Dropping the entry before the
	// commit would let a concurrent read repopulate it from the pre-commit row.
	for _, r := range rows {
		if r.w.Def != nil {
			db.defCache.Delete(defKey{name: r.w.Name, version: r.w.Version})
		}
	}
	return nil
}

// SaveDefinition persists a new process definition version with its dependencies.
// If channel is non-empty, the channel pointer is updated in the same transaction
// so a crash cannot leave a definition saved without a channel pointing to it.
func (db *DB) SaveDefinition(def *model.ProcessDefinition, version int, deps []DependencyRow, hash string, channel string) error {
	data, err := json.Marshal(def)
	if err != nil {
		return err
	}
	ctx := context.Background()
	now := nowMillis()
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		if err := qtx.InsertDefinition(ctx, dbgen.InsertDefinitionParams{
			Name:        def.Name,
			Version:     int64(version),
			Definition:  string(data),
			ContentHash: hash,
			CreatedAt:   now,
		}); err != nil {
			return err
		}
		if err := qtx.DeleteDependencies(ctx, dbgen.DeleteDependenciesParams{
			ParentName:    def.Name,
			ParentVersion: int64(version),
		}); err != nil {
			return err
		}
		for _, d := range deps {
			if err := qtx.InsertDependency(ctx, dbgen.InsertDependencyParams{
				ParentName:    d.ParentName,
				ParentVersion: int64(d.ParentVersion),
				TaskID:        d.TaskID,
				ChildKey:      d.ChildKey,
				ChildName:     d.ChildName,
				ChildVersion:  int64(d.ChildVersion),
			}); err != nil {
				return err
			}
		}
		if channel != "" {
			if err := qtx.UpsertChannel(ctx, dbgen.UpsertChannelParams{
				Name:      def.Name,
				Channel:   channel,
				Version:   int64(version),
				UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// Drop any stale cache entry: InsertDefinition uses ON CONFLICT DO UPDATE, so
	// re-registering an existing (name, version) can change its content.
	db.defCache.Delete(defKey{name: def.Name, version: version})
	return nil
}

func (db *DB) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	key := defKey{name: name, version: version}

	raw, ok := db.defCache.Load(key)
	if !ok {
		row, err := db.q.GetDefinition(context.Background(), dbgen.GetDefinitionParams{Name: name, Version: int64(version)})
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("definition %q v%d: %w", name, version, ErrNotFound)
		}
		if err != nil {
			return nil, err
		}
		raw = row.Definition
		db.defCache.Store(key, raw)
	}

	// Unmarshal a fresh copy every call so callers never share mutable Task pointers.
	var def model.ProcessDefinition
	return &def, json.Unmarshal([]byte(raw.(string)), &def)
}

func (db *DB) LatestVersion(name string) (int, error) {
	v, err := db.q.LatestVersion(context.Background(), name)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, fmt.Errorf("no definitions found for %q: %w", name, ErrNotFound)
	}
	return int(v.(int64)), nil
}

// definitionPaginator is the pagination policy for ListDefinitions. Sort is by
// name, keyed on the (name, version) PRIMARY KEY (its unique tiebreaker), so the
// keyset rides the PK index.
var definitionPaginator = paginator{
	table:      "process_definitions",
	columns:    "name, version, definition, created_at",
	filterCols: []string{"created_at"},
	sorts: map[string]sortMode{
		// created carries name+version too: created_at alone is not unique, and a keyset
		// cursor on a non-total order can skip or repeat a row at the page boundary.
		"created": {{"created_at", kindInt}, {"name", kindText}, {"version", kindInt}},
		// The only text sort in the codebase, so the only one collation-dependent: SQLite compares
		// bytes, Postgres uses the locale. Each engine is self-consistent (paging never skips or
		// repeats), but no test may assert a text order of its own.
		"name": {{"name", kindText}, {"version", kindInt}},
	},
	defSort:  "created",
	defDesc:  true, // newest first, as every list endpoint defaults; name is opt-in
	defLimit: 20,
	maxLimit: 100,
}

func definitionCursorVals(sort string, vd VersionedDef) []any {
	if sort == "name" {
		return []any{vd.Def.Name, int64(vd.Version)}
	}
	return []any{vd.CreatedAt.UnixMilli(), vd.Def.Name, int64(vd.Version)}
}

// ListDefinitions returns a page of stored definitions, newest-registered first, bounded
// by a Window on created_at (zero = unbounded). Pass PageReq.Sort "name" for the
// alphabetical order instead — the window still applies, but it is then a filter rather
// than the seek the caller is walking by.
func (db *DB) ListDefinitions(created Window, req PageReq) ([]VersionedDef, PageInfo, error) {
	b, err := created.apply(definitionPaginator.query(req), "created_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, func(s rowScanner) (VersionedDef, error) {
		var name, definition string
		var version, createdAt int64
		if err := s.Scan(&name, &version, &definition, &createdAt); err != nil {
			return VersionedDef{}, err
		}
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(definition), &def); err != nil {
			return VersionedDef{}, err
		}
		return VersionedDef{Version: int(version), Def: &def, CreatedAt: toTime(createdAt)}, nil
	}, definitionCursorVals)
}

func (db *DB) FindVersionByHash(name, hash string) (int, error) {
	v, err := db.q.FindVersionByHash(context.Background(), dbgen.FindVersionByHashParams{
		Name:        name,
		ContentHash: hash,
	})
	if err != nil {
		return 0, err
	}
	// Deliberately not ErrNotFound: "no identical version exists" is the ordinary
	// answer here, the one that sends applyBatch down the save-a-new-version path.
	if v == nil {
		return 0, fmt.Errorf("no version found for %q with given hash", name)
	}
	return int(v.(int64)), nil
}

// ResolveChildVersion answers which version of `childName` a parent at
// (parentName, parentVersion) spawns from taskID: a non-zero declared version wins; else a
// self-reference inherits the parent's own version; else the version pinned at
// registration, falling back to the child's latest. depKey is the child_map key ("" for
// child and child_list).
//
// One rule, two callers. The engine resolves it at spawn; an upgrade resolves it against
// the version a parent is moving TO, so the children it moves are the ones its new
// definition names. Two copies would drift, and the drift is silent -- a parent running a
// child version its definition never mentions. specs/version-compatibility.md s3c.
func (db *DB) ResolveChildVersion(parentName string, parentVersion int, taskID, childName string, declared int, depKey string) (int, error) {
	if declared != 0 {
		return declared, nil
	}
	if childName == parentName {
		return parentVersion, nil
	}
	if v, err := db.GetDependencyVersion(parentName, parentVersion, taskID, depKey); err == nil {
		return v, nil
	}
	return db.LatestVersion(childName)
}

func (db *DB) GetDependencyVersion(parentName string, parentVersion int, taskID string, childKey string) (int, error) {
	v, err := db.q.GetDependencyVersion(context.Background(), dbgen.GetDependencyVersionParams{
		ParentName:    parentName,
		ParentVersion: int64(parentVersion),
		TaskID:        taskID,
		ChildKey:      childKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("dependency for %q v%d task %q child %q: %w", parentName, parentVersion, taskID, childKey, ErrNotFound)
	}
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// ChildPin is one child version a definition version was registered against.
type ChildPin struct {
	Name    string
	Version int
}

// ListDependencies returns every child version (parentName, parentVersion) is pinned to.
// Self-references are absent by construction: buildResolvedDeps excludes them, because the
// engine runs a self-call at the caller's own version rather than a recorded one.
func (db *DB) ListDependencies(parentName string, parentVersion int) ([]ChildPin, error) {
	rows, err := db.q.ListDependencies(context.Background(), dbgen.ListDependenciesParams{
		ParentName:    parentName,
		ParentVersion: int64(parentVersion),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ChildPin, len(rows))
	for i, r := range rows {
		out[i] = ChildPin{Name: r.ChildName, Version: int(r.ChildVersion)}
	}
	return out, nil
}

func (db *DB) FindStaleRefs(channel string) ([]StaleRefRow, error) {
	rows, err := db.q.FindStaleRefs(context.Background(), channel)
	if err != nil {
		return nil, err
	}
	out := make([]StaleRefRow, len(rows))
	for i, r := range rows {
		out[i] = StaleRefRow{
			ParentName:     r.ParentName,
			ParentVersion:  int(r.ParentVersion),
			TaskID:         r.TaskID,
			ChildName:      r.ChildName,
			BakedVersion:   int(r.BakedVersion),
			ChannelVersion: int(r.ChannelVersion),
		}
	}
	return out, nil
}

// ── Channels ──────────────────────────────────────────────────────────────────

func (db *DB) SaveChannel(name, channel string, version int) error {
	return db.q.UpsertChannel(context.Background(), dbgen.UpsertChannelParams{
		Name:      name,
		Channel:   channel,
		Version:   int64(version),
		UpdatedAt: nowMillis(),
	})
}

func (db *DB) GetChannel(name, channel string) (int, error) {
	v, err := db.q.GetChannel(context.Background(), dbgen.GetChannelParams{Name: name, Channel: channel})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("process %q has no channel %q: %w", name, channel, ErrNotFound)
	}
	return int(v), err
}

func (db *DB) DeleteChannel(name, channel string) error {
	return db.q.DeleteChannel(context.Background(), dbgen.DeleteChannelParams{Name: name, Channel: channel})
}

// ChannelRow is one (channel → version) mapping for a process, returned ordered
// by ListChannels.
type ChannelRow struct {
	Channel string
	Version int
}

// channelPaginator is the pagination policy for ListChannels. It is always scoped
// to one process name (a required filter); within that name the channel is unique,
// so it is both the sort key and its own tiebreaker — and (name, channel) is the
// PRIMARY KEY, so the keyset rides the PK index.
var channelPaginator = paginator{
	table:      "process_channels",
	columns:    "channel, version",
	filterCols: []string{"name"},
	sorts: map[string]sortMode{
		"channel": {{"channel", kindText}},
	},
	defSort:  "channel",
	defDesc:  false,
	defLimit: 20,
	maxLimit: 100,
}

func channelCursorVals(sort string, r ChannelRow) []any {
	return []any{r.Channel}
}

func (db *DB) ListChannels(name string, req PageReq) ([]ChannelRow, PageInfo, error) {
	b, err := channelPaginator.query(req).Eq("name", name).build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, func(s rowScanner) (ChannelRow, error) {
		var r ChannelRow
		var version int64
		if err := s.Scan(&r.Channel, &version); err != nil {
			return ChannelRow{}, err
		}
		r.Version = int(version)
		return r, nil
	}, channelCursorVals)
}

func (db *DB) LoadDefinitionsOnChannel(channel string) ([]VersionedDef, error) {
	rows, err := db.q.LoadDefinitionsOnChannel(context.Background(), channel)
	if err != nil {
		return nil, err
	}
	out := make([]VersionedDef, 0, len(rows))
	for _, r := range rows {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(r.Definition), &def); err != nil {
			return nil, err
		}
		out = append(out, VersionedDef{Version: int(r.Version), Def: &def})
	}
	return out, nil
}
