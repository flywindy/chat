package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/natsutil"
)

// requestIDMiddleware extracts X-Request-ID (or mints via idgen) and stores it on Gin keys, c.Request.Context() via natsutil, and the response header.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(natsutil.RequestIDHeader)
		if !idgen.IsValidUUID(id) {
			id = idgen.GenerateRequestID()
		}
		c.Set("request_id", id)
		c.Request = c.Request.WithContext(natsutil.WithRequestID(c.Request.Context(), id))
		c.Header(natsutil.RequestIDHeader, id)
		c.Next()
	}
}

// corsMiddleware allows browser clients from any origin to call the API and
// short-circuits the preflight OPTIONS request with 204. The wildcard origin
// is incompatible with credentialed requests (cookies / Authorization), but
// /auth uses neither — it accepts a JSON body and returns a JWT.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		c.Header("Access-Control-Max-Age", "300")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// accessLogMiddleware logs method, path, status, and latency for each request.
func accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		slog.Info("request",
			"request_id", c.GetString("request_id"),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}
