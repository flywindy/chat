package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/subject"
)

// historyRequestTimeout matches the nats.go default request timeout.
const historyRequestTimeout = 2 * time.Second

// historyParentFetcher implements ParentMessageFetcher by issuing a NATS
// request to history-service's GetMessageByID handler. The base URL is used
// to build messageLink; it is injected so unit tests can supply any value.
type historyParentFetcher struct {
	nc          *otelnats.Conn
	chatBaseURL string
}

func newHistoryParentFetcher(nc *otelnats.Conn, chatBaseURL string) *historyParentFetcher {
	return &historyParentFetcher{nc: nc, chatBaseURL: chatBaseURL}
}

// getMessageByIDRequest mirrors history-service's GetMessageByIDRequest wire
// shape (the source struct lives under internal/ and isn't importable).
type getMessageByIDRequest struct {
	MessageID string `json:"messageId"`
}

// FetchQuotedParent issues a NATS request to history-service's GetMessageByID
// handler at subject.MsgGet(account, roomID, siteID). On a successful reply,
// projects the returned cassandra.Message into a cassandra.QuotedParentMessage
// snapshot. Any error (NATS timeout, no responder, natsrouter error envelope,
// unmarshal failure) is wrapped and returned — the caller treats every error
// as a soft-fail signal.
func (f *historyParentFetcher) FetchQuotedParent(
	ctx context.Context,
	account, roomID, siteID, messageID string,
) (*cassandra.QuotedParentMessage, error) {
	reqBytes, err := json.Marshal(getMessageByIDRequest{MessageID: messageID})
	if err != nil {
		return nil, fmt.Errorf("marshal GetMessageByID request: %w", err)
	}

	subj := subject.MsgGet(account, roomID, siteID)
	msg, err := f.nc.Request(ctx, subj, reqBytes, historyRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("history request: %w", err)
	}

	// natsrouter encodes errors as {"error":"...","code":"..."}. Detect that
	// shape first; a successful Message has no top-level "error" field, so
	// this can't false-positive on a real response.
	var errEnv model.ErrorResponse
	if jsonErr := json.Unmarshal(msg.Data, &errEnv); jsonErr == nil && errEnv.Error != "" {
		return nil, fmt.Errorf("history response error: %s", errEnv.Error)
	}

	var parent cassandra.Message
	if err := json.Unmarshal(msg.Data, &parent); err != nil {
		return nil, fmt.Errorf("unmarshal parent message: %w", err)
	}

	return &cassandra.QuotedParentMessage{
		MessageID:             parent.MessageID,
		RoomID:                parent.RoomID,
		Sender:                parent.Sender,
		CreatedAt:             parent.CreatedAt,
		Msg:                   parent.Msg,
		Mentions:              parent.Mentions,
		MessageLink:           fmt.Sprintf("%s/%s/%s", f.chatBaseURL, parent.RoomID, parent.MessageID),
		ThreadParentID:        parent.ThreadParentID,
		ThreadParentCreatedAt: parent.ThreadParentCreatedAt,
	}, nil
}
