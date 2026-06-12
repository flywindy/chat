package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func defaultBotRoomThresholdsForTest() BotRoomThresholds {
	return BotRoomThresholds{
		P95LatencyMs: 500, P99LatencyMs: 1000,
		ErrorRate: 0.001, PendingGrowth: 1000,
		GCPauseInconclusive: 50, RateTolerance: 0.05,
	}
}

func TestEvaluateBotRoomStep_PassWhenHealthy(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(10, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{"notification-worker": {Delta: 50}},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.False(t, r.Tripped)
	assert.False(t, r.Inconclusive)
}

func TestEvaluateBotRoomStep_TripsOnNotificationBacklog(t *testing.T) {
	in := botRoomStepInputs{
		Size: 5000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(20, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{"notification-worker": {Delta: 9000}},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Tripped)
	assert.Contains(t, r.TrippedReasons[0], "notification-worker")
}

func TestEvaluateBotRoomStep_TripsOnLatency(t *testing.T) {
	in := botRoomStepInputs{
		Size: 5000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(900, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Tripped)
}

func TestEvaluateBotRoomStep_InconclusiveOnRateShortfall(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 3000, Failed: 0,
		E2Samples:       msSamples(10, 3000),
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Inconclusive)
}

func TestEvaluateBotRoomStep_InconclusiveOnGCPause(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(10, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 99},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Inconclusive)
}

// msSamples returns n samples each of approximately `ms` milliseconds.
func msSamples(ms float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = ms
	}
	return out
}
