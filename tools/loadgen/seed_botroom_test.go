package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
)

func TestBuildBotRoomFixtures_Deterministic(t *testing.T) {
	p, ok := BuiltinBotRoomPreset("botroom-small")
	require.True(t, ok)

	f1, l1 := BuildBotRoomFixtures(&p, 42, "site-local")
	f2, l2 := BuildBotRoomFixtures(&p, 42, "site-local")

	assert.Equal(t, f1.Rooms, f2.Rooms)
	assert.Equal(t, f1.Subscriptions, f2.Subscriptions)
	assert.Equal(t, f1.Users, f2.Users)
	assert.Equal(t, f1.RoomKeys, f2.RoomKeys)
	assert.Equal(t, l1.RoomsBySize, l2.RoomsBySize)
}

func TestBuiltinBotRoomPreset_Unknown(t *testing.T) {
	_, ok := BuiltinBotRoomPreset("nope")
	assert.False(t, ok)
}

func TestBotRoomLayout_RoomIDs(t *testing.T) {
	p, _ := BuiltinBotRoomPreset("botroom-small")
	f, layout := BuildBotRoomFixtures(&p, 1, "site-local")

	all := botRoomRoomIDs(&layout)
	assert.Len(t, all, len(p.Sizes)*p.RoomsPerSize)
	assert.Len(t, f.Rooms, len(all))
}

func TestBuildBotRoomFixtures_Shape(t *testing.T) {
	p, ok := BuiltinBotRoomPreset("botroom-small")
	require.True(t, ok)

	f, layout := BuildBotRoomFixtures(&p, 7, "site-local")

	// One set of RoomsPerSize rooms per size.
	for _, size := range p.Sizes {
		rooms := layout.RoomsBySize[size]
		require.Len(t, rooms, p.RoomsPerSize, "size %d", size)
	}

	// Each room has exactly `size` subscriptions, the first being the bot owner.
	subsByRoom := map[string][]model.Subscription{}
	for _, s := range f.Subscriptions {
		subsByRoom[s.RoomID] = append(subsByRoom[s.RoomID], s)
	}
	for _, size := range p.Sizes {
		for _, rid := range layout.RoomsBySize[size] {
			subs := subsByRoom[rid]
			require.Len(t, subs, size, "room %s", rid)
			assert.Equal(t, layout.BotAccount, subs[0].User.Account, "room %s first sub must be the bot", rid)
			ownerCount := 0
			botIsOwner := false
			for _, s := range subs {
				for _, r := range s.Roles {
					if r == model.RoleOwner {
						ownerCount++
						if s.User.Account == layout.BotAccount {
							botIsOwner = true
						}
					}
				}
			}
			assert.Equal(t, 1, ownerCount, "room %s owner count", rid)
			assert.True(t, botIsOwner, "room %s bot must be owner", rid)
		}
	}

	// Every room is a channel with UserCount == size and a room key.
	roomByID := map[string]model.Room{}
	for _, r := range f.Rooms {
		roomByID[r.ID] = r
	}
	for _, size := range p.Sizes {
		for _, rid := range layout.RoomsBySize[size] {
			assert.Equal(t, model.RoomTypeChannel, roomByID[rid].Type)
			assert.Equal(t, size, roomByID[rid].UserCount)
			_, hasKey := f.RoomKeys[rid]
			assert.True(t, hasKey, "room %s must have a key", rid)
		}
	}
}
