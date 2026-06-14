package userstore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/hmchangw/chat/pkg/model"
)

// Cache fronts a UserStore with two LRU+TTL stores (byID, byAccount) sharing
// value pointers; populate writes to both so a hit on either satisfies the
// other. Singleflight collapses concurrent misses. Pod-local: entries ~500B,
// few-MB working set, writes rare — Valkey buys nothing at this size.
type Cache struct {
	byID      *lru.LRU[string, *model.User]
	byAccount *lru.LRU[string, *model.User]
	store     UserStore
	sf        singleflight.Group

	hits     atomic.Uint64
	misses   atomic.Uint64
	loadErrs atomic.Uint64
}

// Stats is a snapshot of the cache's counters.
type Stats struct {
	Hits, Misses, LoadErrors uint64
	SizeByID, SizeByAccount  int
}

// NewCache returns a Cache. size applies to each LRU independently; ttl applies to both.
func NewCache(store UserStore, size int, ttl time.Duration) (*Cache, error) {
	if store == nil {
		return nil, fmt.Errorf("userstore: store must not be nil")
	}
	if size <= 0 {
		return nil, fmt.Errorf("userstore: cache size must be positive, got %d", size)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("userstore: cache ttl must be positive, got %v", ttl)
	}
	return &Cache{
		byID:      lru.NewLRU[string, *model.User](size, nil, ttl),
		byAccount: lru.NewLRU[string, *model.User](size, nil, ttl),
		store:     store,
	}, nil
}

// FindUserByID serves from byID; misses fall through. ErrUserNotFound is not negatively cached.
func (c *Cache) FindUserByID(ctx context.Context, id string) (*model.User, error) {
	if v, ok := c.byID.Get(id); ok {
		c.hits.Add(1)
		return v, nil
	}
	c.misses.Add(1)
	v, err, _ := c.sf.Do(id, func() (interface{}, error) {
		if cached, ok := c.byID.Get(id); ok {
			return cached, nil
		}
		u, err := c.store.FindUserByID(ctx, id)
		if err != nil {
			return nil, err
		}
		c.populate(u)
		return u, nil
	})
	if err != nil {
		c.loadErrs.Add(1)
		if errors.Is(err, ErrUserNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("find cached user %q: %w", id, err)
	}
	return v.(*model.User), nil
}

// FindUserByAccount serves from byAccount; cross-populates byID; SF key "account:"+account avoids ID collision.
func (c *Cache) FindUserByAccount(ctx context.Context, account string) (*model.User, error) {
	if v, ok := c.byAccount.Get(account); ok {
		c.hits.Add(1)
		return v, nil
	}
	c.misses.Add(1)
	v, err, _ := c.sf.Do("account:"+account, func() (interface{}, error) {
		if cached, ok := c.byAccount.Get(account); ok {
			return cached, nil
		}
		u, err := c.store.FindUserByAccount(ctx, account)
		if err != nil {
			return nil, err
		}
		c.populate(u)
		return u, nil
	})
	if err != nil {
		c.loadErrs.Add(1)
		if errors.Is(err, ErrUserNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("find cached user by account %q: %w", account, err)
	}
	return v.(*model.User), nil
}

// FindUsersByAccounts hits byAccount; misses forwarded in one call; input deduped; partial hits on store errors.
func (c *Cache) FindUsersByAccounts(ctx context.Context, accounts []string) ([]model.User, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(accounts))
	hits := make([]model.User, 0, len(accounts))
	missing := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		if u, ok := c.byAccount.Get(a); ok {
			c.hits.Add(1)
			hits = append(hits, *u)
			continue
		}
		c.misses.Add(1)
		missing = append(missing, a)
	}
	if len(missing) == 0 {
		return hits, nil
	}
	fresh, err := c.store.FindUsersByAccounts(ctx, missing)
	if err != nil {
		return hits, fmt.Errorf("cached find users by accounts: %w", err)
	}
	for i := range fresh {
		c.populate(&fresh[i])
	}
	return append(hits, fresh...), nil
}

// populate writes the user (same pointer) under both byID and byAccount LRUs.
func (c *Cache) populate(u *model.User) {
	if u == nil {
		return
	}
	c.byID.Add(u.ID, u)
	if u.Account != "" {
		c.byAccount.Add(u.Account, u)
	}
}

// Stats returns a snapshot of cache counters.
func (c *Cache) Stats() Stats {
	return Stats{
		Hits:          c.hits.Load(),
		Misses:        c.misses.Load(),
		LoadErrors:    c.loadErrs.Load(),
		SizeByID:      c.byID.Len(),
		SizeByAccount: c.byAccount.Len(),
	}
}

// Invalidate drops cached entries; empty userID or account skips that LRU.
func (c *Cache) Invalidate(userID, account string) {
	if userID != "" {
		c.byID.Remove(userID)
	}
	if account != "" {
		c.byAccount.Remove(account)
	}
}
