package emoji_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/emoji"
)

// fakeRecorder counts cache outcomes for assertions.
type fakeRecorder struct{ hits, misses, errs int }

func (r *fakeRecorder) Hit(context.Context)   { r.hits++ }
func (r *fakeRecorder) Miss(context.Context)  { r.misses++ }
func (r *fakeRecorder) Error(context.Context) { r.errs++ }

func TestCachedLookup_Metrics_HitMissError(t *testing.T) {
	ctx := context.Background()
	inner := newCountingLookup()
	inner.results["site-a|tada"] = true
	rec := &fakeRecorder{}
	c, err := emoji.NewCachedLookup(inner, 16, time.Minute, emoji.WithMetrics(rec))
	require.NoError(t, err)

	// Miss: not cached, inner load succeeds.
	_, err = c.CustomEmojiExists(ctx, "site-a", "tada")
	require.NoError(t, err)
	assert.Equal(t, [3]int{0, 1, 0}, [3]int{rec.hits, rec.misses, rec.errs})

	// Hit.
	_, err = c.CustomEmojiExists(ctx, "site-a", "tada")
	require.NoError(t, err)
	assert.Equal(t, [3]int{1, 1, 0}, [3]int{rec.hits, rec.misses, rec.errs})

	// Error: inner load fails.
	inner.err = errors.New("mongo down")
	_, err = c.CustomEmojiExists(ctx, "site-a", "other")
	require.Error(t, err)
	assert.Equal(t, [3]int{1, 1, 1}, [3]int{rec.hits, rec.misses, rec.errs})
}

func TestCachedLookup_Metrics_DefaultRecorderNoPanic(t *testing.T) {
	c, err := emoji.NewCachedLookup(newCountingLookup(), 16, time.Minute)
	require.NoError(t, err)
	assert.NotPanics(t, func() { _, _ = c.CustomEmojiExists(context.Background(), "s", "x") })
}
