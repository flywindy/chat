//go:build integration

package mongorepo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/testutil"
)

func setupMongo(t *testing.T) *mongo.Database {
	return testutil.MongoDB(t, "history_service_test")
}

// --- Collection[T] integration tests ---

type testDoc struct {
	ID   string `bson:"_id"`
	Name string `bson:"name"`
	Age  int    `bson:"age"`
}

func TestCollection_FindOne_Success(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertOne(ctx, testDoc{ID: "d1", Name: "Alice", Age: 30})
	require.NoError(t, err)

	result, err := col.FindOne(ctx, bson.M{"name": "Alice"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "d1", result.ID)
	assert.Equal(t, "Alice", result.Name)
}

func TestCollection_FindOne_NotFound_ReturnsNilNil(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	result, err := col.FindOne(ctx, bson.M{"name": "Nobody"})
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestCollection_FindByID(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertOne(ctx, testDoc{ID: "d1", Name: "Bob", Age: 25})
	require.NoError(t, err)

	result, err := col.FindByID(ctx, "d1")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "Bob", result.Name)
}

func TestCollection_FindByID_NotFound(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	result, err := col.FindByID(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestCollection_FindMany_Success(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "Alice", Age: 30},
		testDoc{ID: "d2", Name: "Bob", Age: 25},
		testDoc{ID: "d3", Name: "Charlie", Age: 35},
	})
	require.NoError(t, err)

	results, err := col.FindMany(ctx, bson.M{"age": bson.M{"$gte": 30}})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestCollection_FindMany_Empty_ReturnsEmptySlice(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	results, err := col.FindMany(ctx, bson.M{"name": "Nobody"})
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
}

func TestCollection_Raw(t *testing.T) {
	db := setupMongo(t)
	col := NewCollection[testDoc](db.Collection("test_docs"))

	raw := col.Raw()
	assert.Equal(t, "test_docs", raw.Name())
}

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

// --- Collection[T].Aggregate integration tests ---

func TestCollection_Aggregate_ReturnsMatchingDocs(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs"))

	_, err := db.Collection("agg_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "Alice", Age: 30},
		testDoc{ID: "d2", Name: "Bob", Age: 25},
		testDoc{ID: "d3", Name: "Charlie", Age: 35},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.M{"age": bson.M{"$gte": 30}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "d1", results[0].ID) // Alice, age 30
	assert.Equal(t, "d3", results[1].ID) // Charlie, age 35
}

func TestCollection_Aggregate_Empty_ReturnsEmptySlice(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
}

// --- Collection[T].AggregatePaged integration tests ---

func TestCollection_AggregatePaged_ReturnsDataAndTotal(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs"))

	_, err := db.Collection("paged_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "A", Age: 1},
		testDoc{ID: "d2", Name: "B", Age: 2},
		testDoc{ID: "d3", Name: "C", Age: 3},
		testDoc{ID: "d4", Name: "D", Age: 4},
		testDoc{ID: "d5", Name: "E", Age: 5},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	// Page 1: offset 0, limit 2 — expects total=5, data=[d1,d2]
	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "d1", page.Data[0].ID)
	assert.Equal(t, "d2", page.Data[1].ID)

	// Page 2: offset 2, limit 2 — expects total=5, data=[d3,d4]
	page2, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(2, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page2.Total)
	require.Len(t, page2.Data, 2)
	assert.Equal(t, "d3", page2.Data[0].ID)
	assert.Equal(t, "d4", page2.Data[1].ID)
}

func TestCollection_AggregatePaged_EmptyCollection(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(0), page.Total)
	assert.NotNil(t, page.Data)
	assert.Empty(t, page.Data)
}
