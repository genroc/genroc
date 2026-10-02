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

// withObjectRefs marks every `objects` path relative to at, where v sits in the response. The
// wire leaves the slot absent (specs/object-store.md §The wire); a reader needs the marker.
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

// pathUnder strips the prefix at. A section can name paths outside the rendered value (`get`
// lists the whole response's), and those belong to nobody here.
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

// logData never fetches: a trail is scanned and these payloads are large by definition, so
// `genctl object <ref>` gets the one that matters.
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

// spliceObjects is client-side because the server materializing every value behind a query
// parameter would be an unbounded response.
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

// place skips a missing step rather than creating it: the path came from this response, so a
// miss means it disagrees with its objects section, and invented structure would hide that.
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
