package main

// The objects protocol, client side: a response lists its externalized values, and a display
// puts them back -- as a {ref, size} marker, or as the value itself under --resolve.
// specs/object-store.md §The wire.

import (
	"encoding/json"
	"fmt"
	"net/url"

	"genroc/internal/numeric"
)

// objectEntry is one row of a response's `objects` section: where a value was cut from, and the
// handle to fetch it with.
type objectEntry struct {
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// withObjectRefs puts a {ref, size} marker at every path an `objects` section names, relative
// to at -- where v itself sits in the response. The wire leaves the slot absent so nothing in a
// payload can be mistaken for a reference (specs/object-store.md §The wire); a reader needs one.
func withObjectRefs(v any, objects []objectEntry, at ...any) any {
	if len(objects) == 0 {
		return v
	}
	// A synthetic root, so a value externalized whole (its path IS at) can replace v itself.
	root := map[string]any{"": v}
	for _, o := range objects {
		rest, ok := pathUnder(o.Path, at)
		if !ok {
			continue
		}
		place(root, append([]any{""}, rest...), map[string]any{"ref": o.Ref, "size": o.Size})
	}
	return root[""]
}

// pathUnder returns path with the prefix at removed, and whether it was under it at all. One
// response's section can name paths outside the value being rendered (`get` lists the whole
// response's, of which State is one branch), and those belong to nobody here.
func pathUnder(path, at []any) ([]any, bool) {
	if len(path) < len(at) {
		return nil, false
	}
	for i, seg := range at {
		if path[i] != seg {
			return nil, false
		}
	}
	return path[len(at):], true
}

// logData renders a log payload for one row: whatever the entry carried inline, with each
// externalized piece shown as its {ref,size} handle in the place it was cut from. logs never
// fetches -- a trail is scanned, and these payloads are large by definition; `genctl object
// <ref>` gets the one that matters.
func logData(raw json.RawMessage, objects []objectEntry) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := numeric.Decode(raw, &value); err != nil {
		return string(raw)
	}
	value = withObjectRefs(value, objects, "data")
	if str, ok := value.(string); ok {
		return str
	}
	b, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// spliceObjects fetches every value a response listed under `objects` and puts it back at the
// path it named, which is the whole of what a recipient owes the objects protocol.
//
// The paths are arrays of keys, so walking one needs no parser and no unescaping — that is why
// they are arrays and not JSON Pointers. Client-side because the server materializing every
// value behind a query parameter is an unbounded response nobody asked the size of.
func spliceObjects(server string, raw json.RawMessage) json.RawMessage {
	var body map[string]any
	if err := numeric.Decode(raw, &body); err != nil {
		return raw
	}
	entries, _ := body["objects"].([]any)
	if len(entries) == 0 {
		return raw
	}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		ref, _ := entry["ref"].(string)
		path, _ := entry["path"].([]any)
		if ref == "" || len(path) == 0 {
			continue
		}
		var resp struct {
			Data string `json:"data"`
		}
		if err := callGet(server+"/api/objects/"+url.PathEscape(ref), &resp); err != nil {
			fatal("fetch object %s: %v", ref, err)
		}
		var value any
		if err := numeric.Decode([]byte(resp.Data), &value); err != nil {
			value = resp.Data // not JSON (a raw log payload): put it back as the string it is
		}
		place(body, path, value)
	}
	delete(body, "objects")
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

// place walks path and writes value at the end of it. A step that does not exist is skipped
// rather than created: the path came from this same response, so a miss means the response and
// its objects section disagree, and inventing structure would hide that.
func place(root any, path []any, value any) {
	cur := root
	for i, seg := range path {
		last := i == len(path)-1
		switch node := cur.(type) {
		case map[string]any:
			key, ok := seg.(string)
			if !ok {
				return
			}
			if last {
				node[key] = value
				return
			}
			cur = node[key]
		case []any:
			idx, ok := seg.(float64) // JSON numbers decode as float64
			if !ok || int(idx) < 0 || int(idx) >= len(node) {
				return
			}
			if last {
				node[int(idx)] = value
				return
			}
			cur = node[int(idx)]
		default:
			return
		}
	}
}

// runObjectCmd fetches one externalized value by the ref a response listed for it. The escape
// hatch that lets `logs` print an id instead of a payload: a trail is scanned, and the one entry
// you care about is fetched on purpose.
func runObjectCmd(server string, args []string) {
	fs := newFlagSet("object", args)
	serverFlag := addServerFlag(fs, server)
	pos := leadingArgs(fs, args)
	if len(pos) != 1 {
		fatal("usage: genctl object <ref>")
	}
	ref := pos[0]

	var resp struct {
		Data string `json:"data"`
	}
	if err := callGet(*serverFlag+"/api/objects/"+url.PathEscape(ref), &resp); err != nil {
		fatal("%v", err)
	}
	fmt.Println(resp.Data)
}
