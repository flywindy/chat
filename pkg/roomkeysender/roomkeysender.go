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

// Marshal stamps the event Timestamp and serializes it once into a payload that
// can be fanned out to many accounts via SendData without re-marshaling per
// recipient. The event is accepted by value; Marshal must not mutate the
// caller's struct.
//
//nolint:gocritic // hugeParam: by-value is intentional for immutability; the copy cost is acceptable.
func (s *Sender) Marshal(evt model.RoomKeyEvent) ([]byte, error) {
	evt.Timestamp = time.Now().UTC().UnixMilli()
	// #nosec G117 -- RoomKeyEvent.PrivateKey is the intended payload: room-key distribution to the authorized account over its auth-callout-gated per-user subject, not a leak
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal room key event: %w", err)
	}
	return data, nil
}

// SendData publishes an already-marshaled payload (from Marshal) to the room key
// update subject for the given user account.
func (s *Sender) SendData(account string, data []byte) error {
	subj := subject.RoomKeyUpdate(account)
	if err := s.pub.Publish(subj, data); err != nil {
		return fmt.Errorf("publish room key event: %w", err)
	}
	return nil
}

// Send publishes evt to the room key update subject for the given user account.
// The event is accepted by value; Send stamps its own Timestamp before publishing.
// The value copy is intentional: Send must not mutate the caller's struct.
//
//nolint:gocritic // hugeParam: by-value is intentional for immutability; the copy cost is acceptable.
func (s *Sender) Send(account string, evt model.RoomKeyEvent) error {
	data, err := s.Marshal(evt)
	if err != nil {
		return err
	}
	return s.SendData(account, data)
}
