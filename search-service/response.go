package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hmchangw/chat/pkg/model"
)

// rawHit is generic over the source type so both message and room
// results can share the response-envelope shape.
type rawHit[T any] struct {
	Source T `json:"_source"`
}

type rawResponse[T any] struct {
	Hits struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total"`
		Hits []rawHit[T] `json:"hits"`
	} `json:"hits"`
}

// messageSource mirrors MessageSearchIndex in search-sync-worker. The
// `tshow` flag is used by the restricted-room access clauses at query
// time and not surfaced in the response.
type messageSource struct {
	MessageID             string     `json:"messageId"`
	RoomID                string     `json:"roomId"`
	SiteID                string     `json:"siteId"`
	UserID                string     `json:"userId"`
	UserAccount           string     `json:"userAccount"`
	Content               string     `json:"content"`
	CreatedAt             time.Time  `json:"createdAt"`
	ThreadParentID        string     `json:"threadParentMessageId,omitempty"`
	ThreadParentCreatedAt *time.Time `json:"threadParentMessageCreatedAt,omitempty"`
}

// roomSource is the ES `_source` shape for a spotlight hit.
type roomSource struct {
	RoomID      string    `json:"roomId"`
	RoomName    string    `json:"roomName"`
	RoomType    string    `json:"roomType"`
	UserAccount string    `json:"userAccount"`
	SiteID      string    `json:"siteId"`
	JoinedAt    time.Time `json:"joinedAt"`
}

func parseMessagesResponse(raw json.RawMessage) (*model.SearchMessagesResponse, error) {
	var rr rawResponse[messageSource]
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("parse messages response: %w", err)
	}

	out := &model.SearchMessagesResponse{
		Total:   rr.Hits.Total.Value,
		Results: make([]model.MessageSearchHit, 0, len(rr.Hits.Hits)),
	}
	for i := range rr.Hits.Hits {
		src := &rr.Hits.Hits[i].Source
		out.Results = append(out.Results, model.MessageSearchHit{
			MessageID:             src.MessageID,
			RoomID:                src.RoomID,
			SiteID:                src.SiteID,
			UserID:                src.UserID,
			UserAccount:           src.UserAccount,
			Content:               src.Content,
			CreatedAt:             src.CreatedAt,
			ThreadParentMessageID: src.ThreadParentID,
			ThreadParentCreatedAt: src.ThreadParentCreatedAt,
		})
	}
	return out, nil
}

func parseRoomsResponse(raw json.RawMessage) (*model.SearchRoomsResponse, error) {
	var rr rawResponse[roomSource]
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("parse rooms response: %w", err)
	}

	out := &model.SearchRoomsResponse{
		Total:   rr.Hits.Total.Value,
		Results: make([]model.RoomSearchHit, 0, len(rr.Hits.Hits)),
	}
	for i := range rr.Hits.Hits {
		src := &rr.Hits.Hits[i].Source
		out.Results = append(out.Results, model.RoomSearchHit{
			RoomID:      src.RoomID,
			RoomName:    src.RoomName,
			RoomType:    src.RoomType,
			UserAccount: src.UserAccount,
			SiteID:      src.SiteID,
			JoinedAt:    src.JoinedAt,
		})
	}
	return out, nil
}
