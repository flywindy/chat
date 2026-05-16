package roomkeysender

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// Publisher abstracts NATS publishing so the sender is testable.
// *nats.Conn satisfies this interface directly.
type Publisher interface {
	Publish(subject string, data []byte) error
}

// Sender publishes room key events to user NATS subjects.
type Sender struct {
	pub Publisher
}

// NewSender creates a Sender backed by the given publisher.
func NewSender(pub Publisher) *Sender {
	return &Sender{pub: pub}
}

// Send publishes evt to the room key update subject for the given user account.
// The event is accepted by value; Send stamps its own Timestamp before publishing.
// The value copy is intentional: Send must not mutate the caller's struct.
//
//nolint:gocritic // hugeParam: by-value is intentional for immutability; the copy cost is acceptable.
func (s *Sender) Send(account string, evt model.RoomKeyEvent) error {
	evt.Timestamp = time.Now().UTC().UnixMilli()
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal room key event: %w", err)
	}
	subj := subject.RoomKeyUpdate(account)
	if err := s.pub.Publish(subj, data); err != nil {
		return fmt.Errorf("publish room key event: %w", err)
	}
	return nil
}
