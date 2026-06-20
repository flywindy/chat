//go:build integration

package publisher

import (
	"context"
	"testing"
	"time"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/testutil"
	"github.com/hmchangw/chat/user-service/service"
)

// Compile-time assertion: `go vet -tags integration` fails if Publisher drifts from EventPublisher.
var _ service.EventPublisher = (*Publisher)(nil)

func TestMain(m *testing.M) { testutil.RunTests(m) }

// dial returns a connected *otelnats.Conn backed by the shared test NATS
// server. The connection is drained on test cleanup.
func dial(t *testing.T) *otelnats.Conn {
	t.Helper()
	nc, err := otelnats.Connect(testutil.NATS(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = nc.Drain() })
	return nc
}

func TestPublish_Integration(t *testing.T) {
	nc := dial(t)

	const subj = "test.publisher.subject"
	want := []byte(`{"hello":"world"}`)

	received := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe(subj, func(m otelnats.Msg) {
		received <- m.Msg
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Stamp a request ID so we can assert it propagates onto the published msg.
	ctx := natsutil.WithRequestID(context.Background(), "22222222-2222-7222-8222-222222222222")
	err = New(nc).Publish(ctx, subj, want)
	require.NoError(t, err)

	select {
	case got := <-received:
		require.Equal(t, want, got.Data)
		assert.Equal(t, "22222222-2222-7222-8222-222222222222", got.Header.Get(natsutil.RequestIDHeader),
			"request id must propagate onto the outbox event")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for published message")
	}
}

// TestPublish_ClosedConn_Integration: closed connection must surface the wrapped "publish outbox event" error.
func TestPublish_ClosedConn_Integration(t *testing.T) {
	nc, err := otelnats.Connect(testutil.NATS(t))
	require.NoError(t, err)
	nc.Close()

	err = New(nc).Publish(context.Background(), "test.publisher.closed", []byte(`{}`))
	require.Error(t, err)
	require.ErrorContains(t, err, "publish outbox event")
}
