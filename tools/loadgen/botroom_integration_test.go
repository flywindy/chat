//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

func TestBotRoom_SeedShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := testutil.MongoDB(t, "botroom")

	p, _ := BuiltinBotRoomPreset("botroom-small")
	fixtures, layout := BuildBotRoomFixtures(&p, 1, "site-local")
	require.NoError(t, Seed(ctx, db, &fixtures))

	// Each size-50 room must have exactly 50 subscriptions in Mongo.
	count, err := db.Collection("subscriptions").CountDocuments(ctx,
		map[string]any{"roomId": layout.RoomsBySize[50][0]})
	require.NoError(t, err)
	assert.Equal(t, int64(50), count)

	// The bot must be the owner of that room.
	ownerCount, err := db.Collection("subscriptions").CountDocuments(ctx,
		map[string]any{"roomId": layout.RoomsBySize[50][0], "roles": "owner", "u.account": layout.BotAccount})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ownerCount)
}
