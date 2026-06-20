package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/model"
)

// A cancelled client context must short-circuit the cross-site fan-out: each
// site RPC takes ~5s, so firing them for a request nobody awaits is pure waste.
// In-flight calls still fail fast via the ctx passed to GetRoomsInfo.
func TestEnrichCrossSite_ContextCancelled_SkipsRPC(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	subs := []model.Subscription{
		{ID: "b", RoomID: "r2", SiteID: "site-b"},
	}
	idxBySite := map[string][]int{"site-b": {0}}

	c := ctx("alice", "site-a")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	c.SetContext(cancelled)

	rooms.EXPECT().GetRoomsInfo(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	svc.enrichCrossSite(c, subs, idxBySite)

	assert.Nil(t, subs[0].Room, "cancelled fan-out leaves the sub without a room object")
}

// key32 builds a valid 32-byte room secret whose bytes are all b.
func key32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestEnrichWithRoomInfo_LocalAndCrossSite(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	seen := time.UnixMilli(100).UTC()
	localMsg := time.UnixMilli(150).UTC()
	newer := int64(200)
	subs := []model.Subscription{
		// LOCAL: enriched from the $lookup baseline (RoomName/UserCount/LastMsg*) + key.
		{ID: "a", RoomID: "r1", SiteID: "site-a", Name: "eng-sub", LastSeenAt: &seen,
			RoomName: "Eng", UserCount: 7, AppCount: 2, LastMsgAt: &localMsg, LastMsgID: "m-7",
			Alert: true, HasMention: true},
		// CROSS-SITE: enriched via the room-service RPC.
		{ID: "b", RoomID: "r2", SiteID: "site-b", LastSeenAt: &seen},
	}
	// LOCAL path: one key read for the local rooms; NO GetRoomsInfo for site-a.
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).
		Return([]model.RoomInfo{{RoomID: "r2", Found: true, Name: "Ops", UserCount: 3, LastMsgAt: &newer, LastMsgID: "m-3"}}, nil)

	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

	assert.Equal(t, "eng-sub", subs[0].Name, "subscription name must survive enrichment")
	assert.True(t, subs[0].Alert, "stored alert preserved")
	assert.True(t, subs[0].HasMention, "stored hasMention preserved")
	require.NotNil(t, subs[0].Room)
	assert.Equal(t, "Eng", subs[0].Room.Name, "local room name comes from the baseline")
	assert.Equal(t, 7, subs[0].Room.UserCount) // baseline value (no RPC for local)
	assert.Equal(t, 2, subs[0].Room.AppCount)
	assert.Equal(t, "m-7", subs[0].Room.LastMsgID)
	require.NotNil(t, subs[0].Room.LastMsgAt)
	assert.Equal(t, localMsg.UnixMilli(), *subs[0].Room.LastMsgAt)

	require.NotNil(t, subs[1].Room)
	assert.Equal(t, "Ops", subs[1].Room.Name)
	assert.False(t, subs[1].Alert, "no stored alert ⇒ stays false; never set from room data")
	assert.Equal(t, 3, subs[1].Room.UserCount) // cross-site sub gets room fields via RPC
	assert.Equal(t, "m-3", subs[1].Room.LastMsgID)
}

// TestEnrichWithRoomInfo_LocalKeyMaterial pins that a LOCAL sub whose room has a
// key gets base64 PrivateKey + KeyVersion from the $lookup baseline, with NO RPC.
func TestEnrichWithRoomInfo_LocalKeyMaterial(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	subs := []model.Subscription{
		// LOCAL sub carrying the room key in its $lookup baseline (current slot).
		{ID: "a", RoomID: "r1", SiteID: "site-a", RoomName: "Eng", UserCount: 5,
			RoomKeyPriv: key32(0xAB), RoomKeyVer: 4},
	}
	// No GetRoomsInfo expectation: an all-local input must never hit the RPC.

	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

	require.NotNil(t, subs[0].Room)
	assert.Equal(t, "Eng", subs[0].Room.Name)
	assert.Equal(t, 5, subs[0].Room.UserCount)
	require.NotNil(t, subs[0].Room.PrivateKey)
	// base64 of 32 bytes of 0xAB.
	assert.Equal(t, "q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s=", *subs[0].Room.PrivateKey)
	require.NotNil(t, subs[0].Room.KeyVersion)
	assert.Equal(t, 4, *subs[0].Room.KeyVersion)
}

// TestEnrichWithRoomInfo_LocalNoKey pins that a LOCAL sub whose room has no key
// still gets a baseline room object with no key material.
func TestEnrichWithRoomInfo_LocalNoKey(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	subs := []model.Subscription{
		{ID: "a", RoomID: "r1", SiteID: "site-a", RoomName: "Eng", UserCount: 5, LastMsgID: "m-base"},
	}

	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

	require.NotNil(t, subs[0].Room, "local room must still be built from the baseline")
	assert.Equal(t, "Eng", subs[0].Room.Name)
	assert.Equal(t, 5, subs[0].Room.UserCount)
	assert.Equal(t, "m-base", subs[0].Room.LastMsgID)
	assert.Nil(t, subs[0].Room.PrivateKey, "no key ⇒ no PrivateKey")
	assert.Nil(t, subs[0].Room.KeyVersion)
}

// TestEnrichWithRoomInfo_LocalInvalidKeyLength pins that a baseline key whose
// secret isn't 32 bytes is treated as absent — the local room object is still
// built, just with no key material.
func TestEnrichWithRoomInfo_LocalInvalidKeyLength(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	subs := []model.Subscription{
		{ID: "a", RoomID: "r1", SiteID: "site-a", RoomName: "Eng", UserCount: 5,
			RoomKeyPriv: []byte("short"), RoomKeyVer: 2},
	}

	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

	require.NotNil(t, subs[0].Room, "invalid-length key still yields a baseline room object")
	assert.Equal(t, "Eng", subs[0].Room.Name)
	assert.Equal(t, 5, subs[0].Room.UserCount)
	assert.Nil(t, subs[0].Room.PrivateKey)
	assert.Nil(t, subs[0].Room.KeyVersion)
}

// TestEnrichWithRoomInfo_AllRoomTypesKeyed pins the Option-A contract: a LOCAL
// sub of ANY room type whose room carries a 32-byte key gets base64 PrivateKey +
// KeyVersion from the $lookup baseline — keying is not gated by room type.
func TestEnrichWithRoomInfo_AllRoomTypesKeyed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roomType model.RoomType
	}{
		{"channel", model.RoomTypeChannel},
		{"dm", model.RoomTypeDM},
		{"botDM", model.RoomTypeBotDM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _, _, _ := newSvc(t)
			subs := []model.Subscription{
				{ID: "a", RoomID: "r1", SiteID: "site-a", RoomType: tc.roomType,
					RoomName: "room", RoomKeyPriv: key32(0xAB), RoomKeyVer: 4},
			}

			svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

			require.NotNil(t, subs[0].Room)
			require.NotNil(t, subs[0].Room.PrivateKey, "every room type returns its key")
			assert.Equal(t, "q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s=", *subs[0].Room.PrivateKey)
			require.NotNil(t, subs[0].Room.KeyVersion)
			assert.Equal(t, 4, *subs[0].Room.KeyVersion)
		})
	}
}

// TestEnrichWithRoomInfo_CrossSiteRPCZeroFields pins that a found cross-site
// room's RPC entry is authoritative for the room object even when fields are
// zero; the internal baseline stays on the flattened sub fields only.
func TestEnrichWithRoomInfo_CrossSiteRPCZeroFields(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	subs := []model.Subscription{{ID: "a", RoomID: "r2", SiteID: "site-b", UserCount: 5, LastMsgID: "m-base"}}
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).
		Return([]model.RoomInfo{{RoomID: "r2", Found: true, Name: "Ops"}}, nil)
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	require.NotNil(t, subs[0].Room)
	assert.Equal(t, "Ops", subs[0].Room.Name)
	assert.Equal(t, 5, subs[0].UserCount, "internal baseline untouched")
	assert.Equal(t, "m-base", subs[0].LastMsgID, "internal baseline untouched")
}

// TestEnrichWithRoomInfo_CrossSiteNotFoundNoRoom pins that a cross-site room the
// RPC reports as not-found yields NO room object — there is no local baseline to
// fall back to for a remote room.
func TestEnrichWithRoomInfo_CrossSiteNotFoundNoRoom(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	subs := []model.Subscription{{ID: "a", RoomID: "r2", SiteID: "site-b", UserCount: 5, LastMsgID: "m-base"}}
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).
		Return([]model.RoomInfo{{RoomID: "r2", Found: false}}, nil)
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	assert.Len(t, subs, 1)
	assert.False(t, subs[0].Alert)
	assert.Nil(t, subs[0].Room, "not-found cross-site room ⇒ no room object (no local baseline)")
}

// TestEnrichWithRoomInfo_LocalDeletedRoomNoRoom pins that a LOCAL sub whose room is
// soft-deleted (baseline name "Del-...") is kept but gets NO room object.
func TestEnrichWithRoomInfo_LocalDeletedRoomNoRoom(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	subs := []model.Subscription{
		{ID: "a", RoomID: "r1", SiteID: "site-a", Name: "team", RoomName: "Del-Team",
			UserCount: 5, RoomKeyPriv: key32(0xAB), RoomKeyVer: 1},
	}
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	assert.Nil(t, subs[0].Room, "soft-deleted local room ⇒ no room object")
	assert.Equal(t, "team", subs[0].Name, "the subscription itself is kept")
}

// TestEnrichWithRoomInfo_CrossSiteDeletedRoomNoRoom pins that a cross-site sub whose
// room-service entry is soft-deleted (name "Del-...") gets NO room object.
func TestEnrichWithRoomInfo_CrossSiteDeletedRoomNoRoom(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	subs := []model.Subscription{{ID: "a", RoomID: "r2", SiteID: "site-b"}}
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).
		Return([]model.RoomInfo{{RoomID: "r2", Found: true, Name: "Del-Ops"}}, nil)
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	assert.Nil(t, subs[0].Room, "soft-deleted cross-site room ⇒ no room object")
}

// TestEnrichWithRoomInfo_CrossSiteRPCFailDegradesSiteKeepsOthers pins per-site
// degradation: a failed site RPC leaves that site's subs without a room object,
// while sibling sites are still enriched. The local sub is built from the baseline.
func TestEnrichWithRoomInfo_CrossSiteRPCFailDegradesSiteKeepsOthers(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	seen := time.UnixMilli(100).UTC()
	newer := int64(200)
	subs := []model.Subscription{
		{ID: "loc", RoomID: "r1", SiteID: "site-a", LastSeenAt: &seen, RoomName: "Eng"},
		{ID: "b", RoomID: "r2", SiteID: "site-b", LastSeenAt: &seen, Alert: true},
		{ID: "c", RoomID: "r3", SiteID: "site-c", LastSeenAt: &seen},
	}
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).Return(nil, errors.New("down"))
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-c", []string{"r3"}).
		Return([]model.RoomInfo{{RoomID: "r3", Found: true, Name: "Ops", LastMsgAt: &newer}}, nil)

	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)

	require.NotNil(t, subs[0].Room, "local sub built from baseline")
	assert.Equal(t, "Eng", subs[0].Room.Name)
	assert.Nil(t, subs[1].Room, "degraded cross-site sub gets no room object")
	assert.True(t, subs[1].Alert, "stored alert preserved")
	require.NotNil(t, subs[2].Room)
	assert.Equal(t, "Ops", subs[2].Room.Name) // site-c still enriched
}

func TestEnrichWithRoomInfo_Empty(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	// No GetRoomsInfo / GetMany expectations: empty input must short-circuit before any call.
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), nil)
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), []model.Subscription{})
}

// TestEnrichWithRoomInfo_LocalNeverRecomputesFlags pins that local enrichment
// leaves alert/hasMention alone — they are stored subscription state, never
// derived from room timestamps.
func TestEnrichWithRoomInfo_LocalNeverRecomputesFlags(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	seen := time.UnixMilli(100).UTC()
	newer := time.UnixMilli(999).UTC()
	mentionAt := time.UnixMilli(999).UTC()
	subs := []model.Subscription{
		{ID: "a", RoomID: "r1", SiteID: "site-a", LastSeenAt: &seen, RoomName: "Eng",
			LastMsgAt: &newer, LastMentionAllAt: &mentionAt, Alert: false, HasMention: false},
	}
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	assert.False(t, subs[0].Alert, "room lastMsgAt newer than lastSeen must NOT flip stored alert")
	assert.False(t, subs[0].HasMention, "room lastMentionAllAt newer than lastSeen must NOT flip stored hasMention")
}

func TestUnread(t *testing.T) {
	seen := time.UnixMilli(100).UTC()
	older := int64(50)
	newer := int64(200)
	cases := []struct {
		name     string
		lastSeen *time.Time
		ms       *int64
		want     bool
	}{
		{"nil ms is never unread", &seen, nil, false},
		{"nil lastSeen with msg is unread", nil, &newer, true},
		{"msg newer than lastSeen is unread", &seen, &newer, true},
		{"msg older than lastSeen is read", &seen, &older, false},
		{"msg equal to lastSeen is read", &seen, ptrInt64(seen.UTC().UnixMilli()), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unread(tc.lastSeen, tc.ms))
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

// TestEnrichWithRoomInfo_LocalDedupsRoomIDs pins that two local subs on the same
// room cause a single deduped key read and both receive the baseline room.
func TestEnrichWithRoomInfo_LocalDedupsRoomIDs(t *testing.T) {
	svc, _, _, _, _, _ := newSvc(t)
	seen := time.UnixMilli(100).UTC()
	subs := []model.Subscription{
		{ID: "a", RoomID: "r1", SiteID: "site-a", LastSeenAt: &seen, RoomName: "Eng"},
		{ID: "b", RoomID: "r1", SiteID: "site-a", LastSeenAt: &seen, RoomName: "Eng"}, // same room, second sub
	}
	// EXPECT exactly ["r1"], not ["r1","r1"] — gomock fails the call on arg mismatch.
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	require.NotNil(t, subs[0].Room)
	require.NotNil(t, subs[1].Room)
	assert.Equal(t, "Eng", subs[0].Room.Name)
	assert.Equal(t, "Eng", subs[1].Room.Name) // both subs enriched from the single baseline
}

// TestEnrichWithRoomInfo_CrossSiteDedupsRoomIDs pins the cross-site RPC also dedups.
func TestEnrichWithRoomInfo_CrossSiteDedupsRoomIDs(t *testing.T) {
	svc, _, _, _, rooms, _ := newSvc(t)
	seen := time.UnixMilli(100).UTC()
	newer := int64(200)
	subs := []model.Subscription{
		{ID: "a", RoomID: "r2", SiteID: "site-b", LastSeenAt: &seen},
		{ID: "b", RoomID: "r2", SiteID: "site-b", LastSeenAt: &seen}, // same room, second sub
	}
	rooms.EXPECT().GetRoomsInfo(gomock.Any(), "site-b", []string{"r2"}).
		Return([]model.RoomInfo{{RoomID: "r2", Found: true, Name: "Ops", LastMsgAt: &newer}}, nil)
	svc.enrichWithRoomInfo(ctx("alice", "site-a"), subs)
	require.NotNil(t, subs[0].Room)
	require.NotNil(t, subs[1].Room)
	assert.Equal(t, "Ops", subs[0].Room.Name)
	assert.Equal(t, "Ops", subs[1].Room.Name)
}
