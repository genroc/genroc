package api

import (
	"encoding/json"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/numeric"
)

// logObjects re-roots the stored payload-rooted paths at the ENTRY: ["data", …].
// specs/object-store.md §The wire.
func logObjects(refs []*model.ObjectRef) []ObjectEntry {
	var out []ObjectEntry
	for _, r := range refs {
		out = append(out, ObjectEntry{Path: childPath([]any{"data"}, r.Path), Ref: r.Ref, Size: r.Size})
	}
	return out
}

func childPath(root []any, rest []any) []any {
	out := make([]any, 0, len(root)+len(rest))
	return append(append(out, root...), rest...)
}

// logData reads a malformed column back as the raw string rather than failing the listing.
// numeric.Decode, not json.Unmarshal, which rounds literals through float64.
// specs/number-precision.md.
func logData(raw string) any {
	if raw == "" {
		return nil
	}
	var v any
	if err := numeric.Decode([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}

func (h *Handlers) listInstanceLogs(id string, raw json.RawMessage) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	req, err := decodeOptionalBody[ListLogsReq](raw)
	if err != nil {
		return errReply(err)
	}
	// The floor is refused here rather than filtered to nothing downstream: a level outside the
	// vocabulary has no set of levels above it, and an empty trail reads as "nothing happened".
	if req.Level != "" && model.LogLevelsAtLeast(model.LogLevel(req.Level)) == nil {
		return invalid("level %q is not one of debug, info, warn, error", req.Level).reply()
	}
	opts := db.LogQuery{
		Level:   req.Level,
		Created: db.Window{After: req.CreatedAfter, Before: req.CreatedBefore},
		Page:    req.page(),
	}
	logs, info, err := h.db.LogsFor(id, req.Flat, opts)
	if err != nil {
		return errReply(err)
	}
	resp := make([]LogEntryResp, len(logs))
	// Never inlined, and no preview: a trail is scanned, not read, so an entry lists its
	// handle and `genctl object <ref>` fetches the one that matters.
	for i, l := range logs {
		data, objects := logData(l.Data), logObjects(l.Objects)
		resp[i] = LogEntryResp{
			Time:     l.CreatedAt.Format(time.RFC3339Nano),
			Instance: l.InstanceID,
			Level:    l.Level,
			Event:    l.Event,
			Task:     l.TaskID,
			Message:  l.Message,
			Code:     l.Code,
			Actor:    l.Actor,
			Data:     data,
			Meta:     l.Meta,
			Objects:  objects,
		}
	}
	return okReply(PageResp[LogEntryResp]{Items: resp, Page: info})
}

// getObject is addressed by hash alone, so it discloses only existence.
// specs/object-store.md.
func (h *Handlers) getObject(hash string) Reply {
	if hash == "" {
		return invalid("ref is required").reply()
	}
	content, _, err := h.db.GetObjectContent(hash)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"data": content})
}
