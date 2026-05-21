package roomkeystore

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHashClient is a test double for hashCommander.
// It simulates an in-memory Valkey hash store with injectable per-method errors.
type fakeHashClient struct {
	store                map[string]map[string]string
	hsetErr              error
	hgetallErr           error
	hgetallCallCount     int // tracks number of hgetall calls made
	hgetallErrOnCall     int // if >0, hgetallErr fires only on this call number (1-based)
	hgetallManyCallCount int
	rotatePipelineErr    error
	deletePipelineErr    error
	closeErr             error
	closed               bool
}

func (f *fakeHashClient) hset(_ context.Context, key string, priv string) error {
	if f.hsetErr != nil {
		return f.hsetErr
	}
	if f.store == nil {
		f.store = make(map[string]map[string]string)
	}
	f.store[key] = map[string]string{"priv": priv, "ver": "0"}
	return nil
}

func (f *fakeHashClient) hsetWithVersion(_ context.Context, key string, priv string, version int) error {
	if f.hsetErr != nil {
		return f.hsetErr
	}
	if f.store == nil {
		f.store = make(map[string]map[string]string)
	}
	f.store[key] = map[string]string{"priv": priv, "ver": strconv.Itoa(version)}
	return nil
}

func (f *fakeHashClient) hgetall(_ context.Context, key string) (map[string]string, error) {
	f.hgetallCallCount++
	if f.hgetallErr != nil && (f.hgetallErrOnCall == 0 || f.hgetallCallCount == f.hgetallErrOnCall) {
		return nil, f.hgetallErr
	}
	if f.store == nil {
		return map[string]string{}, nil
	}
	m, ok := f.store[key]
	if !ok {
		return map[string]string{}, nil
	}
	return m, nil
}

func (f *fakeHashClient) hgetallMany(ctx context.Context, keys []string) ([]map[string]string, error) {
	f.hgetallManyCallCount++
	out := make([]map[string]string, len(keys))
	for i, k := range keys {
		m, err := f.hgetall(ctx, k)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func (f *fakeHashClient) rotatePipeline(_ context.Context, currentKey, prevKey string, priv string, _ time.Duration) (int, error) {
	if f.rotatePipelineErr != nil {
		return 0, f.rotatePipelineErr
	}
	if f.store == nil {
		return 0, ErrNoCurrentKey
	}
	cur, ok := f.store[currentKey]
	if !ok {
		return 0, ErrNoCurrentKey
	}
	curVer, _ := strconv.Atoi(cur["ver"])
	newVer := curVer + 1
	// Copy current to prev.
	f.store[prevKey] = map[string]string{"priv": cur["priv"], "ver": cur["ver"]}
	// Write new current.
	f.store[currentKey] = map[string]string{"priv": priv, "ver": strconv.Itoa(newVer)}
	return newVer, nil
}

func (f *fakeHashClient) deletePipeline(_ context.Context, currentKey, prevKey string) error {
	if f.deletePipelineErr != nil {
		return f.deletePipelineErr
	}
	if f.store != nil {
		delete(f.store, currentKey)
		delete(f.store, prevKey)
	}
	return nil
}

func (f *fakeHashClient) closeClient() error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed = true
	return nil
}

// newTestStore creates a valkeyStore backed by the given fake for unit tests.
func newTestStore(fake *fakeHashClient) *valkeyStore {
	return &valkeyStore{client: fake, gracePeriod: time.Hour}
}

func TestValkeyStore_Set(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PrivateKey: privKey}

	tests := []struct {
		name        string
		fake        *fakeHashClient
		roomID      string
		wantErr     bool
		errContains string
	}{
		{
			name:   "happy path — stores key pair with no TTL",
			fake:   &fakeHashClient{},
			roomID: "room-1",
		},
		{
			name:        "hset error — returns wrapped error",
			fake:        &fakeHashClient{hsetErr: errors.New("connection refused")},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "set room key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			ver, err := store.Set(context.Background(), tt.roomID, pair)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 0, ver)
			// Verify the hash was written under the correct Valkey key.
			stored := tt.fake.store[roomkey(tt.roomID)]
			require.NotNil(t, stored, "hash should exist in fake store")
			assert.NotEmpty(t, stored["priv"], "priv field should be set")
			assert.Equal(t, "0", stored["ver"], "ver field should be 0")
		})
	}
}

func TestValkeyStore_SetWithVersion(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	pair := RoomKeyPair{PrivateKey: privKey}

	t.Run("writes pair at supplied version", func(t *testing.T) {
		fake := &fakeHashClient{}
		s := newTestStore(fake)
		require.NoError(t, s.SetWithVersion(context.Background(), "r1", pair, 5))
		stored := fake.store[roomkey("r1")]
		require.NotNil(t, stored)
		assert.Equal(t, "5", stored["ver"])
		assert.NotEmpty(t, stored["priv"])
	})

	t.Run("overwrites existing version", func(t *testing.T) {
		fake := &fakeHashClient{}
		s := newTestStore(fake)
		require.NoError(t, s.SetWithVersion(context.Background(), "r1", pair, 2))
		require.NoError(t, s.SetWithVersion(context.Background(), "r1", pair, 7))
		stored := fake.store[roomkey("r1")]
		require.NotNil(t, stored)
		assert.Equal(t, "7", stored["ver"])
	})

	t.Run("propagates write error", func(t *testing.T) {
		want := errors.New("valkey down")
		fake := &fakeHashClient{hsetErr: want}
		s := newTestStore(fake)
		err := s.SetWithVersion(context.Background(), "r1", pair, 3)
		require.Error(t, err)
		assert.ErrorIs(t, err, want)
	})
}

func TestValkeyStore_Get(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)

	tests := []struct {
		name        string
		fake        *fakeHashClient
		roomID      string
		wantPair    *RoomKeyPair
		wantVer     int
		wantErr     bool
		errContains string
	}{
		{
			name: "happy path — returns VersionedKeyPair with correct Version",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				store := newTestStore(f)
				_, _ = store.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				return f
			}(),
			roomID:   "room-1",
			wantPair: &RoomKeyPair{PrivateKey: privKey},
			wantVer:  0,
		},
		{
			name:   "missing key — returns nil, nil",
			fake:   &fakeHashClient{},
			roomID: "nonexistent",
		},
		{
			name:        "hgetall error — returns wrapped error",
			fake:        &fakeHashClient{hgetallErr: errors.New("io timeout")},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "get room key",
		},
		{
			name: "corrupted priv base64 — returns error",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"priv": "!!!notbase64!!!", "ver": "0"},
				},
			},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "get room key",
		},
		{
			name: "non-numeric version — returns error containing parse version",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "not-a-number"},
				},
			},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "parse version",
		},
		{
			name: "old row with pub field — pub is ignored, priv is decoded",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"pub": "AQID", "priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "0"},
				},
			},
			roomID:   "room-1",
			wantPair: &RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0xCD}, 32)},
			wantVer:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			got, err := store.Get(context.Background(), tt.roomID)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tt.wantPair == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantVer, got.Version)
			assert.Equal(t, tt.wantPair.PrivateKey, got.KeyPair.PrivateKey)
		})
	}
}

func TestValkeyStore_GetByVersion(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	privKey2 := bytes.Repeat([]byte{0x22}, 32)

	tests := []struct {
		name        string
		fake        *fakeHashClient
		roomID      string
		version     int
		wantPair    *RoomKeyPair
		wantErr     bool
		errContains string
	}{
		{
			name: "matches current key",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				return f
			}(),
			roomID:   "room-1",
			version:  0,
			wantPair: &RoomKeyPair{PrivateKey: privKey},
		},
		{
			name: "matches previous key after rotation",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				_, _ = s.Rotate(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey2})
				return f
			}(),
			roomID:   "room-1",
			version:  0,
			wantPair: &RoomKeyPair{PrivateKey: privKey},
		},
		{
			name: "no match — returns nil, nil",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				return f
			}(),
			roomID:  "room-1",
			version: 999,
		},
		{
			name:    "no keys at all — returns nil, nil",
			fake:    &fakeHashClient{},
			roomID:  "room-1",
			version: 0,
		},
		{
			name:        "hgetall error on current key — returns wrapped error",
			fake:        &fakeHashClient{hgetallErr: errors.New("connection reset")},
			roomID:      "room-1",
			version:     0,
			wantErr:     true,
			errContains: "get room key by version",
		},
		{
			name: "hgetall error on previous key — returns wrapped error",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				// Set a current key with a different version so the code falls through to check previous.
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				f.hgetallErr = errors.New("connection reset")
				f.hgetallErrOnCall = 2 // error only on the second hgetall (previous key lookup)
				return f
			}(),
			roomID:      "room-1",
			version:     99,
			wantErr:     true,
			errContains: "get room key by version",
		},
		{
			name: "corrupted previous key base64 — returns error",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"):     {"priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "0"},
					roomprevkey("room-1"): {"priv": "!!!bad!!!", "ver": "99"},
				},
			},
			roomID:      "room-1",
			version:     99,
			wantErr:     true,
			errContains: "get room key by version",
		},
		{
			name: "corrupted current key base64 — returns error when version matches current",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"priv": "!!!bad!!!", "ver": "0"},
				},
			},
			roomID:      "room-1",
			version:     0,
			wantErr:     true,
			errContains: "get room key by version",
		},
		{
			name: "old row with pub field — pub is ignored, priv is decoded",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"pub": "AQID", "priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "0"},
				},
			},
			roomID:   "room-1",
			version:  0,
			wantPair: &RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0xCD}, 32)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			got, err := store.GetByVersion(context.Background(), tt.roomID, tt.version)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tt.wantPair == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantPair.PrivateKey, got.PrivateKey)
		})
	}
}

func TestValkeyStore_Rotate(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	newPrivKey := bytes.Repeat([]byte{0x22}, 32)

	tests := []struct {
		name        string
		fake        *fakeHashClient
		roomID      string
		setupFn     func(s *valkeyStore)
		wantVer     int
		wantErr     bool
		errContains string
		errIs       error
	}{
		{
			name:   "happy path — new pair becomes current, old becomes previous",
			fake:   &fakeHashClient{},
			roomID: "room-1",
			setupFn: func(s *valkeyStore) {
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
			},
			wantVer: 1,
		},
		{
			name:        "no current key — returns ErrNoCurrentKey",
			fake:        &fakeHashClient{},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "rotate room key",
			errIs:       ErrNoCurrentKey,
		},
		{
			name:   "replaces existing previous key",
			fake:   &fakeHashClient{},
			roomID: "room-1",
			setupFn: func(s *valkeyStore) {
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				// First rotation creates a previous key.
				_, _ = s.Rotate(context.Background(), "room-1", RoomKeyPair{PrivateKey: newPrivKey})
			},
			wantVer: 2,
		},
		{
			name:   "pipeline error — returns wrapped error",
			fake:   &fakeHashClient{rotatePipelineErr: errors.New("pipeline broken")},
			roomID: "room-1",
			setupFn: func(s *valkeyStore) {
				// Temporarily clear the error so Set works.
				s.client.(*fakeHashClient).rotatePipelineErr = nil
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				s.client.(*fakeHashClient).rotatePipelineErr = errors.New("pipeline broken")
			},
			wantErr:     true,
			errContains: "rotate room key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			if tt.setupFn != nil {
				tt.setupFn(store)
			}
			ver, err := store.Rotate(context.Background(), tt.roomID, RoomKeyPair{PrivateKey: newPrivKey})
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				if tt.errIs != nil {
					assert.True(t, errors.Is(err, tt.errIs), "expected errors.Is match for %v", tt.errIs)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantVer, ver)

			// Verify new pair is current with correct version.
			got, err := store.Get(context.Background(), tt.roomID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantVer, got.Version)
			assert.Equal(t, newPrivKey, got.KeyPair.PrivateKey)
		})
	}
}

func TestValkeyStore_Delete(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)

	tests := []struct {
		name        string
		fake        *fakeHashClient
		roomID      string
		wantErr     bool
		errContains string
	}{
		{
			name: "happy path — deletes both current and previous keys",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				_, _ = s.Rotate(context.Background(), "room-1", RoomKeyPair{
					PrivateKey: bytes.Repeat([]byte{0x22}, 32),
				})
				return f
			}(),
			roomID: "room-1",
		},
		{
			name:   "missing key — no-op, no error",
			fake:   &fakeHashClient{},
			roomID: "nonexistent",
		},
		{
			name:        "deletePipeline error — returns wrapped error",
			fake:        &fakeHashClient{deletePipelineErr: errors.New("connection lost")},
			roomID:      "room-1",
			wantErr:     true,
			errContains: "delete room key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			err := store.Delete(context.Background(), tt.roomID)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			require.NoError(t, err)
			// Verify both current and previous keys are gone.
			_, exists := tt.fake.store[roomkey(tt.roomID)]
			assert.False(t, exists, "current key should be removed after Delete")
			_, exists = tt.fake.store[roomprevkey(tt.roomID)]
			assert.False(t, exists, "previous key should be removed after Delete")
		})
	}
}

func TestValkeyStore_GetMany(t *testing.T) {
	privKey := bytes.Repeat([]byte{0xCD}, 32)
	privKey2 := bytes.Repeat([]byte{0x22}, 32)

	tests := []struct {
		name          string
		fake          *fakeHashClient
		roomIDs       []string
		wantLen       int
		wantRoomIDs   []string
		wantErr       bool
		errContains   string
		wantCallCount int // expected hgetallManyCallCount
	}{
		{
			name:          "empty input — empty map, no error, hgetallMany not called",
			fake:          &fakeHashClient{},
			roomIDs:       []string{},
			wantLen:       0,
			wantCallCount: 0,
		},
		{
			name: "all present — both rooms returned with Version==0",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				_, _ = s.Set(context.Background(), "room-2", RoomKeyPair{PrivateKey: privKey2})
				return f
			}(),
			roomIDs:       []string{"room-1", "room-2"},
			wantLen:       2,
			wantRoomIDs:   []string{"room-1", "room-2"},
			wantCallCount: 1,
		},
		{
			name:          "all absent — empty map",
			fake:          &fakeHashClient{},
			roomIDs:       []string{"room-1", "room-2"},
			wantLen:       0,
			wantCallCount: 1,
		},
		{
			name: "mixed — only present rooms in map",
			fake: func() *fakeHashClient {
				f := &fakeHashClient{}
				s := newTestStore(f)
				_, _ = s.Set(context.Background(), "room-1", RoomKeyPair{PrivateKey: privKey})
				_, _ = s.Set(context.Background(), "room-2", RoomKeyPair{PrivateKey: privKey2})
				return f
			}(),
			roomIDs:       []string{"room-1", "room-absent", "room-2"},
			wantLen:       2,
			wantRoomIDs:   []string{"room-1", "room-2"},
			wantCallCount: 1,
		},
		{
			name: "decode error — error containing room ID",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					// room-1 has a valid 32-byte secret so decoding succeeds.
					roomkey("room-1"): {"priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "0"},
					roomkey("room-2"): {"priv": "!!!notbase64!!!", "ver": "0"},
				},
			},
			roomIDs:       []string{"room-1", "room-2"},
			wantErr:       true,
			errContains:   "room-2",
			wantCallCount: 1,
		},
		{
			name: "version parse error — error containing room ID",
			fake: &fakeHashClient{
				store: map[string]map[string]string{
					roomkey("room-1"): {"priv": "zc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc3Nzc0=", "ver": "not-a-number"},
				},
			},
			roomIDs:       []string{"room-1"},
			wantErr:       true,
			errContains:   "room-1",
			wantCallCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(tt.fake)
			got, err := store.GetMany(context.Background(), tt.roomIDs)
			assert.Equal(t, tt.wantCallCount, tt.fake.hgetallManyCallCount)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, tt.wantLen)
			for _, id := range tt.wantRoomIDs {
				vkp, ok := got[id]
				require.True(t, ok, "expected room %s in result", id)
				assert.Equal(t, 0, vkp.Version)
			}
		})
	}
}
