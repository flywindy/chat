//go:build integration

package mongorepo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
)

// --- SubscriptionRepo integration tests ---

func TestSubscriptionRepo_GetSubscription(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	joinTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.Collection("subscriptions").InsertOne(ctx, model.Subscription{
		ID:     "s1",
		User:   model.SubscriptionUser{ID: "u1", Account: "u1"},
		RoomID: "r1", SiteID: "site-local",
		Roles: []model.Role{model.RoleMember}, HistorySharedSince: &joinTime, JoinedAt: joinTime,
	})
	require.NoError(t, err)

	sub, err := repo.GetSubscription(ctx, "u1", "r1")
	require.NoError(t, err)
	require.NotNil(t, sub)
	assert.Equal(t, "u1", sub.User.ID)
	assert.Equal(t, "r1", sub.RoomID)
	require.NotNil(t, sub.HistorySharedSince)
	assert.Equal(t, joinTime.UTC(), sub.HistorySharedSince.UTC())
}

func TestSubscriptionRepo_GetSubscription_NotFound(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	sub, err := repo.GetSubscription(ctx, "nonexistent", "r1")
	require.NoError(t, err)
	assert.Nil(t, sub)
}

func TestSubscriptionRepo_GetHistorySharedSince_NilHSS(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	// Insert subscription with no HistorySharedSince (owner — full history access)
	_, err := db.Collection("subscriptions").InsertOne(ctx, model.Subscription{
		ID:     "s2",
		User:   model.SubscriptionUser{ID: "owner", Account: "owner"},
		RoomID: "r1", SiteID: "site-local",
		Roles: []model.Role{model.RoleOwner}, JoinedAt: time.Now(),
	})
	require.NoError(t, err)

	accessSince, subscribed, err := repo.GetHistorySharedSince(ctx, "owner", "r1")
	require.NoError(t, err)
	assert.True(t, subscribed)
	assert.Nil(t, accessSince) // nil = no lower-bound restriction (full history access)
}

func TestSubscriptionRepo_GetHistorySharedSince_WithHSS(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	joinTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.Collection("subscriptions").InsertOne(ctx, model.Subscription{
		ID:     "s3",
		User:   model.SubscriptionUser{ID: "u2", Account: "u2"},
		RoomID: "r2", SiteID: "site-local",
		Roles: []model.Role{model.RoleMember}, HistorySharedSince: &joinTime, JoinedAt: joinTime,
	})
	require.NoError(t, err)

	accessSince, subscribed, err := repo.GetHistorySharedSince(ctx, "u2", "r2")
	require.NoError(t, err)
	assert.True(t, subscribed)
	require.NotNil(t, accessSince)
	assert.Equal(t, joinTime.UTC(), accessSince.UTC())
}

func TestSubscriptionRepo_GetHistorySharedSince_NotSubscribed(t *testing.T) {
	db := setupMongo(t)
	repo := NewSubscriptionRepo(db)
	ctx := context.Background()

	accessSince, subscribed, err := repo.GetHistorySharedSince(ctx, "nobody", "r1")
	require.NoError(t, err)
	assert.False(t, subscribed)
	assert.Nil(t, accessSince)
}
