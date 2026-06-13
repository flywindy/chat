//go:build integration

package roomkeystore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/testutil"
)

// setupMongo returns a Mongo-backed store over an isolated test database's
// rooms collection, plus the collection for direct fixture setup.
func setupMongo(t *testing.T, gracePeriod time.Duration) (*mongoStore, *mongo.Collection) {
	t.Helper()
	db := testutil.MongoDB(t, "roomkeystore")
	col := db.Collection("rooms")
	return newMongoStore(col, gracePeriod), col
}

// insertRoom creates a minimal room document so a key can be attached to it.
func insertRoom(t *testing.T, col *mongo.Collection, roomID string) {
	t.Helper()
	_, err := col.InsertOne(context.Background(), bson.M{"_id": roomID})
	require.NoError(t, err)
}

func TestMongoStore_Integration_RoundTrip(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-1")

	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PrivateKey: privKey}

	ver, err := store.Set(ctx, "room-1", pair)
	require.NoError(t, err)
	assert.Equal(t, 0, ver)

	got, err := store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 0, got.Version)
	assert.Equal(t, privKey, got.KeyPair.PrivateKey)

	err = store.Delete(ctx, "room-1")
	require.NoError(t, err)

	got, err = store.Get(ctx, "room-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMongoStore_Integration_SetRoomNotFound(t *testing.T) {
	store, _ := setupMongo(t, time.Hour)
	ctx := context.Background()

	_, err := store.Set(ctx, "ghost-room", RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0x01}, 32)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRoomNotFound), "Set on a missing room must return ErrRoomNotFound")

	err = store.SetWithVersion(ctx, "ghost-room", RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0x01}, 32)}, 3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRoomNotFound), "SetWithVersion on a missing room must return ErrRoomNotFound")
}

func TestMongoStore_Integration_SetWithVersion(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-replicated")

	privKey := bytes.Repeat([]byte{0xCD}, 32)
	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", RoomKeyPair{PrivateKey: privKey}, 7))

	got, err := store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Version, "version must match the caller-supplied value")
	assert.Equal(t, privKey, got.KeyPair.PrivateKey)

	// Overwriting at a higher version is allowed (idempotent for catch-up).
	newPriv := bytes.Repeat([]byte{0xEE}, 32)
	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", RoomKeyPair{PrivateKey: newPriv}, 9))
	got, err = store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 9, got.Version)
	assert.Equal(t, newPriv, got.KeyPair.PrivateKey)
}

func TestMongoStore_Integration_MissingKey(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()

	// Absent room.
	got, err := store.Get(ctx, "nonexistent-room")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on missing room must return nil, nil")

	// Room exists but has no key yet.
	insertRoom(t, col, "keyless-room")
	got, err = store.Get(ctx, "keyless-room")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on a keyless room must return nil, nil")
}

func TestMongoStore_Integration_RotateRoundTrip(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-rot")

	oldPriv := bytes.Repeat([]byte{0xBB}, 32)
	newPriv := bytes.Repeat([]byte{0xDD}, 32)

	ver, err := store.Set(ctx, "room-rot", RoomKeyPair{PrivateKey: oldPriv})
	require.NoError(t, err)
	assert.Equal(t, 0, ver)

	ver, err = store.Rotate(ctx, "room-rot", RoomKeyPair{PrivateKey: newPriv})
	require.NoError(t, err)
	assert.Equal(t, 1, ver)

	got, err := store.Get(ctx, "room-rot")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, newPriv, got.KeyPair.PrivateKey)

	oldPair, err := store.GetByVersion(ctx, "room-rot", 0)
	require.NoError(t, err)
	require.NotNil(t, oldPair)
	assert.Equal(t, oldPriv, oldPair.PrivateKey)

	newPair, err := store.GetByVersion(ctx, "room-rot", 1)
	require.NoError(t, err)
	require.NotNil(t, newPair)
	assert.Equal(t, newPriv, newPair.PrivateKey)

	unknown, err := store.GetByVersion(ctx, "room-rot", 999)
	require.NoError(t, err)
	assert.Nil(t, unknown)
}

func TestMongoStore_Integration_GracePeriodExpiry(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-grace")

	oldPriv := bytes.Repeat([]byte{0x02}, 32)
	newPriv := bytes.Repeat([]byte{0x04}, 32)

	_, err := store.Set(ctx, "room-grace", RoomKeyPair{PrivateKey: oldPriv})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-grace", RoomKeyPair{PrivateKey: newPriv})
	require.NoError(t, err)

	oldPair, err := store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	require.NotNil(t, oldPair, "old key should be retrievable during grace period")

	// Advance the store's clock past the grace period rather than sleeping.
	store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	oldPair, err = store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	assert.Nil(t, oldPair, "old key should be expired after grace period")

	got, err := store.Get(ctx, "room-grace")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
}

func TestMongoStore_Integration_RotateNoCurrentKey(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-empty")

	_, err := store.Rotate(ctx, "room-empty", RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0x02}, 32)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoCurrentKey), "should return ErrNoCurrentKey")
}

func TestMongoStore_Integration_DeleteBothKeys(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()
	insertRoom(t, col, "room-del")

	_, err := store.Set(ctx, "room-del", RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0xBB}, 32)})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-del", RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0xDD}, 32)})
	require.NoError(t, err)

	err = store.Delete(ctx, "room-del")
	require.NoError(t, err)

	got, err := store.Get(ctx, "room-del")
	require.NoError(t, err)
	assert.Nil(t, got)

	prev, err := store.GetByVersion(ctx, "room-del", 0)
	require.NoError(t, err)
	assert.Nil(t, prev)
}

func TestMongoStore_Integration_GetMany(t *testing.T) {
	store, col := setupMongo(t, time.Hour)
	ctx := context.Background()

	priv1 := bytes.Repeat([]byte{0x02}, 32)
	priv2 := bytes.Repeat([]byte{0x04}, 32)
	priv3 := bytes.Repeat([]byte{0x06}, 32)

	for _, id := range []string{"getmany-room-1", "getmany-room-2", "getmany-room-3", "getmany-keyless"} {
		insertRoom(t, col, id)
	}
	_, err := store.Set(ctx, "getmany-room-1", RoomKeyPair{PrivateKey: priv1})
	require.NoError(t, err)
	_, err = store.Set(ctx, "getmany-room-2", RoomKeyPair{PrivateKey: priv2})
	require.NoError(t, err)
	_, err = store.Set(ctx, "getmany-room-3", RoomKeyPair{PrivateKey: priv3})
	require.NoError(t, err)

	got, err := store.GetMany(ctx, []string{"getmany-room-1", "getmany-room-2", "getmany-room-3", "getmany-keyless", "getmany-room-missing"})
	require.NoError(t, err)
	require.Len(t, got, 3, "keyless and missing rooms must be omitted from result")

	require.Contains(t, got, "getmany-room-1")
	assert.Equal(t, 0, got["getmany-room-1"].Version)
	assert.Equal(t, priv1, got["getmany-room-1"].KeyPair.PrivateKey)

	require.Contains(t, got, "getmany-room-2")
	assert.Equal(t, priv2, got["getmany-room-2"].KeyPair.PrivateKey)

	require.Contains(t, got, "getmany-room-3")
	assert.Equal(t, priv3, got["getmany-room-3"].KeyPair.PrivateKey)

	_, missing := got["getmany-room-missing"]
	assert.False(t, missing)
	_, keyless := got["getmany-keyless"]
	assert.False(t, keyless)

	empty, err := store.GetMany(ctx, []string{})
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Empty(t, empty)
}
