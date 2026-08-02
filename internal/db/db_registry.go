package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	Version int
	Def     *model.ProcessDefinition
}

// ── Process Definitions ───────────────────────────────────────────────────────

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
	table:   "process_definitions",
	columns: "name, version, definition",
	sorts: map[string]sortMode{
		"name": {{"name", kindText}, {"version", kindInt}},
	},
	defSort:  "name",
	defDesc:  false,
	defLimit: 20,
	maxLimit: 100,
}

func definitionCursorVals(sort string, vd VersionedDef) []any {
	return []any{vd.Def.Name, int64(vd.Version)}
}

func (db *DB) ListDefinitions(req PageReq) ([]VersionedDef, PageInfo, error) {
	b, err := definitionPaginator.query(req).build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, func(s rowScanner) (VersionedDef, error) {
		var name, definition string
		var version int64
		if err := s.Scan(&name, &version, &definition); err != nil {
			return VersionedDef{}, err
		}
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(definition), &def); err != nil {
			return VersionedDef{}, err
		}
		return VersionedDef{Version: int(version), Def: &def}, nil
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

// FindParentsOf returns all processes on channel that have deps referencing any
// of the given children. stale = dep version doesn't match the target; current = matches.
// A parent is stale if ANY of its relevant deps are mismatched.
func (db *DB) FindParentsOf(channel string, childVersions map[string]int) (stale, current []VersionedDef, err error) {
	if len(childVersions) == 0 {
		return nil, nil, nil
	}
	names := make([]string, 0, len(childVersions))
	for name := range childVersions {
		names = append(names, name)
	}
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.q.FindParentsOf(context.Background(), dbgen.FindParentsOfParams{
		Channel: channel,
		Names:   string(namesJSON),
	})
	if err != nil {
		return nil, nil, err
	}

	type entry struct {
		version int
		def     string // raw definition JSON, carried by every row of this parent
		isStale bool
	}
	byName := make(map[string]*entry)
	for _, r := range rows {
		e := byName[r.ParentName]
		if e == nil {
			e = &entry{version: int(r.ParentVersion), def: r.ParentDefinition}
			byName[r.ParentName] = e
		}
		if int(r.BakedVersion) != childVersions[r.ChildName] {
			e.isStale = true
		}
	}

	for name, e := range byName {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(e.def), &def); err != nil {
			return nil, nil, fmt.Errorf("unmarshal definition %q v%d: %w", name, e.version, err)
		}
		vd := VersionedDef{Version: e.version, Def: &def}
		if e.isStale {
			stale = append(stale, vd)
		} else {
			current = append(current, vd)
		}
	}
	return stale, current, nil
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
