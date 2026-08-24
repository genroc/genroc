package api

import (
	"encoding/json"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// decodeLogData unpacks the stored log-data envelope into the API view: an inline payload as its
// string value, an externalized one as the ref it is listed under, a non-envelope value verbatim.
func decodeLogData(raw string) (string, *model.ObjectRef) {
	if raw == "" {
		return "", nil
	}
	var env model.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw, nil
	}
	if env.IsRef() {
		return "", env.Refs[0]
	}
	s, _ := env.Data.(string)
	return s, nil
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
		data, ref := decodeLogData(l.Data)
		var objects []ObjectEntry
		if ref != nil {
			objects = []ObjectEntry{{Path: []any{"data"}, Ref: ref.Ref, Size: ref.Size}}
		}
		resp[i] = LogEntryResp{
			Time:     l.CreatedAt.Format(time.RFC3339Nano),
			Instance: l.InstanceID,
			Depth:    l.Depth,
			Level:    l.Level,
			Event:    l.Event,
			Task:     l.TaskID,
			Message:  l.Message,
			Code:     l.Code,
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
