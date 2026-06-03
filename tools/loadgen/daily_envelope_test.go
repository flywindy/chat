package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateMultiplier(t *testing.T) {
	hold := 180 * time.Second
	cases := []struct {
		name    string
		elapsed time.Duration
		minWant float64
		maxWant float64
	}{
		{"start", 0, 0.39, 0.55},
		{"first peak", hold / 3, 0.95, 1.01},
		{"trough between peaks", hold / 2, 0.55, 0.85},
		{"second peak", 2 * hold / 3, 0.95, 1.01},
		{"end", hold, 0.39, 0.55},
		{"beyond end clamped", hold + time.Second, 0.39, 0.55},
		{"negative clamped", -time.Second, 0.39, 0.55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rateMultiplier(tc.elapsed, hold)
			require.GreaterOrEqual(t, got, tc.minWant, "got=%f", got)
			require.LessOrEqual(t, got, tc.maxWant, "got=%f", got)
		})
	}
}

func TestRateMultiplier_ZeroHold(t *testing.T) {
	require.Equal(t, 1.0, rateMultiplier(0, 0))
}
