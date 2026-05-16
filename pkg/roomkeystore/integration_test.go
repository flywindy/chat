//go:build integration

package roomkeystore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hmchangw/chat/pkg/testutil/testimages"
)

// setupValkey starts a valkey/valkey:8 container and returns a connected valkeyStore.
// The container is terminated via t.Cleanup.
func setupValkey(t *testing.T, gracePeriod time.Duration) RoomKeyStore {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        testimages.Valkey,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	require.NoError(t, err, "start valkey container")
	t.Cleanup(func() {
		_ = container.Terminate(ctx) // best-effort; ignore cleanup errors
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	store, err := NewValkeyStore(Config{
		Addr:        fmt.Sprintf("%s:%s", host, port.Port()),
		GracePeriod: gracePeriod,
	})
	require.NoError(t, err, "create valkeyStore")
	return store
}

func TestValkeyStore_Integration_RoundTrip(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	pubKey := bytes.Repeat([]byte{0xAB}, 65)
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PublicKey: pubKey, PrivateKey: privKey}

	// Set
	ver, err := store.Set(ctx, "room-1", pair)
	require.NoError(t, err)
	assert.Equal(t, 0, ver)

	// Get — should return the stored pair with version
	got, err := store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 0, got.Version)
	assert.Equal(t, pubKey, got.KeyPair.PublicKey)
	assert.Equal(t, privKey, got.KeyPair.PrivateKey)

	// Delete
	err = store.Delete(ctx, "room-1")
	require.NoError(t, err)

	// Get after delete — should return nil, nil
	got, err = store.Get(ctx, "room-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestValkeyStore_Integration_SetWithVersion(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	pubKey := bytes.Repeat([]byte{0xAB}, 65)
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PublicKey: pubKey, PrivateKey: privKey}

	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", pair, 7))

	got, err := store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Version, "version must match the caller-supplied value")
	assert.Equal(t, pubKey, got.KeyPair.PublicKey)
	assert.Equal(t, privKey, got.KeyPair.PrivateKey)

	// Overwriting at a higher version is allowed (idempotent for replication catch-up).
	newPub := bytes.Repeat([]byte{0xEE}, 65)
	require.NoError(t, store.SetWithVersion(ctx, "room-replicated", RoomKeyPair{PublicKey: newPub, PrivateKey: privKey}, 9))
	got, err = store.Get(ctx, "room-replicated")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 9, got.Version)
	assert.Equal(t, newPub, got.KeyPair.PublicKey)
}

func TestValkeyStore_Integration_MissingKey(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	got, err := store.Get(ctx, "nonexistent-room")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on missing key must return nil, nil")
}

func TestValkeyStore_Integration_RotateRoundTrip(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	oldPub := bytes.Repeat([]byte{0xAA}, 65)
	oldPriv := bytes.Repeat([]byte{0xBB}, 32)
	newPub := bytes.Repeat([]byte{0xCC}, 65)
	newPriv := bytes.Repeat([]byte{0xDD}, 32)

	// Set initial key pair.
	ver, err := store.Set(ctx, "room-rot", RoomKeyPair{PublicKey: oldPub, PrivateKey: oldPriv})
	require.NoError(t, err)
	assert.Equal(t, 0, ver)

	// Rotate to new key pair.
	ver, err = store.Rotate(ctx, "room-rot", RoomKeyPair{PublicKey: newPub, PrivateKey: newPriv})
	require.NoError(t, err)
	assert.Equal(t, 1, ver)

	// Get — should return new key pair as current.
	got, err := store.Get(ctx, "room-rot")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, newPub, got.KeyPair.PublicKey)
	assert.Equal(t, newPriv, got.KeyPair.PrivateKey)

	// GetByVersion with old version — should return old key pair from previous slot.
	oldPair, err := store.GetByVersion(ctx, "room-rot", 0)
	require.NoError(t, err)
	require.NotNil(t, oldPair)
	assert.Equal(t, oldPub, oldPair.PublicKey)
	assert.Equal(t, oldPriv, oldPair.PrivateKey)

	// GetByVersion with new version — should return new key pair from current slot.
	newPair, err := store.GetByVersion(ctx, "room-rot", 1)
	require.NoError(t, err)
	require.NotNil(t, newPair)
	assert.Equal(t, newPub, newPair.PublicKey)
	assert.Equal(t, newPriv, newPair.PrivateKey)

	// GetByVersion with unknown version — should return nil, nil.
	unknown, err := store.GetByVersion(ctx, "room-rot", 999)
	require.NoError(t, err)
	assert.Nil(t, unknown)
}

func TestValkeyStore_Integration_GracePeriodExpiry(t *testing.T) {
	// Use a 1-second grace period so the test completes quickly.
	store := setupValkey(t, 1*time.Second)
	ctx := context.Background()

	oldPub := bytes.Repeat([]byte{0x01}, 65)
	oldPriv := bytes.Repeat([]byte{0x02}, 32)
	newPub := bytes.Repeat([]byte{0x03}, 65)
	newPriv := bytes.Repeat([]byte{0x04}, 32)

	_, err := store.Set(ctx, "room-grace", RoomKeyPair{PublicKey: oldPub, PrivateKey: oldPriv})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-grace", RoomKeyPair{PublicKey: newPub, PrivateKey: newPriv})
	require.NoError(t, err)

	// Immediately after rotate, old key should still be retrievable.
	oldPair, err := store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	require.NotNil(t, oldPair, "old key should be retrievable during grace period")

	// Wait for grace period to elapse. This sleep is intentional — we are
	// waiting for an external Valkey TTL, not synchronising goroutines.
	time.Sleep(2 * time.Second)

	// Old key should now be expired.
	oldPair, err = store.GetByVersion(ctx, "room-grace", 0)
	require.NoError(t, err)
	assert.Nil(t, oldPair, "old key should be expired after grace period")

	// Current key should still be present (no TTL).
	got, err := store.Get(ctx, "room-grace")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
}

func TestValkeyStore_Integration_RotateNoCurrentKey(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	_, err := store.Rotate(ctx, "room-empty", RoomKeyPair{
		PublicKey:  bytes.Repeat([]byte{0x01}, 65),
		PrivateKey: bytes.Repeat([]byte{0x02}, 32),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoCurrentKey), "should return ErrNoCurrentKey")
}

func TestValkeyStore_Integration_DeleteBothKeys(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	// Set + Rotate to create both current and previous keys.
	_, err := store.Set(ctx, "room-del", RoomKeyPair{
		PublicKey:  bytes.Repeat([]byte{0xAA}, 65),
		PrivateKey: bytes.Repeat([]byte{0xBB}, 32),
	})
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "room-del", RoomKeyPair{
		PublicKey:  bytes.Repeat([]byte{0xCC}, 65),
		PrivateKey: bytes.Repeat([]byte{0xDD}, 32),
	})
	require.NoError(t, err)

	// Delete should remove both.
	err = store.Delete(ctx, "room-del")
	require.NoError(t, err)

	// Current key should be gone.
	got, err := store.Get(ctx, "room-del")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Previous key should also be gone.
	prev, err := store.GetByVersion(ctx, "room-del", 0)
	require.NoError(t, err)
	assert.Nil(t, prev)
}

func TestValkeyStore_Integration_GetMany(t *testing.T) {
	store := setupValkey(t, time.Hour)
	ctx := context.Background()

	pub1 := bytes.Repeat([]byte{0x01}, 65)
	priv1 := bytes.Repeat([]byte{0x02}, 32)
	pub2 := bytes.Repeat([]byte{0x03}, 65)
	priv2 := bytes.Repeat([]byte{0x04}, 32)
	pub3 := bytes.Repeat([]byte{0x05}, 65)
	priv3 := bytes.Repeat([]byte{0x06}, 32)

	_, err := store.Set(ctx, "room-1", RoomKeyPair{PublicKey: pub1, PrivateKey: priv1})
	require.NoError(t, err)
	_, err = store.Set(ctx, "room-2", RoomKeyPair{PublicKey: pub2, PrivateKey: priv2})
	require.NoError(t, err)
	_, err = store.Set(ctx, "room-3", RoomKeyPair{PublicKey: pub3, PrivateKey: priv3})
	require.NoError(t, err)

	got, err := store.GetMany(ctx, []string{"room-1", "room-2", "room-3", "room-missing"})
	require.NoError(t, err)
	require.Len(t, got, 3, "missing room must be omitted from result")

	require.Contains(t, got, "room-1")
	assert.Equal(t, 0, got["room-1"].Version)
	assert.Equal(t, pub1, got["room-1"].KeyPair.PublicKey)
	assert.Equal(t, priv1, got["room-1"].KeyPair.PrivateKey)

	require.Contains(t, got, "room-2")
	assert.Equal(t, 0, got["room-2"].Version)
	assert.Equal(t, pub2, got["room-2"].KeyPair.PublicKey)
	assert.Equal(t, priv2, got["room-2"].KeyPair.PrivateKey)

	require.Contains(t, got, "room-3")
	assert.Equal(t, 0, got["room-3"].Version)
	assert.Equal(t, pub3, got["room-3"].KeyPair.PublicKey)
	assert.Equal(t, priv3, got["room-3"].KeyPair.PrivateKey)

	_, missing := got["room-missing"]
	assert.False(t, missing, "room-missing must not be present in result")

	empty, err := store.GetMany(ctx, []string{})
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Empty(t, empty)
}
