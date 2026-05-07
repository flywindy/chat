package natsrouter

import (
	"context"
	"log/slog"
	"time"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/natsutil"
)

// Middleware is a handler that participates in the middleware chain.
// Call c.Next() to continue, or return/call c.Abort() to short-circuit.
type Middleware = HandlerFunc

// requestIDKey is the context key used to store the request ID.
const requestIDKey = "requestID"

// RequestID returns middleware that extracts X-Request-ID (or mints via idgen) and stores it on both the natsrouter keys map and the underlying ctx.
func RequestID() HandlerFunc {
	return func(c *Context) {
		reqID := ""
		if c.Msg != nil && c.Msg.Header != nil {
			reqID = c.Msg.Header.Get(natsutil.RequestIDHeader)
		}
		if !idgen.IsValidUUID(reqID) {
			reqID = idgen.GenerateRequestID()
		}
		c.Set(requestIDKey, reqID)
		c.SetContext(natsutil.WithRequestID(c.ctx, reqID))
		c.Next()
	}
}

// requestAttrs returns common log attributes including the request ID if present.
func requestAttrs(c *Context) []any {
	var attrs []any
	if c.Msg != nil {
		attrs = append(attrs, "subject", c.Msg.Subject)
	}
	if id, ok := c.Get(requestIDKey); ok {
		attrs = append(attrs, "requestID", id)
	}
	return attrs
}

// Recovery returns middleware that catches panics, logs them, and replies with "internal error".
func Recovery() HandlerFunc {
	return func(c *Context) {
		defer func() {
			if r := recover(); r != nil {
				attrs := append(requestAttrs(c), "panic", r)
				slog.Error("panic recovered", attrs...)
				c.ReplyError("internal error")
				c.Abort()
			}
		}()
		c.Next()
	}
}

// Logging returns middleware that logs each request with subject, duration, and request ID.
func Logging() HandlerFunc {
	return func(c *Context) {
		start := time.Now()
		c.Next()
		attrs := append(requestAttrs(c), "duration", time.Since(start))
		slog.Info("nats request", attrs...)
	}
}

// HandlerTimeout returns middleware that wraps the handler context with a
// deadline of d. Downstream calls that respect context (Cassandra/Mongo
// drivers, otelnats.Conn.Publish, etc.) will abort if the chain runs longer
// than d. The deadline is released when the chain returns.
//
// Caveat — the timeout does NOT actively interrupt a running handler. A
// handler doing pure CPU work or calling a non-context-aware library will
// run to completion past the deadline, holding its admission slot the
// whole time. Make sure handlers either propagate ctx into every blocking
// call or are short by construction.
//
// Reply mapping — when a context-aware downstream call returns
// context.DeadlineExceeded and the handler returns
// `fmt.Errorf("...: %w", err)`, the router's replyErr path falls through
// to `"internal error"` (no RouteError match). Recommended pattern: in
// the handler, map the deadline-expired sentinel explicitly, e.g.
//
//	if errors.Is(err, context.DeadlineExceeded) {
//	    return nil, natsrouter.ErrUnavailable("request timed out")
//	}
//
// so the caller sees a structured "unavailable" code instead of a
// generic internal error.
//
// Place AFTER RequestID so the request ID is set before any
// timeout-related downstream work runs. Position relative to Logging
// does not affect what Logging records: Logging measures via
// `time.Since(start)` in its post-`c.Next()` phase, which captures the
// full chain duration regardless of where HandlerTimeout sits.
func HandlerTimeout(d time.Duration) HandlerFunc {
	return func(c *Context) {
		ctx, cancel := context.WithTimeout(c.ctx, d)
		defer cancel()
		c.SetContext(ctx)
		c.Next()
	}
}
