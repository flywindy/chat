// Package publisher adapts a JetStream context to the service.EventPublisher
// interface.
package publisher

import (
	"context"
	"fmt"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hmchangw/chat/pkg/natsutil"
)

// Publisher publishes byte payloads to JetStream subjects. Each publish blocks
// on PubAck so transient JetStream failures surface as errors to the caller.
type Publisher struct {
	js oteljetstream.JetStream
}

func New(js oteljetstream.JetStream) *Publisher {
	return &Publisher{js: js}
}

// Publish sends data to subj via JetStream with Nats-Msg-Id = msgID.
func (p *Publisher) Publish(ctx context.Context, subj string, data []byte, msgID string) error {
	msg := natsutil.NewMsg(ctx, subj, data)
	if _, err := p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(msgID)); err != nil {
		return fmt.Errorf("publishing to %q: %w", subj, err)
	}
	return nil
}
