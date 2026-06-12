package main

import (
	"fmt"
)

// botRoomGatedDurables are the message-pipeline consumers whose backlog growth
// fails a step. Unlike the daily scenario, notification-worker is NOT exempt —
// it is the headline O(N)-per-message signal this scenario hunts.
var botRoomGatedDurables = []string{"message-worker", "broadcast-worker", "notification-worker"}

// BotRoomThresholds gates a botroom step.
type BotRoomThresholds struct {
	P95LatencyMs        float64
	P99LatencyMs        float64
	ErrorRate           float64
	PendingGrowth       int64
	GCPauseInconclusive float64
	RateTolerance       float64
}

// botRoomStepInputs is the raw measurement bundle for one size step.
type botRoomStepInputs struct {
	Size            int
	Rooms           int
	TargetRate      int
	HoldSeconds     float64
	Attempted       int64
	Failed          int64
	E2Samples       []float64 // publish→broadcast latency, milliseconds
	ReadSamples     []float64 // room-service read latency, milliseconds (nil when reads off)
	ConsumerPending map[string]ConsumerPendingDelta
	Self            SelfMetrics
}

// BotRoomStepResult is the evaluated verdict for one size step.
type BotRoomStepResult struct {
	Size            int
	Rooms           int
	TargetRate      int
	AchievedRate    float64
	E2P50Ms         float64
	E2P95Ms         float64
	E2P99Ms         float64
	ReadP95Ms       float64
	ReadP99Ms       float64
	ErrorRate       float64
	Attempted       int64
	Failed          int64
	ConsumerPending map[string]ConsumerPendingDelta
	Tripped         bool
	Inconclusive    bool
	TrippedReasons  []string
}

//nolint:gocritic // hugeParam: pure-function signature is intentional; the per-step copy cost is negligible.
func evaluateBotRoomStep(in botRoomStepInputs, th BotRoomThresholds) BotRoomStepResult {
	r := BotRoomStepResult{
		Size: in.Size, Rooms: in.Rooms, TargetRate: in.TargetRate,
		Attempted: in.Attempted, Failed: in.Failed,
		ConsumerPending: in.ConsumerPending,
	}
	if in.HoldSeconds > 0 {
		r.AchievedRate = float64(in.Attempted) / in.HoldSeconds
	}
	if in.Attempted > 0 {
		r.ErrorRate = float64(in.Failed) / float64(in.Attempted)
	}
	r.E2P50Ms = percentile(in.E2Samples, 0.50)
	r.E2P95Ms = percentile(in.E2Samples, 0.95)
	r.E2P99Ms = percentile(in.E2Samples, 0.99)
	r.ReadP95Ms = percentile(in.ReadSamples, 0.95)
	r.ReadP99Ms = percentile(in.ReadSamples, 0.99)

	// INCONCLUSIVE overrides everything: the verdict signals can't be trusted.
	if in.Self.GCPauseP99Ms > th.GCPauseInconclusive {
		r.Inconclusive = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("inconclusive: gc=%.1fms > %.0fms", in.Self.GCPauseP99Ms, th.GCPauseInconclusive))
		return r
	}
	if in.TargetRate > 0 && r.AchievedRate < float64(in.TargetRate)*(1-th.RateTolerance) {
		r.Inconclusive = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("inconclusive: achieved %.0f/s < %d/s target (load box limited)", r.AchievedRate, in.TargetRate))
		return r
	}

	// TRIP signals.
	if r.E2P95Ms > th.P95LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("E2E p95=%.0fms > %.0fms", r.E2P95Ms, th.P95LatencyMs))
	}
	if r.E2P99Ms > th.P99LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("E2E p99=%.0fms > %.0fms", r.E2P99Ms, th.P99LatencyMs))
	}
	if len(in.ReadSamples) > 0 {
		if r.ReadP95Ms > th.P95LatencyMs {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("read p95=%.0fms > %.0fms", r.ReadP95Ms, th.P95LatencyMs))
		}
		if r.ReadP99Ms > th.P99LatencyMs {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("read p99=%.0fms > %.0fms", r.ReadP99Ms, th.P99LatencyMs))
		}
	}
	if in.Attempted > 0 && r.ErrorRate > th.ErrorRate {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("error rate %.4f > %.4f", r.ErrorRate, th.ErrorRate))
	}
	for _, durable := range botRoomGatedDurables {
		d, ok := in.ConsumerPending[durable]
		if !ok {
			continue
		}
		switch {
		case d.Delta > th.PendingGrowth:
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s pending +%d > +%d", durable, d.Delta, th.PendingGrowth))
		case d.End == 0 && d.Start > 0:
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s disappeared mid-hold (had %d pending at start)", durable, d.Start))
		}
	}
	return r
}
