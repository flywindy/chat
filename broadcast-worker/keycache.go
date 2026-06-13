package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/hmchangw/chat/pkg/roomkeystore"
)

// CachedKeyProvider wraps a RoomKeyProvider with a bounded in-process
// LRU+TTL cache. Concurrent misses on the same roomID are coalesced via
// singleflight so a hot room never triggers more than one in-flight inner
// fetch. The backing store is a size-capped expirable LRU (the same
// pattern as pkg/roommetacache), so memory is bounded by the configured
// size regardless of how many distinct rooms the worker sees over its
// lifetime — cold rooms are evicted by capacity or TTL rather than leaking.
//
// Staleness contract: after a key rotation, this cache keeps returning the
// previous version for up to TTL. That is safe only while TTL stays below
// the roomkeystore grace period — clients still hold the previous key
// during the grace window and can decrypt either version. main enforces
// this with keyCacheTTLSafe before wiring the cache.
//
// Negative results (nil, nil — meaning the room has no provisioned key)
// are deliberately not cached, so a freshly-provisioned key is picked up
// on the next call rather than being shadowed for up to TTL.
type CachedKeyProvider struct {
	inner RoomKeyProvider
	lru   *lru.LRU[string, *roomkeystore.VersionedKeyPair]
	sf    singleflight.Group

	hits   atomic.Int64
	misses atomic.Int64
}

// NewCachedKeyProvider returns a cache wrapping inner with the given
// capacity and TTL. size and ttl must both be positive; main wires the
// cache only when both are configured > 0.
func NewCachedKeyProvider(inner RoomKeyProvider, size int, ttl time.Duration) *CachedKeyProvider {
	return &CachedKeyProvider{
		inner: inner,
		lru:   lru.NewLRU[string, *roomkeystore.VersionedKeyPair](size, nil, ttl),
	}
}

// Get returns the room's current key, serving from cache when fresh and
// falling through to the inner store on miss or expiry.
//
// The shared fetch detaches from the caller's cancellation so that a
// per-caller cancel does not abort the in-flight inner Get for other
// callers waiting on the same room. Each caller still observes its own
// ctx.Done() via the select below and can give up independently.
func (c *CachedKeyProvider) Get(ctx context.Context, roomID string) (*roomkeystore.VersionedKeyPair, error) {
	if key, ok := c.lru.Get(roomID); ok {
		c.hits.Add(1)
		return key, nil
	}
	c.misses.Add(1)

	resCh := c.sf.DoChan(roomID, func() (any, error) {
		// Re-check under the singleflight gate: another flight may have
		// just populated the cache for this room between our initial
		// miss and this point.
		if key, ok := c.lru.Get(roomID); ok {
			return key, nil
		}
		// WithoutCancel preserves trace/log values but strips cancellation
		// so one caller's ctx cancel cannot poison the herd.
		fetchCtx := context.WithoutCancel(ctx)
		key, err := c.inner.Get(fetchCtx, roomID)
		if err != nil {
			return nil, fmt.Errorf("get room key for %q: %w", roomID, err)
		}
		if key == nil {
			return nil, nil
		}
		c.lru.Add(roomID, key)
		return key, nil
	})

	select {
	case res := <-resCh:
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Val == nil {
			return nil, nil
		}
		return res.Val.(*roomkeystore.VersionedKeyPair), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stats returns the current hit/miss counts without resetting them.
func (c *CachedKeyProvider) stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// snapshot returns and resets the hit/miss counters, so the next call
// observes a fresh window.
func (c *CachedKeyProvider) snapshot() (hits, misses int64) {
	return c.hits.Swap(0), c.misses.Swap(0)
}

// RunStatsLogger emits a slog summary of cache hits/misses/rate every
// interval until ctx is canceled. Each line summarizes the window since
// the previous tick; counters are reset on every emit. Callers spawn
// this in a goroutine and cancel ctx during shutdown.
func (c *CachedKeyProvider) RunStatsLogger(ctx context.Context, interval time.Duration) {
	c.runStatsLogger(ctx, interval, slog.Default())
}

func (c *CachedKeyProvider) runStatsLogger(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hits, misses := c.snapshot()
			total := hits + misses
			rate := "n/a"
			if total > 0 {
				rate = fmt.Sprintf("%.2f%%", float64(hits)/float64(total)*100)
			}
			logger.Info("room-key cache",
				"hits", hits,
				"misses", misses,
				"hit_rate", rate,
				"size", c.lru.Len(),
				"window", interval,
			)
		}
	}
}

// keyCacheTTLSafe reports whether a cache TTL is safe to use given the key
// rotation grace period. Serving a cached key for longer than the grace
// period risks handing out a version the store has already retired from its
// previous-key slot, which clients can no longer decrypt. The cache TTL
// must therefore stay strictly below the grace period (and be positive).
func keyCacheTTLSafe(ttl, grace time.Duration) bool {
	return ttl > 0 && ttl < grace
}
