package model

import "time"

type RoomType string

const (
	RoomTypeChannel    RoomType = "channel"
	RoomTypeDM         RoomType = "dm"
	RoomTypeBotDM      RoomType = "botDM"
	RoomTypeDiscussion RoomType = "discussion"
)

type Room struct {
	ID               string     `json:"id" bson:"_id"`
	Name             string     `json:"name" bson:"name"`
	Type             RoomType   `json:"type" bson:"type"`
	CreatedBy        string     `json:"createdBy" bson:"createdBy"`
	SiteID           string     `json:"siteId" bson:"siteId"`
	UserCount        int        `json:"userCount" bson:"userCount"`
	LastMsgAt        *time.Time `json:"lastMsgAt,omitempty" bson:"lastMsgAt,omitempty"`
	LastMsgID        string     `json:"lastMsgId" bson:"lastMsgId"`
	LastMentionAllAt *time.Time `json:"lastMentionAllAt,omitempty" bson:"lastMentionAllAt,omitempty"`
	CreatedAt        time.Time  `json:"createdAt" bson:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt" bson:"updatedAt"`
	Restricted       bool       `json:"restricted,omitempty" bson:"restricted,omitempty"`
}

type CreateRoomRequest struct {
	Name             string   `json:"name"`
	Type             RoomType `json:"type"`
	CreatedBy        string   `json:"createdBy"`
	CreatedByAccount string   `json:"createdByAccount"`
	SiteID           string   `json:"siteId"`
	Members          []string `json:"members,omitempty"`
}

type ListRoomsResponse struct {
	Rooms []Room `json:"rooms"`
}

// RoomsInfoBatchRequest is the NATS request body for the batch room info RPC.
type RoomsInfoBatchRequest struct {
	RoomIDs []string `json:"roomIds"`
}

// RoomInfo is a single aggregated room record: Mongo metadata + Valkey key.
type RoomInfo struct {
	RoomID           string  `json:"roomId"`
	Found            bool    `json:"found"`
	SiteID           string  `json:"siteId,omitempty"`
	Name             string  `json:"name,omitempty"`
	LastMsgAt        *int64  `json:"lastMsgAt,omitempty"`
	LastMentionAllAt *int64  `json:"lastMentionAllAt,omitempty"`
	PrivateKey       *string `json:"privateKey,omitempty"`
	KeyVersion       *int    `json:"keyVersion,omitempty"`
	Error            string  `json:"error,omitempty"`
}

// RoomsInfoBatchResponse contains one entry per requested roomID, in input order.
type RoomsInfoBatchResponse struct {
	Rooms []RoomInfo `json:"rooms"`
}
