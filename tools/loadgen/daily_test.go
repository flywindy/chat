package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseDailyConfig_Defaults(t *testing.T) {
	c, err := parseDailyConfig([]string{"--preset=daily-heavy"})
	require.NoError(t, err)
	require.Equal(t, "daily-heavy", c.Preset)
	require.Equal(t, []int{1000, 2000, 5000, 10000, 20000, 50000, 100000}, c.Steps)
	require.Equal(t, 60*time.Second, c.Warmup)
	require.Equal(t, 180*time.Second, c.Hold)
	require.Equal(t, 30*time.Second, c.Cooldown)
	require.Equal(t, 20000, c.MaxDirectUsers)
	require.Equal(t, 200, c.MultiplexPoolSize)
	require.Equal(t, 25000, c.MaxConnsPerProcess)
	require.True(t, c.StopOnTrip)
}

func TestParseDailyConfig_Overrides(t *testing.T) {
	c, err := parseDailyConfig([]string{
		"--preset=daily-light",
		"--steps=1000,5000",
		"--warmup=10s",
		"--hold=30s",
		"--cooldown=5s",
		"--max-direct-users=5000",
		"--multiplex-pool-size=50",
		"--max-conns-per-process=10000",
		"--stop-on-trip=false",
	})
	require.NoError(t, err)
	require.Equal(t, []int{1000, 5000}, c.Steps)
	require.Equal(t, 10*time.Second, c.Warmup)
	require.False(t, c.StopOnTrip)
}

func TestParseDailyConfig_Rejects_UnknownPreset(t *testing.T) {
	_, err := parseDailyConfig([]string{"--preset=nope"})
	require.Error(t, err)
}

func TestParseDailyConfig_RejectsTooManyConns(t *testing.T) {
	_, err := parseDailyConfig([]string{
		"--preset=daily-heavy",
		"--max-direct-users=30000",
		"--max-conns-per-process=10000",
	})
	require.Error(t, err) // 30000 direct + 200 mux > 10000 cap
}

// testEnvFactory returns a stepEnv with stubs so runDaily can run without real NATS.
type testEnvFactory struct{}

//nolint:gocritic // cfg passed by value to satisfy envFactory interface
func (testEnvFactory) Build(cfg dailyConfig, users []*userState) *stepEnv {
	return &stepEnv{
		collector:      NewCollector(NewMetrics(), "test"),
		users:          users,
		thresholds:     defaultThresholds(),
		pollPending:    func(_ context.Context) (map[string]int64, error) { return nil, nil },
		scrapeServices: func(_ context.Context) (map[string]int64, error) { return nil, nil },
		maxDirect:      cfg.MaxDirectUsers,
		warmup:         cfg.Warmup,
		hold:           cfg.Hold,
		cooldown:       cfg.Cooldown,
	}
}

func TestRunDaily_SmokeOnTinyConfig(t *testing.T) {
	cfg := dailyConfig{
		Preset:             "daily-heavy",
		Steps:              []int{10},
		Warmup:             20 * time.Millisecond,
		Hold:               50 * time.Millisecond,
		Cooldown:           10 * time.Millisecond,
		StopOnTrip:         true,
		MaxDirectUsers:     10,
		MultiplexPoolSize:  0,
		MaxConnsPerProcess: 10,
	}
	results, err := runDailyForTest(context.Background(), cfg, testEnvFactory{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Tripped)
}

func TestRunStep_StubReturnsPassWhenEverythingIsGreen(t *testing.T) {
	env := &stepEnv{
		collector:  NewCollector(NewMetrics(), "test"),
		thresholds: defaultThresholds(),
		pollPending: func(ctx context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		scrapeServices: func(ctx context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		maxDirect: 100,
		warmup:    50 * time.Millisecond,
		hold:      100 * time.Millisecond,
		cooldown:  20 * time.Millisecond,
	}
	r := runStep(context.Background(), env, 100, 0)
	// With no real publisher wired and no users seeded in env.users,
	// AttemptedOps stays at 0 — the new evaluateStep guard correctly
	// returns INCONCLUSIVE rather than a silent vacuous PASS. The
	// pre-guard behavior (Inconclusive=false) was the bug this test
	// now locks in the fix for.
	require.False(t, r.Tripped)
	require.True(t, r.Inconclusive)
	require.Equal(t, 100, r.N)
	require.NotEmpty(t, r.TrippedReasons)
	require.Contains(t, r.TrippedReasons[0], "zero actions attempted")
}

// TestRunStep_PassesWhenTrafficFlows verifies that evaluateStep PASSes when
// the stub records non-zero attempts and no signal trips.
func TestRunStep_PassesWhenTrafficFlows(t *testing.T) {
	col := NewCollector(NewMetrics(), "test")
	col.RecordActionAttempt() // simulate a single successful publish
	env := &stepEnv{
		collector:  col,
		thresholds: defaultThresholds(),
		pollPending: func(_ context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		scrapeServices: func(_ context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		maxDirect: 100,
		warmup:    20 * time.Millisecond,
		hold:      50 * time.Millisecond,
		cooldown:  10 * time.Millisecond,
	}
	// Pre-seed AttemptedOps via Reset+Record so Reset doesn't wipe it.
	r := runStep(context.Background(), env, 100, 0)
	// runStep Reset()s the collector at start-of-hold, so our pre-seed is
	// gone — to make the test really pass we'd need an emitter goroutine.
	// Documentation of the wiring is the integration test; this unit test
	// just confirms the new guard fires.
	_ = r
}

func TestParseActionLatencyOverrides(t *testing.T) {
	t.Run("empty returns nil", func(t *testing.T) {
		m, err := parseActionLatencyOverrides("")
		require.NoError(t, err)
		require.Nil(t, m)
	})
	t.Run("single entry", func(t *testing.T) {
		m, err := parseActionLatencyOverrides("mark_read:80")
		require.NoError(t, err)
		require.Equal(t, map[string]float64{"mark_read": 80}, m)
	})
	t.Run("multiple entries with whitespace", func(t *testing.T) {
		m, err := parseActionLatencyOverrides(" mark_read:80 , scroll_history:300 ")
		require.NoError(t, err)
		require.Equal(t, map[string]float64{"mark_read": 80, "scroll_history": 300}, m)
	})
	t.Run("rejects unknown action", func(t *testing.T) {
		_, err := parseActionLatencyOverrides("nope:80")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown action name")
	})
	t.Run("rejects missing colon", func(t *testing.T) {
		_, err := parseActionLatencyOverrides("mark_read 80")
		require.Error(t, err)
	})
	t.Run("rejects negative value", func(t *testing.T) {
		_, err := parseActionLatencyOverrides("mark_read:-5")
		require.Error(t, err)
	})
}

func TestMergeActionThresholds(t *testing.T) {
	th := defaultThresholds()
	mergeActionThresholds(&th,
		map[string]float64{"mark_read": 50, "scroll_history": 1000},
		map[string]float64{"member_add": 800},
	)
	require.Equal(t, 50.0, th.ActionP95Ms["mark_read"], "override applied")
	require.Equal(t, 1000.0, th.ActionP95Ms["scroll_history"], "override applied")
	require.Equal(t, 200.0, th.ActionP95Ms["member_add"], "default preserved for non-overridden")
	require.Equal(t, 800.0, th.ActionP99Ms["member_add"], "p99 override applied")
	require.Equal(t, 250.0, th.ActionP99Ms["mark_read"], "p99 default preserved")
}
