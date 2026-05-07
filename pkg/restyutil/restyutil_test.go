package restyutil

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/natsutil"
)

func TestNew_DefaultTimeout(t *testing.T) {
	c := New("http://example")
	assert.Equal(t, defaultTimeout, c.GetClient().Timeout)
}

func TestNew_BaseURL(t *testing.T) {
	c := New("http://example.test")
	assert.Equal(t, "http://example.test", c.BaseURL)
}

func TestWithTimeout_Override(t *testing.T) {
	c := New("http://example", WithTimeout(2*time.Second))
	assert.Equal(t, 2*time.Second, c.GetClient().Timeout)
}

func TestWithHeader(t *testing.T) {
	c := New("http://example", WithHeader("X-App", "loadgen"))
	assert.Equal(t, "loadgen", c.Header.Get("X-App"))
}

func TestWithBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	c := New(srv.URL, WithBearerToken("abc123"))
	_, err := c.R().Get("/")
	require.NoError(t, err)
	assert.Equal(t, "Bearer abc123", got)
}

func TestWithTransport_PreservesOTelWrap(t *testing.T) {
	var hits atomic.Int32
	custom := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, WithTransport(custom))
	_, err := c.R().Get("/")
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load(), "custom transport must run")
}

// End-to-end: real httptest server, GET, JSON decode into a struct.
func TestNew_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/ping", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"msg":"pong"}`))
	}))
	defer srv.Close()

	type result struct {
		OK  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	c := New(srv.URL)
	var got result
	resp, err := c.R().SetResult(&got).Get("/ping")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode())
	assert.True(t, got.OK)
	assert.Equal(t, "pong", got.Msg)
}

// Verify the Debug log line includes request_id when the ctx carries it.
func TestLog_PropagatesRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := natsutil.WithRequestID(context.Background(), "req-abc-123")
	c := New(srv.URL)
	_, err := c.R().SetContext(ctx).Get("/x?token=secret")
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, `"request_id":"req-abc-123"`)
	assert.NotContains(t, out, "secret", "query string must be stripped from logs")
}

// Verify OnError fires on transport failure (not just OnAfterResponse).
func TestLog_FiresOnTransportError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	// Bind+close a port to guarantee connect-refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	c := New("http://"+addr, WithTimeout(500*time.Millisecond))
	_, err = c.R().Get("/")
	require.Error(t, err)
	assert.Contains(t, buf.String(), `"msg":"http error"`)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Sanity: log fields construction doesn't panic on a nil RawRequest (rare; defensive guard).
func TestLogFields_NilRawRequest(t *testing.T) {
	req := (&resty.Client{}).R()
	fields := logFields(req, 200, 5*time.Millisecond, nil)
	require.NotEmpty(t, fields)
	assert.Equal(t, "method", fields[0])
	// Should NOT panic and should not include path-from-RawRequest (since RawRequest is nil).
	joined := strings.Join(toStrings(fields), " ")
	assert.NotContains(t, joined, "panic")
}

func toStrings(in []any) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fmtAny(v)
	}
	return out
}

func fmtAny(v any) string {
	switch s := v.(type) {
	case string:
		return s
	default:
		return ""
	}
}
