package emoji

import (
	"context"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/hmchangw/chat/pkg/cachemetrics"
)

// Recorder records the outcome of a cache lookup. cachemetrics.Recorder
// satisfies it; tests substitute a spy.
type Recorder interface {
	Hit(ctx context.Context)
	Miss(ctx context.Context)
	Error(ctx context.Context)
}

// CachedLookup wraps a CustomEmojiLookup with a process-local LRU+TTL cache
// keyed by (siteID, shortcode). Negative results are cached too. Admin
// add/delete becomes visible at most TTL after the change (no active
// invalidation). Mirrors pkg/roommetacache.
type CachedLookup struct {
	inner CustomEmojiLookup
	lru   *lru.LRU[cacheKey, bool]
	sf    singleflight.Group

	metrics Recorder
}

type cacheKey struct {
	siteID    string
	shortcode string
}

// Option configures a CachedLookup at construction.
type Option func(*CachedLookup)

// WithMetrics overrides the hit/miss/error recorder. Defaults to the
// package-default cachemetrics recorder tagged cache="emoji",tier="l1".
func WithMetrics(r Recorder) Option {
	return func(c *CachedLookup) { c.metrics = r }
}

// String returns the canonical flat form for the singleflight dedup key;
// `\x00` separator is collision-free for ASCII siteIDs and validated shortcodes.
func (k cacheKey) String() string {
	return k.siteID + "\x00" + k.shortcode
}

// NewCachedLookup wraps inner with an LRU+TTL cache; size, ttl, inner all required.
func NewCachedLookup(inner CustomEmojiLookup, size int, ttl time.Duration, opts ...Option) (*CachedLookup, error) {
	if inner == nil {
		return nil, fmt.Errorf("emoji cached lookup: inner must not be nil")
	}
	if size <= 0 {
		return nil, fmt.Errorf("emoji cached lookup: size must be positive, got %d", size)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("emoji cached lookup: ttl must be positive, got %v", ttl)
	}
	c := &CachedLookup{
		inner:   inner,
		lru:     lru.NewLRU[cacheKey, bool](size, nil, ttl),
		metrics: cachemetrics.For("emoji", "l1"),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// CustomEmojiExists serves from cache; inner errors are not cached.
func (c *CachedLookup) CustomEmojiExists(ctx context.Context, siteID, shortcode string) (bool, error) {
	k := cacheKey{siteID: siteID, shortcode: shortcode}
	if v, ok := c.lru.Get(k); ok {
		c.metrics.Hit(ctx)
		return v, nil
	}

	v, err, _ := c.sf.Do(k.String(), func() (interface{}, error) {
		if cached, ok := c.lru.Get(k); ok {
			return cached, nil
		}
		exists, err := c.inner.CustomEmojiExists(ctx, siteID, shortcode)
		if err != nil {
			return false, err
		}
		c.lru.Add(k, exists)
		return exists, nil
	})
	if err != nil {
		c.metrics.Error(ctx)
		return false, fmt.Errorf("custom emoji lookup %q for site %q: %w", shortcode, siteID, err)
	}
	c.metrics.Miss(ctx)
	return v.(bool), nil
}

// Invalidate removes the cached entry for (siteID, shortcode); safe on a miss.
func (c *CachedLookup) Invalidate(siteID, shortcode string) {
	c.lru.Remove(cacheKey{siteID: siteID, shortcode: shortcode})
}
