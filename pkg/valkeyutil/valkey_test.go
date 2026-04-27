package valkeyutil_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/valkeyutil"
)

type fakeClient struct {
	store       map[string]string
	ttls        map[string]time.Duration
	setErr      error
	getErr      error
	delCalls    [][]string
	closeCalled int
	closeErr    error
}

func newFake() *fakeClient {
	return &fakeClient{
		store: make(map[string]string),
		ttls:  make(map[string]time.Duration),
	}
}

func (f *fakeClient) Get(_ context.Context, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.store[key]
	if !ok {
		return "", valkeyutil.ErrCacheMiss
	}
	return v, nil
}

func (f *fakeClient) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.store[key] = value
	f.ttls[key] = ttl
	return nil
}

func (f *fakeClient) Del(_ context.Context, keys ...string) error {
	f.delCalls = append(f.delCalls, keys)
	for _, k := range keys {
		delete(f.store, k)
		delete(f.ttls, k)
	}
	return nil
}

func (f *fakeClient) Close() error {
	f.closeCalled++
	return f.closeErr
}

type cached struct {
	Rooms []string `json:"rooms"`
}

func TestGetJSON_HitAndMiss(t *testing.T) {
	ctx := context.Background()
	client := newFake()

	t.Run("miss returns ErrCacheMiss", func(t *testing.T) {
		var out cached
		err := valkeyutil.GetJSON(ctx, client, "missing", &out)
		assert.ErrorIs(t, err, valkeyutil.ErrCacheMiss)
	})

	t.Run("hit decodes JSON", func(t *testing.T) {
		require.NoError(t, valkeyutil.SetJSONWithTTL(ctx, client, "k1", cached{Rooms: []string{"r1", "r2"}}, 5*time.Minute))
		var out cached
		require.NoError(t, valkeyutil.GetJSON(ctx, client, "k1", &out))
		assert.Equal(t, []string{"r1", "r2"}, out.Rooms)
	})

	t.Run("Set persists TTL", func(t *testing.T) {
		require.NoError(t, valkeyutil.SetJSONWithTTL(ctx, client, "k2", cached{}, 30*time.Second))
		assert.Equal(t, 30*time.Second, client.ttls["k2"])
	})

	t.Run("malformed JSON wraps unmarshal error", func(t *testing.T) {
		client.store["bad"] = "{not json"
		var out cached
		err := valkeyutil.GetJSON(ctx, client, "bad", &out)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, valkeyutil.ErrCacheMiss)
	})

	t.Run("transport error propagates", func(t *testing.T) {
		broken := newFake()
		broken.getErr = errors.New("boom")
		var out cached
		err := valkeyutil.GetJSON(ctx, broken, "k", &out)
		assert.Error(t, err)
		assert.NotErrorIs(t, err, valkeyutil.ErrCacheMiss)
	})
}

func TestDisconnect(t *testing.T) {
	t.Run("nil is safe", func(t *testing.T) {
		valkeyutil.Disconnect(nil)
	})

	t.Run("happy path calls Close once", func(t *testing.T) {
		f := newFake()
		valkeyutil.Disconnect(f)
		assert.Equal(t, 1, f.closeCalled)
	})

	t.Run("Close error is logged but does not panic", func(t *testing.T) {
		f := newFake()
		f.closeErr = errors.New("close failed")
		// Disconnect swallows the error (logs only). The assertion is
		// that Close WAS called — the error path doesn't skip or retry.
		valkeyutil.Disconnect(f)
		assert.Equal(t, 1, f.closeCalled)
	})
}

func TestConnect_ErrorPath(t *testing.T) {
	// Point at a port that refuses connections (port 1 is well-known to
	// reject without a listener). The internal Ping must fail fast and
	// Connect must return a wrapped error — no real Valkey needed.
	_, err := valkeyutil.Connect(context.Background(), "127.0.0.1:1", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "valkey connect")
}
