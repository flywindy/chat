package emoji_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/emoji"
)

// countingLookup is a CustomEmojiLookup that records calls and returns scripted results.
type countingLookup struct {
	mu      sync.Mutex
	calls   map[string]int
	results map[string]bool
	err     error
	delay   time.Duration
	block   chan struct{} // when non-nil, blocks (unconditionally) before returning
	entered chan struct{} // when non-nil (buffered), signals once when entered
}

func newCountingLookup() *countingLookup {
	return &countingLookup{
		calls:   map[string]int{},
		results: map[string]bool{},
	}
}

func (l *countingLookup) CustomEmojiExists(ctx context.Context, siteID, shortcode string) (bool, error) {
	key := siteID + "|" + shortcode
	l.mu.Lock()
	l.calls[key]++
	l.mu.Unlock()
	if l.entered != nil {
		select {
		case l.entered <- struct{}{}:
		default:
		}
	}
	if l.block != nil {
		<-l.block
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if l.delay > 0 {
		time.Sleep(l.delay)
	}
	if l.err != nil {
		return false, l.err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.results[key], nil
}

func (l *countingLookup) callCount(siteID, shortcode string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[siteID+"|"+shortcode]
}

func TestNewCachedLookup_Validates(t *testing.T) {
	inner := newCountingLookup()

	_, err := emoji.NewCachedLookup(nil, 16, time.Minute)
	require.Error(t, err)

	_, err = emoji.NewCachedLookup(inner, 0, time.Minute)
	require.Error(t, err)

	_, err = emoji.NewCachedLookup(inner, -1, time.Minute)
	require.Error(t, err)

	_, err = emoji.NewCachedLookup(inner, 16, 0)
	require.Error(t, err)

	_, err = emoji.NewCachedLookup(inner, 16, -1*time.Second)
	require.Error(t, err)

	_, err = emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)
}

func TestCachedLookup_Miss_FetchesAndCaches(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	got, err := c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, 1, inner.callCount("site-a", "tada"))

	got, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, 1, inner.callCount("site-a", "tada"), "second call should hit cache, not inner")
}

func TestCachedLookup_NegativeResultCached(t *testing.T) {
	inner := newCountingLookup() // results map empty → returns false
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	got, err := c.CustomEmojiExists(context.Background(), "site-a", "unknown")
	require.NoError(t, err)
	assert.False(t, got)
	assert.Equal(t, 1, inner.callCount("site-a", "unknown"))

	got, err = c.CustomEmojiExists(context.Background(), "site-a", "unknown")
	require.NoError(t, err)
	assert.False(t, got)
	assert.Equal(t, 1, inner.callCount("site-a", "unknown"), "negative result must be cached too")
}

func TestCachedLookup_PerSiteIsolation(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|party"] = true
	inner.results["site-b|party"] = false
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	gotA, err := c.CustomEmojiExists(context.Background(), "site-a", "party")
	require.NoError(t, err)
	assert.True(t, gotA)

	gotB, err := c.CustomEmojiExists(context.Background(), "site-b", "party")
	require.NoError(t, err)
	assert.False(t, gotB, "same shortcode on different site must not share cache entry")
}

func TestCachedLookup_InnerErrorNotCached(t *testing.T) {
	inner := newCountingLookup()
	sentinel := errors.New("mongo down")
	inner.err = sentinel
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	_, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)

	inner.err = nil
	inner.results["site-a|tada"] = true
	got, err := c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.True(t, got, "error must not be cached; second call must reach inner")
	assert.Equal(t, 2, inner.callCount("site-a", "tada"))
}

func TestCachedLookup_TTLExpiry(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	c, err := emoji.NewCachedLookup(inner, 16, 50*time.Millisecond)
	require.NoError(t, err)

	_, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.Equal(t, 1, inner.callCount("site-a", "tada"))

	time.Sleep(100 * time.Millisecond)

	_, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.Equal(t, 2, inner.callCount("site-a", "tada"), "expired entry must refetch")
}

func TestCachedLookup_Singleflight_DedupesConcurrentMisses(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	inner.delay = 50 * time.Millisecond
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.CustomEmojiExists(context.Background(), "site-a", "tada")
			if err == nil && got {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(10), ok.Load())
	assert.Equal(t, 1, inner.callCount("site-a", "tada"), "10 concurrent misses must collapse to 1 inner call")
}

func TestCachedLookup_Invalidate(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	_, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	c.Invalidate("site-a", "tada")
	_, err = c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.Equal(t, 2, inner.callCount("site-a", "tada"))

	c.Invalidate("site-a", "never-cached")
}

func TestCachedLookup_LeaderCancelDoesNotPoisonWaiters(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	inner.block = make(chan struct{})
	inner.entered = make(chan struct{}, 1)
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, e := c.CustomEmojiExists(leaderCtx, "site-a", "tada")
		leaderDone <- e
	}()
	<-inner.entered // leader is inside the shared load, holding the singleflight key

	waiterReady := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterReady)
		_, e := c.CustomEmojiExists(context.Background(), "site-a", "tada")
		waiterDone <- e
	}()
	<-waiterReady // waiter goroutine is running and about to coalesce

	cancelLeader() // leader abandons via its own ctx
	require.ErrorIs(t, <-leaderDone, context.Canceled)
	close(inner.block) // release the (detached) shared load
	require.NoError(t, <-waiterDone, "waiter with a valid ctx must not be poisoned by the leader's cancel")

	// The shared load must have populated the cache: a fresh lookup does not hit inner.
	got, err := c.CustomEmojiExists(context.Background(), "site-a", "tada")
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, 1, inner.callCount("site-a", "tada"))
}

func TestCachedLookup_CallerCancelReturnsCtxErr(t *testing.T) {
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	inner.block = make(chan struct{})
	inner.entered = make(chan struct{}, 1)
	defer close(inner.block)
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := c.CustomEmojiExists(ctx, "site-a", "tada")
		done <- e
	}()
	<-inner.entered
	cancel()

	select {
	case e := <-done:
		require.ErrorIs(t, e, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return on its own ctx cancel within 2s (blocking Do?)")
	}
}
