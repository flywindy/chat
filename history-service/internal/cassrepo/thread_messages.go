package cassrepo

import (
	"context"
	"fmt"

	"github.com/gocql/gocql"

	"github.com/hmchangw/chat/history-service/internal/models"
)

// Subset of messages_by_room — no tshow, thread_parent_created_at, or pinned_* columns.
const threadMessageColumns = "room_id, thread_room_id, created_at, message_id, thread_parent_id, " +
	"sender, target_user, msg, mentions, attachments, file, card, card_action, " +
	"quoted_parent_message, visible_to, unread, reactions, deleted, " +
	"type, sys_msg_data, site_id, edited_at, updated_at"

const threadMessageQuery = "SELECT " + threadMessageColumns + " FROM thread_messages_by_room"

func threadMessageScanDest(m *models.Message) []any {
	return []any{
		&m.RoomID, &m.ThreadRoomID, &m.CreatedAt, &m.MessageID, &m.ThreadParentID,
		&m.Sender, &m.TargetUser, &m.Msg,
		&m.Mentions, &m.Attachments, &m.File,
		&m.Card, &m.CardAction,
		&m.QuotedParentMessage,
		&m.VisibleTo, &m.Unread, &m.Reactions,
		&m.Deleted, &m.Type, &m.SysMsgData,
		&m.SiteID, &m.EditedAt, &m.UpdatedAt,
	}
}

func scanThreadMessages(iter *gocql.Iter) []models.Message {
	return scanWith(iter, threadMessageScanDest)
}

// Partition + clustering key equality avoids ALLOW FILTERING.
func (r *Repository) GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q PageRequest) (Page[models.Message], error) {
	var messages []models.Message

	nextCursor, err := NewQueryBuilder(
		r.session.Query(
			threadMessageQuery+` WHERE room_id = ? AND thread_room_id = ? ORDER BY created_at DESC`,
			roomID, threadRoomID,
		).WithContext(ctx),
	).
		WithCursor(q.Cursor).
		WithPageSize(q.PageSize).
		Fetch(func(iter *gocql.Iter) {
			messages = scanThreadMessages(iter)
		})
	if err != nil {
		return Page[models.Message]{}, fmt.Errorf("querying thread messages: %w", err)
	}

	return Page[models.Message]{
		Data:       messages,
		NextCursor: nextCursor,
		HasNext:    nextCursor != "",
	}, nil
}
