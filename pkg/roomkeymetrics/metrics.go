// Package roomkeymetrics exposes OTel metric instruments for the room-key
// fan-out and inter-site RPC code paths shared by room-worker and inbox-worker.
package roomkeymetrics

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

var (
	// FanoutErrors counts the number of failed RoomKeyEvent sends to a single account.
	FanoutErrors metric.Int64Counter
	// StoreErrors counts room-key store (MongoDB) operation failures, tagged by operation name.
	StoreErrors metric.Int64Counter
	// KeyAbsentErrors fires when the store is healthy but no current key exists for a room.
	// Distinct from StoreErrors which counts I/O failures.
	KeyAbsentErrors metric.Int64Counter
)

func init() {
	m := otel.Meter("room-key")

	var err error
	FanoutErrors, err = m.Int64Counter(
		"room_key_fanout_errors_total",
		metric.WithDescription("Number of failed RoomKeyEvent sends to a single account"),
	)
	if err != nil {
		// Fall back to a no-op counter so the program continues to run even if
		// the global meter provider is not yet initialised at package init time.
		FanoutErrors, _ = noop.NewMeterProvider().Meter("room-key").Int64Counter("room_key_fanout_errors_total")
	}

	StoreErrors, err = m.Int64Counter(
		"room_key_store_errors_total",
		metric.WithDescription("Number of room-key store operation failures"),
	)
	if err != nil {
		StoreErrors, _ = noop.NewMeterProvider().Meter("room-key").Int64Counter("room_key_store_errors_total")
	}

	KeyAbsentErrors, err = m.Int64Counter(
		"room_key_absent_errors_total",
		metric.WithDescription("Number of times the store returned (nil, nil) for a room key — key absent, not a transient error"),
	)
	if err != nil {
		KeyAbsentErrors, _ = noop.NewMeterProvider().Meter("room-key").Int64Counter("room_key_absent_errors_total")
	}
}
