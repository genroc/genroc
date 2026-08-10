package api

import (
	"encoding/json"
	"fmt"
	"sort"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// Everything here is resolution, never judgement: two selectors into two
// one-version-per-process tables, reconciled. Design: specs/version-compatibility.md.

// resolvedEntry is one process on one side, with the thing that decides what a missing
// counterpart means: whether the caller named it, or it came along.
type resolvedEntry struct {
	def     *model.ProcessDefinition
	version int // 0 = a submitted document, which has no version yet
	// explicit is true when the caller named this process — as a versions entry or a
	// submitted document. An implicit entry arrived via a channel listing or by being
	// pinned as some other version's child.
	explicit bool
}

type resolvedSide map[string]resolvedEntry

func (h *Handlers) definitionsCompat(raw json.RawMessage) Reply {
	req, err := decodeBody[CompatReq](raw)
	if err != nil {
		return errReply(err)
	}
	if len(req.To.Definitions) > 0 && (req.To.Channel != "" || len(req.To.Versions) > 0) {
		// -f and an explicit target both define the same side; silently preferring one
		// would compare something the caller did not ask for.
		return invalid("to: submitted definitions already name the target side").reply()
	}
	// Validate submitted documents before comparing: "unanalysable" is a worse answer than
	// validate's, which names the task and expression. Stored versions are NOT re-validated.
	for _, sel := range []CompatSelector{req.From, req.To} {
		if len(sel.Definitions) > 0 {
			if _, err := h.validateSubmitted(sel.Definitions); err != nil {
				return errReply(err)
			}
		}
	}

	from, err := h.resolveCompatSide(req.From, "from", req.Process)
	if err != nil {
		return errReply(err)
	}
	to, err := h.resolveCompatSide(req.To, "to", req.Process)
	if err != nil {
		return errReply(err)
	}
	if err := reconcile(from, to); err != nil {
		return errReply(err)
	}

	report, err := validation.CompareSet(entriesFor(from), entriesFor(to))
	if err != nil {
		return errReply(err)
	}
	// A selection the server will not take is a 400, not a fault: it is the same class as
	// -f plus an explicit --to above, and refusing it here means a second consumer gets the
	// same answer as the CLI.
	if err := report.ApplySelection(req.Ignore); err != nil {
		return invalid("%s", err).reply()
	}
	return okReply(compatResp(report))
}

// reconcile settles what a missing counterpart means — the only place a comparison refuses.
// Deliberately asymmetric: naming a process and getting silence is a mistake worth catching;
// an implicit arrival with no target is simply not moving, so it carries over.
func reconcile(from, to resolvedSide) error {
	var orphans []string
	for name, e := range from {
		if _, ok := to[name]; ok {
			continue
		}
		if e.explicit {
			orphans = append(orphans, name)
			continue
		}
		to[name] = resolvedEntry{def: e.def, version: e.version}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return invalid("named on the from side but absent from the to side: %v; "+
			"name a target version for each, or drop them from --from", orphans)
	}
	// A process on the to side only is NEW: no previous version exists, so nothing is
	// being upgraded and nothing can break. It is reported, not refused, and not carried
	// backwards — inventing a from-side entry would fabricate a comparison.
	return nil
}

func entriesFor(side resolvedSide) map[string]validation.SideEntry {
	out := make(map[string]validation.SideEntry, len(side))
	for name, e := range side {
		out[name] = validation.SideEntry{Def: e.def, Version: e.version}
	}
	return out
}

// resolveCompatSide turns one selector into the table it names, then closes that table over
// the child versions its definitions were registered against. Exactly one of the three forms
// may be set: two is ambiguous, none hides which documents were compared behind a default.
func (h *Handlers) resolveCompatSide(sel CompatSelector, side, process string) (resolvedSide, error) {
	forms := 0
	for _, set := range []bool{sel.Channel != "", len(sel.Versions) > 0, len(sel.Definitions) > 0} {
		if set {
			forms++
		}
	}
	switch {
	case forms == 0:
		return nil, invalid("%s: name a channel, explicit versions, or definitions", side)
	case forms > 1:
		return nil, invalid("%s: name exactly one of channel, versions or definitions", side)
	}

	out := resolvedSide{}
	switch {
	case sel.Channel != "":
		loaded, err := h.db.LoadDefinitionsOnChannel(sel.Channel)
		if err != nil {
			return nil, fmt.Errorf("%s: channel %q: %w", side, sel.Channel, err)
		}
		// A channel nobody created resolves to nothing rather than failing, so a typo would
		// otherwise be a target side every process is missing from — a report of unjudged rows
		// and exit 0. Only the TARGET: an empty from side is the bootstrap case.
		if len(loaded) == 0 && side == "to" {
			return nil, invalid("to: channel %q carries no processes; there is nothing to "+
				"compare against", sel.Channel)
		}
		for _, vd := range loaded {
			out[vd.Def.Name] = resolvedEntry{def: vd.Def, version: vd.Version}
		}
	case len(sel.Versions) > 0:
		for name, ref := range sel.Versions {
			version, err := h.resolveVersionRef(name, ref)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", side, err)
			}
			def, err := h.db.GetDefinition(name, version)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", side, err)
			}
			out[name] = resolvedEntry{def: def, version: version, explicit: true}
		}
	default:
		// Version 0 is how a submitted document says it has none yet; compatResp renders
		// that as null rather than inventing the version an apply would assign.
		for i := range sel.Definitions {
			d := &sel.Definitions[i]
			out[d.Name] = resolvedEntry{def: d, explicit: true}
		}
	}

	if err := h.closeOverDependencies(out); err != nil {
		return nil, fmt.Errorf("%s: %w", side, err)
	}
	if process != "" {
		scoped, err := h.scopeToSubtree(out, process)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", side, err)
		}
		out = scoped
	}
	return out, nil
}

func (h *Handlers) resolveVersionRef(name string, ref VersionRef) (int, error) {
	if ref.Channel == "" {
		return ref.Version, nil
	}
	return h.db.GetChannel(name, ref.Channel)
}

// closeOverDependencies adds, transitively, the child version each entry was registered
// against, so a named parent compares the graph it runs. An entry already in the table
// wins — anything else depends on map order — which also terminates recursive walks.
func (h *Handlers) closeOverDependencies(side resolvedSide) error {
	queue := make([]resolvedEntry, 0, len(side))
	for _, e := range side {
		queue = append(queue, e)
	}
	for len(queue) > 0 {
		e := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if e.version == 0 {
			continue // a submitted document is not registered, so it has no pins
		}
		pins, err := h.db.ListDependencies(e.def.Name, e.version)
		if err != nil {
			return err
		}
		for _, p := range pins {
			if _, have := side[p.Name]; have {
				continue
			}
			def, err := h.db.GetDefinition(p.Name, p.Version)
			if err != nil {
				return fmt.Errorf("pinned child %q v%d: %w", p.Name, p.Version, err)
			}
			added := resolvedEntry{def: def, version: p.Version}
			side[p.Name] = added
			queue = append(queue, added)
		}
	}
	return nil
}

// scopeToSubtree narrows a side to one process and the children it reaches, so a large
// channel can be asked a small question.
func (h *Handlers) scopeToSubtree(side resolvedSide, process string) (resolvedSide, error) {
	defs := make([]db.VersionedDef, 0, len(side))
	for _, e := range side {
		defs = append(defs, db.VersionedDef{Version: e.version, Def: e.def})
	}
	kept, err := subtree(defs, process)
	if err != nil {
		return nil, err
	}
	out := resolvedSide{}
	for _, vd := range kept {
		out[vd.Def.Name] = side[vd.Def.Name]
	}
	return out, nil
}

func compatResp(r validation.SetReport) CompatResp {
	return CompatResp{Compatible: r.Compatible, Passes: r.Passes, Processes: r.Processes}
}
