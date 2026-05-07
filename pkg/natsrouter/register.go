package natsrouter

import (
	"encoding/json"
	"errors"
	"log/slog"
)

// Register subscribes a typed handler to a subject pattern.
// Handler receives *Context (implements context.Context) and unmarshalled request.
// Panics if subscription fails (startup-only, fatal).
func Register[Req, Resp any](
	r *Router,
	pattern string,
	fn func(c *Context, req Req) (*Resp, error),
) {
	handler := HandlerFunc(func(c *Context) {
		var req Req
		if err := json.Unmarshal(c.Msg.Data, &req); err != nil {
			c.ReplyError("invalid request payload")
			return
		}

		resp, err := fn(c, req)
		if err != nil {
			replyErr(c, err)
			return
		}

		c.ReplyJSON(resp)
	})

	r.addRoute(pattern, []HandlerFunc{handler})
}

// RegisterNoBody subscribes a handler that takes no request body.
func RegisterNoBody[Resp any](
	r *Router,
	pattern string,
	fn func(c *Context) (*Resp, error),
) {
	handler := HandlerFunc(func(c *Context) {
		resp, err := fn(c)
		if err != nil {
			replyErr(c, err)
			return
		}

		c.ReplyJSON(resp)
	})

	r.addRoute(pattern, []HandlerFunc{handler})
}

// RegisterVoid subscribes a handler that processes a request without replying.
func RegisterVoid[Req any](
	r *Router,
	pattern string,
	fn func(c *Context, req Req) error,
) {
	handler := HandlerFunc(func(c *Context) {
		var req Req
		if err := json.Unmarshal(c.Msg.Data, &req); err != nil {
			slog.Error("invalid payload in void handler", "error", err, "subject", c.Msg.Subject)
			return
		}

		if err := fn(c, req); err != nil {
			slog.Error("void handler error", "error", err, "subject", c.Msg.Subject)
		}
	})

	r.addRoute(pattern, []HandlerFunc{handler})
}

func replyErr(c *Context, err error) {
	var routeErr *RouteError
	if errors.As(err, &routeErr) {
		c.ReplyJSON(routeErr)
		return
	}
	slog.Error("handler error", "error", err, "subject", c.Msg.Subject)
	c.ReplyError("internal error")
}
