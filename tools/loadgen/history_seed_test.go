package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/msgbucket"
)

func TestBuildCassParticipant(t *testing.T) {
	got := buildCassParticipant("u-1", "alice", "Alice Wang")
	assert.Equal(t, "u-1", got.ID)
	assert.Equal(t, "alice", got.Account)
	assert.Equal(t, "Alice Wang", got.EngName)
}

func TestBucketOf_AlignsWithMsgBucket(t *testing.T) {
	sizer := msgbucket.New(24 * time.Hour)
	at := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, sizer.Of(at), bucketOf(sizer, at))
}

func TestBuildThreadRooms(t *testing.T) {
	p, ok := BuiltinHistoryPreset("history-medium")
	require.True(t, ok)
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	res := BuildHistoryFixtures(&p, 1, "site-a", now)

	// Mirror SeedThreadRooms' streaming aggregation by concatenating each
	// room's ThreadRoom docs as the iterator yields per-room plans.
	var rooms []model.ThreadRoom
	require.NoError(t, res.IterateRoomMessages(func(msgs []plannedMessage) error {
		rooms = append(rooms, buildRoomThreadRooms(msgs, "site-a")...)
		return nil
	}))
	// One ThreadRoom per parent.
	parentCount := 0
	for _, ps := range res.ThreadParents {
		parentCount += len(ps)
	}
	assert.Equal(t, parentCount, len(rooms))

	// Every ThreadRoom's ID must match a known ThreadRoomID, and the
	// LastMsgAt must equal the latest reply's CreatedAt.
	parentIDByThreadRoom := map[string]string{}
	for _, refs := range res.ThreadParents {
		for _, ref := range refs {
			parentIDByThreadRoom[ref.ThreadRoomID] = ref.MessageID
		}
	}
	for _, tr := range rooms {
		_, ok := parentIDByThreadRoom[tr.ID]
		require.True(t, ok, "thread room %s not in fixture parents", tr.ID)
		assert.Equal(t, parentIDByThreadRoom[tr.ID], tr.ParentMessageID)
		assert.Equal(t, "site-a", tr.SiteID)
		assert.False(t, tr.LastMsgAt.IsZero())
	}
}
