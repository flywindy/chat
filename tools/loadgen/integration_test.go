//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/mongoutil"
	"github.com/hmchangw/chat/pkg/stream"
	"github.com/hmchangw/chat/pkg/subject"
	"github.com/hmchangw/chat/pkg/testutil/testimages"
)

// setupNATS starts a JetStream-enabled NATS container via the generic
// testcontainers interface (no dedicated NATS module is required).
func setupNATS(t *testing.T) (string, func()) {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        testimages.NATS,
			Cmd:          []string{"-js"},
			ExposedPorts: []string{"4222/tcp"},
			WaitingFor:   wait.ForLog("Server is ready").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "4222")
	require.NoError(t, err)
	return fmt.Sprintf("nats://%s:%s", host, port.Port()), func() { _ = c.Terminate(ctx) }
}

func setupMongo(t *testing.T) (string, func()) {
	t.Helper()
	ctx := context.Background()
	c, err := mongodb.Run(ctx, testimages.Mongo)
	require.NoError(t, err)
	uri, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	return uri, func() { _ = c.Terminate(ctx) }
}

// TestLoadgenSmallPreset_EndToEnd verifies the generator publishes messages,
// a fake gatekeeper forwards them to MESSAGES_CANONICAL, two JetStream
// consumers drain the stream, a fake broadcast-worker emits room events,
// and MongoDB shows the seeded room data.
func TestLoadgenSmallPreset_EndToEnd(t *testing.T) {
	ctx := context.Background()
	natsURI, stopNATS := setupNATS(t)
	defer stopNATS()
	mongoURI, stopMongo := setupMongo(t)
	defer stopMongo()

	nc, err := nats.Connect(natsURI)
	require.NoError(t, err)
	defer nc.Drain()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	siteID := "site-test"
	canonical := stream.MessagesCanonical(siteID)
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     canonical.Name,
		Subjects: canonical.Subjects,
	})
	require.NoError(t, err)

	// Two durable consumers that simply ack — stand in for message-worker
	// and broadcast-worker so the canonical stream drains to zero.
	for _, durable := range []string{"message-worker", "broadcast-worker"} {
		cons, err := js.CreateOrUpdateConsumer(ctx, canonical.Name, jetstream.ConsumerConfig{
			Durable:   durable,
			AckPolicy: jetstream.AckExplicitPolicy,
		})
		require.NoError(t, err)
		cc, err := cons.Consume(func(msg jetstream.Msg) { _ = msg.Ack() })
		require.NoError(t, err)
		defer cc.Stop()
	}

	// Connect Mongo and seed fixtures.
	client, err := mongoutil.Connect(ctx, mongoURI, "", "")
	require.NoError(t, err)
	defer mongoutil.Disconnect(ctx, client)
	db := client.Database("chat")

	preset, _ := BuiltinPreset("small")
	fixtures := BuildFixtures(&preset, 42, siteID)
	require.NoError(t, Seed(ctx, db, fixtures))

	metrics := NewMetrics()
	collector := NewCollector(metrics, preset.Name)

	// Fake gatekeeper: frontdoor subject → publish MessageEvent to canonical.
	gkSub, err := nc.Subscribe(
		subject.MsgSendWildcard(siteID),
		func(m *nats.Msg) {
			var req model.SendMessageRequest
			if err := json.Unmarshal(m.Data, &req); err != nil {
				return
			}
			evt := model.MessageEvent{
				Message: model.Message{
					ID:        req.ID,
					Content:   req.Content,
					CreatedAt: time.Now().UTC(),
				},
				SiteID:    siteID,
				Timestamp: time.Now().UnixMilli(),
			}
			data, _ := json.Marshal(evt)
			_, _ = js.Publish(ctx, subject.MsgCanonicalCreated(siteID), data)
		},
	)
	require.NoError(t, err)
	defer gkSub.Unsubscribe()

	// Fake broadcast-worker: canonical event → room event.
	bwSub, err := nc.Subscribe(
		subject.MsgCanonicalCreated(siteID),
		func(m *nats.Msg) {
			var evt model.MessageEvent
			if err := json.Unmarshal(m.Data, &evt); err != nil {
				return
			}
			roomEvt := model.RoomEvent{
				Type:    model.RoomEventNewMessage,
				RoomID:  "r",
				Message: &model.ClientMessage{Message: evt.Message},
			}
			data, _ := json.Marshal(roomEvt)
			_ = nc.Publish("chat.room.r.event", data)
		},
	)
	require.NoError(t, err)
	defer bwSub.Unsubscribe()

	publisher := &natsCorePublisher{nc: nc}
	gen := NewGenerator(&GeneratorConfig{
		Preset:    &preset,
		Fixtures:  fixtures,
		SiteID:    siteID,
		Rate:      50,
		Inject:    InjectFrontdoor,
		Publisher: publisher,
		Metrics:   metrics,
		Collector: collector,
	}, 42)

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	require.NoError(t, gen.Run(runCtx))

	// Allow trailing events to flow.
	time.Sleep(2 * time.Second)

	// Assert the canonical stream drained.
	for _, durable := range []string{"message-worker", "broadcast-worker"} {
		cons, err := js.Consumer(ctx, canonical.Name, durable)
		require.NoError(t, err)
		info, err := cons.Info(ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(0), info.NumPending, "durable %s still has pending", durable)
	}

	// Assert seed data is visible in Mongo.
	var room model.Room
	err = db.Collection("rooms").FindOne(ctx, bson.M{"_id": fixtures.Rooms[0].ID}).Decode(&room)
	require.NoError(t, err)
	require.Equal(t, fixtures.Rooms[0].ID, room.ID)
}
