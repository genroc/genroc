package api

import (
	"encoding/json"
	"fmt"
)

func (h *Handlers) putChannel(raw json.RawMessage) Reply {
	req, err := decodeBody[PutChannelReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Name == "" || req.Channel == "" || req.Version < 1 {
		return invalid("name, channel, and version (≥1) are required").reply()
	}
	// Forwarded rather than reworded, so a missing definition reports not_found and a
	// failed read reports internal instead of both collapsing into one message.
	if _, err := h.db.GetDefinition(req.Name, req.Version); err != nil {
		return errReply(err)
	}
	if err := h.db.SaveChannel(req.Name, req.Channel, req.Version); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"name": req.Name, "channel": req.Channel, "version": req.Version})
}

func (h *Handlers) deleteChannel(raw json.RawMessage) Reply {
	req, err := decodeBody[DeleteChannelReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Name == "" || req.Channel == "" {
		return invalid("name and channel are required").reply()
	}
	if err := h.db.DeleteChannel(req.Name, req.Channel); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"deleted": true})
}

func (h *Handlers) listChannels(raw json.RawMessage) Reply {
	req, err := decodeBody[ListChannelsReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Name == "" {
		return invalid("name is required").reply()
	}
	channels, info, err := h.db.ListChannels(req.Name, req.page())
	if err != nil {
		return errReply(err)
	}
	entries := make([]ChannelEntry, len(channels))
	for i, c := range channels {
		entries[i] = ChannelEntry{Channel: c.Channel, Version: c.Version}
	}
	return okReply(PageResp[ChannelEntry]{Items: entries, Page: info})
}

func (h *Handlers) promoteChannel(raw json.RawMessage) Reply {
	req, err := decodeBody[PromoteChannelReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.From == "" || req.To == "" {
		return invalid("from and to are required").reply()
	}
	if req.From == req.To {
		return invalid("from and to must differ").reply()
	}

	defs, err := h.db.LoadDefinitionsOnChannel(req.From)
	if err != nil {
		return errReply(fmt.Errorf("load channel %q: %w", req.From, err))
	}

	// If scoped to a process, collect only its dependency subtree.
	if req.Process != nil {
		defs, err = subtree(defs, *req.Process)
		if err != nil {
			return errReply(err)
		}
	}

	promoted := make([]map[string]any, 0, len(defs))
	for _, vd := range defs {
		if err := h.db.SaveChannel(vd.Def.Name, req.To, vd.Version); err != nil {
			return errReply(fmt.Errorf("promote %s: %w", vd.Def.Name, err))
		}
		promoted = append(promoted, map[string]any{"name": vd.Def.Name, "version": vd.Version})
	}
	return okReply(map[string]any{"from": req.From, "to": req.To, "promoted": promoted})
}

func (h *Handlers) channelStatus(raw json.RawMessage) Reply {
	req, err := decodeBody[ChannelStatusReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Channel == "" {
		return invalid("channel is required").reply()
	}

	defs, err := h.db.LoadDefinitionsOnChannel(req.Channel)
	if err != nil {
		return errReply(err)
	}

	staleRows, err := h.db.FindStaleRefs(req.Channel)
	if err != nil {
		return errReply(err)
	}

	type parentKey struct {
		name    string
		version int
	}
	staleByParent := make(map[parentKey][]StaleRef, len(staleRows))
	for _, r := range staleRows {
		k := parentKey{r.ParentName, r.ParentVersion}
		staleByParent[k] = append(staleByParent[k], StaleRef{
			TaskID:         r.TaskID,
			ChildName:      r.ChildName,
			BakedVersion:   r.BakedVersion,
			ChannelVersion: r.ChannelVersion,
		})
	}

	items := make([]ChannelStatusItem, 0, len(defs))
	for _, vd := range defs {
		k := parentKey{vd.Def.Name, vd.Version}
		items = append(items, ChannelStatusItem{
			Name:      vd.Def.Name,
			Version:   vd.Version,
			StaleRefs: staleByParent[k],
		})
	}
	return okReply(items)
}
