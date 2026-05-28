package main

import (
	"sync"
	"time"
)

// HistoryEndpoint enumerates the request types the workload exercises.
type HistoryEndpoint string

const (
	HistoryEndpointHistory HistoryEndpoint = "history"
	HistoryEndpointThread  HistoryEndpoint = "thread"
)

// errClass labels per-request failure modes for tally + summary reporting.
type errClass string

const (
	errClassTimeout    errClass = "timeout"
	errClassReply      errClass = "reply"
	errClassBadReply   errClass = "bad_reply"
	errClassBadRequest errClass = "bad_request"
)

// HistorySample captures one completed request's per-call signal.
type HistorySample struct {
	Endpoint     HistoryEndpoint
	Latency      time.Duration
	PayloadBytes int
	At           time.Time
	// PageDepthMs is the spread of createdAt across the reply page (in ms),
	// a coarse proxy for how many Cassandra buckets the walker traversed.
	// Populated only on scrollback-mode history requests.
	PageDepthMs int64
}

// HistoryCollector aggregates samples and errors across the workload run.
// All public methods are safe for concurrent use.
type HistoryCollector struct {
	mu              sync.Mutex
	historySamples  []HistorySample
	threadSamples   []HistorySample
	errors          map[HistoryEndpoint]map[errClass]int
	noThreadParents int
	saturation      int
}

// NewHistoryCollector returns an empty collector.
func NewHistoryCollector() *HistoryCollector {
	return &HistoryCollector{
		errors: map[HistoryEndpoint]map[errClass]int{
			HistoryEndpointHistory: {},
			HistoryEndpointThread:  {},
		},
	}
}

// RecordSample stores one completed-call sample.
func (c *HistoryCollector) RecordSample(s HistorySample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch s.Endpoint {
	case HistoryEndpointThread:
		c.threadSamples = append(c.threadSamples, s)
	default:
		c.historySamples = append(c.historySamples, s)
	}
}

// RecordError tallies a per-class error against the endpoint.
func (c *HistoryCollector) RecordError(endpoint HistoryEndpoint, class errClass, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errors[endpoint] == nil {
		c.errors[endpoint] = map[errClass]int{}
	}
	c.errors[endpoint][class]++
}

// RecordBadRequest tallies a request that failed before issue (e.g. marshal).
func (c *HistoryCollector) RecordBadRequest(endpoint HistoryEndpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errors[endpoint] == nil {
		c.errors[endpoint] = map[errClass]int{}
	}
	c.errors[endpoint][errClassBadRequest]++
}

// RecordNoThreadParents tallies a thread request that fell back because the
// chosen room has no thread parents in the fixture.
func (c *HistoryCollector) RecordNoThreadParents() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noThreadParents++
}

// RecordSaturation tallies a tick that fired while the in-flight semaphore was
// full.
func (c *HistoryCollector) RecordSaturation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saturation++
}

// HistorySamples returns a defensive copy of the LoadHistory sample tape.
func (c *HistoryCollector) HistorySamples() []HistorySample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HistorySample, len(c.historySamples))
	copy(out, c.historySamples)
	return out
}

// ThreadSamples returns a defensive copy of the GetThreadMessages sample tape.
func (c *HistoryCollector) ThreadSamples() []HistorySample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HistorySample, len(c.threadSamples))
	copy(out, c.threadSamples)
	return out
}

// TimeoutErrors returns the aggregate timeout-class error count across all endpoints.
func (c *HistoryCollector) TimeoutErrors() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, byClass := range c.errors {
		total += byClass[errClassTimeout]
	}
	return total
}

// ReplyErrors returns the aggregate reply-class error count across all endpoints.
func (c *HistoryCollector) ReplyErrors() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, byClass := range c.errors {
		total += byClass[errClassReply]
	}
	return total
}

// BadReplyCount returns the count of replies that failed to JSON-decode.
func (c *HistoryCollector) BadReplyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, byClass := range c.errors {
		total += byClass[errClassBadReply]
	}
	return total
}

// NoThreadParentsCount returns the count of thread fallbacks.
func (c *HistoryCollector) NoThreadParentsCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noThreadParents
}

// SaturationCount returns the count of saturation events.
func (c *HistoryCollector) SaturationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saturation
}

// DiscardBefore drops samples whose At < cutoff. Used to exclude the warmup
// window from the published statistics.
func (c *HistoryCollector) DiscardBefore(cutoff time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.historySamples = filterAfter(c.historySamples, cutoff)
	c.threadSamples = filterAfter(c.threadSamples, cutoff)
}

func filterAfter(samples []HistorySample, cutoff time.Time) []HistorySample {
	out := samples[:0]
	for _, s := range samples {
		if !s.At.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}
