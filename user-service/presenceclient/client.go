package presenceclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// presenceRPCTimeout is tighter than the sibling clients' 5s: presence is a
// degradable hot-path field, so a slow lookup should fail fast to offline.
const presenceRPCTimeout = time.Second

// Client implements service.PresenceClient via NATS request/reply over the
// server-to-server lane; the passed siteID must be the accounts' home site.
type Client struct {
	nc *otelnats.Conn
}

// New returns a Client wired to nc.
func New(nc *otelnats.Conn) *Client { return &Client{nc: nc} }

// QueryPresence runs the batch presence query at siteID; non-OK envelopes relay via errcode.Parse.
func (c *Client) QueryPresence(ctx context.Context, siteID string, accounts []string) ([]model.PresenceState, error) {
	body, err := json.Marshal(model.PresenceQuery{Accounts: accounts})
	if err != nil {
		return nil, fmt.Errorf("marshal presence-query request: %w", err)
	}
	msg, err := c.nc.Request(ctx, subject.PresenceQueryBatchPeer(siteID), body, presenceRPCTimeout)
	if err != nil {
		return nil, fmt.Errorf("presence-query rpc: %w", err)
	}
	if e, ok := errcode.Parse(msg.Data); ok {
		return nil, e
	}
	var out model.PresenceQueryResponse
	if err := json.Unmarshal(msg.Data, &out); err != nil {
		return nil, fmt.Errorf("decode presence-query response: %w", err)
	}
	return out.States, nil
}
