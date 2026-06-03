package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRenderConsole_IncludesAnswerLine(t *testing.T) {
	results := []StepResult{
		{N: 1000, P50LatencyMs: 12, P95LatencyMs: 45, P99LatencyMs: 89, ErrorRate: 0,
			ConsumerPending: map[string]ConsumerPendingDelta{"broadcast-worker": {Delta: 12}}},
		{N: 2000, P50LatencyMs: 14, P95LatencyMs: 480, P99LatencyMs: 980, ErrorRate: 0,
			ConsumerPending: map[string]ConsumerPendingDelta{"broadcast-worker": {Delta: 1240}},
			Tripped:         true, TrippedReasons: []string{"broadcast-worker pending +1240"}},
	}
	var buf bytes.Buffer
	renderConsole(&buf, results)
	out := buf.String()
	require.Contains(t, out, "1000")
	require.Contains(t, out, "PASS")
	require.Contains(t, out, "TRIP")
	require.Contains(t, out, "ANSWER: N = 1000")
}

func TestWriteCSV_OneRowPerStep(t *testing.T) {
	results := []StepResult{
		{N: 1000, P50LatencyMs: 10, StartedAt: time.Unix(1700000000, 0)},
		{N: 2000, P50LatencyMs: 20, StartedAt: time.Unix(1700000200, 0), Tripped: true},
	}
	path := filepath.Join(t.TempDir(), "out.csv")
	require.NoError(t, writeDailyCSV(path, results))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(string(body), "\n")) // header + 2 rows
}
