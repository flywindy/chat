package cassrepo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocql/gocql"

	"github.com/hmchangw/chat/history-service/internal/models"
)

// messageByIDExtraColumns are only present in messages_by_id, not messages_by_room.
const messageByIDExtraColumns = ", pinned_at, pinned_by"

const messageByIDQuery = "SELECT " + baseColumns + messageByIDExtraColumns + " FROM messages_by_id"

func messageByIDScanDest(m *models.Message) []any {
	return append(baseScanDest(m), &m.PinnedAt, &m.PinnedBy)
}

func scanMessagesByID(iter *gocql.Iter) []models.Message { return scanWith(iter, messageByIDScanDest) }

// Returns (nil, nil) when not found.
func (r *Repository) GetMessageByID(ctx context.Context, messageID string) (*models.Message, error) {
	var m models.Message
	err := r.session.Query(
		messageByIDQuery+` WHERE message_id = ?`,
		messageID,
	).WithContext(ctx).Scan(messageByIDScanDest(&m)...)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying message by id: %w", err)
	}
	return &m, nil
}

// Missing IDs are silently omitted; order is not guaranteed.
func (r *Repository) GetMessagesByIDs(ctx context.Context, messageIDs []string) ([]models.Message, error) {
	if len(messageIDs) == 0 {
		return []models.Message{}, nil
	}
	iter := r.session.Query(
		messageByIDQuery+` WHERE message_id IN ?`,
		messageIDs,
	).WithContext(ctx).Iter()
	messages := scanMessagesByID(iter)
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("querying messages by IDs: %w", err)
	}
	return messages, nil
}

func (r *Repository) UpdateMessageContent(ctx context.Context, msg *models.Message, newMsg string, editedAt time.Time) error {
	if err := r.session.Query(
		`UPDATE messages_by_id SET msg = ?, edited_at = ?, updated_at = ? WHERE message_id = ? AND created_at = ?`,
		newMsg, editedAt, editedAt, msg.MessageID, msg.CreatedAt,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("update messages_by_id: %w", err)
	}

	if msg.ThreadParentID == "" {
		if err := r.session.Query(
			`UPDATE messages_by_room SET msg = ?, edited_at = ?, updated_at = ? WHERE room_id = ? AND created_at = ? AND message_id = ?`,
			newMsg, editedAt, editedAt, msg.RoomID, msg.CreatedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("update messages_by_room: %w", err)
		}
	} else {
		if err := r.session.Query(
			`UPDATE thread_messages_by_room SET msg = ?, edited_at = ?, updated_at = ? WHERE room_id = ? AND thread_room_id = ? AND created_at = ? AND message_id = ?`,
			newMsg, editedAt, editedAt, msg.RoomID, msg.ThreadRoomID, msg.CreatedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("update thread_messages_by_room: %w", err)
		}
	}

	if msg.PinnedAt != nil {
		if err := r.session.Query(
			`UPDATE pinned_messages_by_room SET msg = ?, edited_at = ?, updated_at = ? WHERE room_id = ? AND created_at = ? AND message_id = ?`,
			newMsg, editedAt, editedAt, msg.RoomID, *msg.PinnedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("update pinned_messages_by_room: %w", err)
		}
	}

	return nil
}

// SoftDeleteMessage uses a Cassandra LWT on messages_by_id as a one-shot gate so only
// the winning goroutine runs mirror-table updates and tcount decrement, preventing double-decrement.
// `IF deleted != true` matches NULL (message-worker never writes deleted) and false, excluding true.
func (r *Repository) SoftDeleteMessage(ctx context.Context, msg *models.Message, deletedAt time.Time) (time.Time, bool, error) {
	var current bool
	applied, err := r.session.Query(
		`UPDATE messages_by_id SET deleted = true, updated_at = ? WHERE message_id = ? AND created_at = ? IF deleted != true`,
		deletedAt, msg.MessageID, msg.CreatedAt,
	).WithContext(ctx).ScanCAS(&current)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("cas update messages_by_id: %w", err)
	}
	if !applied {
		// Concurrent delete won. Read the existing updated_at so the caller
		// can return an accurate response timestamp.
		var existing time.Time
		if err := r.session.Query(
			`SELECT updated_at FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
			msg.MessageID, msg.CreatedAt,
		).WithContext(ctx).Scan(&existing); err != nil {
			if errors.Is(err, gocql.ErrNotFound) {
				return time.Time{}, false, nil
			}
			return time.Time{}, false, fmt.Errorf("read updated_at after cas miss: %w", err)
		}
		return existing, false, nil
	}

	if msg.ThreadParentID == "" {
		if err := r.session.Query(
			`UPDATE messages_by_room SET deleted = true, updated_at = ? WHERE room_id = ? AND created_at = ? AND message_id = ?`,
			deletedAt, msg.RoomID, msg.CreatedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return time.Time{}, false, fmt.Errorf("update messages_by_room: %w", err)
		}
	} else {
		if err := r.session.Query(
			`UPDATE thread_messages_by_room SET deleted = true, updated_at = ? WHERE room_id = ? AND thread_room_id = ? AND created_at = ? AND message_id = ?`,
			deletedAt, msg.RoomID, msg.ThreadRoomID, msg.CreatedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return time.Time{}, false, fmt.Errorf("update thread_messages_by_room: %w", err)
		}
	}

	if msg.PinnedAt != nil {
		if err := r.session.Query(
			`UPDATE pinned_messages_by_room SET deleted = true, updated_at = ? WHERE room_id = ? AND created_at = ? AND message_id = ?`,
			deletedAt, msg.RoomID, *msg.PinnedAt, msg.MessageID,
		).WithContext(ctx).Exec(); err != nil {
			return time.Time{}, false, fmt.Errorf("update pinned_messages_by_room: %w", err)
		}
	}

	if msg.ThreadParentID != "" {
		if err := r.decrementParentTcount(ctx, msg); err != nil {
			return time.Time{}, false, fmt.Errorf("decrement parent tcount: %w", err)
		}
	}

	return deletedAt, true, nil
}

// decrementParentTcount silently skips if ThreadParentCreatedAt is nil or if the parent row is missing.
func (r *Repository) decrementParentTcount(ctx context.Context, msg *models.Message) error {
	if msg.ThreadParentCreatedAt == nil {
		return nil
	}
	parentID := msg.ThreadParentID
	parentCreatedAt := *msg.ThreadParentCreatedAt

	// CAS decrement on messages_by_id.
	var tcount *int
	if err := r.session.Query(
		`SELECT tcount FROM messages_by_id WHERE message_id = ? AND created_at = ?`,
		parentID, parentCreatedAt,
	).WithContext(ctx).Scan(&tcount); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("read tcount for parent %s in messages_by_id: %w", parentID, err)
	}
	if err := casDecrement(casMaxRetries, tcount, func(newVal int, expected *int) (bool, *int, error) {
		var current *int
		applied, err := r.session.Query(
			`UPDATE messages_by_id SET tcount = ? WHERE message_id = ? AND created_at = ? IF tcount = ?`,
			newVal, parentID, parentCreatedAt, expected,
		).WithContext(ctx).ScanCAS(&current)
		return applied, current, err
	}); err != nil {
		return fmt.Errorf("cas tcount decrement in messages_by_id for parent %s: %w", parentID, err)
	}

	// CAS decrement on messages_by_room.
	if err := r.session.Query(
		`SELECT tcount FROM messages_by_room WHERE room_id = ? AND created_at = ? AND message_id = ?`,
		msg.RoomID, parentCreatedAt, parentID,
	).WithContext(ctx).Scan(&tcount); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("read tcount for parent %s in messages_by_room: %w", parentID, err)
	}
	if err := casDecrement(casMaxRetries, tcount, func(newVal int, expected *int) (bool, *int, error) {
		var current *int
		applied, err := r.session.Query(
			`UPDATE messages_by_room SET tcount = ? WHERE room_id = ? AND created_at = ? AND message_id = ? IF tcount = ?`,
			newVal, msg.RoomID, parentCreatedAt, parentID, expected,
		).WithContext(ctx).ScanCAS(&current)
		return applied, current, err
	}); err != nil {
		return fmt.Errorf("cas tcount decrement in messages_by_room for parent %s: %w", parentID, err)
	}

	return nil
}
