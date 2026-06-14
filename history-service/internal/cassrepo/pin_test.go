package cassrepo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hmchangw/chat/history-service/internal/models"
)

func TestHasRoomTimelineRow(t *testing.T) {
	tests := []struct {
		name string
		msg  models.Message
		want bool
	}{
		{
			name: "channel message",
			msg:  models.Message{MessageID: "m1"},
			want: true,
		},
		{
			name: "tshow thread reply (also sent to channel)",
			msg:  models.Message{MessageID: "m2", ThreadParentID: "p1", ThreadRoomID: "t1", TShow: true},
			want: true,
		},
		{
			name: "thread-only reply (tshow=false) has no timeline row",
			msg:  models.Message{MessageID: "m3", ThreadParentID: "p1", ThreadRoomID: "t1"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasRoomTimelineRow(&tt.msg))
		})
	}
}

func TestPinBatchTables(t *testing.T) {
	assert.Equal(t, "messages_by_id, pinned_messages_by_room, messages_by_room", pinBatchTables(true))
	assert.Equal(t, "messages_by_id, pinned_messages_by_room", pinBatchTables(false))
}
