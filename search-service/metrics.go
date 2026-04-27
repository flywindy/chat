package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hmchangw/chat/pkg/natsrouter"
)

// Collector names match the Observability → Metrics table in the spec.
// All collectors register with the default Prometheus registry via
// promauto so a plain promhttp.Handler() exposes them on /metrics.
var (
	metricRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "search_service_requests_total",
		Help: "Total NATS request/reply invocations handled, partitioned by endpoint and terminal status.",
	}, []string{"kind", "status"})

	metricRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "search_service_request_duration_seconds",
		Help:    "End-to-end handler latency in seconds, from NATS request receipt to response emission.",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})

	metricCacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "search_service_cache_hits_total",
		Help: "Restricted-rooms Valkey cache hits.",
	}, []string{"kind"})

	metricCacheMisses = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "search_service_cache_misses_total",
		Help: "Restricted-rooms Valkey cache misses (including transport errors that fall through to ES).",
	}, []string{"kind"})

	metricESDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "search_service_es_duration_seconds",
		Help:    "Elasticsearch call latency in seconds, partitioned by operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"op"})
)

// Metric label constants. Cardinality is fixed and bounded so every
// permutation can be pre-resolved at init (see below) to avoid a
// per-request map lookup inside `WithLabelValues`.
const (
	metricKindMessages = "messages"
	metricKindRooms    = "rooms"

	metricOpSearch      = "search"
	metricOpUserRoomGet = "user_room_get"
)

// Pre-resolved per-kind handles for the request-path metrics. The
// `status` label on requests_total is resolved lazily (5 values × 2
// kinds = 10 perms would clutter here); the others are fully bound.
var (
	durMessages = metricRequestDuration.WithLabelValues(metricKindMessages)
	durRooms    = metricRequestDuration.WithLabelValues(metricKindRooms)

	cacheHitMessages  = metricCacheHits.WithLabelValues(metricKindMessages)
	cacheHitRooms     = metricCacheHits.WithLabelValues(metricKindRooms)
	cacheMissMessages = metricCacheMisses.WithLabelValues(metricKindMessages)
	cacheMissRooms    = metricCacheMisses.WithLabelValues(metricKindRooms)

	esDurSearch      = metricESDuration.WithLabelValues(metricOpSearch)
	esDurUserRoomGet = metricESDuration.WithLabelValues(metricOpUserRoomGet)
)

// observeRequest captures a handler's total latency and terminal status.
// The status is classified at fire-time from the named `err` return, so
// late-bound error classification (wrapping, defer-assigned) is counted
// correctly. Usage:
//
//	func (h *handler) search(...) (resp *R, err error) {
//	    defer observeRequest(metricKindMessages, &err)()
//	    ...
//	}
func observeRequest(kind string, errPtr *error) func() {
	start := time.Now()
	dur := durFor(kind)
	return func() {
		dur.Observe(time.Since(start).Seconds())
		metricRequestsTotal.WithLabelValues(kind, statusLabel(*errPtr)).Inc()
	}
}

func observeES(op string) func() {
	start := time.Now()
	h := esDurFor(op)
	return func() { h.Observe(time.Since(start).Seconds()) }
}

// durFor / esDurFor / cacheHitFor / cacheMissFor return the pre-resolved
// observer for the given label. All four fall back to the messages/search
// variant on an unknown label so a caller typo surfaces as misattributed
// metrics rather than a nil-observer panic at fire time.
func durFor(kind string) prometheus.Observer {
	if kind == metricKindRooms {
		return durRooms
	}
	return durMessages
}

func esDurFor(op string) prometheus.Observer {
	if op == metricOpUserRoomGet {
		return esDurUserRoomGet
	}
	return esDurSearch
}

func cacheHitFor(kind string) prometheus.Counter {
	if kind == metricKindRooms {
		return cacheHitRooms
	}
	return cacheHitMessages
}

func cacheMissFor(kind string) prometheus.Counter {
	if kind == metricKindRooms {
		return cacheMissRooms
	}
	return cacheMissMessages
}

// statusLabel maps a handler's returned error onto the requests_total
// `status` label. nil → "ok"; non-internal RouteError → its Code
// (bad_request, not_found, forbidden, conflict) so operators can
// distinguish 4xx-equivalents; everything else → "internal".
func statusLabel(err error) string {
	if err == nil {
		return "ok"
	}
	var rerr *natsrouter.RouteError
	if errors.As(err, &rerr) && rerr.Code != "" && rerr.Code != natsrouter.CodeInternal {
		return rerr.Code
	}
	return natsrouter.CodeInternal
}

func metricsHandler() http.Handler { return promhttp.Handler() }
