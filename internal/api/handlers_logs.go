package api

import (
	"encoding/json"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// logObjects roots an entry's stored references at the ENTRY, which is where a list's objects
// section belongs -- ["data", …], never ["items", 3, "data"]. The stored paths are rooted at the
// payload; this adds the one step to it. specs/object-store.md §The wire.
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

// logData is the stored payload as a value. A malformed column reads back as the raw string
// rather than failing the listing: an audit row is best-effort, and a trail that will not render
// is worse than one entry that reads oddly.
func logData(raw string) any {
	if raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
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
	opts := db.LogQuery{
		Level:   req.Level,
		Created: db.Window{After: req.CreatedAfter, Before: req.CreatedBefore},
		Page:    req.page(),
	}
	var (
		logs []*model.LogEntry
		info db.PageInfo
	)
	if req.Recursive {
		logs, info, err = h.db.ListTreeLogs(id, opts)
	} else {
		logs, info, err = h.db.ListLogs(id, opts)
	}
	if err != nil {
		return errReply(err)
	}
	resp := make([]LogEntryResp, len(logs))
	// The payload is never inlined here: resolve=true is gone, and a trail is scanned rather
	// than read, so the entry lists its handle and `genctl object <ref>` fetches the one line
	// that matters. The preview is gone with it -- a truncated excerpt of a value nobody asked
	// for is the cost this whole change is about.
	for i, l := range logs {
		data, objects := logData(l.Data), logObjects(l.Objects)
		resp[i] = LogEntryResp{
			Time:     l.CreatedAt.Format(time.RFC3339Nano),
			Instance: l.InstanceID,
			Depth:    l.Depth,
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

// getObject serves an object's content, addressed by its hash and nothing else. Knowing a hash
// is knowing the bytes that produce it, so this discloses nothing a holder of the hash did not
// already have -- what it does disclose is existence. specs/object-store.md.
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
