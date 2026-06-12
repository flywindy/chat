package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewMetrics_BotRoomFamiliesRegistered(t *testing.T) {
	m := NewMetrics()
	m.BotRoomPublished.WithLabelValues("botroom-small", "measured", "100").Inc()
	m.BotRoomPublishErrors.WithLabelValues("publish").Inc()
	m.BotRoomE2ELatency.WithLabelValues("100").Observe(0.01)
	m.BotRoomReadLatency.WithLabelValues("100").Observe(0.01)

	mfs, err := m.Registry.Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	require.True(t, names["loadgen_botroom_published_total"])
	require.True(t, names["loadgen_botroom_publish_errors_total"])
	require.True(t, names["loadgen_botroom_e2e_latency_seconds"])
	require.True(t, names["loadgen_botroom_read_latency_seconds"])
}
