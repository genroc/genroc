package api

import (
	"encoding/json"
	"fmt"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// The comparison stands alone: "what did I change, and can anything running observe it?"
// needs no upgrade machinery, which is why it was built first and why it is useful without
// the rest. Design, and the full list of what a shape check cannot see:
// specs/version-compatibility.md.

func (h *Handlers) definitionsCompat(raw json.RawMessage) Reply {
	req, err := decodeBody[CompatReq](raw)
	if err != nil {
		return errReply(err)
	}
	from, err := h.resolveCompatSide(req.From, "from", req.Process)
	if err != nil {
		return errReply(err)
	}
	to, err := h.resolveCompatSide(req.To, "to", req.Process)
	if err != nil {
		return errReply(err)
	}

	oldDefs, oldVersions := byName(from)
	newDefs, newVersions := byName(to)

	// Compat does not validate. A submitted document's child refs are unresolved and
	// Generate does not need them — only ValidateChildProcessRefs does, and that has its
	// own endpoint. Keeping batch resolution out is what stops this growing a second copy
	// of applyBatch's planning pass; a document that cannot be analysed is reported as
	// such rather than rejected.
	report, err := validation.CompareSet(oldDefs, newDefs)
	if err != nil {
		return errReply(err)
	}
	return okReply(compatResp(report, oldVersions, newVersions))
}

// resolveCompatSide turns one selector into the definitions it names. Exactly one of the
// three forms may be set: naming two would leave the pairing ambiguous, and naming none
// hides which documents were compared behind a default.
func (h *Handlers) resolveCompatSide(sel CompatSelector, side, process string) ([]db.VersionedDef, error) {
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

	var defs []db.VersionedDef
	switch {
	case sel.Channel != "":
		loaded, err := h.db.LoadDefinitionsOnChannel(sel.Channel)
		if err != nil {
			return nil, fmt.Errorf("%s: channel %q: %w", side, sel.Channel, err)
		}
		defs = loaded
	case len(sel.Versions) > 0:
		for name, version := range sel.Versions {
			def, err := h.db.GetDefinition(name, version)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", side, err)
			}
			defs = append(defs, db.VersionedDef{Version: version, Def: def})
		}
	default:
		// Version 0 is how a submitted document says it has none yet; compatResp renders
		// that as null rather than inventing the version an apply would assign.
		for i := range sel.Definitions {
			defs = append(defs, db.VersionedDef{Def: &sel.Definitions[i]})
		}
	}

	if process != "" {
		scoped, err := subtree(defs, process)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", side, err)
		}
		defs = scoped
	}
	return defs, nil
}

func byName(defs []db.VersionedDef) (map[string]*model.ProcessDefinition, map[string]int) {
	byname := make(map[string]*model.ProcessDefinition, len(defs))
	versions := make(map[string]int, len(defs))
	for _, vd := range defs {
		byname[vd.Def.Name] = vd.Def
		versions[vd.Def.Name] = vd.Version
	}
	return byname, versions
}

// versionOf renders a resolved version for the response: nil for a submitted document,
// which has none, and nil for a name the side does not carry at all.
func versionOf(versions map[string]int, name string) *int {
	v, ok := versions[name]
	if !ok || v == 0 {
		return nil
	}
	return &v
}

func compatResp(r validation.SetReport, oldVersions, newVersions map[string]int) CompatResp {
	resp := CompatResp{Compatible: r.Compatible, Processes: make([]CompatProcessResp, 0, len(r.Processes))}
	for _, p := range r.Processes {
		resp.Processes = append(resp.Processes, CompatProcessResp{
			Report: p,
			From:   versionOf(oldVersions, p.Name),
			To:     versionOf(newVersions, p.Name),
		})
	}
	for _, c := range r.Children {
		versions := newVersions
		if c.ParentSide == validation.SideFrom {
			versions = oldVersions
		}
		resp.Children = append(resp.Children, CompatChildResp{
			Parent:        c.Parent,
			ParentVersion: versionOf(versions, c.Parent),
			Task:          c.Task,
			ChildKey:      c.ChildKey,
			Child:         c.Child,
			Compatible:    c.Compatible,
			Reason:        c.Reason,
		})
	}
	for _, u := range r.Unanalysable {
		resp.Unanalysable = append(resp.Unanalysable, compatIssue(u.Name, u.Side, u.Reason, oldVersions, newVersions))
	}
	for _, u := range r.Unpaired {
		resp.Unpaired = append(resp.Unpaired, compatIssue(u.Name, u.Side, "", oldVersions, newVersions))
	}
	return resp
}

func compatIssue(name string, side validation.Side, reason string, oldVersions, newVersions map[string]int) CompatIssueResp {
	versions := newVersions
	if side == validation.SideFrom {
		versions = oldVersions
	}
	return CompatIssueResp{Name: name, Version: versionOf(versions, name), Side: string(side), Reason: reason}
}
