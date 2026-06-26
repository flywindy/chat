package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/subject"
)

func startOtelNATS(t *testing.T) *otelnats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Port: -1})
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "nats server did not become ready")
	t.Cleanup(ns.Shutdown)

	nc, err := otelnats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

func TestHistoryMessageReader_GetMessageRoomAndCreatedAt(t *testing.T) {
	const (
		account   = "alice"
		roomID    = "room-1"
		siteID    = "site-a"
		messageID = "msg-uuid"
	)
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("happy path returns room, createdAt, sender from history-service", func(t *testing.T) {
		nc := startOtelNATS(t)
		msg := cassandra.Message{
			MessageID: messageID,
			RoomID:    roomID,
			CreatedAt: createdAt,
			Sender:    cassandra.Participant{ID: "u-alice", Account: account},
		}
		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(m otelnats.Msg) {
			data, _ := json.Marshal(msg)
			_ = m.Msg.Respond(data)
		})
		require.NoError(t, err)

		r := newHistoryMessageReader(nc, siteID)
		gotRoom, gotCreated, gotSender, found, err := r.GetMessageRoomAndCreatedAt(context.Background(), account, roomID, messageID)

		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, roomID, gotRoom)
		assert.Equal(t, createdAt, gotCreated.UTC())
		assert.Equal(t, account, gotSender)
	})

	t.Run("history NotFound maps to found=false with no error", func(t *testing.T) {
		nc := startOtelNATS(t)
		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(m otelnats.Msg) {
			data, _ := json.Marshal(errcode.NotFound("message not found"))
			_ = m.Msg.Respond(data)
		})
		require.NoError(t, err)

		r := newHistoryMessageReader(nc, siteID)
		_, _, _, found, err := r.GetMessageRoomAndCreatedAt(context.Background(), account, roomID, messageID)

		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("other history errcode is propagated", func(t *testing.T) {
		nc := startOtelNATS(t)
		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(m otelnats.Msg) {
			data, _ := json.Marshal(errcode.Forbidden("message is outside access window",
				errcode.WithReason(errcode.MessageOutsideAccessWindow)))
			_ = m.Msg.Respond(data)
		})
		require.NoError(t, err)

		r := newHistoryMessageReader(nc, siteID)
		_, _, _, found, err := r.GetMessageRoomAndCreatedAt(context.Background(), account, roomID, messageID)

		assert.False(t, found)
		require.Error(t, err)
		assert.True(t, errcode.HasReason(err, errcode.MessageOutsideAccessWindow))
	})

	t.Run("malformed reply surfaces an unmarshal error", func(t *testing.T) {
		nc := startOtelNATS(t)
		_, err := nc.Subscribe(subject.MsgGet(account, roomID, siteID), func(m otelnats.Msg) {
			_ = m.Msg.Respond([]byte("not json"))
		})
		require.NoError(t, err)

		r := newHistoryMessageReader(nc, siteID)
		_, _, _, found, err := r.GetMessageRoomAndCreatedAt(context.Background(), account, roomID, messageID)

		assert.False(t, found)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshal")
	})

	t.Run("no responder degrades to unavailable", func(t *testing.T) {
		nc := startOtelNATS(t) // no subscriber registered

		r := newHistoryMessageReader(nc, siteID)
		_, _, _, found, err := r.GetMessageRoomAndCreatedAt(context.Background(), account, roomID, messageID)

		assert.False(t, found)
		require.Error(t, err)
		assert.True(t, errcode.HasReason(err, errcode.RoomReadReceiptsUnavailable))
	})
}
