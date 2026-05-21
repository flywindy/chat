//go:build integration

package roomkeystore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

// setupValkey starts a cluster-mode Valkey container via testutil and returns
// a RoomKeyStore plus the raw *redis.ClusterClient (needed for CLUSTER KEYSLOT
// assertions). The container and client are terminated/closed via t.Cleanup.
func setupValkey(t *testing.T, gracePeriod time.Duration) (RoomKeyStore, *redis.ClusterClient) {
	t.Helper()
	c := testutil.StartValkeyCluster(t)
	store := &valkeyStore{
		client:      &clusterAdapter{c: c},
		closer:      c,
		gracePeriod: gracePeriod,
	}
	return store, c
}

func TestValkeyStore_Integration_RoundTrip(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

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

func TestValkeyStore_Integration_SetWithVersion(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PrivateKey: privKey}

	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", pair, 7))

	got, err := store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Version, "version must match the caller-supplied value")
	assert.Equal(t, privKey, got.KeyPair.PrivateKey)

	// Overwriting at a higher version is allowed (idempotent for replication catch-up).
	newPriv := bytes.Repeat([]byte{0xEE}, 32)
	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", RoomKeyPair{PrivateKey: newPriv}, 9))
	got, err = store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 9, got.Version)
	assert.Equal(t, newPriv, got.KeyPair.PrivateKey)
}

func TestValkeyStore_Integration_MissingKey(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	got, err := store.Get(ctx, "nonexistent-room")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on missing key must return nil, nil")
}

func TestValkeyStore_Integration_RotateRoundTrip(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	oldPriv := bytes.Repeat([]byte{0xBB}, 32)
	newPriv := bytes.Repeat([]byte{0xDD}, 32)

	// Set initial key pair.
	ver, err := store.Set(ctx, "room-rot", RoomKeyPair{PrivateKey: oldPriv})
	require.NoError(t, err)
	assert.Equal(t, 0, ver)

	// Rotate to new key pair.
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

func TestValkeyStore_Integration_GracePeriodExpiry(t *testing.T) {
	store, _ := setupValkey(t, 1*time.Second)
	ctx := context.Background()

	oldPriv := bytes.Repeat([]byte{0x02}, 32)
	newPriv := bytes.Repeat([]byte{0x04}, 32)

	_, err := store.Set(ctx, "room-grace", RoomKeyPair{PrivateKey: oldPriv})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-grace", RoomKeyPair{PrivateKey: newPriv})
	require.NoError(t, err)

	oldPair, err := store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	require.NotNil(t, oldPair, "old key should be retrievable during grace period")

	// Wait for grace period to elapse. This sleep is intentional — we are
	// waiting for an external Valkey TTL, not synchronising goroutines.
	time.Sleep(2 * time.Second)

	oldPair, err = store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	assert.Nil(t, oldPair, "old key should be expired after grace period")

	got, err := store.Get(ctx, "room-grace")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
}

func TestValkeyStore_Integration_RotateNoCurrentKey(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	_, err := store.Rotate(ctx, "room-empty", RoomKeyPair{
		PrivateKey: bytes.Repeat([]byte{0x02}, 32),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoCurrentKey), "should return ErrNoCurrentKey")
}

func TestValkeyStore_Integration_DeleteBothKeys(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	_, err := store.Set(ctx, "room-del", RoomKeyPair{
		PrivateKey: bytes.Repeat([]byte{0xBB}, 32),
	})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-del", RoomKeyPair{
		PrivateKey: bytes.Repeat([]byte{0xDD}, 32),
	})
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

func TestValkeyStore_Integration_GetMany(t *testing.T) {
	store, _ := setupValkey(t, time.Hour)
	ctx := context.Background()

	priv1 := bytes.Repeat([]byte{0x02}, 32)
	priv2 := bytes.Repeat([]byte{0x04}, 32)
	priv3 := bytes.Repeat([]byte{0x06}, 32)

	_, err := store.Set(ctx, "getmany-room-1", RoomKeyPair{PrivateKey: priv1})
	require.NoError(t, err)
	_, err = store.Set(ctx, "getmany-room-2", RoomKeyPair{PrivateKey: priv2})
	require.NoError(t, err)
	_, err = store.Set(ctx, "getmany-room-3", RoomKeyPair{PrivateKey: priv3})
	require.NoError(t, err)

	got, err := store.GetMany(ctx, []string{"getmany-room-1", "getmany-room-2", "getmany-room-3", "getmany-room-missing"})
	require.NoError(t, err)
	require.Len(t, got, 3, "missing room must be omitted from result")

	require.Contains(t, got, "getmany-room-1")
	assert.Equal(t, 0, got["getmany-room-1"].Version)
	assert.Equal(t, priv1, got["getmany-room-1"].KeyPair.PrivateKey)

	require.Contains(t, got, "getmany-room-2")
	assert.Equal(t, 0, got["getmany-room-2"].Version)
	assert.Equal(t, priv2, got["getmany-room-2"].KeyPair.PrivateKey)

	require.Contains(t, got, "getmany-room-3")
	assert.Equal(t, 0, got["getmany-room-3"].Version)
	assert.Equal(t, priv3, got["getmany-room-3"].KeyPair.PrivateKey)

	_, missing := got["getmany-room-missing"]
	assert.False(t, missing, "getmany-room-missing must not be present in result")

	empty, err := store.GetMany(ctx, []string{})
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Empty(t, empty)
}

// TestValkeyStore_Integration_HashTagSlotConsistency verifies that the
// {roomID} hash tag in both key names forces them onto the same cluster slot.
// Without hash tags the Lua rotate script would fail with a CROSSSLOT error.
func TestValkeyStore_Integration_HashTagSlotConsistency(t *testing.T) {
	store, c := setupValkey(t, time.Hour)
	ctx := context.Background()

	const roomID = "test-hash-tag-room"

	currentKey := roomkey(roomID)
	prevKey := roomprevkey(roomID)

	slotCurrent, err := c.Do(ctx, "CLUSTER", "KEYSLOT", currentKey).Int()
	require.NoError(t, err, "CLUSTER KEYSLOT current")
	slotPrev, err := c.Do(ctx, "CLUSTER", "KEYSLOT", prevKey).Int()
	require.NoError(t, err, "CLUSTER KEYSLOT prev")

	assert.Equal(t, slotCurrent, slotPrev,
		"current key %q and prev key %q must hash to the same cluster slot", currentKey, prevKey)

	_, err = store.Set(ctx, roomID, RoomKeyPair{
		PrivateKey: bytes.Repeat([]byte{0x02}, 32),
	})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, roomID, RoomKeyPair{
		PrivateKey: bytes.Repeat([]byte{0x04}, 32),
	})
	require.NoError(t, err, "rotate must not return CROSSSLOT — hash tags ensure both keys share a slot")
}
