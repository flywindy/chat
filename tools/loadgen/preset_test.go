package main

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
)

func TestBuiltinPresets_ContainsAllFour(t *testing.T) {
	names := []string{"small", "medium", "large", "realistic"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p, ok := BuiltinPreset(name)
			require.True(t, ok, "preset %q must exist", name)
			assert.Equal(t, name, p.Name)
			assert.Greater(t, p.Users, 0)
			assert.Greater(t, p.Rooms, 0)
		})
	}
}

func TestBuiltinPresets_UnknownReturnsFalse(t *testing.T) {
	_, ok := BuiltinPreset("nonexistent")
	assert.False(t, ok)
}

func TestBuiltinPresets_UniformShape(t *testing.T) {
	for _, name := range []string{"small", "medium", "large"} {
		t.Run(name, func(t *testing.T) {
			p, ok := BuiltinPreset(name)
			require.True(t, ok)
			assert.Equal(t, DistUniform, p.RoomSizeDist)
			assert.Equal(t, DistUniform, p.SenderDist)
			assert.InDelta(t, 0.0, p.MentionRate, 1e-9)
			assert.InDelta(t, 0.0, p.ThreadRate, 1e-9)
		})
	}
}

func TestBuiltinPresets_RealisticShape(t *testing.T) {
	p, ok := BuiltinPreset("realistic")
	require.True(t, ok)
	assert.Equal(t, DistMixed, p.RoomSizeDist)
	assert.Equal(t, DistZipf, p.SenderDist)
	assert.Greater(t, p.MentionRate, 0.0)
	assert.Greater(t, p.ThreadRate, 0.0)
	assert.Greater(t, p.ContentBytes.Max, p.ContentBytes.Min)
}

func TestBuildFixtures_DeterministicAcrossCalls(t *testing.T) {
	p, _ := BuiltinPreset("small")
	a := BuildFixtures(&p, 42, "site-local")
	b := BuildFixtures(&p, 42, "site-local")
	assert.Equal(t, a.Users, b.Users)
	assert.Equal(t, a.Rooms, b.Rooms)
	assert.Equal(t, a.Subscriptions, b.Subscriptions)
	assert.Equal(t, a.RoomKeys, b.RoomKeys)
}

func TestBuildFixtures_RoomKeysOnePerRoom(t *testing.T) {
	p, _ := BuiltinPreset("small")
	f := BuildFixtures(&p, 42, "site-local")
	require.Len(t, f.RoomKeys, len(f.Rooms))
	for _, r := range f.Rooms {
		pair, ok := f.RoomKeys[r.ID]
		require.True(t, ok, "missing key for room %s", r.ID)
		assert.Len(t, pair.PrivateKey, 32)
	}
}

func TestBuildFixtures_RoomKeysDifferAcrossSeeds(t *testing.T) {
	p, _ := BuiltinPreset("small")
	a := BuildFixtures(&p, 42, "site-local")
	b := BuildFixtures(&p, 43, "site-local")
	require.Len(t, a.RoomKeys, len(a.Rooms))
	require.Equal(t, len(a.RoomKeys), len(b.RoomKeys))
	// At least one room should have a different key.
	differ := false
	for id, pa := range a.RoomKeys {
		pb := b.RoomKeys[id]
		if !bytes.Equal(pa.PrivateKey, pb.PrivateKey) {
			differ = true
			break
		}
	}
	assert.True(t, differ, "seeds 42 and 43 must produce different keys")
}

func TestBuildFixtures_SmallCountsAndShape(t *testing.T) {
	p, _ := BuiltinPreset("small")
	f := BuildFixtures(&p, 42, "site-local")
	assert.Len(t, f.Users, 10)
	assert.Len(t, f.Rooms, 5)
	// uniform: every user is in at least one room
	users := make(map[string]bool)
	for _, s := range f.Subscriptions {
		users[s.User.ID] = true
		assert.Equal(t, "site-local", s.SiteID)
	}
	assert.Len(t, users, 10)
	for _, r := range f.Rooms {
		assert.Equal(t, "channel", string(r.Type))
		assert.Equal(t, "site-local", r.SiteID)
	}
}

func TestBuildFixtures_RealisticMixesChannelAndDM(t *testing.T) {
	p, _ := BuiltinPreset("realistic")
	f := BuildFixtures(&p, 42, "site-local")
	var channels, dms int
	for _, r := range f.Rooms {
		switch r.Type { //nolint:exhaustive
		case "channel":
			channels++
		case "dm":
			dms++
		}
	}
	assert.Greater(t, channels, 0)
	assert.Greater(t, dms, 0)
	// DM rooms must have exactly 2 members
	dmMembers := make(map[string]int)
	for _, s := range f.Subscriptions {
		for _, r := range f.Rooms {
			if r.ID == s.RoomID && r.Type == "dm" {
				dmMembers[r.ID]++
			}
		}
	}
	for id, n := range dmMembers {
		assert.Equal(t, 2, n, "dm room %s must have 2 members", id)
	}
}

func TestBuildFixtures_FewerUsersThanRooms_PadsToTwoMembers(t *testing.T) {
	// Synthetic preset: 3 users, 5 rooms — round-robin alone leaves rooms 3
	// and 4 with fewer than 2 members, exercising the padding branch.
	p := &Preset{
		Name: "tiny", Users: 3, Rooms: 5,
		RoomSizeDist: DistUniform, SenderDist: DistUniform,
		ContentBytes: Range{Min: 200, Max: 200},
	}
	f := BuildFixtures(p, 42, "site-local")
	require.Len(t, f.Rooms, 5)
	for i := range f.Rooms {
		assert.GreaterOrEqual(t, f.Rooms[i].UserCount, 2,
			"room %s must have at least 2 members after padding", f.Rooms[i].ID)
	}
}

func TestSampleWithoutReplacement_CapsAtUserCount(t *testing.T) {
	// Requesting more samples than users available silently caps at len(users).
	r := rand.New(rand.NewSource(1))
	users := []model.User{{ID: "u-0"}, {ID: "u-1"}}
	out := sampleWithoutReplacement(r, users, 99)
	assert.Len(t, out, 2)
}
