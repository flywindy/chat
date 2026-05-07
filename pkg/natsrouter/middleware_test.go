package natsrouter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerTimeout_SetsDeadline(t *testing.T) {
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	var observedDeadline time.Time
	var ok bool
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(50 * time.Millisecond),
		func(c *Context) {
			observedDeadline, ok = c.Deadline()
		},
	}
	c.Next()

	require.True(t, ok, "deadline must be set inside the chain")
	assert.WithinDuration(t, time.Now().Add(50*time.Millisecond), observedDeadline, 30*time.Millisecond)
}

func TestHandlerTimeout_DoneFiresAfterExpiry(t *testing.T) {
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(20 * time.Millisecond),
		func(c *Context) {
			// Generous outer budget (2s) to absorb CI scheduler jitter — the
			// 20ms timer is what we're verifying fires; the outer bound only
			// catches a totally broken implementation.
			select {
			case <-c.Done():
				// expected
			case <-time.After(2 * time.Second):
				t.Fatal("ctx.Done() did not fire within 2s after a 20ms timeout")
			}
		},
	}
	c.Next()
}

// TestHandlerTimeout_DoesNotCancelParentContext verifies that the
// timeout middleware's defer-cancel only cancels its own derived ctx,
// never the parent ctx supplied to the handler chain. The earlier
// name ("DoesNotLeakDeadlineToCallerAfterChainEnds") was misleading
// — what's actually being asserted is parent-ctx isolation.
func TestHandlerTimeout_DoesNotCancelParentContext(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	c := &Context{ctx: parent, chain: &chainState{index: -1}}
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(20 * time.Millisecond),
		func(c *Context) {
			// no-op, return immediately
		},
	}
	c.Next()
	// HandlerTimeout's `defer cancel()` only cancels the derived ctx it
	// installed via SetContext. The parent supplied at acquireContext
	// time must remain unaffected — verify that here.
	select {
	case <-parent.Done():
		t.Fatal("parent context must not be cancelled by HandlerTimeout")
	default:
	}
}
