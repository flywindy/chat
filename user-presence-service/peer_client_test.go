package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

func startInProcessNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{Port: -1}
	ns, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "nats server did not become ready")
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

func TestNATSPeerPresenceClient_HappyPath(t *testing.T) {
	nc := startInProcessNATS(t)
	client := NewNATSPeerPresenceClient(nc, 2*time.Second)

	want := []model.PresenceState{
		{Account: "carol", SiteID: "site-b", Status: model.StatusBusy},
		{Account: "dave", SiteID: "site-b", Status: model.StatusOffline},
	}
	sub, err := nc.Subscribe(subject.PresenceQueryBatchPeer("site-b"), func(m *nats.Msg) {
		data, _ := json.Marshal(model.PresenceQueryResponse{States: want})
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe() //nolint:errcheck

	got, err := client.QueryPeer(context.Background(), "site-b", []string{"carol", "dave"})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestNATSPeerPresenceClient_BodyAndSubject(t *testing.T) {
	nc := startInProcessNATS(t)
	client := NewNATSPeerPresenceClient(nc, 2*time.Second)

	gotSubject := make(chan string, 1)
	gotAccounts := make(chan []string, 1)
	sub, err := nc.Subscribe(subject.PresenceQueryBatchPeer("site-b"), func(m *nats.Msg) {
		var req model.PresenceQuery
		_ = json.Unmarshal(m.Data, &req)
		gotSubject <- m.Subject
		gotAccounts <- req.Accounts
		_ = m.Respond([]byte(`{"states":[]}`))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe() //nolint:errcheck

	_, err = client.QueryPeer(context.Background(), "site-b", []string{"carol", "dave"})
	require.NoError(t, err)
	assert.Equal(t, "chat.server.request.presence.site-b.query.batch", <-gotSubject)
	assert.Equal(t, []string{"carol", "dave"}, <-gotAccounts)
}

func TestNATSPeerPresenceClient_RemoteError(t *testing.T) {
	nc := startInProcessNATS(t)
	client := NewNATSPeerPresenceClient(nc, 2*time.Second)

	sub, err := nc.Subscribe(subject.PresenceQueryBatchPeer("site-b"), func(m *nats.Msg) {
		errData, _ := json.Marshal(errcode.BadRequest("batch exceeds max"))
		_ = m.Respond(errData)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe() //nolint:errcheck

	_, err = client.QueryPeer(context.Background(), "site-b", []string{"carol"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote presence query")
	assert.Contains(t, err.Error(), "batch exceeds max")
}

func TestNATSPeerPresenceClient_InvalidJSONReply(t *testing.T) {
	nc := startInProcessNATS(t)
	client := NewNATSPeerPresenceClient(nc, 2*time.Second)

	sub, err := nc.Subscribe(subject.PresenceQueryBatchPeer("site-b"), func(m *nats.Msg) {
		_ = m.Respond([]byte(`{not json`))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe() //nolint:errcheck

	_, err = client.QueryPeer(context.Background(), "site-b", []string{"carol"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal peer presence reply")
}

func TestNATSPeerPresenceClient_Timeout(t *testing.T) {
	nc := startInProcessNATS(t)
	client := NewNATSPeerPresenceClient(nc, 100*time.Millisecond)

	// release gates the responder so it replies only after the client has
	// already timed out — deterministic, no sleep-based synchronization.
	release := make(chan struct{})
	sub, err := nc.Subscribe(subject.PresenceQueryBatchPeer("site-b"), func(m *nats.Msg) {
		<-release
		_ = m.Respond([]byte(`{"states":[]}`))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe() //nolint:errcheck

	_, err = client.QueryPeer(context.Background(), "site-b", []string{"carol"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "presence peer query to site-b")
	close(release) // let the responder goroutine finish before the test ends
}
