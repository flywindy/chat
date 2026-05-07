//go:build integration

package mongoutil

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestBulkWrite_MixedUpsertAndDelete(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_mixed"))

	// Pre-seed one doc so the Delete model has something to remove.
	_, err := db.Collection("bulk_mixed").InsertOne(ctx, testDoc{ID: "to-delete", Name: "X", Age: 1})
	require.NoError(t, err)

	models := []mongo.WriteModel{
		UpsertModel(bson.M{"_id": "u1"}, bson.M{"$set": bson.M{"name": "Alice", "age": 30}}),
		UpsertModel(bson.M{"_id": "u2"}, bson.M{"$set": bson.M{"name": "Bob", "age": 25}}),
		DeleteModel(bson.M{"_id": "to-delete"}),
	}
	res, err := col.BulkWrite(ctx, models)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(2), res.Upserted, "two upsert-inserts expected")
	assert.Equal(t, int64(1), res.Deleted)
	assert.Equal(t, int64(0), res.Matched, "no pre-existing docs matched the upsert filters")
}

func TestBulkWrite_PartialFailureUnderUnordered(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	rawCol := db.Collection("bulk_partial")

	// Create a unique index on `name` to force a duplicate-key error.
	// IndexModel.Options is options.Lister[options.IndexOptions]; use the
	// builder from go.mongodb.org/mongo-driver/v2/mongo/options.
	_, err := rawCol.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)

	col := NewCollection[testDoc](rawCol)

	// Insert one doc that'll cause the second upsert to collide on name.
	_, err = rawCol.InsertOne(ctx, testDoc{ID: "first", Name: "duplicate", Age: 1})
	require.NoError(t, err)

	models := []mongo.WriteModel{
		UpsertModel(bson.M{"_id": "ok-1"}, bson.M{"$set": bson.M{"name": "unique-1", "age": 10}}),
		UpsertModel(bson.M{"_id": "collision"}, bson.M{"$set": bson.M{"name": "duplicate", "age": 20}}),
		UpsertModel(bson.M{"_id": "ok-2"}, bson.M{"$set": bson.M{"name": "unique-2", "age": 30}}),
	}
	res, err := col.BulkWrite(ctx, models)

	// Partial failure: one collision, two successes. Under unordered execution
	// the wrapper returns BOTH a non-nil result AND a wrapped exception.
	require.Error(t, err)
	require.NotNil(t, res, "partial-success result must survive the error return")
	assert.Equal(t, int64(2), res.Upserted, "two non-colliding upserts succeeded")

	// Two cross-checks: the typed BulkWriteException via errors.As (so callers
	// can inspect WriteErrors) AND mongo.IsDuplicateKeyError (the codebase
	// canonical helper). If either fails the wrapper has broken the error
	// chain via fmt.Errorf and callers won't be able to discriminate.
	var bwe mongo.BulkWriteException
	require.True(t, errors.As(err, &bwe), "wrapped error must unwrap to BulkWriteException")
	assert.Len(t, bwe.WriteErrors, 1, "exactly one write error from the duplicate key")
	assert.True(t, mongo.IsDuplicateKeyError(err), "duplicate-key helper must traverse the wrap")
}

func TestBulkUpsert_HundredRecordsByID(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_upsert_100"))

	items := make([]testDoc, 100)
	for i := range items {
		items[i] = testDoc{ID: fmt.Sprintf("d%03d", i), Name: fmt.Sprintf("name-%d", i), Age: i}
	}

	res, err := col.BulkUpsert(ctx, items, func(d testDoc) any {
		return bson.M{"_id": d.ID}
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(100), res.Upserted, "all 100 inserted via upsert")
	assert.Equal(t, int64(0), res.Matched)

	// Re-running with the same items: now all 100 match (no inserts).
	for i := range items {
		items[i].Age = i + 1000 // bump age so Modified count != 0
	}
	res2, err := col.BulkUpsert(ctx, items, func(d testDoc) any {
		return bson.M{"_id": d.ID}
	})
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.Equal(t, int64(0), res2.Upserted, "no new inserts on second run")
	assert.Equal(t, int64(100), res2.Matched)
	assert.Equal(t, int64(100), res2.Modified)
}

// TestBulkUpsert_MergeSemantics verifies that $set MERGES rather than REPLACES.
// A field that exists in the stored doc but is not present in the upserted struct
// must be preserved.
func TestBulkUpsert_MergeSemantics(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()

	// Note the inline struct — this test uses a doc shape with an extra field
	// not present in the upsert payload, to prove merge semantics.
	type payload struct {
		ID   string `bson:"_id"`
		Name string `bson:"name"`
	}
	type stored struct {
		ID    string `bson:"_id"`
		Name  string `bson:"name"`
		Extra string `bson:"extra,omitempty"`
	}

	rawCol := db.Collection("bulk_upsert_merge")
	_, err := rawCol.InsertOne(ctx, stored{ID: "x", Name: "old", Extra: "preserve me"})
	require.NoError(t, err)

	col := NewCollection[payload](rawCol)
	res, err := col.BulkUpsert(ctx, []payload{{ID: "x", Name: "new"}}, func(p payload) any {
		return bson.M{"_id": p.ID}
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Matched)
	assert.Equal(t, int64(1), res.Modified)

	// Read back as the wider type — Extra must still be there.
	var got stored
	err = rawCol.FindOne(ctx, bson.M{"_id": "x"}).Decode(&got)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name, "Name updated")
	assert.Equal(t, "preserve me", got.Extra, "Extra preserved across merge upsert")
}

func TestInsertMany_HappyPath(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("insertmany_happy"))

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30},
		{ID: "b", Name: "Bob", Age: 25},
		{ID: "c", Name: "Charlie", Age: 35},
	}
	n, err := col.InsertMany(ctx, items)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n, "all 3 items inserted")

	got, err := col.FindByID(ctx, "b")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Bob", got.Name)
}

func TestInsertMany_DuplicateKeyDetectableUnderUnordered(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	rawCol := db.Collection("insertmany_dup")
	col := NewCollection[testDoc](rawCol)

	// Pre-seed a doc with _id "b" so the second item collides.
	_, err := rawCol.InsertOne(ctx, testDoc{ID: "b", Name: "Pre", Age: 1})
	require.NoError(t, err)

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30},   // OK
		{ID: "b", Name: "Bob", Age: 25},     // collision on _id
		{ID: "c", Name: "Charlie", Age: 35}, // OK under unordered
	}
	n, err := col.InsertMany(ctx, items)
	require.Error(t, err)
	assert.True(t, mongo.IsDuplicateKeyError(err), "must be detectable via mongo.IsDuplicateKeyError")
	// Defense-in-depth: also assert errors.As resolves to BulkWriteException
	// so callers who walk the chain manually aren't surprised.
	var bwe mongo.BulkWriteException
	require.True(t, errors.As(err, &bwe), "wrapped error must unwrap to BulkWriteException")
	assert.Equal(t, int64(2), n, "two non-colliding inserts succeeded under unordered execution")

	// Under unordered, the non-colliding inserts succeeded.
	gotA, err := col.FindByID(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, gotA, "ok-1 should have been inserted under unordered execution")
	gotC, err := col.FindByID(ctx, "c")
	require.NoError(t, err)
	require.NotNil(t, gotC, "ok-2 should have been inserted under unordered execution")
}

func TestBulkUpsertByID_DelegatesToBulkUpsert(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_upsert_byid"))

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30},
		{ID: "b", Name: "Bob", Age: 25},
	}
	res, err := col.BulkUpsertByID(ctx, items, func(d testDoc) string { return d.ID })
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(2), res.Upserted)

	// Confirm the docs landed at the expected _id values.
	got, err := col.FindByID(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Alice", got.Name)
}
