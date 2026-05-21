package roomkeysender_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/roomkeysender"
)

// mockPublisher captures the subject and data from the last Publish call.
type mockPublisher struct {
	subject string
	data    []byte
	err     error // error to return from Publish
}

func (m *mockPublisher) Publish(subject string, data []byte) error {
	m.subject = subject
	m.data = data
	return m.err
}

func TestSender_Send(t *testing.T) {
	priv32 := make([]byte, 32)
	for i := range priv32 {
		priv32[i] = byte(i + 100)
	}

	tests := []struct {
		name       string
		account    string
		evt        model.RoomKeyEvent
		publishErr error
		wantSubj   string
		wantErr    string
	}{
		{
			name:    "valid send",
			account: "alice",
			evt: model.RoomKeyEvent{
				RoomID:     "room-1",
				Version:    0,
				PrivateKey: priv32,
			},
			wantSubj: "chat.user.alice.event.room.key",
		},
		{
			name:    "different user produces different subject",
			account: "bob",
			evt: model.RoomKeyEvent{
				RoomID:     "room-2",
				Version:    1,
				PrivateKey: []byte{0x0a},
			},
			wantSubj: "chat.user.bob.event.room.key",
		},
		{
			name:    "publish error is wrapped and returned",
			account: "carol",
			evt: model.RoomKeyEvent{
				RoomID:     "room-3",
				Version:    2,
				PrivateKey: []byte{0x01},
			},
			publishErr: errors.New("connection lost"),
			wantErr:    "publish room key event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Deep-copy the caller's event for the post-call non-mutation check:
			// the shallow struct copy alone would share PrivateKey backing array
			// with tt.evt, so an in-place slice mutation by Send would be
			// invisible to a plain assert.Equal(before, tt.evt).
			before := tt.evt
			before.PrivateKey = append([]byte(nil), tt.evt.PrivateKey...)

			pub := &mockPublisher{err: tt.publishErr}
			sender := roomkeysender.NewSender(pub)

			err := sender.Send(tt.account, tt.evt)

			// Non-mutation contract: Send takes the event by value and stamps Timestamp
			// on its local copy — the caller's struct must be unchanged on success or error.
			assert.Equal(t, before, tt.evt, "Send must not mutate the caller's RoomKeyEvent")

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.ErrorIs(t, err, tt.publishErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantSubj, pub.subject)

			// Verify payload round-trips correctly.
			var got model.RoomKeyEvent
			require.NoError(t, json.Unmarshal(pub.data, &got))
			assert.Equal(t, tt.evt.RoomID, got.RoomID)
			assert.Equal(t, tt.evt.Version, got.Version)
			assert.Equal(t, tt.evt.PrivateKey, got.PrivateKey)
			assert.Greater(t, got.Timestamp, int64(0))
		})
	}
}
