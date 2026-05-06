package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/subject"
)

type publishedMsg struct {
	subj string
	data []byte
}

func TestHandler_ProcessRoleUpdate_Promote(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").
		Return(&model.Subscription{ID: "s1", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1", SiteID: "site-a", Roles: []model.Role{model.RoleMember, model.RoleOwner}}, nil)
	store.EXPECT().GetUser(gomock.Any(), "bob").
		Return(&model.User{ID: "u2", Account: "bob", SiteID: "site-a"}, nil)

	var published []publishedMsg
	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	data, _ := json.Marshal(req)
	if err := h.processRoleUpdate(context.Background(), data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(published))
	}
	if published[0].subj != "chat.user.bob.event.subscription.update" {
		t.Errorf("subject = %q, want subscription update for bob", published[0].subj)
	}

	var evt model.SubscriptionUpdateEvent
	if err := json.Unmarshal(published[0].data, &evt); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if evt.Action != "role_updated" {
		t.Errorf("action = %q, want role_updated", evt.Action)
	}
	if evt.UserID != "u2" {
		t.Errorf("userID = %q, want u2", evt.UserID)
	}
	if !slices.Contains(evt.Subscription.Roles, model.RoleOwner) {
		t.Errorf("subscription roles = %v, want to contain owner", evt.Subscription.Roles)
	}
	if !slices.Contains(evt.Subscription.Roles, model.RoleMember) {
		t.Errorf("subscription roles = %v, want to contain member", evt.Subscription.Roles)
	}
	if evt.Timestamp <= 0 {
		t.Error("expected Timestamp > 0")
	}
}

func TestHandler_ProcessRoleUpdate_Demote(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleMember).Return(nil)
	store.EXPECT().RemoveRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").
		Return(&model.Subscription{ID: "s1", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1", SiteID: "site-a", Roles: []model.Role{model.RoleMember}}, nil)
	store.EXPECT().GetUser(gomock.Any(), "bob").
		Return(&model.User{ID: "u2", Account: "bob", SiteID: "site-a"}, nil)

	var published []publishedMsg
	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleMember, Timestamp: 1}
	data, _ := json.Marshal(req)
	if err := h.processRoleUpdate(context.Background(), data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(published))
	}

	var evt model.SubscriptionUpdateEvent
	if err := json.Unmarshal(published[0].data, &evt); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if evt.Action != "role_updated" {
		t.Errorf("action = %q, want role_updated", evt.Action)
	}
	if slices.Contains(evt.Subscription.Roles, model.RoleOwner) {
		t.Errorf("subscription roles = %v, should not contain owner after demote", evt.Subscription.Roles)
	}
	if !slices.Contains(evt.Subscription.Roles, model.RoleMember) {
		t.Errorf("subscription roles = %v, want to contain member", evt.Subscription.Roles)
	}
}

func TestHandler_ProcessRoleUpdate_CrossSite(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").
		Return(&model.Subscription{ID: "s1", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1", SiteID: "site-a", Roles: []model.Role{model.RoleMember, model.RoleOwner}}, nil)
	// User is on site-b (different from handler's site-a) → cross-site
	store.EXPECT().GetUser(gomock.Any(), "bob").
		Return(&model.User{ID: "u2", Account: "bob", SiteID: "site-b"}, nil)

	var published []publishedMsg
	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	data, _ := json.Marshal(req)
	if err := h.processRoleUpdate(context.Background(), data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(published) != 2 {
		t.Fatalf("expected 2 publishes, got %d", len(published))
	}
	if published[0].subj != "chat.user.bob.event.subscription.update" {
		t.Errorf("first subject = %q, want subscription update", published[0].subj)
	}

	wantOutboxSubj := "outbox.site-a.to.site-b.role_updated"
	if published[1].subj != wantOutboxSubj {
		t.Errorf("second subject = %q, want %q", published[1].subj, wantOutboxSubj)
	}

	var outbox model.OutboxEvent
	if err := json.Unmarshal(published[1].data, &outbox); err != nil {
		t.Fatalf("unmarshal outbox event: %v", err)
	}
	if outbox.Type != "role_updated" {
		t.Errorf("outbox type = %q, want role_updated", outbox.Type)
	}

	var innerEvt model.SubscriptionUpdateEvent
	if err := json.Unmarshal(outbox.Payload, &innerEvt); err != nil {
		t.Fatalf("unmarshal inner event: %v", err)
	}
	if !slices.Contains(innerEvt.Subscription.Roles, model.RoleOwner) {
		t.Errorf("inner subscription roles = %v, want to contain owner", innerEvt.Subscription.Roles)
	}
	if !slices.Contains(innerEvt.Subscription.Roles, model.RoleMember) {
		t.Errorf("inner subscription roles = %v, want to contain member", innerEvt.Subscription.Roles)
	}
}

// --- Error-path tests for processRoleUpdate ---

func TestHandler_ProcessRoleUpdate_FallsBackToNowOnInvalidTimestamp(t *testing.T) {
	// A missing timestamp should not short-circuit the handler. We confirm
	// processing reached the store layer by stubbing the first store call to
	// return a downstream error and asserting the error is NOT the timestamp
	// rejection.
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(fmt.Errorf("db error"))
	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error {
		return nil
	})
	req := model.UpdateRoleRequest{
		RoomID:    "r1",
		Account:   "bob",
		NewRole:   model.RoleOwner,
		Timestamp: 0,
	}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timestamp must be > 0")
	assert.Contains(t, err.Error(), "add owner role")
}

func TestHandler_ProcessRoleUpdate_InvalidJSON(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		t.Fatal("publish should not be called")
		return nil
	}}

	err := h.processRoleUpdate(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestHandler_ProcessRoleUpdate_AddRoleError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(fmt.Errorf("db error"))

	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		t.Fatal("publish should not be called")
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	if err == nil {
		t.Fatal("expected error for AddRole failure")
	}
}

func TestHandler_ProcessRoleUpdate_RemoveRoleError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleMember).Return(nil)
	store.EXPECT().RemoveRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(fmt.Errorf("db error"))

	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		t.Fatal("publish should not be called")
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleMember, Timestamp: 1}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	if err == nil {
		t.Fatal("expected error for RemoveRole failure")
	}
}

func TestHandler_ProcessRoleUpdate_GetSubscriptionError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").Return(nil, fmt.Errorf("db error"))

	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		t.Fatal("publish should not be called")
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	if err == nil {
		t.Fatal("expected error for GetSubscription failure")
	}
}

func TestHandler_ProcessRoleUpdate_PublishError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").
		Return(&model.Subscription{ID: "s1", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1", SiteID: "site-a", Roles: []model.Role{model.RoleMember, model.RoleOwner}}, nil)

	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		return fmt.Errorf("nats down")
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	if err == nil {
		t.Fatal("expected error for publish failure")
	}
}

func TestHandler_ProcessRoleUpdate_UnsupportedRole(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	h := &Handler{store: store, siteID: "site-a", publish: func(_ context.Context, _ string, _ []byte, _ string) error {
		t.Fatal("publish should not be called")
		return nil
	}}

	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: "admin", Timestamp: 1}
	data, _ := json.Marshal(req)
	err := h.processRoleUpdate(context.Background(), data)
	if err == nil {
		t.Fatal("expected error for unsupported role")
	}
}

func TestHandler_ProcessRoleUpdate_PropagatesRequestID(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	store.EXPECT().AddRole(gomock.Any(), "bob", "r1", model.RoleOwner).Return(nil)
	store.EXPECT().GetSubscription(gomock.Any(), "bob", "r1").
		Return(&model.Subscription{ID: "s1", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1", SiteID: "site-a", Roles: []model.Role{model.RoleMember, model.RoleOwner}}, nil)
	store.EXPECT().GetUser(gomock.Any(), "bob").
		Return(&model.User{ID: "u2", Account: "bob", SiteID: "site-a"}, nil)

	var capturedCtx context.Context
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		capturedCtx = ctx
		return nil
	}
	h := NewHandler(store, "site1", publish)

	ctx := natsutil.WithRequestID(context.Background(), "req-rw-test")
	req := model.UpdateRoleRequest{RoomID: "r1", Account: "bob", NewRole: model.RoleOwner, Timestamp: 1}
	reqData, _ := json.Marshal(req)
	err := h.processRoleUpdate(ctx, reqData)
	require.NoError(t, err)
	require.NotNil(t, capturedCtx, "publish wrapper must receive a non-nil ctx")
	assert.Equal(t, "req-rw-test", natsutil.RequestIDFromContext(capturedCtx),
		"publish wrapper must receive ctx that still carries the request ID")
}

// --- processRemoveMember tests ---

func TestHandler_ProcessRemoveMember_FallsBackToNowOnInvalidTimestamp(t *testing.T) {
	// A missing timestamp should not short-circuit the handler. We confirm
	// processing reached the store layer by stubbing the first store call to
	// return a downstream error and asserting the error is NOT the timestamp
	// rejection.
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().GetUserWithMembership(gomock.Any(), "r1", "alice").Return(nil, fmt.Errorf("db error"))
	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error {
		return nil
	})
	req := model.RemoveMemberRequest{
		RoomID:    "r1",
		Account:   "alice",
		Requester: "alice",
		Timestamp: 0,
	}
	data, _ := json.Marshal(req)
	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timestamp must be > 0")
}

func TestHandler_ProcessRemoveMember_SelfLeave_IndividualOnly(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID  = "room-1"
		account = "alice"
		siteID  = "site-a"
	)

	userResult := &UserWithMembership{
		User: model.User{
			ID:          "u1",
			Account:     account,
			SiteID:      siteID,
			EngName:     "Alice",
			ChineseName: "愛麗絲",
		},
		HasOrgMembership: false,
	}

	store.EXPECT().
		GetUserWithMembership(gomock.Any(), roomID, account).
		Return(userResult, nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), roomID, account).
		Return(int64(1), nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)

	var published []publishedMsg
	h := NewHandler(store, siteID, func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	})

	// Self-leave: Requester == Account
	req := model.RemoveMemberRequest{RoomID: roomID, Requester: account, Account: account, Timestamp: 1}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.NoError(t, err)

	// Expect: subscription update + member change event + system message = 3 publishes
	assert.Len(t, published, 3, "expected 3 publishes: sub update, member event, sys msg")

	subjSet := make(map[string]bool)
	for _, p := range published {
		subjSet[p.subj] = true
	}

	assert.True(t, subjSet[subject.SubscriptionUpdate(account)], "expected subscription update published")
	assert.True(t, subjSet[subject.MemberEvent(roomID)], "expected member event published")

	for _, p := range published {
		if p.subj != subject.SubscriptionUpdate(account) {
			continue
		}
		var evt model.SubscriptionUpdateEvent
		require.NoError(t, json.Unmarshal(p.data, &evt))
		assert.Equal(t, model.RoomTypeChannel, evt.Subscription.RoomType, "subscription update should carry RoomType")
	}

	// Verify timestamps on all events
	for _, p := range published {
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(p.data, &raw))
		tsRaw, ok := raw["timestamp"]
		if !ok {
			continue // sys msg may not have timestamp at top level
		}
		var ts int64
		require.NoError(t, json.Unmarshal(tsRaw, &ts))
		assert.NotZero(t, ts, "timestamp should be non-zero for subject %s", p.subj)
	}
}

func TestHandler_ProcessRemoveMember_SelfLeave_DualMembership(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID  = "room-1"
		account = "alice"
		siteID  = "site-a"
	)

	userResult := &UserWithMembership{
		User: model.User{
			ID:      "u1",
			Account: account,
			SiteID:  siteID,
		},
		HasOrgMembership: true,
		Roles:            []model.Role{model.RoleMember},
	}

	// Only DeleteRoomMember(individual) called — no subscription delete, no events,
	// no role change (target is not an owner).
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), roomID, account).
		Return(userResult, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u1").
		Return(nil)

	var published []publishedMsg
	h := NewHandler(store, siteID, func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	})

	req := model.RemoveMemberRequest{RoomID: roomID, Requester: account, Account: account, Timestamp: 1}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.NoError(t, err)

	assert.Empty(t, published, "expected no publishes for dual-membership self-leave")
}

func TestHandler_ProcessRemoveMember_DualMembership_OwnerDemoted(t *testing.T) {
	// Dual-member who also holds the owner role must be demoted when their
	// individual source is removed — org members cannot be owners.
	cases := []struct {
		name      string
		requester string
	}{
		{"self-leave", "alice"},
		{"owner-removes-other", "bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockSubscriptionStore(ctrl)

			const (
				roomID  = "room-1"
				account = "alice"
				siteID  = "site-a"
			)

			userResult := &UserWithMembership{
				User:             model.User{ID: "u1", Account: account, SiteID: siteID},
				HasOrgMembership: true,
				Roles:            []model.Role{model.RoleOwner, model.RoleMember},
			}

			gomock.InOrder(
				store.EXPECT().
					GetUserWithMembership(gomock.Any(), roomID, account).
					Return(userResult, nil),
				store.EXPECT().
					DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u1").
					Return(nil),
				store.EXPECT().
					RemoveRole(gomock.Any(), account, roomID, model.RoleOwner).
					Return(nil),
			)

			var published []publishedMsg
			h := NewHandler(store, siteID, func(_ context.Context, subj string, data []byte, _ string) error {
				published = append(published, publishedMsg{subj: subj, data: data})
				return nil
			})

			req := model.RemoveMemberRequest{RoomID: roomID, Requester: tc.requester, Account: account, Timestamp: 1}
			data, _ := json.Marshal(req)

			err := h.processRemoveMember(context.Background(), data)
			require.NoError(t, err)
			assert.Empty(t, published, "dual-membership removal emits no events")
		})
	}
}

func TestHandler_ProcessRemoveMember_OwnerRemovesIndividual(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID    = "room-1"
		account   = "bob"
		requester = "alice"
		siteID    = "site-a"
	)

	userResult := &UserWithMembership{
		User: model.User{
			ID:          "u2",
			Account:     account,
			SiteID:      siteID,
			EngName:     "Bob",
			ChineseName: "鮑伯",
		},
		HasOrgMembership: false,
	}

	store.EXPECT().
		GetUserWithMembership(gomock.Any(), roomID, account).
		Return(userResult, nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), roomID, account).
		Return(int64(1), nil)
	// Owner-removes uses the same single-entry delete as self-leave since the
	// dual-membership branch is the only case that needs separate handling.
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u2").
		Return(nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)

	var published []publishedMsg
	h := NewHandler(store, siteID, func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	})

	// requester != account means this is owner-removes-other
	req := model.RemoveMemberRequest{RoomID: roomID, Requester: requester, Account: account, Timestamp: 1}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.NoError(t, err)

	assert.Len(t, published, 3, "expected 3 publishes: sub update, member event, sys msg")

	// Verify the sys msg has type "member_removed"
	for _, p := range published {
		if p.subj == subject.MemberEvent(roomID) {
			var evt model.MemberRemoveEvent
			require.NoError(t, json.Unmarshal(p.data, &evt))
			assert.Equal(t, "member_removed", evt.Type)
			assert.Contains(t, evt.Accounts, account)
		}
	}
}

// --- processAddMembers tests ---

func TestHandler_ProcessAddMembers_FallsBackToNowOnInvalidTimestamp(t *testing.T) {
	// A missing timestamp should not short-circuit the handler. We confirm
	// processing reached the store layer by stubbing the first store call to
	// return a downstream error and asserting the error is NOT the timestamp
	// rejection.
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(nil, fmt.Errorf("db error"))
	h := NewHandler(store, "site1", func(_ context.Context, _ string, _ []byte, _ string) error {
		return nil
	})
	req := model.AddMembersRequest{
		RoomID:           "r1",
		RequesterAccount: "alice",
		Users:            []string{"bob"},
		Timestamp:        0,
	}
	data, _ := json.Marshal(req)
	ctx := natsutil.WithRequestID(context.Background(), "req-fallback-ts-test")
	err := h.processAddMembers(ctx, data)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timestamp must be > 0")
}

func TestHandler_ProcessAddMembers(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob", "charlie"}, "r1").
		Return([]string{"bob", "charlie"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "charlie"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
		{ID: "u3", Account: "charlie", SiteID: "site-b"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, subs []*model.Subscription) error {
			assert.Len(t, subs, 2)
			for _, s := range subs {
				assert.Equal(t, "site-a", s.SiteID)
				assert.Equal(t, model.RoomTypeChannel, s.RoomType)
				assert.Equal(t, []model.Role{model.RoleMember}, s.Roles)
				require.NotNil(t, s.HistorySharedSince)
				assert.Equal(t, s.JoinedAt, *s.HistorySharedSince)
			}
			return nil
		})
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	req := model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob", "charlie"},
		History:   model.HistoryConfig{Mode: model.HistoryModeNone},
		Timestamp: 1,
	}
	reqData, _ := json.Marshal(req)

	ctx := natsutil.WithRequestID(context.Background(), "req-add-members-basic")
	err := h.processAddMembers(ctx, reqData)
	require.NoError(t, err)

	// 2 SubscriptionUpdate + 1 MemberAddEvent + 1 system msg + 1 batched outbox (site-b)
	assert.GreaterOrEqual(t, len(published), 4)

	// Verify exactly 1 outbox event for site-b (batched, not per-member)
	var outboxCount int
	for _, p := range published {
		if strings.Contains(p.subj, "outbox") {
			outboxCount++
			assert.Contains(t, p.subj, "site-b")
			var outboxEvt model.OutboxEvent
			require.NoError(t, json.Unmarshal(p.data, &outboxEvt))
			var change model.MemberAddEvent
			require.NoError(t, json.Unmarshal(outboxEvt.Payload, &change))
			assert.Equal(t, []string{"charlie"}, change.Accounts)
		}
	}
	assert.Equal(t, 1, outboxCount, "should publish exactly 1 batched outbox event per destination site")
}

func TestHandler_ProcessAddMembers_HistoryAll(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	publish := func(_ context.Context, _ string, _ []byte, _ string) error { return nil }
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob"}, "r1").
		Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, subs []*model.Subscription) error {
			assert.Len(t, subs, 1)
			assert.Nil(t, subs[0].HistorySharedSince, "HistorySharedSince should be nil for mode all")
			return nil
		})
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	req := model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		History:   model.HistoryConfig{Mode: model.HistoryModeAll},
		Timestamp: 1,
	}
	reqData, _ := json.Marshal(req)

	ctx := natsutil.WithRequestID(context.Background(), "req-history-all-test")
	err := h.processAddMembers(ctx, reqData)
	require.NoError(t, err)
}

// findMemberAddEvent returns the decoded MemberAddEvent published locally on
// RoomMemberEvent(roomID). Fails the test if no such publish occurred.
func findMemberAddEvent(t *testing.T, published []publishedMsg, roomID string) (model.MemberAddEvent, []byte) {
	t.Helper()
	want := subject.RoomMemberEvent(roomID)
	for _, p := range published {
		if p.subj != want {
			continue
		}
		var evt model.MemberAddEvent
		require.NoError(t, json.Unmarshal(p.data, &evt))
		return evt, p.data
	}
	t.Fatalf("no MemberAddEvent published to %s (got %d messages)", want, len(published))
	return model.MemberAddEvent{}, nil
}

// TestHandler_ProcessAddMembers_RestrictedPropagatesPointer verifies that a
// restricted room (HistoryModeNone) emits a MemberAddEvent whose
// HistorySharedSince is a non-nil pointer equal to the request timestamp,
// both for the same-site RoomMemberEvent publish and for the batched
// cross-site outbox event.
func TestHandler_ProcessAddMembers_RestrictedPropagatesPointer(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob", "charlie"}, "r1").
		Return([]string{"bob", "charlie"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "charlie"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
		{ID: "u3", Account: "charlie", SiteID: "site-b"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	const reqTS int64 = 1744300000000
	req := model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob", "charlie"},
		History:   model.HistoryConfig{Mode: model.HistoryModeNone},
		Timestamp: reqTS,
	}
	reqData, _ := json.Marshal(req)
	ctxR := natsutil.WithRequestID(context.Background(), "req-restricted-propagates")
	require.NoError(t, h.processAddMembers(ctxR, reqData))

	// Local RoomMemberEvent: HSS must be a non-nil pointer equal to request ts.
	memberAddEvt, _ := findMemberAddEvent(t, published, "r1")
	require.NotNil(t, memberAddEvt.HistorySharedSince,
		"restricted room must publish non-nil HistorySharedSince")
	assert.Equal(t, reqTS, *memberAddEvt.HistorySharedSince)

	// Batched outbox to site-b: same HSS pointer on the payload.
	var foundOutbox bool
	for _, p := range published {
		if !strings.Contains(p.subj, "outbox") {
			continue
		}
		foundOutbox = true
		var outboxEvt model.OutboxEvent
		require.NoError(t, json.Unmarshal(p.data, &outboxEvt))
		var change model.MemberAddEvent
		require.NoError(t, json.Unmarshal(outboxEvt.Payload, &change))
		require.NotNil(t, change.HistorySharedSince,
			"outbox restricted payload must carry HistorySharedSince")
		assert.Equal(t, reqTS, *change.HistorySharedSince)
	}
	assert.True(t, foundOutbox, "expected a batched outbox publish for site-b")
}

// TestHandler_ProcessAddMembers_UnrestrictedOmitsFieldFromWire verifies that
// an unrestricted room (HistoryModeAll) produces a MemberAddEvent whose JSON
// wire form does NOT contain the "historySharedSince" key. This is the
// documented publisher contract.
func TestHandler_ProcessAddMembers_UnrestrictedOmitsFieldFromWire(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob"}, "r1").
		Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	req := model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		History:   model.HistoryConfig{Mode: model.HistoryModeAll},
		Timestamp: 1,
	}
	reqData, _ := json.Marshal(req)
	ctxU := natsutil.WithRequestID(context.Background(), "req-unrestricted-wire")
	require.NoError(t, h.processAddMembers(ctxU, reqData))

	evt, raw := findMemberAddEvent(t, published, "r1")
	assert.Nil(t, evt.HistorySharedSince, "unrestricted event must decode HSS as nil")

	var rawMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &rawMap))
	_, present := rawMap["historySharedSince"]
	assert.False(t, present, "unrestricted event must omit historySharedSince on the wire")
}

func TestHandler_ProcessAddMembers_WithOrgs(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	publish := func(_ context.Context, _ string, _ []byte, _ string) error { return nil }
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), []string{"eng"}, []string{"bob"}, "r1").
		Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	// With orgs: BulkCreateRoomMembers called once with individual "bob" + org "eng" + backfill "alice"
	store.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, members []*model.RoomMember) error {
			assert.Len(t, members, 3)
			return nil
		})
	// Backfill: GetSubscriptionAccounts returns existing "alice" + new "bob"
	store.EXPECT().GetSubscriptionAccounts(gomock.Any(), "r1").Return([]string{"alice", "bob"}, nil)
	// Backfill: FindUsersByAccounts for existing accounts that aren't new
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"alice"}).Return([]model.User{
		{ID: "u1", Account: "alice", SiteID: "site-a"},
	}, nil)

	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		Orgs:             []string{"eng"},
		RequesterAccount: "alice",
		Timestamp:        1000,
		History:          model.HistoryConfig{Mode: model.HistoryModeAll},
	}
	reqData, _ := json.Marshal(req)

	ctxOrgs := natsutil.WithRequestID(context.Background(), "req-with-orgs-test")
	err := h.processAddMembers(ctxOrgs, reqData)
	require.NoError(t, err)
}

// New permanent-error contract: when ListNewMembers resolves a candidate
// account that's no longer present in the users collection, processAddMembers
// must NOT silently materialize a smaller membership. It returns errPermanent
// so JetStream Acks (no infinite redelivery) and the requester sees an
// async-job error event naming the missing account.
func TestHandler_ProcessAddMembers_UserNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	publish := func(_ context.Context, _ string, _ []byte, _ string) error { return nil }
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob", "ghost"}, "r1").
		Return([]string{"bob", "ghost"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "ghost"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
	}, nil)
	// BulkCreateSubscriptions / ReconcileMemberCounts / HasOrgRoomMembers
	// MUST NOT be called once a missing account is detected.

	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob", "ghost"},
		RequesterAccount: "alice",
		Timestamp:        1000,
		History:          model.HistoryConfig{Mode: model.HistoryModeAll},
	}
	reqData, _ := json.Marshal(req)

	ctxUNF := natsutil.WithRequestID(context.Background(), "req-user-not-found-test")
	err := h.processAddMembers(ctxUNF, reqData)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPermanent)
	assert.Contains(t, err.Error(), "ghost")
	assert.Contains(t, err.Error(), "r1")
}

func TestHandler_ProcessAddMembers_MultipleSiteOutbox(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"alice", "bob", "charlie"}, "r1").
		Return([]string{"alice", "bob", "charlie"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"alice", "bob", "charlie"}).Return([]model.User{
		{ID: "u1", Account: "alice", SiteID: "site-b"},
		{ID: "u2", Account: "bob", SiteID: "site-b"},
		{ID: "u3", Account: "charlie", SiteID: "site-c"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"alice", "bob", "charlie"},
		RequesterAccount: "alice",
		Timestamp:        1000,
		History:          model.HistoryConfig{Mode: model.HistoryModeAll},
	}
	reqData, _ := json.Marshal(req)

	ctxMS := natsutil.WithRequestID(context.Background(), "req-multi-site-outbox-test")
	err := h.processAddMembers(ctxMS, reqData)
	require.NoError(t, err)

	var outboxEvents []publishedMsg
	for _, p := range published {
		if strings.HasPrefix(p.subj, "outbox.") {
			outboxEvents = append(outboxEvents, p)
		}
	}
	assert.Len(t, outboxEvents, 2, "should batch outbox by site: 1 for site-b, 1 for site-c")

	for _, p := range outboxEvents {
		var outboxEvt model.OutboxEvent
		require.NoError(t, json.Unmarshal(p.data, &outboxEvt))
		var change model.MemberAddEvent
		require.NoError(t, json.Unmarshal(outboxEvt.Payload, &change))

		if strings.Contains(p.subj, "site-b") {
			assert.Len(t, change.Accounts, 2, "site-b should have alice and bob")
		} else if strings.Contains(p.subj, "site-c") {
			assert.Equal(t, []string{"charlie"}, change.Accounts)
		}
	}
}

func TestHandler_ProcessRemoveMember_OwnerRemovesOrg(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID    = "room-1"
		orgID     = "org-1"
		requester = "alice"
		siteID    = "site-a"
	)

	// 3 org members: carol and dave have no individual membership, eve does
	orgMembers := []OrgMemberStatus{
		{Account: "carol", SiteID: siteID, SectName: "Engineering", HasIndividualMembership: false},
		{Account: "dave", SiteID: siteID, SectName: "Engineering", HasIndividualMembership: false},
		{Account: "eve", SiteID: siteID, SectName: "Engineering", HasIndividualMembership: true},
	}

	store.EXPECT().
		GetOrgMembersWithIndividualStatus(gomock.Any(), roomID, orgID).
		Return(orgMembers, nil)
	store.EXPECT().
		DeleteSubscriptionsByAccounts(gomock.Any(), roomID, gomock.InAnyOrder([]string{"carol", "dave"})).
		Return(int64(2), nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberOrg, orgID).
		Return(nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), roomID).Return(nil) // recount after removal

	var published []publishedMsg
	h := NewHandler(store, siteID, func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	})

	req := model.RemoveMemberRequest{RoomID: roomID, Requester: requester, OrgID: orgID, Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.NoError(t, err)

	// Expect: 2 sub updates (carol, dave) + 1 member event + 1 sys msg = 4 publishes
	assert.Len(t, published, 4, "expected 4 publishes: 2 sub updates, member event, sys msg")

	subjSet := make(map[string]bool)
	for _, p := range published {
		subjSet[p.subj] = true
	}

	assert.True(t, subjSet[subject.SubscriptionUpdate("carol")], "expected subscription update for carol")
	assert.True(t, subjSet[subject.SubscriptionUpdate("dave")], "expected subscription update for dave")
	assert.False(t, subjSet[subject.SubscriptionUpdate("eve")], "eve has individual membership, should not be removed")
	assert.True(t, subjSet[subject.MemberEvent(roomID)], "expected member event published")
}

func TestHandler_ProcessRemoveMember_CrossSiteOutbox(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID    = "room-1"
		account   = "alice"
		localSite = "site-a"
		userSite  = "site-b" // user is on a different site
	)

	userResult := &UserWithMembership{
		User: model.User{
			ID:      "u1",
			Account: account,
			SiteID:  userSite, // different from local site
		},
		HasOrgMembership: false,
	}

	store.EXPECT().
		GetUserWithMembership(gomock.Any(), roomID, account).
		Return(userResult, nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), roomID, account).
		Return(int64(1), nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)

	var published []publishedMsg
	h := NewHandler(store, localSite, func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	})

	req := model.RemoveMemberRequest{RoomID: roomID, Requester: account, Account: account, Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.NoError(t, err)

	// Expect: sub update + member event + sys msg + outbox = 4 publishes
	assert.Len(t, published, 4, "expected 4 publishes including outbox for federated user")

	outboxSubj := subject.Outbox(localSite, userSite, "member_removed")
	subjSet := make(map[string]bool)
	for _, p := range published {
		subjSet[p.subj] = true
	}
	assert.True(t, subjSet[outboxSubj], "expected outbox event published for remote user")
}

func TestHandler_ProcessRemoveMember_UnmarshalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })

	err := h.processRemoveMember(context.Background(), []byte("{not json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestHandler_ProcessRemoveIndividual_GetUserError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), "r1", "alice").
		Return(nil, fmt.Errorf("db down"))

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "alice", Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get user with membership")
}

func TestHandler_ProcessRemoveIndividual_DeleteRoomMemberError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), "r1", "alice").
		Return(&UserWithMembership{
			User:  model.User{ID: "u1", Account: "alice"},
			Roles: []model.Role{model.RoleMember},
		}, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), "r1", model.RoomMemberIndividual, "u1").
		Return(fmt.Errorf("write failed"))

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "alice", Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete room member")
}

func TestHandler_ProcessRemoveIndividual_DualDemoteError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), "r1", "alice").
		Return(&UserWithMembership{
			User:             model.User{ID: "u1", Account: "alice"},
			HasOrgMembership: true,
			Roles:            []model.Role{model.RoleOwner, model.RoleMember},
		}, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), "r1", model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		RemoveRole(gomock.Any(), "alice", "r1", model.RoleOwner).
		Return(fmt.Errorf("write failed"))

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "alice", Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demote dual-member owner")
}

func TestHandler_ProcessRemoveIndividual_DeleteSubscriptionError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), "r1", "alice").
		Return(&UserWithMembership{
			User:  model.User{ID: "u1", Account: "alice"},
			Roles: []model.Role{model.RoleMember},
		}, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), "r1", model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), "r1", "alice").
		Return(int64(0), fmt.Errorf("write failed"))

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "alice", Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete subscription")
}

func TestHandler_ProcessRemoveIndividual_ReconcileMemberCountsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().
		GetUserWithMembership(gomock.Any(), "r1", "alice").
		Return(&UserWithMembership{
			User:  model.User{ID: "u1", Account: "alice"},
			Roles: []model.Role{model.RoleMember},
		}, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), "r1", model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), "r1", "alice").
		Return(int64(1), nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), "r1").
		Return(fmt.Errorf("write failed"))

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil })
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "alice", Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reconcile member counts")
}

func TestHandler_ProcessAddMembers_ExistingOrgsWritesIndividuals(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	publish := func(_ context.Context, _ string, _ []byte, _ string) error { return nil }
	h := NewHandler(store, "site-a", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob"}, "r1").
		Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site-a"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(true, nil)
	store.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, members []*model.RoomMember) error {
			require.Len(t, members, 1)
			assert.Equal(t, model.RoomMemberIndividual, members[0].Member.Type)
			assert.Equal(t, "bob", members[0].Member.Account)
			return nil
		})

	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterAccount: "alice",
		Timestamp:        1000,
		History:          model.HistoryConfig{Mode: model.HistoryModeAll},
	}
	reqData, _ := json.Marshal(req)

	ctxEO := natsutil.WithRequestID(context.Background(), "req-existing-orgs-test")
	err := h.processAddMembers(ctxEO, reqData)
	require.NoError(t, err)
}

// Bug 4: outbox publish failure must propagate (NAK), not be swallowed.
func TestHandler_ProcessRemoveIndividual_OutboxFailurePropagates(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID    = "room-1"
		account   = "alice"
		localSite = "site-a"
		userSite  = "site-b"
	)

	store.EXPECT().
		GetUserWithMembership(gomock.Any(), roomID, account).
		Return(&UserWithMembership{
			User:             model.User{ID: "u1", Account: account, SiteID: userSite},
			HasOrgMembership: false,
		}, nil)
	store.EXPECT().
		DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberIndividual, "u1").
		Return(nil)
	store.EXPECT().
		DeleteSubscription(gomock.Any(), roomID, account).
		Return(int64(1), nil)
	store.EXPECT().
		ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)

	outboxSubj := subject.Outbox(localSite, userSite, "member_removed")
	publish := func(_ context.Context, subj string, _ []byte, _ string) error {
		if subj == outboxSubj {
			return fmt.Errorf("outbox publish broken")
		}
		return nil
	}
	h := NewHandler(store, localSite, publish)

	req := model.RemoveMemberRequest{RoomID: roomID, Requester: account, Account: account, Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err, "outbox failure must return error so JetStream NAKs and retries")
	assert.Contains(t, err.Error(), "outbox")
}

func TestHandler_ProcessRemoveOrg_OutboxFailurePropagates(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	const (
		roomID     = "room-1"
		orgID      = "org-1"
		requester  = "alice"
		localSite  = "site-a"
		remoteSite = "site-b"
	)

	orgMembers := []OrgMemberStatus{
		{Account: "carol", SiteID: remoteSite, SectName: "Eng", HasIndividualMembership: false},
	}

	store.EXPECT().GetOrgMembersWithIndividualStatus(gomock.Any(), roomID, orgID).Return(orgMembers, nil)
	store.EXPECT().DeleteSubscriptionsByAccounts(gomock.Any(), roomID, []string{"carol"}).Return(int64(1), nil)
	store.EXPECT().DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberOrg, orgID).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)

	outboxSubj := subject.Outbox(localSite, remoteSite, "member_removed")
	publish := func(_ context.Context, subj string, _ []byte, _ string) error {
		if subj == outboxSubj {
			return fmt.Errorf("outbox publish broken")
		}
		return nil
	}
	h := NewHandler(store, localSite, publish)

	req := model.RemoveMemberRequest{RoomID: roomID, Requester: requester, OrgID: orgID, Timestamp: 1000}
	data, _ := json.Marshal(req)

	err := h.processRemoveMember(context.Background(), data)
	require.Error(t, err, "outbox failure must return error so JetStream NAKs and retries")
	assert.Contains(t, err.Error(), "outbox")
}

func TestHandler_processAddMembers_PublishesSuccessEventToRequesterSubject(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var capturedSubject string
	var capturedData []byte
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		if strings.HasPrefix(subj, "chat.user.") {
			capturedSubject = subj
			capturedData = data
		}
		return nil
	}
	h := NewHandler(store, "site1", publish)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site1"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), []string{"bob"}, "r1").Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u2", Account: "bob", SiteID: "site1"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	ctx := natsutil.WithRequestID(context.Background(), "req-async-test")
	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterAccount: "alice",
		Timestamp:        1000,
	}
	reqData, _ := json.Marshal(req)
	err := h.processAddMembers(ctx, reqData)
	require.NoError(t, err)

	assert.Equal(t, subject.UserResponse("alice", "req-async-test"), capturedSubject)
	var result model.AsyncJobResult
	require.NoError(t, json.Unmarshal(capturedData, &result))
	assert.Equal(t, "req-async-test", result.RequestID)
	assert.Equal(t, model.AsyncJobOpRoomMemberAdd, result.Operation)
	assert.Equal(t, "ok", result.Status)
	assert.Equal(t, "", result.Error)
	assert.Greater(t, result.Timestamp, int64(0))
}

func TestHandler_processAddMembers_PublishesFailureEventOnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var capturedSubject string
	var capturedData []byte
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		if strings.HasPrefix(subj, "chat.user.") {
			capturedSubject = subj
			capturedData = data
		}
		return nil
	}
	h := NewHandler(store, "site1", publish)

	// Mock store to fail on FindUsersByAccounts (first store operation after ListNewMembers)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site1"}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), []string{"bob"}, "r1").Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return(nil, fmt.Errorf("database connection failed"))

	ctx := natsutil.WithRequestID(context.Background(), "req-error-test")
	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterAccount: "alice",
		Timestamp:        1000,
	}
	reqData, _ := json.Marshal(req)
	err := h.processAddMembers(ctx, reqData)
	require.Error(t, err, "processAddMembers must return error on FindUsersByAccounts failure")
	assert.Contains(t, err.Error(), "find users by accounts")

	// Verify failure event was published to requester
	assert.Equal(t, subject.UserResponse("alice", "req-error-test"), capturedSubject)
	var result model.AsyncJobResult
	require.NoError(t, json.Unmarshal(capturedData, &result))
	assert.Equal(t, "req-error-test", result.RequestID)
	assert.Equal(t, model.AsyncJobOpRoomMemberAdd, result.Operation)
	assert.Equal(t, "error", result.Status, "failure event must have Status=error")
	assert.Equal(t, "operation failed", result.Error, "failure event must carry sanitized error message")
	assert.Greater(t, result.Timestamp, int64(0))
}

func TestHandler_publishAsyncJobResult_PopulatesErrorOnFailure(t *testing.T) {
	var capturedSubject string
	var capturedData []byte
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		if strings.HasPrefix(subj, "chat.user.") {
			capturedSubject = subj
			capturedData = data
		}
		return nil
	}
	h := NewHandler(nil, "site1", publish)

	ctx := natsutil.WithRequestID(context.Background(), "req-err-test")
	jobErr := errors.New("oops")
	h.publishAsyncJobResult(ctx, "alice", model.AsyncJobOpRoomMemberAdd, "r1", jobErr)

	assert.Equal(t, subject.UserResponse("alice", "req-err-test"), capturedSubject)
	var result model.AsyncJobResult
	require.NoError(t, json.Unmarshal(capturedData, &result))
	assert.Equal(t, "req-err-test", result.RequestID)
	assert.Equal(t, model.AsyncJobOpRoomMemberAdd, result.Operation)
	assert.Equal(t, "error", result.Status)
	assert.Equal(t, "operation failed", result.Error)
	assert.Equal(t, "r1", result.RoomID)
}

func TestHandler_publishAsyncJobResult_NoOpOnEmptyRequestID(t *testing.T) {
	called := false
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		called = true
		return nil
	}
	h := NewHandler(nil, "site1", publish)

	// No WithRequestID on ctx → empty request ID → publish is skipped.
	h.publishAsyncJobResult(context.Background(), "alice", model.AsyncJobOpRoomMemberAdd, "r1", nil)
	assert.False(t, called, "publish must be skipped when request ID is empty")
}

func TestHandler_publishAsyncJobResult_NoOpOnEmptyRequester(t *testing.T) {
	called := false
	publish := func(ctx context.Context, subj string, data []byte, msgID string) error {
		called = true
		return nil
	}
	h := NewHandler(nil, "site1", publish)

	ctx := natsutil.WithRequestID(context.Background(), "req-test")
	h.publishAsyncJobResult(ctx, "", model.AsyncJobOpRoomMemberAdd, "r1", nil)
	assert.False(t, called, "publish must be skipped when requester account is empty")
}

// ---------------------------------------------------------------------------
// processAddMembers tests (Tasks 12, 14, 14b, 15, 16)
// ---------------------------------------------------------------------------

// newAddMembersTestHandler builds a Handler with a mock store and a capture-publish
// closure, returning (handler, mockStore, getPublished).
func newAddMembersTestHandler(t *testing.T) (*Handler, *MockSubscriptionStore, func() []publishedMsg) {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockStore := NewMockSubscriptionStore(ctrl)
	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := &Handler{
		store:   mockStore,
		publish: publish,
		siteID:  "site-A",
	}
	return h, mockStore, func() []publishedMsg { return published }
}

// setupAddMembersHappyPath sets up the standard happy-path mock expectations.
// All users are on site-A (no cross-site outbox). HasOrgRoomMembers returns false.
func setupAddMembersHappyPath(t *testing.T, mockStore *MockSubscriptionStore, accounts []string) {
	t.Helper()
	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return(accounts, nil)
	users := make([]model.User, len(accounts))
	for i, a := range accounts {
		users[i] = model.User{ID: "u_" + a, Account: a, SiteID: "site-A", EngName: "X", ChineseName: "X"}
	}
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), accounts).Return(users, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
}

// Task 12: missing X-Request-ID must return a permanent error immediately.
func TestProcessAddMembers_RequiresRequestID(t *testing.T) {
	h, _, _ := newAddMembersTestHandler(t)
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, err)

	// ctx has no request ID
	err = h.processAddMembers(context.Background(), body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing X-Request-ID")
	assert.ErrorIs(t, err, errPermanent)
}

// Task 14: subscription must carry Name == room.Name and RoomType == channel.
func TestProcessAddMembers_PopulatesSubName(t *testing.T) {
	h, mockStore, _ := newAddMembersTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	// Use a custom BulkCreateSubscriptions expectation to capture subs.
	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-A", EngName: "X", ChineseName: "X"},
	}, nil)
	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1740000000000,
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	require.Len(t, capturedSubs, 1)
	assert.Equal(t, "deal team", capturedSubs[0].Name)
	assert.Equal(t, model.RoomTypeChannel, capturedSubs[0].RoomType)
}

// Task 14b: HistoryModeNone with explicit SharedSince — sub.HistorySharedSince must match.
func TestProcessAddMembers_HistoryNone_WithExplicitTimestamp(t *testing.T) {
	h, mockStore, _ := newAddMembersTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "0193abcd-0193-7abc-89ab-0193abcd0001")

	explicitMs := int64(1700000000000)
	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-A", EngName: "X", ChineseName: "X"},
	}, nil)
	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: 1740000000000,
		History:   model.HistoryConfig{Mode: model.HistoryModeNone, SharedSince: &explicitMs},
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	require.Len(t, capturedSubs, 1)
	require.NotNil(t, capturedSubs[0].HistorySharedSince)
	wantTime := time.UnixMilli(explicitMs).UTC()
	assert.Equal(t, wantTime, *capturedSubs[0].HistorySharedSince)
}

// Task 14b: HistoryModeNone without explicit SharedSince — sub.HistorySharedSince falls back to acceptedAt.
func TestProcessAddMembers_HistoryNone_NoTimestamp(t *testing.T) {
	h, mockStore, _ := newAddMembersTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "0193abcd-0193-7abc-89ab-0193abcd0002")

	const reqTimestampMs = int64(1740000000000)
	acceptedAt := time.UnixMilli(reqTimestampMs).UTC()

	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-A", EngName: "X", ChineseName: "X"},
	}, nil)
	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	// No SharedSince in HistoryConfig — falls back to req.Timestamp (acceptedAt).
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: reqTimestampMs,
		History:   model.HistoryConfig{Mode: model.HistoryModeNone},
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	require.Len(t, capturedSubs, 1)
	require.NotNil(t, capturedSubs[0].HistorySharedSince)
	assert.Equal(t, acceptedAt, *capturedSubs[0].HistorySharedSince)
}

// Task 14b: no History.Mode set — sub.HistorySharedSince must be nil.
func TestProcessAddMembers_NoHistoryConfig_LeavesNil(t *testing.T) {
	h, mockStore, _ := newAddMembersTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "0193abcd-0193-7abc-89ab-0193abcd0003")

	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-A", EngName: "X", ChineseName: "X"},
	}, nil)
	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	// No History.Mode — HistorySharedSince must remain nil.
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: 1740000000000,
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	require.Len(t, capturedSubs, 1)
	assert.Nil(t, capturedSubs[0].HistorySharedSince)
}

// Task 15: outbox MemberAddEvent for cross-site members must carry RoomName.
func TestProcessAddMembers_OutboxCarriesRoomName(t *testing.T) {
	h, mockStore, getPublished := newAddMembersTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	// Cross-site member: bob lives on site-B.
	mockStore.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mockStore.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-B", EngName: "Bob", ChineseName: "鲍勃"},
	}, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	// Find outbox publish to site-B with member_added.
	pub := getPublished()
	var found bool
	for _, m := range pub {
		if !strings.HasPrefix(m.subj, "outbox.site-A.to.site-B.member_added") {
			continue
		}
		found = true
		var envelope model.OutboxEvent
		require.NoError(t, json.Unmarshal(m.data, &envelope))
		var evt model.MemberAddEvent
		require.NoError(t, json.Unmarshal(envelope.Payload, &evt))
		assert.Equal(t, "deal team", evt.RoomName)
	}
	require.True(t, found, "expected outbox publish to site-B with member_added subject")
}

// Task 16: successful processAddMembers must publish AsyncJobResult with status "ok".
func TestProcessAddMembers_PublishesAsyncJobOnSuccess(t *testing.T) {
	h, mockStore, getPublished := newAddMembersTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	setupAddMembersHappyPath(t, mockStore, []string{"bob"})
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID: "r1", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, err)
	require.NoError(t, h.processAddMembers(ctx, body))

	// Find async-job publish on subject.UserResponse("alice", reqID).
	expectedSubj := subject.UserResponse("alice", reqID)
	pub := getPublished()
	var found *publishedMsg
	for i := range pub {
		if pub[i].subj == expectedSubj {
			found = &pub[i]
			break
		}
	}
	require.NotNil(t, found, "expected async-job publish on %s", expectedSubj)

	var got model.AsyncJobResult
	require.NoError(t, json.Unmarshal(found.data, &got))
	assert.Equal(t, reqID, got.RequestID)
	assert.Equal(t, model.AsyncJobOpRoomMemberAdd, got.Operation)
	assert.Equal(t, "ok", got.Status)
}

func TestResolveRoomName(t *testing.T) {
	tests := map[string]struct {
		req      model.CreateRoomRequest
		roomType model.RoomType
		want     string
	}{
		"dm empty":           {model.CreateRoomRequest{RoomID: "u_a|u_b"}, model.RoomTypeDM, ""},
		"botDM empty":        {model.CreateRoomRequest{RoomID: "u_a|u_w"}, model.RoomTypeBotDM, ""},
		"channel given name": {model.CreateRoomRequest{Name: "deal team", RoomID: "r1"}, model.RoomTypeChannel, "deal team"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveRoomName(&tc.req, tc.roomType))
		})
	}
}

func TestNewSubSetsAllFields(t *testing.T) {
	user := &model.User{ID: "u1", Account: "alice"}
	room := &model.Room{ID: "r1", SiteID: "site-A", Type: model.RoomTypeChannel}
	now := time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC)

	sub := newSub("s1", user, room, []model.Role{model.RoleOwner},
		"deal team", false, now)

	assert.Equal(t, "s1", sub.ID)
	assert.Equal(t, "u1", sub.User.ID)
	assert.Equal(t, "alice", sub.User.Account)
	assert.Equal(t, "r1", sub.RoomID)
	assert.Equal(t, "site-A", sub.SiteID)
	assert.Equal(t, []model.Role{model.RoleOwner}, sub.Roles)
	assert.Equal(t, "deal team", sub.Name)
	assert.Equal(t, model.RoomTypeChannel, sub.RoomType)
	assert.False(t, sub.IsSubscribed)
	assert.Equal(t, now, sub.JoinedAt)
}

// ---- processCreateRoom test helpers ----

// newCreateRoomTestHandler builds a Handler with a mock store and capture-publish,
// returning (handler, mockStore, getPublished). siteID is always "site-A".
func newCreateRoomTestHandler(t *testing.T) (*Handler, *MockSubscriptionStore, func() []publishedMsg) {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockStore := NewMockSubscriptionStore(ctrl)
	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := &Handler{store: mockStore, publish: publish, siteID: "site-A"}
	return h, mockStore, func() []publishedMsg { return published }
}

// subscriptionUpdates filters published messages to subscription.update events.
func subscriptionUpdates(published []publishedMsg) []publishedMsg {
	var out []publishedMsg
	for _, p := range published {
		if strings.HasPrefix(p.subj, "chat.user.") && strings.HasSuffix(p.subj, ".event.subscription.update") {
			out = append(out, p)
		}
	}
	return out
}

// messagesCanonical filters published messages to the canonical message stream for a siteID.
func messagesCanonical(published []publishedMsg, siteID string) []publishedMsg {
	var out []publishedMsg
	prefix := fmt.Sprintf("chat.msg.canonical.%s", siteID)
	for _, p := range published {
		if strings.HasPrefix(p.subj, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// outboxFor filters published messages to the outbox stream for a specific destSiteID and eventType.
func outboxFor(published []publishedMsg, destSiteID, eventType string) []publishedMsg {
	var out []publishedMsg
	subj := fmt.Sprintf("outbox.site-A.to.%s.%s", destSiteID, eventType)
	for _, p := range published {
		if p.subj == subj {
			out = append(out, p)
		}
	}
	return out
}

// userResponseFor filters published messages to the async-job result for an account.
func userResponseFor(published []publishedMsg, account string) []publishedMsg {
	var out []publishedMsg
	prefix := fmt.Sprintf("chat.user.%s.response.", account)
	for _, p := range published {
		if strings.HasPrefix(p.subj, prefix) {
			out = append(out, p)
		}
	}
	return out
}

const testRequestID = "0193abcd-0193-7abc-89ab-0193abcd0193"

// makeCreateRoomBody marshals a CreateRoomRequest to JSON.
func makeCreateRoomBody(t *testing.T, req *model.CreateRoomRequest) []byte {
	t.Helper()
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return data
}

// ---- Task 32: skeleton tests ----

func TestProcessCreateRoom_RequiresRequestID(t *testing.T) {
	h, _, _ := newCreateRoomTestHandler(t)
	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room1", RequesterAccount: "alice", Timestamp: time.Now().UnixMilli(),
		Users: []string{"bob"},
	})

	err := h.processCreateRoom(context.Background(), body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing X-Request-ID")
	assert.ErrorIs(t, err, errPermanent)
}

// ---- Task 33: DM branch tests ----

func TestProcessCreateRoom_DM_BuildsTwoSubs(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	other := &model.User{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().GetUser(gomock.Any(), "bob").Return(other, nil)

	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-dm-1").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID:           "room-dm-1",
		RequesterAccount: "alice",
		Users:            []string{"bob"}, // no orgs/channels/name → DM
		Timestamp:        time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	require.Len(t, capturedSubs, 2)

	// alice's sub: Name = other's account
	aliceSub := capturedSubs[0]
	assert.Equal(t, "u_alice", aliceSub.User.ID)
	assert.Equal(t, other.Account, aliceSub.Name)
	assert.Nil(t, aliceSub.Roles)
	assert.False(t, aliceSub.IsSubscribed)
	assert.Equal(t, model.RoomTypeDM, aliceSub.RoomType)

	// bob's sub: Name = requester's account
	bobSub := capturedSubs[1]
	assert.Equal(t, "u_bob", bobSub.User.ID)
	assert.Equal(t, requester.Account, bobSub.Name)
	assert.Nil(t, bobSub.Roles)
	assert.False(t, bobSub.IsSubscribed)

	// No sys messages for DM
	assert.Empty(t, messagesCanonical(getPublished(), "site-A"))
}

func TestProcessCreateRoom_DM_EmitsNoSysMessages(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	other := &model.User{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().GetUser(gomock.Any(), "bob").Return(other, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-dm-1").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-dm-1", RequesterAccount: "alice",
		Users: []string{"bob"}, Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	assert.Empty(t, messagesCanonical(getPublished(), "site-A"), "DM must emit no sys-messages")
}

// ---- Task 33: botDM branch tests ----

func TestProcessCreateRoom_BotDM_HasIsSubscribed(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	bot := &model.User{ID: "u_bot", Account: "helper.bot", SiteID: "site-A"}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().GetUser(gomock.Any(), "helper.bot").Return(bot, nil)

	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-bot-1").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-bot-1", RequesterAccount: "alice",
		Users:     []string{"helper.bot"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	require.Len(t, capturedSubs, 2)

	// human side (alice): Name = bot's account, IsSubscribed = true
	humanSub := capturedSubs[0]
	assert.Equal(t, "u_alice", humanSub.User.ID)
	assert.Equal(t, bot.Account, humanSub.Name)
	assert.True(t, humanSub.IsSubscribed)

	// bot side: Name = requester's account, IsSubscribed = false
	botSub := capturedSubs[1]
	assert.Equal(t, "u_bot", botSub.User.ID)
	assert.Equal(t, requester.Account, botSub.Name)
	assert.False(t, botSub.IsSubscribed)

	assert.Empty(t, messagesCanonical(getPublished(), "site-A"), "botDM must emit no sys-messages")
}

// ---- Task 34: Channel branch tests ----

func TestProcessCreateRoom_Channel_BuildsSubsAndMembers(t *testing.T) {
	h, mockStore, _ := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
		{ID: "u_carol", Account: "carol", EngName: "Carol C", ChineseName: "卡羅", SiteID: "site-A"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	// orgs present → ListNewMembersForNewRoom returns bob+carol (alice already stripped by service)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), []string{"org1"}, []string{"bob", "carol"}, "alice").
		Return([]string{"bob", "carol"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "carol"}).Return(invited, nil)

	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})

	var capturedMembers []*model.RoomMember
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, members []*model.RoomMember) error {
			capturedMembers = members
			return nil
		})
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-1").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-1", Name: "Deal Team", RequesterAccount: "alice",
		Users: []string{"bob", "carol"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob", "carol"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	// 3 subs: alice (owner — first), bob (member), carol (member)
	require.Len(t, capturedSubs, 3)
	ownerSub := capturedSubs[0]
	assert.Equal(t, "u_alice", ownerSub.User.ID)
	assert.Equal(t, []model.Role{model.RoleOwner}, ownerSub.Roles)
	assert.Equal(t, "Deal Team", ownerSub.Name)

	memberSub := capturedSubs[1]
	assert.Equal(t, []model.Role{model.RoleMember}, memberSub.Roles)

	// 4 room_members: 2 individuals (bob+carol) + 1 org + 1 owner (alice)
	require.Len(t, capturedMembers, 4)
	types := make([]model.RoomMemberType, 0, 4)
	for _, m := range capturedMembers {
		types = append(types, m.Member.Type)
	}
	assert.Equal(t, 3, countType(types, model.RoomMemberIndividual))
	assert.Equal(t, 1, countType(types, model.RoomMemberOrg))
}

// countType counts occurrences of t in slice.
func countType(types []model.RoomMemberType, mt model.RoomMemberType) int {
	n := 0
	for _, t := range types {
		if t == mt {
			n++
		}
	}
	return n
}

func TestProcessCreateRoom_Channel_NoOrgsSkipsRoomMembers(t *testing.T) {
	h, mockStore, _ := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Nil(), []string{"bob"}, "alice").
		Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return(invited, nil)

	var capturedSubs []*model.Subscription
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, subs []*model.Subscription) error {
			capturedSubs = subs
			return nil
		})
	// Lite-mode: BulkCreateRoomMembers MUST NOT be called when no orgs are
	// resolved. room_members stays empty until an org later joins, at which
	// point the backfill loop in processAddMembers reads from subscriptions.
	// (gomock fails the test on any unexpected call, so omitting the EXPECT
	// is the assertion.)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-2").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-2", Name: "Small Channel", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{}, // no orgs
		ResolvedUsers: []string{"bob"}, ResolvedOrgs: []string{},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	// Owner + invited individual sub still land in `subscriptions` — that's
	// the source of truth for who is in the room while in lite-mode.
	require.Len(t, capturedSubs, 2)
}

// ---- Task 35: subscription.update fan-out ----

func TestProcessCreateRoom_Channel_FiresSubscriptionUpdateForEverySub(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return(invited, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-3").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-3", Name: "Test Channel", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	updates := subscriptionUpdates(getPublished())
	// 2 subs (bob + alice) → 2 subscription.update events
	assert.Len(t, updates, 2)

	// Verify subjects cover both accounts
	subjects := make([]string, 0, len(updates))
	for _, u := range updates {
		subjects = append(subjects, u.subj)
	}
	assert.Contains(t, subjects, subject.SubscriptionUpdate("alice"))
	assert.Contains(t, subjects, subject.SubscriptionUpdate("bob"))
}

// ---- Task 36: sys-messages ----

func TestProcessCreateRoom_Channel_EmitsSysMessages(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return(invited, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-4").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-4", Name: "Sys Msg Channel", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	canonical := messagesCanonical(getPublished(), "site-A")
	require.Len(t, canonical, 2, "expected room_created + members_added sys messages")

	// Unmarshal and verify message types
	var evt1 model.MessageEvent
	require.NoError(t, json.Unmarshal(canonical[0].data, &evt1))
	assert.Equal(t, model.MessageTypeRoomCreated, evt1.Message.Type)
	assert.Equal(t, "room-ch-4", evt1.Message.RoomID)
	// ID must be deterministic from requestID
	expectedID1 := idgen.MessageIDFromRequestID(testRequestID, "room_created")
	assert.Equal(t, expectedID1, evt1.Message.ID)

	var evt2 model.MessageEvent
	require.NoError(t, json.Unmarshal(canonical[1].data, &evt2))
	assert.Equal(t, model.MessageTypeMembersAdded, evt2.Message.Type)
	expectedID2 := idgen.MessageIDFromRequestID(testRequestID, "members_added")
	assert.Equal(t, expectedID2, evt2.Message.ID)
}

// Sys-message payloads must carry the LITERAL request (Users/Orgs/Channels), not the
// post-expansion resolved set. This guards against drift if someone later changes the
// worker to use ResolvedUsers/ResolvedOrgs in the sys-msg path.
func TestProcessCreateRoom_Channel_SysMsgUsesLiteralRequest(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	// Resolved set expands org1 → [bob, carol, dave] but the literal request only named [bob] + [org1].
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
		{ID: "u_carol", Account: "carol", EngName: "Carol C", ChineseName: "卡羅", SiteID: "site-A"},
		{ID: "u_dave", Account: "dave", EngName: "Dave D", ChineseName: "戴夫", SiteID: "site-A"},
	}
	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]string{"bob", "carol", "dave"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "carol", "dave"}).Return(invited, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-lit").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-lit", Name: "Literal Test", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob", "carol", "dave"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	canonical := messagesCanonical(getPublished(), "site-A")
	require.Len(t, canonical, 2)

	// room_created sys-msg payload
	var evt1 model.MessageEvent
	require.NoError(t, json.Unmarshal(canonical[0].data, &evt1))
	var rc model.RoomCreated
	require.NoError(t, json.Unmarshal(evt1.Message.SysMsgData, &rc))
	assert.Equal(t, []string{"bob"}, rc.Users, "RoomCreated.Users must be the literal request, not the resolved set")
	assert.Equal(t, []string{"org1"}, rc.Orgs, "RoomCreated.Orgs must be the literal request")

	// members_added sys-msg payload
	var evt2 model.MessageEvent
	require.NoError(t, json.Unmarshal(canonical[1].data, &evt2))
	var ma model.MembersAdded
	require.NoError(t, json.Unmarshal(evt2.Message.SysMsgData, &ma))
	assert.Equal(t, []string{"bob"}, ma.Individuals, "MembersAdded.Individuals must be the literal request")
	assert.Equal(t, []string{"org1"}, ma.Orgs, "MembersAdded.Orgs must be the literal request")
}

// ---- Task 37: outbox + async-job ----

func TestProcessCreateRoom_Channel_OutboxPerRemoteSite(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	// bob is on site-B → should trigger outbox
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-B"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return(invited, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-5").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-5", Name: "Cross Site", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	outboxMsgs := outboxFor(getPublished(), "site-B", model.MessageTypeRoomCreated)
	require.Len(t, outboxMsgs, 1)

	var envelope model.OutboxEvent
	require.NoError(t, json.Unmarshal(outboxMsgs[0].data, &envelope))
	assert.Equal(t, model.MessageTypeRoomCreated, envelope.Type)
	assert.Equal(t, "site-A", envelope.SiteID)
	assert.Equal(t, "site-B", envelope.DestSiteID)

	var payload model.RoomCreatedOutbox
	require.NoError(t, json.Unmarshal(envelope.Payload, &payload))
	assert.Equal(t, "room-ch-5", payload.RoomID)
	assert.Equal(t, []string{"bob"}, payload.Accounts)
	assert.Equal(t, "alice", payload.RequesterAccount)
}

func TestProcessCreateRoom_Channel_EmitsAsyncJobOk(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}
	invited := []model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob B", ChineseName: "鮑伯", SiteID: "site-A"},
	}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]string{"bob"}, nil)
	mockStore.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return(invited, nil)
	mockStore.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().BulkCreateRoomMembers(gomock.Any(), gomock.Any()).Return(nil)
	mockStore.EXPECT().ReconcileMemberCounts(gomock.Any(), "room-ch-6").Return(nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID: "room-ch-6", Name: "Job Test", RequesterAccount: "alice",
		Users: []string{"bob"}, Orgs: []string{"org1"},
		ResolvedUsers: []string{"bob"}, ResolvedOrgs: []string{"org1"},
		Timestamp: time.Now().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	responses := userResponseFor(getPublished(), "alice")
	require.NotEmpty(t, responses)

	var result model.AsyncJobResult
	require.NoError(t, json.Unmarshal(responses[0].data, &result))
	assert.Equal(t, model.AsyncJobStatusOK, result.Status)
	assert.Equal(t, model.AsyncJobOpRoomCreate, result.Operation)
}

// ---- Permanent-error coverage for HandleJetStreamMsg Ack path + new permanentError type ----

func TestProcessCreateRoom_RoomIDCollisionMismatchType_ReturnsPermanent(t *testing.T) {
	h, mockStore, getPublished := newCreateRoomTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), testRequestID)

	requester := &model.User{ID: "u_alice", Account: "alice", EngName: "Alice A", ChineseName: "艾麗斯", SiteID: "site-A"}

	mockStore.EXPECT().GetUser(gomock.Any(), "alice").Return(requester, nil)
	// Insert collides on _id.
	mockStore.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(mongo.WriteException{
		WriteErrors: []mongo.WriteError{{Code: 11000, Message: "duplicate key"}},
	})
	// Existing room has DIFFERENT type (channel) than the request (DM).
	mockStore.EXPECT().GetRoom(gomock.Any(), gomock.Any()).Return(&model.Room{
		ID: "room-collide", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)

	body := makeCreateRoomBody(t, &model.CreateRoomRequest{
		RoomID:           "room-collide",
		RequesterAccount: "alice",
		Users:            []string{"bob"}, // DM intent
		Timestamp:        time.Now().UnixMilli(),
	})

	err := h.processCreateRoom(ctx, body)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPermanent)
	assert.Contains(t, err.Error(), "room ID collision")

	// Async-job error event must be published (defer fires before return).
	responses := userResponseFor(getPublished(), "alice")
	require.NotEmpty(t, responses, "permanent error must publish async-job error event")
	var result model.AsyncJobResult
	require.NoError(t, json.Unmarshal(responses[0].data, &result))
	assert.Equal(t, model.AsyncJobStatusError, result.Status)
	assert.Contains(t, result.Error, "room ID collision")
	// Sanitized error must NOT contain the trailing ": permanent" suffix.
	assert.NotContains(t, result.Error, ": permanent")
}

func TestSanitizeAsyncJobError_PermanentErrorTypeReturnsCleanMessage(t *testing.T) {
	err := newPermanent("counterpart not found")
	got := sanitizeAsyncJobError(err)
	assert.Equal(t, "counterpart not found", got)
}

func TestSanitizeAsyncJobError_LegacyWrappedSentinelStillTrimmed(t *testing.T) {
	err := fmt.Errorf("legacy reason: %w", errPermanent)
	got := sanitizeAsyncJobError(err)
	assert.Equal(t, "legacy reason", got)
}

func TestSanitizeAsyncJobError_NonPermanentCollapsed(t *testing.T) {
	err := fmt.Errorf("transient store error: %w", errors.New("connection reset"))
	got := sanitizeAsyncJobError(err)
	assert.Equal(t, "operation failed", got)
}

// Caller-supplied req.History.SharedSince must propagate identically to the
// local subscription, the per-user SubscriptionUpdateEvent fan-out, and the
// MemberAddEvent published locally — without it, the event would carry
// req.Timestamp instead and remote-site sub creation would diverge from
// home-site sub creation.
func TestHandler_ProcessAddMembers_HistorySharedSinceWinsOverTimestamp(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)

	var published []publishedMsg
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		published = append(published, publishedMsg{subj: subj, data: data})
		return nil
	}
	h := NewHandler(store, "site-a", publish)

	const sharedSince int64 = 500
	const timestamp int64 = 1500
	require.NotEqual(t, sharedSince, timestamp, "test premise: explicit cutoff must differ from acceptedAt")

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-a", Type: model.RoomTypeChannel}, nil)
	store.EXPECT().ListNewMembers(gomock.Any(), nil, []string{"bob"}, "r1").Return([]string{"bob"}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-a", EngName: "Bob", ChineseName: "鮑伯"},
	}, nil)
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, subs []*model.Subscription) error {
			require.Len(t, subs, 1)
			require.NotNil(t, subs[0].HistorySharedSince, "explicit cutoff must reach sub")
			assert.Equal(t, time.UnixMilli(sharedSince).UTC(), *subs[0].HistorySharedSince,
				"sub HistorySharedSince must equal req.History.SharedSince, not req.Timestamp")
			return nil
		})
	store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
	store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)

	ss := sharedSince
	req := model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterAccount: "alice",
		Timestamp:        timestamp,
		History:          model.HistoryConfig{Mode: model.HistoryModeNone, SharedSince: &ss},
	}
	reqData, _ := json.Marshal(req)
	ctx := natsutil.WithRequestID(context.Background(), "req-shared-since-precedence")
	require.NoError(t, h.processAddMembers(ctx, reqData))

	addEvt, _ := findMemberAddEvent(t, published, "r1")
	require.NotNil(t, addEvt.HistorySharedSince, "MemberAddEvent must carry the explicit cutoff")
	assert.Equal(t, sharedSince, *addEvt.HistorySharedSince,
		"MemberAddEvent.HistorySharedSince must equal req.History.SharedSince, not req.Timestamp")
}
