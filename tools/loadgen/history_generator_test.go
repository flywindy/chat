package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model/cassandra"
)

// loadHistoryResponseTestShape is enough of the response for assertion-side
// JSON shaping: we only need Messages with CreatedAt populated.
type loadHistoryResponseTestShape struct {
	Messages []cassandra.Message `json:"messages"`
}

func TestParseEndpointMix(t *testing.T) {
	cases := []struct {
		in      string
		history int
		thread  int
		err     bool
	}{
		{"history:80,thread:20", 80, 20, false},
		{"history:100,thread:0", 100, 0, false},
		{"history:0,thread:100", 0, 100, false},
		{"history:50,thread:50", 50, 50, false},
		{"history:80", 0, 0, true}, // missing thread
		{"history:80,thread:20,extra:5", 0, 0, true},
		{"history:abc,thread:20", 0, 0, true},
		{"history:0,thread:0", 0, 0, true},    // both zero
		{"history:80,history:20", 0, 0, true}, // duplicate key
		{"thread:50,thread:50", 0, 0, true},   // duplicate key
		{"", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			mix, err := ParseEndpointMix(tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.history, mix.History)
			assert.Equal(t, tc.thread, mix.Thread)
		})
	}
}

func TestParseBeforeMode(t *testing.T) {
	cases := []struct {
		in     string
		open   int
		scroll int
		err    bool
	}{
		{"open:70,scrollback:30", 70, 30, false},
		{"open:100,scrollback:0", 100, 0, false},
		{"open:0,scrollback:100", 0, 100, false},
		{"open:50", 0, 0, true},
		{"open:0,scrollback:0", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			mode, err := ParseBeforeMode(tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.open, mode.Open)
			assert.Equal(t, tc.scroll, mode.Scrollback)
		})
	}
}

// fakeHistoryRequester records every request and replies with the configured
// payload (zero-latency unless configured).
type fakeHistoryRequester struct {
	mu       sync.Mutex
	requests []fakeRequest
	reply    []byte
	latency  time.Duration
	err      error
}

type fakeRequest struct {
	Subject string
	Data    []byte
}

func (f *fakeHistoryRequester) Request(ctx context.Context, subj string, data []byte, timeout time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Subject: subj, Data: append([]byte(nil), data...)})
	f.mu.Unlock()
	// Mirror natsHistoryRequester: wrap the parent ctx with the per-call
	// timeout so per-request DeadlineExceeded (timeout fired) is
	// distinguishable from parent Canceled (run shutdown).
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if f.latency > 0 {
		select {
		case <-time.After(f.latency):
		case <-reqCtx.Done():
			return nil, reqCtx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.reply == nil {
		// Default: empty LoadHistoryResponse so JSON parses successfully.
		return []byte(`{"messages":[]}`), nil
	}
	return f.reply, nil
}

func (f *fakeHistoryRequester) snapshot() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func makeHistoryGenCfg(t *testing.T, presetName string, req *fakeHistoryRequester, mix EndpointMix, beforeMode BeforeMode) (*HistoryGeneratorConfig, *HistoryFixtures) {
	t.Helper()
	p, ok := BuiltinHistoryPreset(presetName)
	require.True(t, ok)
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	res := BuildHistoryFixtures(&p, 42, "site-a", now)
	collector := NewHistoryCollector()
	return &HistoryGeneratorConfig{
		Preset:          &p,
		Fixtures:        &res,
		SiteID:          "site-a",
		Rate:            50,
		Mix:             mix,
		BeforeMode:      beforeMode,
		ScrollbackPages: 3,
		PageLimit:       20,
		RequestTimeout:  1 * time.Second,
		Requester:       req,
		Collector:       collector,
		MaxInFlight:     8,
	}, &res
}

func TestHistoryGenerator_PickRoom_ZipfSkew(t *testing.T) {
	// With many requests, the lowest-indexed rooms should dominate. We measure
	// "top-3 rooms account for more than 50% of picks" — a soft Zipf invariant
	// that holds for any reasonable Zipf shape on >= 5 rooms.
	gen, _ := newGenForRoomPickTest(t)
	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		counts[gen.pickRoom()]++
	}
	// Sort rooms by count desc.
	type kv struct {
		room  string
		count int
	}
	all := make([]kv, 0, len(counts))
	for r, c := range counts {
		all = append(all, kv{r, c})
	}
	// Top 1 must dominate top 5 by a wide margin under Zipf.
	max1 := 0
	for _, k := range all {
		if k.count > max1 {
			max1 = k.count
		}
	}
	assert.Greater(t, max1, 5000/len(all)*2, "Zipf should skew picks to a few rooms; got max=%d/%d", max1, 5000)
}

func newGenForRoomPickTest(t *testing.T) (*HistoryGenerator, *HistoryFixtures) {
	t.Helper()
	cfg, res := makeHistoryGenCfg(t, "history-small", &fakeHistoryRequester{}, EndpointMix{History: 100, Thread: 0}, BeforeMode{Open: 100})
	gen := NewHistoryGenerator(cfg, 1)
	return gen, res
}

func TestHistoryGenerator_EndpointMix_Respected(t *testing.T) {
	req := &fakeHistoryRequester{}
	cfg, _ := makeHistoryGenCfg(t, "history-medium", req, EndpointMix{History: 80, Thread: 20}, BeforeMode{Open: 100})
	cfg.Rate = 2000
	gen := NewHistoryGenerator(cfg, 7)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	reqs := req.snapshot()
	require.NotEmpty(t, reqs)

	historyHits, threadHits := 0, 0
	for _, r := range reqs {
		switch {
		case strings.HasSuffix(r.Subject, ".msg.history"):
			historyHits++
		case strings.HasSuffix(r.Subject, ".msg.thread"):
			threadHits++
		}
	}
	ratio := float64(historyHits) / float64(historyHits+threadHits)
	assert.InDelta(t, 0.80, ratio, 0.10, "history ratio outside tolerance (got %.2f)", ratio)
}

func TestHistoryGenerator_ThreadEndpoint_BuildsValidPayload(t *testing.T) {
	req := &fakeHistoryRequester{}
	cfg, res := makeHistoryGenCfg(t, "history-medium", req, EndpointMix{History: 0, Thread: 100}, BeforeMode{Open: 100})
	cfg.Rate = 1000
	gen := NewHistoryGenerator(cfg, 13)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	reqs := req.snapshot()
	require.NotEmpty(t, reqs)

	// Every thread request payload must carry a non-empty threadMessageId, and
	// that ID must exist in the fixture's ThreadParents.
	allParents := map[string]bool{}
	for _, refs := range res.ThreadParents {
		for _, ref := range refs {
			allParents[ref.MessageID] = true
		}
	}
	require.NotEmpty(t, allParents)

	for _, r := range reqs {
		if !strings.HasSuffix(r.Subject, ".msg.thread") {
			continue
		}
		var body getThreadMessagesRequest
		require.NoError(t, json.Unmarshal(r.Data, &body))
		require.NotEmpty(t, body.ThreadMessageID)
		assert.True(t, allParents[body.ThreadMessageID], "thread req references unknown parent %s", body.ThreadMessageID)
	}
}

func TestHistoryGenerator_BeforeMode_Open_NoCursor(t *testing.T) {
	req := &fakeHistoryRequester{}
	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 100}, BeforeMode{Open: 100})
	cfg.Rate = 500
	gen := NewHistoryGenerator(cfg, 21)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	reqs := req.snapshot()
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		var body loadHistoryRequest
		require.NoError(t, json.Unmarshal(r.Data, &body))
		assert.Nil(t, body.Before, "open-mode request must not set before")
		assert.Equal(t, cfg.PageLimit, body.Limit)
	}
}

func TestHistoryGenerator_BeforeMode_Scrollback_ChainsCursor(t *testing.T) {
	// Replier returns one message with a fixed createdAt so the scrollback
	// cursor advances. Run sequentially (rate=1, single-thread) so the chain
	// is observable in request order.
	createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	resp := loadHistoryResponseTestShape{Messages: []cassandra.Message{{CreatedAt: createdAt}}}
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	req := &fakeHistoryRequester{reply: body}

	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 100}, BeforeMode{Scrollback: 100})
	cfg.Rate = 200
	cfg.MaxInFlight = 0 // serial — chain is deterministic
	cfg.ScrollbackPages = 3
	gen := NewHistoryGenerator(cfg, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	reqs := req.snapshot()
	require.GreaterOrEqual(t, len(reqs), 6, "need multiple requests to observe chain")

	// Per (account, room) we expect the chain: first request has Before=nil
	// (page 1), then Before=createdAt (page 2, 3), then back to nil (chain
	// reset after ScrollbackPages).
	type chainKey struct{ account, room string }
	groupOrder := map[chainKey][]loadHistoryRequest{}
	for i := range reqs {
		r := reqs[i]
		var body loadHistoryRequest
		require.NoError(t, json.Unmarshal(r.Data, &body))
		// Extract account + room from subject:
		// chat.user.{account}.request.room.{roomID}.{siteID}.msg.history
		parts := strings.Split(r.Subject, ".")
		require.GreaterOrEqual(t, len(parts), 8)
		key := chainKey{account: parts[2], room: parts[5]}
		groupOrder[key] = append(groupOrder[key], body)
	}
	hadChainReset := false
	for _, chain := range groupOrder {
		if len(chain) < 4 {
			continue
		}
		// First in each chain must have nil Before.
		assert.Nil(t, chain[0].Before, "first req in chain must be open-mode")
		// Subsequent reqs (until ScrollbackPages) must have Before set.
		assert.NotNil(t, chain[1].Before)
		assert.NotNil(t, chain[2].Before)
		// 4th req resets the chain.
		if chain[3].Before == nil {
			hadChainReset = true
		}
	}
	assert.True(t, hadChainReset, "expected at least one chain to reset after ScrollbackPages")
}

func TestHistoryGenerator_RecordsLatencyAndPayload(t *testing.T) {
	body, err := json.Marshal(loadHistoryResponseTestShape{Messages: []cassandra.Message{}})
	require.NoError(t, err)
	req := &fakeHistoryRequester{reply: body}
	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 100}, BeforeMode{Open: 100})
	cfg.Rate = 200
	gen := NewHistoryGenerator(cfg, 33)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	samples := cfg.Collector.HistorySamples()
	require.NotEmpty(t, samples)
	// All samples must have non-negative latency and a positive payload size.
	for _, s := range samples {
		assert.GreaterOrEqual(t, s.Latency, time.Duration(0))
		assert.Greater(t, s.PayloadBytes, 0)
	}
}

func TestHistoryGenerator_RecordsErrors_OnTimeout(t *testing.T) {
	req := &fakeHistoryRequester{latency: 50 * time.Millisecond}
	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 100}, BeforeMode{Open: 100})
	cfg.Rate = 100
	cfg.RequestTimeout = 5 * time.Millisecond // forces every request to time out
	gen := NewHistoryGenerator(cfg, 44)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	assert.Greater(t, cfg.Collector.TimeoutErrors(), 0, "expected timeouts to be counted")
}

func TestHistoryGenerator_DoesNotCountShutdownCancelAsTimeout(t *testing.T) {
	// In-flight requests whose ctx is cancelled because the run is shutting
	// down must NOT inflate the timeout counter — that would make short test
	// runs (3-10 s) flake whenever a tail request was still in flight at the
	// deadline. Reproduces the CI failure on PR #230's first build.
	//
	// Setup: latency exceeds the run window, but RequestTimeout is generous
	// so no per-request timeout will fire. The only error path here is
	// context.Canceled from the outer ctx — which should be silently dropped.
	req := &fakeHistoryRequester{latency: 2 * time.Second}
	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 100}, BeforeMode{Open: 100})
	cfg.Rate = 100
	cfg.RequestTimeout = 30 * time.Second
	gen := NewHistoryGenerator(cfg, 88)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	assert.Equal(t, 0, cfg.Collector.TimeoutErrors(),
		"requests cancelled by run shutdown must not be counted as timeouts")
	assert.Equal(t, 0, cfg.Collector.ReplyErrors(),
		"shutdown cancellation is not a reply error either")
}

func TestHistoryGenerator_ThreadFallback_NoParentsInRoom(t *testing.T) {
	// history-small has ThreadRate=0, so ThreadParents is empty. Requesting
	// the thread endpoint must fall back to history (or be skipped) — count
	// the fallback metric.
	req := &fakeHistoryRequester{}
	cfg, _ := makeHistoryGenCfg(t, "history-small", req, EndpointMix{History: 0, Thread: 100}, BeforeMode{Open: 100})
	cfg.Rate = 200
	gen := NewHistoryGenerator(cfg, 55)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = gen.Run(ctx)

	reqs := req.snapshot()
	// Every issued request fell back to history.
	for _, r := range reqs {
		assert.True(t, strings.HasSuffix(r.Subject, ".msg.history"),
			"expected fallback to history endpoint, got %s", r.Subject)
	}
	assert.Greater(t, cfg.Collector.NoThreadParentsCount(), 0)
}

func TestHistoryGenerator_ConcurrencyDoesNotRace(t *testing.T) {
	// Run with the race detector enabled; failure is caught by go test -race.
	req := &fakeHistoryRequester{}
	cfg, _ := makeHistoryGenCfg(t, "history-medium", req, EndpointMix{History: 80, Thread: 20}, BeforeMode{Open: 70, Scrollback: 30})
	cfg.Rate = 2000
	cfg.MaxInFlight = 32
	gen := NewHistoryGenerator(cfg, 77)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var done atomic.Bool
	go func() {
		_ = gen.Run(ctx)
		done.Store(true)
	}()
	<-ctx.Done()
	// Wait deterministically for the goroutine to observe ctx cancellation
	// and exit — CLAUDE.md forbids time.Sleep for synchronization.
	require.Eventually(t, done.Load, 500*time.Millisecond, 5*time.Millisecond,
		"generator did not exit after ctx cancel")
}
