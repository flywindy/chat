package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/roomkeystore"
)

type fakeRoomKeyStore struct {
	sets    map[string]roomkeystore.RoomKeyPair
	deletes []string
	setErr  error
	delErr  error
}

func newFakeRoomKeyStore() *fakeRoomKeyStore {
	return &fakeRoomKeyStore{sets: map[string]roomkeystore.RoomKeyPair{}}
}

func (f *fakeRoomKeyStore) Set(_ context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error) {
	if f.setErr != nil {
		return 0, f.setErr
	}
	f.sets[roomID] = pair
	return 0, nil
}

func (f *fakeRoomKeyStore) Delete(_ context.Context, roomID string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deletes = append(f.deletes, roomID)
	return nil
}

func TestSeedRoomKeys_WritesOnePerRoom(t *testing.T) {
	ks := newFakeRoomKeyStore()
	keys := map[string]roomkeystore.RoomKeyPair{
		"room-a": {PrivateKey: []byte("privA")},
		"room-b": {PrivateKey: []byte("privB")},
		"room-c": {PrivateKey: []byte("privC")},
	}

	require.NoError(t, SeedRoomKeys(context.Background(), ks, keys))

	assert.Len(t, ks.sets, 3)
	assert.Equal(t, []byte("privA"), ks.sets["room-a"].PrivateKey)
	assert.Equal(t, []byte("privB"), ks.sets["room-b"].PrivateKey)
	assert.Equal(t, []byte("privC"), ks.sets["room-c"].PrivateKey)
}

func TestSeedRoomKeys_KeystoreError(t *testing.T) {
	ks := newFakeRoomKeyStore()
	ks.setErr = errors.New("boom")
	keys := map[string]roomkeystore.RoomKeyPair{"room-a": {PrivateKey: []byte("k")}}

	err := SeedRoomKeys(context.Background(), ks, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestSeedRoomKeys_NoRooms(t *testing.T) {
	ks := newFakeRoomKeyStore()
	require.NoError(t, SeedRoomKeys(context.Background(), ks, nil))
	assert.Empty(t, ks.sets)
}

func TestTeardownRoomKeys_DeletesEachRoom(t *testing.T) {
	ks := newFakeRoomKeyStore()
	require.NoError(t, TeardownRoomKeys(context.Background(), ks, []string{"room-a", "room-b"}))
	assert.Equal(t, []string{"room-a", "room-b"}, ks.deletes)
}

func TestTeardownRoomKeys_KeystoreError(t *testing.T) {
	ks := newFakeRoomKeyStore()
	ks.delErr = errors.New("nope")

	err := TeardownRoomKeys(context.Background(), ks, []string{"room-a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestTeardownRoomKeys_NoRooms(t *testing.T) {
	ks := newFakeRoomKeyStore()
	require.NoError(t, TeardownRoomKeys(context.Background(), ks, nil))
	assert.Empty(t, ks.deletes)
}
