package natsrouter

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/natsutil"
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

func TestRequireRequestID_ValidPasses(t *testing.T) {
	const id = "01970a4f-8c2d-7c9a-abcd-e0123456789f"
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{natsutil.RequestIDHeader: []string{id}}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	require.True(t, ran, "handler must run when request ID is a valid UUID")
	got, ok := c.Get(requestIDKey)
	require.True(t, ok)
	assert.Equal(t, id, got)
	assert.Equal(t, id, natsutil.RequestIDFromContext(c))
}

func TestRequireRequestID_MissingAborts(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "handler must NOT run when request ID is missing")
	assert.True(t, c.IsAborted())
	_, stamped := c.Get(requestIDKey)
	assert.False(t, stamped, "request ID must not be stamped on the abort path")
}

func TestRequireRequestID_InvalidAborts(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{natsutil.RequestIDHeader: []string{"not-a-uuid"}}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "handler must NOT run when request ID is malformed")
	assert.True(t, c.IsAborted())
	_, stamped := c.Get(requestIDKey)
	assert.False(t, stamped, "request ID must not be stamped on the abort path")
}

func TestRequireRequestID_NilMsgAborts(t *testing.T) {
	// NewContext-style test context leaves Msg nil; the middleware must abort
	// cleanly (no panic in errnats.Reply) rather than dereference a nil Msg.
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "handler must NOT run when Msg (and thus request ID) is absent")
	assert.True(t, c.IsAborted())
}
