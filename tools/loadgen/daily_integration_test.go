//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

// TestRunDaily_Integration_TinyPresetPasses exercises runDailyForTest end-to-
// end against a real NATS testcontainer. The assertion is that the lifecycle
// (BuildFixtures → activateUsers → pool subscribe → warmup → hold → poll →
// evaluate → cooldown) completes and produces a non-TRIP StepResult.
//
// SKIP: this test needs the full chat backend (message-gatekeeper,
// room-service, broadcast-worker, etc.) subscribed to the subjects the
// emitters publish to. With only a testutil NATS container, every
// request/reply action times out → ErrorRate trips the verdict.
// The full-stack integration check belongs in the docker-compose harness
// (`make -C tools/loadgen/deploy run-daily PRESET=daily-light STEPS=10
// HOLD=10s`) rather than `go test -tags integration`.
//
// Before the recall-review fix that wired emitters into prodEnvFactory,
// this test passed vacuously because no actions were emitted; the
// underlying gap was the missing backend, not the wiring.
func TestRunDaily_Integration_TinyPresetPasses(t *testing.T) {
	t.Skip("requires full docker-compose stack with chat services; testcontainer NATS alone is insufficient — use deploy/run-daily for end-to-end coverage")

	natsURL := testutil.NATS(t)

	cfg := dailyConfig{
		Preset:             "daily-heavy",
		Steps:              []int{10},
		Warmup:             1 * time.Second,
		Hold:               5 * time.Second,
		Cooldown:           500 * time.Millisecond,
		StopOnTrip:         true,
		MaxDirectUsers:     10,
		MultiplexPoolSize:  0,
		MaxConnsPerProcess: 25,
	}

	baseCfg := &config{
		NatsURL:  natsURL,
		MongoURI: "mongodb://unused",
		MongoDB:  "unused",
		SiteID:   "site-test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results, err := runDailyForTest(ctx, cfg, &prodEnvFactory{baseCfg: baseCfg})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Tripped, "reasons: %v", results[0].TrippedReasons)
}
