package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMessagesResponse_HappyPath(t *testing.T) {
	body := json.RawMessage(`{
		"hits": {
			"total": {"value": 2},
			"hits": [
				{"_source": {
					"messageId": "m1", "roomId": "r1", "siteId": "site-a",
					"userId": "u1", "userAccount": "alice", "content": "hello",
					"createdAt": "2026-04-01T12:00:00Z"
				}},
				{"_source": {
					"messageId": "m2", "roomId": "r2", "siteId": "site-b",
					"userId": "u2", "userAccount": "bob", "content": "world",
					"createdAt": "2026-04-02T12:00:00Z",
					"threadParentMessageId": "p1",
					"threadParentMessageCreatedAt": "2026-04-02T11:00:00Z"
				}}
			]
		}
	}`)

	resp, err := parseMessagesResponse(body)
	require.NoError(t, err)
	assert.EqualValues(t, 2, resp.Total)
	require.Len(t, resp.Results, 2)

	assert.Equal(t, "m1", resp.Results[0].MessageID)
	assert.Equal(t, "alice", resp.Results[0].UserAccount)
	assert.Empty(t, resp.Results[0].ThreadParentMessageID)
	assert.Nil(t, resp.Results[0].ThreadParentCreatedAt)

	assert.Equal(t, "p1", resp.Results[1].ThreadParentMessageID)
	require.NotNil(t, resp.Results[1].ThreadParentCreatedAt)
	want := time.Date(2026, 4, 2, 11, 0, 0, 0, time.UTC)
	assert.True(t, resp.Results[1].ThreadParentCreatedAt.Equal(want))
}

func TestParseMessagesResponse_Empty(t *testing.T) {
	body := json.RawMessage(`{"hits":{"total":{"value":0},"hits":[]}}`)
	resp, err := parseMessagesResponse(body)
	require.NoError(t, err)
	assert.EqualValues(t, 0, resp.Total)
	assert.Empty(t, resp.Results)
}

func TestParseMessagesResponse_Malformed(t *testing.T) {
	_, err := parseMessagesResponse(json.RawMessage(`{not json`))
	assert.Error(t, err)
}

func TestParseRoomsResponse_HappyPath(t *testing.T) {
	body := json.RawMessage(`{
		"hits": {
			"total": {"value": 2},
			"hits": [
				{"_source": {
					"roomId": "r1", "roomName": "general", "roomType": "p",
					"userAccount": "alice", "siteId": "site-a",
					"joinedAt": "2026-04-01T12:00:00Z"
				}},
				{"_source": {
					"roomId": "r2", "roomName": "alice-bob", "roomType": "d",
					"userAccount": "alice", "siteId": "site-a",
					"joinedAt": "2026-04-02T12:00:00Z"
				}}
			]
		}
	}`)

	resp, err := parseRoomsResponse(body)
	require.NoError(t, err)
	assert.EqualValues(t, 2, resp.Total)
	require.Len(t, resp.Results, 2)

	assert.Equal(t, "r1", resp.Results[0].RoomID)
	assert.Equal(t, "p", resp.Results[0].RoomType)
	assert.Equal(t, "r2", resp.Results[1].RoomID)
	assert.Equal(t, "d", resp.Results[1].RoomType)
}

func TestParseRoomsResponse_Empty(t *testing.T) {
	body := json.RawMessage(`{"hits":{"total":{"value":0},"hits":[]}}`)
	resp, err := parseRoomsResponse(body)
	require.NoError(t, err)
	assert.EqualValues(t, 0, resp.Total)
	assert.Empty(t, resp.Results)
}

func TestParseRoomsResponse_Malformed(t *testing.T) {
	_, err := parseRoomsResponse(json.RawMessage(`{`))
	assert.Error(t, err)
}
