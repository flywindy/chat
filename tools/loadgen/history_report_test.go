package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrintHistorySummary_RendersAllSections(t *testing.T) {
	s := HistorySummary{
		Preset: "history-medium", Site: "site-a", Seed: 42,
		TargetRate: 200, ActualRate: 198.7,
		Duration: 60 * time.Second, Warmup: 10 * time.Second,
		Sent: 12000, SentMeasured: 10000,
		Mix:        EndpointMix{History: 80, Thread: 20},
		BeforeMode: BeforeMode{Open: 70, Scrollback: 30},
		PageLimit:  20,
		Endpoints: []HistoryEndpointStat{
			{Endpoint: HistoryEndpointHistory, Count: 7991,
				Latency: Percentiles{P50: 4 * time.Millisecond, P95: 12 * time.Millisecond, P99: 28 * time.Millisecond, Max: 84 * time.Millisecond}, PayloadP50: 4312, PayloadP95: 18902},
			{Endpoint: HistoryEndpointThread, Count: 1996,
				Latency: Percentiles{P50: 3 * time.Millisecond, P95: 9 * time.Millisecond, P99: 22 * time.Millisecond, Max: 61 * time.Millisecond}, PayloadP50: 2110, PayloadP95: 6880},
		},
		Timeouts: 3, ReplyErrors: 7, BadReplies: 0,
		NoThreadParents: 3, Saturation: 1,
		SingleBucketReplies: 7612, MultiBucketReplies: 379,
	}
	var buf bytes.Buffer
	require.NoError(t, PrintHistorySummary(&buf, &s))
	out := buf.String()
	assert.Contains(t, out, "history-sustained")
	assert.Contains(t, out, "history-medium")
	assert.Contains(t, out, "history=80% thread=20%")
	assert.Contains(t, out, "open=70% scrollback=30%")
	assert.Contains(t, out, "LoadHistory")
	assert.Contains(t, out, "GetThreadMessages")
	assert.Contains(t, out, "7991")
	assert.Contains(t, out, "1996")
	assert.Contains(t, out, "single-bucket")
	assert.Contains(t, out, "multi-bucket")
}

func TestBuildHistorySummary_PercentileAggregation(t *testing.T) {
	// Two samples per endpoint with known latencies; verify the per-endpoint
	// stats are computed independently.
	c := NewHistoryCollector()
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, Latency: 10 * time.Millisecond, PayloadBytes: 1000, At: time.Now()})
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, Latency: 50 * time.Millisecond, PayloadBytes: 5000, At: time.Now()})
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointThread, Latency: 1 * time.Millisecond, PayloadBytes: 100, At: time.Now()})

	stats := buildEndpointStats(c)
	require.Len(t, stats, 2)
	historyStats := stats[0]
	threadStats := stats[1]
	if historyStats.Endpoint != HistoryEndpointHistory {
		historyStats, threadStats = threadStats, historyStats
	}
	assert.Equal(t, 2, historyStats.Count)
	assert.Equal(t, 1, threadStats.Count)
	assert.Greater(t, historyStats.Latency.P99, time.Duration(0))
	assert.Greater(t, historyStats.PayloadP50, 0)
}

func TestBucketWalkDepth_Classifies(t *testing.T) {
	// Two single-bucket replies (PageDepthMs < bucketMs), one multi-bucket.
	bucketMs := int64(24 * 60 * 60 * 1000)
	c := NewHistoryCollector()
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, At: time.Now(), PageDepthMs: 1000})
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, At: time.Now(), PageDepthMs: bucketMs - 1})
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, At: time.Now(), PageDepthMs: bucketMs * 3})

	single, multi := classifyBucketDepth(c, bucketMs)
	assert.Equal(t, 2, single)
	assert.Equal(t, 1, multi)
}

func TestWriteHistoryCSV_HasOneRowPerSample(t *testing.T) {
	c := NewHistoryCollector()
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointHistory, Latency: 10 * time.Millisecond, PayloadBytes: 1000, At: time.Now()})
	c.RecordSample(HistorySample{Endpoint: HistoryEndpointThread, Latency: 20 * time.Millisecond, PayloadBytes: 500, At: time.Now()})

	var buf bytes.Buffer
	require.NoError(t, writeHistoryCSV(&buf, c))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// 1 header + 2 data rows.
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "endpoint")
	assert.Contains(t, lines[1]+lines[2], "history")
	assert.Contains(t, lines[1]+lines[2], "thread")
}
