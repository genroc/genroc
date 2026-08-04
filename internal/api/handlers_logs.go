package api

import (
	"encoding/json"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// decodeLogData unpacks the stored log-data envelope into the API view: an inline payload
// as its string value, an externalized one as a bare ref (fetched on demand or inlined
// with ?resolve=true), a non-envelope value verbatim.
func decodeLogData(raw string) (string, *LogDataRef) {
	if raw == "" {
		return "", nil
	}
	var env model.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw, nil
	}
	if env.IsRef() {
		return "", &LogDataRef{Ref: env.Refs[0].Ref, Size: env.Refs[0].Size}
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
	for i, l := range logs {
		data, ref := decodeLogData(l.Data)
		// With resolve=true, replace the preview + data_ref with the full payload
		// inline. The object is owned by the log's own instance (l.InstanceID), which
		// differs from the queried root for subtree logs. Log objects are stored
		// pre-redacted, so serving them inline leaks nothing the data_ref didn't.
		if req.Resolve && ref != nil {
			if content, oerr := h.db.GetLogObject(l.InstanceID, ref.Ref); oerr == nil {
				data, ref = content, nil
			}
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
			DataRef:  ref,
			Meta:     l.Meta,
		}
	}
	return okReply(PageResp[LogEntryResp]{Items: resp, Page: info})
}

// getLogObject returns the full payload of an externalized log entry (via its data_ref).
// Only log objects are served — they are stored pre-redacted, so this leaks no secrets.
func (h *Handlers) getLogObject(id, hash string) Reply {
	if id == "" || hash == "" {
		return invalid("id and ref are required").reply()
	}
	content, err := h.db.GetLogObject(id, hash)
	if err != nil {
		// GetLogObject reports an absent object as db.ErrNotFound, so the classification
		// comes through unaided; a read failure stays internal instead of being
		// relabelled "not found" the way it was before.
		return errReply(err)
	}
	return okReply(map[string]any{"data": content})
}
