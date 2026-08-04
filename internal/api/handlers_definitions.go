package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

func (h *Handlers) putDefinition(raw json.RawMessage) Reply {
	req, err := decodeBody[PutDefinitionReq](raw)
	if err != nil {
		return errReply(err)
	}
	// Validate returns a *model.ValidationError for struct-tag failures, which codeOf
	// maps to invalid and errReply expands into per-field detail; the schema/goto
	// checks below it are plain errors and classify the same way via invalid().
	if err := req.Validate(); err != nil {
		return errReply(err)
	}
	latestV, _ := h.db.LatestVersion(req.Name)
	version := latestV + 1
	if _, err := validation.Generate(&req.ProcessDefinition); err != nil {
		return invalid("%w", err).reply()
	}
	if err := validation.ValidateChildProcessRefs(&req.ProcessDefinition, version, h.db); err != nil {
		return invalid("%w", err).reply()
	}
	// Reject registration if a required config var has no value in the server
	// environment, the same rule ResolveConfig enforces at instance start — so a
	// missing GENROC_<PROCESS>_<NAME> surfaces here rather than on first start.
	if _, err := req.ResolveConfig(os.LookupEnv); err != nil {
		return errReply(err)
	}
	if err := h.db.SaveDefinition(&req.ProcessDefinition, version, nil, "", defaultChannel); err != nil {
		return errReply(fmt.Errorf("save: %w", err))
	}
	return okReply(map[string]interface{}{"saved": true, "name": req.Name, "version": version})
}

func (h *Handlers) listDefinitions(raw json.RawMessage) Reply {
	req, err := decodeOptionalBody[ListDefinitionsReq](raw)
	if err != nil {
		return errReply(err)
	}
	defs, info, err := h.db.ListDefinitions(
		db.Window{After: req.CreatedAfter, Before: req.CreatedBefore}, req.page())
	if err != nil {
		return errReply(err)
	}
	summaries := make([]DefinitionSummary, len(defs))
	for i, d := range defs {
		summaries[i] = DefinitionSummary{
			Name:      d.Def.Name,
			Version:   d.Version,
			CreatedAt: d.CreatedAt.Format(time.RFC3339Nano),
			Raises:    d.Def.Raises(),
		}
	}
	return okReply(PageResp[DefinitionSummary]{Items: summaries, Page: info})
}

// resolveDefaultVersion resolves a bare process reference to the version "latest" points
// at, falling back to the highest version for definitions predating that invariant.
func (h *Handlers) resolveDefaultVersion(process string) (int, error) {
	if v, err := h.db.GetChannel(process, defaultChannel); err == nil {
		return v, nil
	}
	return h.db.LatestVersion(process)
}

// batchGetter resolves definitions from an in-memory batch first, then falls back to the DB.
// This lets child-process references within the same batch validate correctly.
type batchGetter struct {
	batch    []*model.ProcessDefinition
	versions map[string]int // server-assigned versions for batch items
	db       *db.DB
}

func (g *batchGetter) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	for _, d := range g.batch {
		if d.Name == name && (version == 0 || g.versions[d.Name] == version) {
			return d, nil
		}
	}
	return g.db.GetDefinition(name, version)
}

func (g *batchGetter) LatestVersion(name string) (int, error) {
	if v, ok := g.versions[name]; ok {
		return v, nil
	}
	return g.db.LatestVersion(name)
}

func (h *Handlers) putDefinitions(raw json.RawMessage) Reply {
	req, err := decodeBody[PutDefinitionsBatchReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Channel == "" {
		req.Channel = defaultChannel
	}
	results, err := h.applyBatch(req.Definitions, req.Channel)
	if err != nil {
		return errReply(err)
	}
	return okReply(results)
}

// applyBatch registers a batch of definitions: it decides and validates every one of
// them first, then commits the lot in a single transaction.
//
// The two passes are not a style choice. Validation of a definition depends on the
// versions its batch siblings resolved to, so an interleaved save/validate loop had
// already written everything ahead of the first rejection — one `apply` landed partially,
// leaving parents pointing at children that were never stored. Nothing below the planning
// pass may write, and nothing in the commit may judge.
func (h *Handlers) applyBatch(defs []model.ProcessDefinition, channel string) ([]BatchApplyResult, error) {
	ptrs := make([]*model.ProcessDefinition, len(defs))
	for i := range defs {
		ptrs[i] = &defs[i]
	}

	sorted, err := topoSort(ptrs)
	if err != nil {
		return nil, invalid("%w", err)
	}

	// batchVersions tracks the resolved version for each process in this batch. It is
	// filled during planning, so a sibling validating against it sees the version the
	// commit will write rather than what the DB currently holds.
	batchVersions := make(map[string]int, len(sorted))

	var (
		plan    []db.DefinitionWrite
		results []BatchApplyResult
	)

	for _, def := range sorted {
		// Normalize schemas to canonical form before any comparison or storage.
		if err := def.Normalize(); err != nil {
			return nil, invalid("%s: normalize: %w", def.Name, err)
		}

		// Server assigns the next version; user-supplied value is ignored.
		latestV, _ := h.db.LatestVersion(def.Name)
		newVersion := latestV + 1

		// Build resolved deps without mutating def (raw def is stored as-is).
		newDeps, err := h.buildResolvedDeps(def, newVersion, channel, batchVersions)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}

		// Content dedup: compute hash and look up any existing version with identical content.
		rawNew, _ := json.Marshal(def)
		hash := contentHash(rawNew, newDeps)
		if v, err := h.db.FindVersionByHash(def.Name, hash); err == nil {
			plan = append(plan, db.DefinitionWrite{
				Name: def.Name, Version: v, Channels: h.channelsFor(def.Name, channel),
			})
			batchVersions[def.Name] = v
			results = append(results, BatchApplyResult{Name: def.Name, Version: v, Saved: false})
			continue
		}

		// Build a validation copy with baked-in versions for validation.
		defForValidation := applyDepsToDefCopy(def, newDeps)
		getter := &batchGetter{batch: sorted, versions: batchVersions, db: h.db}
		// Everything in this block judges the submitted document, so it is invalid,
		// not internal. ResolveConfig below is deliberately left unclassified: an
		// unset GENROC_* var is the server's environment, not the client's request.
		if err := def.Validate(); err != nil {
			return nil, invalid("%s: %w", def.Name, err)
		}
		if _, err := validation.Generate(defForValidation); err != nil {
			return nil, invalid("%s: %w", def.Name, err)
		}
		if err := validation.ValidateChildProcessRefs(defForValidation, newVersion, getter); err != nil {
			return nil, invalid("%s: %w", def.Name, err)
		}
		// Reject if a required config var is unset in the server environment, the
		// same rule ResolveConfig enforces at instance start.
		if _, err := def.ResolveConfig(os.LookupEnv); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}

		plan = append(plan, db.DefinitionWrite{
			Def: def, Name: def.Name, Version: newVersion, Deps: newDeps, Hash: hash,
			Channels: h.channelsFor(def.Name, channel),
		})
		batchVersions[def.Name] = newVersion
		results = append(results, BatchApplyResult{Name: def.Name, Version: newVersion, Saved: true})
	}

	if err := h.db.ApplyDefinitions(plan); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	return results, nil
}

// channelsFor lists the channel pointers a batch entry must set: the requested channel,
// plus the default one when the process does not have it yet (a process is always
// reachable through `latest`).
//
// Read here rather than during the commit: whether `latest` already exists is a question
// about the state before the batch, and asking it mid-transaction would read rows the
// same transaction is writing.
func (h *Handlers) channelsFor(name, channel string) []string {
	channels := []string{channel}
	if channel != defaultChannel {
		if _, err := h.db.GetChannel(name, defaultChannel); err != nil {
			channels = append(channels, defaultChannel)
		}
	}
	return channels
}

// buildResolvedDeps returns dependency rows for a def's child/child_map/child_list tasks,
// resolving version=0 refs via batchVersions or the channel. Self-refs are excluded
// (the engine runs them at the caller's version) and def is not mutated.
func (h *Handlers) buildResolvedDeps(def *model.ProcessDefinition, selfVersion int, channel string, batchVersions map[string]int) ([]db.DependencyRow, error) {
	var deps []db.DependencyRow
	for _, task := range def.Tasks {
		if task.Action == nil {
			continue
		}
		switch task.Action.Type {
		case model.ActionTypeChild, model.ActionTypeChildList:
			if task.Action.Name == def.Name && (task.Action.Version == 0 || task.Action.Version == selfVersion) {
				continue
			}
			version, err := h.resolveChildVersion(task.Action.Name, task.Action.Version, task.ID, "", channel, batchVersions)
			if err != nil {
				return nil, err
			}
			deps = append(deps, db.DependencyRow{
				ParentName:    def.Name,
				ParentVersion: selfVersion,
				TaskID:        task.ID,
				ChildKey:      "",
				ChildName:     task.Action.Name,
				ChildVersion:  version,
			})
		case model.ActionTypeChildMap:
			for key, entry := range task.Action.Children {
				if entry.Name == def.Name && (entry.Version == 0 || entry.Version == selfVersion) {
					continue
				}
				version, err := h.resolveChildVersion(entry.Name, entry.Version, task.ID, key, channel, batchVersions)
				if err != nil {
					return nil, err
				}
				deps = append(deps, db.DependencyRow{
					ParentName:    def.Name,
					ParentVersion: selfVersion,
					TaskID:        task.ID,
					ChildKey:      key,
					ChildName:     entry.Name,
					ChildVersion:  version,
				})
			}
		}
	}
	return deps, nil
}

func (h *Handlers) resolveChildVersion(childName string, childVersion int, taskID, childKey, channel string, batchVersions map[string]int) (int, error) {
	if childVersion != 0 {
		return childVersion, nil
	}
	if v, ok := batchVersions[childName]; ok {
		return v, nil
	}
	v, err := h.db.GetChannel(childName, channel)
	if err != nil {
		label := childName
		if childKey != "" {
			label = fmt.Sprintf("%s[%q]", childName, childKey)
		}
		// Classified invalid rather than letting the wrapped ErrNotFound surface as 404:
		// the submitted parent names a child the channel does not carry, so the fault is
		// in the document, not in a resource the caller asked to read.
		return 0, invalid("task %q child %s: not on channel %q (%w)", taskID, label, channel, err)
	}
	return v, nil
}

// cascadeUpdate repeatedly creates new versions of processes on channel whose deps point
// at superseded versions, until fixpoint; allUpdated accumulates the resolved versions.

// ── helpers ───────────────────────────────────────────────────────────────────

// topoSort returns definitions sorted leaves-first so child refs are resolved
// before the parents that reference them. Returns an error on cycles.
func topoSort(defs []*model.ProcessDefinition) ([]*model.ProcessDefinition, error) {
	byName := make(map[string]*model.ProcessDefinition, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(defs))
	var sorted []*model.ProcessDefinition

	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("cycle detected involving process %q", name)
		}
		state[name] = visiting
		d := byName[name]
		for _, task := range d.Tasks {
			if task.Action == nil {
				continue
			}
			var childNames []string
			switch task.Action.Type {
			case model.ActionTypeChild, model.ActionTypeChildList:
				childNames = []string{task.Action.Name}
			case model.ActionTypeChildMap:
				for _, entry := range task.Action.Children {
					childNames = append(childNames, entry.Name)
				}
			}
			for _, childName := range childNames {
				if childName == name {
					continue // self-reference is valid recursion, not a cycle
				}
				if _, inBatch := byName[childName]; inBatch {
					if err := visit(childName); err != nil {
						return err
					}
				}
			}
		}
		state[name] = done
		sorted = append(sorted, d)
		return nil
	}

	for _, d := range defs {
		if err := visit(d.Name); err != nil {
			return nil, err
		}
	}
	return sorted, nil
}

type taskChildKey struct {
	taskID   string
	childKey string
}

// applyDepsToDefCopy returns a deep copy of def with resolved child versions baked in, as
// a validation copy for genrocschema (the stored def is unchanged). Self-refs keep
// version=0 — the engine resolves them via inst.ProcessVersion.
func applyDepsToDefCopy(def *model.ProcessDefinition, deps []db.DependencyRow) *model.ProcessDefinition {
	data, _ := json.Marshal(def)
	var copy model.ProcessDefinition
	_ = json.Unmarshal(data, &copy)
	lookup := make(map[taskChildKey]int, len(deps))
	for _, d := range deps {
		lookup[taskChildKey{d.TaskID, d.ChildKey}] = d.ChildVersion
	}
	for _, task := range copy.Tasks {
		if task.Action == nil {
			continue
		}
		switch task.Action.Type {
		case model.ActionTypeChild, model.ActionTypeChildList:
			if v, ok := lookup[taskChildKey{task.ID, ""}]; ok {
				task.Action.Version = v
			}
		case model.ActionTypeChildMap:
			for key := range task.Action.Children {
				if v, ok := lookup[taskChildKey{task.ID, key}]; ok {
					entry := task.Action.Children[key]
					entry.Version = v
					task.Action.Children[key] = entry
				}
			}
		}
	}
	return &copy
}

// contentHash is a SHA256 digest over rawJSON and the sorted deps, uniquely identifying
// a (definition, resolved-children) snapshot for content dedup.
func contentHash(rawJSON []byte, deps []db.DependencyRow) string {
	h := sha256.New()
	h.Write(rawJSON)
	sorted := append([]db.DependencyRow(nil), deps...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].TaskID != sorted[j].TaskID {
			return sorted[i].TaskID < sorted[j].TaskID
		}
		return sorted[i].ChildKey < sorted[j].ChildKey
	})
	for _, d := range sorted {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%d", d.TaskID, d.ChildKey, d.ChildName, d.ChildVersion)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// subtree collects the definition for rootName and, recursively, all its dependencies
// present in defs, following baked-in child refs.
func subtree(defs []db.VersionedDef, rootName string) ([]db.VersionedDef, error) {
	byName := make(map[string]*model.ProcessDefinition, len(defs))
	for _, vd := range defs {
		byName[vd.Def.Name] = vd.Def
	}

	visited := make(map[string]bool)
	var collect func(name string) error
	collect = func(name string) error {
		if visited[name] {
			return nil
		}
		d, ok := byName[name]
		if !ok {
			return nil // dependency not on this channel, skip
		}
		visited[name] = true
		for _, task := range d.Tasks {
			if task.Action == nil {
				continue
			}
			switch task.Action.Type {
			case model.ActionTypeChild, model.ActionTypeChildList:
				if err := collect(task.Action.Name); err != nil {
					return err
				}
			case model.ActionTypeChildMap:
				for _, entry := range task.Action.Children {
					if err := collect(entry.Name); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := collect(rootName); err != nil {
		return nil, err
	}

	var out []db.VersionedDef
	for _, vd := range defs {
		if visited[vd.Def.Name] {
			out = append(out, vd)
		}
	}
	return out, nil
}

func (h *Handlers) validateDefinitions(raw json.RawMessage) Reply {
	defs, err := decodeBody[[]model.ProcessDefinition](raw)
	if err != nil {
		return errReply(err)
	}
	ptrs := make([]*model.ProcessDefinition, len(defs))
	for i := range defs {
		ptrs[i] = &defs[i]
	}
	getter := &batchGetter{batch: ptrs, versions: map[string]int{}, db: h.db}
	schemas := make([]validation.SchemaFile, 0, len(ptrs))
	for _, def := range ptrs {
		// Everything here is a verdict on the submitted document, so all of it is
		// invalid — never internal. The %w keeps a *model.ValidationError reachable
		// through the name prefix so its per-field detail survives to the reply.
		if err := def.Validate(); err != nil {
			return invalid("%s: %w", def.Name, err).reply()
		}
		sf, err := validation.Generate(def)
		if err != nil {
			return invalid("%s: %w", def.Name, err).reply()
		}
		if err := validation.ValidateChildProcessRefs(def, 0, getter); err != nil {
			return invalid("%s: %w", def.Name, err).reply()
		}
		schemas = append(schemas, sf)
	}
	return okReply(schemas)
}
