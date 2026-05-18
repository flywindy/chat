package service

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hmchangw/chat/history-service/internal/cassrepo"
	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/subject"
)

const (
	defaultPageSize     = 20
	surroundingPageSize = 50
	maxPageSize         = 100
	maxContentBytes     = 20 * 1024 // 20 KB; mirrors message-gatekeeper's content cap
)

func (s *HistoryService) LoadHistory(c *natsrouter.Context, req models.LoadHistoryRequest) (*models.LoadHistoryResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	accessSince, err := s.getAccessSince(c, account, roomID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	lastMsgAt, createdAt, err := s.resolveRoomTimesOrError(c, roomID, req.Meta, now)
	if err != nil {
		return nil, err
	}

	before := millisToTime(req.Before)
	if before.IsZero() {
		before = now
	}
	// Cap before by lastMsgAt+1ms so the walk starts from the actual last
	// message bucket, not from "now". Year-dead rooms become 1-bucket reads.
	if !lastMsgAt.IsZero() && before.After(lastMsgAt) {
		before = lastMsgAt.Add(time.Millisecond)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	pageReq, err := parsePageRequest("", limit)
	if err != nil {
		return nil, err
	}

	// Issue two independent reads in parallel: the message page (with our
	// bucket-walk floor derived from room.createdAt clamped to historyFloor)
	// and the per-user MinUserLastSeenAt read-receipt floor returned to the
	// client. Failures on the receipt fetch are non-fatal — clients treat
	// absence as "no floor".
	var (
		page          cassrepo.Page[models.Message]
		lastSeenFloor *time.Time
	)
	g, gctx := errgroup.WithContext(c)
	g.Go(func() error {
		var pErr error
		if accessSince == nil {
			// GetMessagesBetweenDesc uses *accessSince as its own floor; the
			// explicit floor is only needed for the unrestricted
			// GetMessagesBefore path. Clamp createdAt to historyFloor so a
			// client hint can't push the walk further back.
			historyFloor := now.Add(-s.historyFloor)
			walkFloor := createdAt
			if walkFloor.IsZero() || walkFloor.Before(historyFloor) {
				walkFloor = historyFloor
			}
			page, pErr = s.msgReader.GetMessagesBefore(gctx, roomID, before, walkFloor, pageReq)
		} else {
			page, pErr = s.msgReader.GetMessagesBetweenDesc(gctx, roomID, *accessSince, before, pageReq)
		}
		return pErr
	})
	g.Go(func() error {
		t, rErr := s.rooms.GetMinUserLastSeenAt(gctx, roomID)
		if rErr != nil {
			slog.Warn("loading minUserLastSeenAt", "error", rErr, "roomID", roomID)
			return nil
		}
		lastSeenFloor = t
		return nil
	})
	if err := g.Wait(); err != nil {
		slog.Error("loading history", "error", err, "roomID", roomID)
		return nil, natsrouter.ErrInternal("failed to load message history")
	}

	var minMs *int64
	if lastSeenFloor != nil {
		ms := lastSeenFloor.UTC().UnixMilli()
		minMs = &ms
	}

	redactUnavailableQuotes(page.Data, accessSince)
	return &models.LoadHistoryResponse{
		Messages:          page.Data,
		MinUserLastSeenAt: minMs,
	}, nil
}

func (s *HistoryService) LoadNextMessages(c *natsrouter.Context, req models.LoadNextMessagesRequest) (*models.LoadNextMessagesResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	accessSince, err := s.getAccessSince(c, account, roomID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	lastMsgAt, createdAt, err := s.resolveRoomTimesOrError(c, roomID, req.Meta, now)
	if err != nil {
		return nil, err
	}

	ceiling, floor := s.walkBounds(lastMsgAt, createdAt, now)

	after := millisToTime(req.After)

	// Lower bound = max(after, accessSince). Zero means no lower bound.
	lowerBound := timeMax(after, derefTime(accessSince))

	limit := req.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	pageReq, err := parsePageRequest(req.Cursor, limit)
	if err != nil {
		return nil, err
	}

	var page cassrepo.Page[models.Message]
	if lowerBound.IsZero() {
		page, err = s.msgReader.GetAllMessagesAsc(c, roomID, floor, ceiling, pageReq)
	} else {
		page, err = s.msgReader.GetMessagesAfter(c, roomID, lowerBound, ceiling, pageReq)
	}
	if err != nil {
		slog.Error("loading next messages", "error", err, "roomID", roomID)
		return nil, natsrouter.ErrInternal("failed to load messages")
	}

	redactUnavailableQuotes(page.Data, accessSince)
	return &models.LoadNextMessagesResponse{
		Messages:   page.Data,
		NextCursor: page.NextCursor,
		HasNext:    page.HasNext,
	}, nil
}

func (s *HistoryService) LoadSurroundingMessages(c *natsrouter.Context, req models.LoadSurroundingMessagesRequest) (*models.LoadSurroundingMessagesResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	accessSince, err := s.getAccessSince(c, account, roomID)
	if err != nil {
		return nil, err
	}

	centralMsg, err := s.findMessage(c, roomID, req.MessageID)
	if err != nil {
		return nil, err
	}
	if accessSince != nil && centralMsg.CreatedAt.Before(*accessSince) {
		return nil, natsrouter.ErrForbidden("message is outside access window")
	}

	now := time.Now().UTC()
	lastMsgAt, createdAt, err := s.resolveRoomTimesOrError(c, roomID, req.Meta, now)
	if err != nil {
		return nil, err
	}

	ceiling, floor := s.walkBounds(lastMsgAt, createdAt, now)

	limit := req.Limit
	if limit <= 0 {
		limit = surroundingPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	// Split limit-1 across before/after; before gets the larger half on odd splits.
	remaining := limit - 1
	if remaining <= 0 {
		only := *centralMsg
		redactUnavailableQuote(&only, accessSince)
		return &models.LoadSurroundingMessagesResponse{
			Messages: []models.Message{only},
		}, nil
	}
	beforeCount := (remaining + 1) / 2
	afterCount := remaining / 2

	beforePageReq, err := parsePageRequest("", beforeCount)
	if err != nil {
		return nil, err
	}
	afterPageReq, err := parsePageRequest("", afterCount)
	if err != nil {
		return nil, err
	}

	// before- and after-walks are independent — issue both in parallel.
	var (
		beforePage cassrepo.Page[models.Message]
		afterPage  cassrepo.Page[models.Message]
	)
	g, gctx := errgroup.WithContext(c)
	g.Go(func() error {
		var berr error
		if accessSince == nil {
			beforePage, berr = s.msgReader.GetMessagesBefore(gctx, roomID, centralMsg.CreatedAt, floor, beforePageReq)
		} else {
			beforePage, berr = s.msgReader.GetMessagesBetweenDesc(gctx, roomID, *accessSince, centralMsg.CreatedAt, beforePageReq)
		}
		if berr != nil {
			slog.Error("loading surrounding messages", "error", berr, "roomID", roomID, "direction", "before")
		}
		return berr
	})
	g.Go(func() error {
		var aerr error
		afterPage, aerr = s.msgReader.GetMessagesAfter(gctx, roomID, centralMsg.CreatedAt, ceiling, afterPageReq)
		if aerr != nil {
			slog.Error("loading surrounding messages", "error", aerr, "roomID", roomID, "direction", "after")
		}
		return aerr
	})
	if err := g.Wait(); err != nil {
		return nil, natsrouter.ErrInternal("failed to load surrounding messages")
	}

	// Assemble: reverse before-page (DESC→ASC) + central + after-page (already ASC).
	messages := make([]models.Message, 0, len(beforePage.Data)+1+len(afterPage.Data))
	for i := len(beforePage.Data) - 1; i >= 0; i-- {
		messages = append(messages, beforePage.Data[i])
	}
	messages = append(messages, *centralMsg)
	messages = append(messages, afterPage.Data...)

	redactUnavailableQuotes(messages, accessSince)
	return &models.LoadSurroundingMessagesResponse{
		Messages:   messages,
		MoreBefore: beforePage.HasNext,
		MoreAfter:  afterPage.HasNext,
	}, nil
}

func (s *HistoryService) GetMessageByID(c *natsrouter.Context, req models.GetMessageByIDRequest) (*models.Message, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	accessSince, err := s.getAccessSince(c, account, roomID)
	if err != nil {
		return nil, err
	}

	msg, err := s.findMessage(c, roomID, req.MessageID)
	if err != nil {
		return nil, err
	}

	if accessSince != nil && msg.CreatedAt.Before(*accessSince) {
		return nil, natsrouter.ErrForbidden("message is outside access window")
	}

	redactUnavailableQuote(msg, accessSince)
	return msg, nil
}

// EditMessage handles chat.user.{account}.request.room.{roomID}.{siteID}.msg.edit.
// Sender-only auth. Writes to all applicable Cassandra tables via
// UpdateMessageContent, then publishes a best-effort canonical MessageEvent
// (Event=updated) to subject.MsgCanonicalUpdated(siteID). broadcast-worker
// fans the canonical event out to per-user RoomEvents; search-sync-worker
// updates the ES index from the same canonical event. Publish failure logs
// and continues — Cassandra is the source of truth.
func (s *HistoryService) EditMessage(c *natsrouter.Context, siteID string, req models.EditMessageRequest) (*models.EditMessageResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	if _, err := s.getAccessSince(c, account, roomID); err != nil {
		return nil, err
	}

	// findMessage returns ErrNotFound for missing IDs and for messages that belong
	// to a different room (same error, no leak).
	msg, err := s.findMessage(c, roomID, req.MessageID)
	if err != nil {
		return nil, err
	}

	// A soft-deleted message must not be editable — that would emit a
	// message_edited event after message_deleted, which downstream consumers
	// can't reconcile. Same ErrNotFound as wrong-room to keep the leak
	// surface symmetric.
	if msg.Deleted {
		return nil, natsrouter.ErrNotFound("message not found")
	}

	if !canModify(msg, account) {
		return nil, natsrouter.ErrForbidden("only the sender can edit")
	}

	if strings.TrimSpace(req.NewMsg) == "" {
		return nil, natsrouter.ErrBadRequest("newMsg must not be empty")
	}
	if len(req.NewMsg) > maxContentBytes {
		return nil, natsrouter.ErrBadRequest("newMsg exceeds maximum size")
	}

	editedAt := time.Now().UTC()
	if err := s.msgWriter.UpdateMessageContent(c, msg, req.NewMsg, editedAt); err != nil {
		slog.Error("edit: update content", "error", err, "messageID", req.MessageID)
		return nil, natsrouter.ErrInternal("failed to edit message")
	}

	editedAtMs := editedAt.UnixMilli()

	// Carry only the fields downstream actually reads: search-sync-worker
	// reindexes by Content/EditedAt/UpdatedAt; broadcast-worker emits a slim
	// MessageEditedPayload of {ID, Content, EditedBy, EditedAt, UpdatedAt}.
	// Mentions intentionally omitted — broadcast-worker re-resolves from Content.
	canonicalEvt := model.MessageEvent{
		Event: model.EventUpdated,
		Message: model.Message{
			ID:          msg.MessageID,
			RoomID:      msg.RoomID,
			UserID:      msg.Sender.ID,
			UserAccount: msg.Sender.Account,
			Content:     req.NewMsg,
			CreatedAt:   msg.CreatedAt,
			EditedAt:    &editedAt,
			UpdatedAt:   &editedAt,
		},
		SiteID:    siteID,
		Timestamp: editedAtMs,
	}
	s.publishCanonicalBestEffort(c, subject.MsgCanonicalUpdated(siteID), &canonicalEvt, req.MessageID, roomID)

	return &models.EditMessageResponse{
		MessageID: req.MessageID,
		EditedAt:  editedAtMs,
	}, nil
}

// DeleteMessage handles chat.user.{account}.request.room.{roomID}.{siteID}.msg.delete.
// Sender-only auth. Soft-deletes (deleted = true, updated_at = ?) across all
// applicable Cassandra tables via SoftDeleteMessage, including tcount
// decrement on the parent for thread replies. On already-deleted messages the
// handler short-circuits and returns success without repeating the UPDATEs or
// publishing a duplicate event — this prevents tcount drift on caller retry.
func (s *HistoryService) DeleteMessage(c *natsrouter.Context, siteID string, req models.DeleteMessageRequest) (*models.DeleteMessageResponse, error) {
	account := c.Param("account")
	roomID := c.Param("roomID")

	if _, err := s.getAccessSince(c, account, roomID); err != nil {
		return nil, err
	}

	msg, err := s.findMessage(c, roomID, req.MessageID)
	if err != nil {
		return nil, err
	}

	if !canModify(msg, account) {
		return nil, natsrouter.ErrForbidden("only the sender can delete")
	}

	// Already-deleted short-circuit: echo the current updated_at as the DeletedAt.
	// Prevents tcount double-decrement on caller retry and avoids duplicate events.
	if msg.Deleted {
		var deletedAtMs int64
		if msg.UpdatedAt != nil {
			deletedAtMs = msg.UpdatedAt.UnixMilli()
		}
		return &models.DeleteMessageResponse{
			MessageID: req.MessageID,
			DeletedAt: deletedAtMs,
		}, nil
	}

	deletedAt := time.Now().UTC()
	actualDeletedAt, applied, err := s.msgWriter.SoftDeleteMessage(c, msg, deletedAt)
	if err != nil {
		slog.Error("delete: soft-delete", "error", err, "messageID", req.MessageID)
		return nil, natsrouter.ErrInternal("failed to delete message")
	}
	if !applied {
		// A concurrent delete won the CAS. Skip the publish — the winning
		// goroutine has emitted (or will emit) the canonical .deleted event —
		// and return the timestamp actually persisted.
		return &models.DeleteMessageResponse{
			MessageID: req.MessageID,
			DeletedAt: actualDeletedAt.UnixMilli(),
		}, nil
	}

	deletedAtMs := actualDeletedAt.UnixMilli()

	canonicalEvt := model.MessageEvent{
		Event: model.EventDeleted,
		Message: model.Message{
			ID:          msg.MessageID,
			RoomID:      msg.RoomID,
			UserID:      msg.Sender.ID,
			UserAccount: msg.Sender.Account,
			CreatedAt:   msg.CreatedAt,
			UpdatedAt:   &actualDeletedAt,
		},
		SiteID:    siteID,
		Timestamp: deletedAtMs,
	}
	s.publishCanonicalBestEffort(c, subject.MsgCanonicalDeleted(siteID), &canonicalEvt, req.MessageID, roomID)

	return &models.DeleteMessageResponse{
		MessageID: req.MessageID,
		DeletedAt: deletedAtMs,
	}, nil
}

// publishCanonicalBestEffort marshals and publishes a canonical MessageEvent.
// Cassandra is already the source of truth, so a marshal/publish failure is
// logged and swallowed rather than failing the RPC (spec D2).
func (s *HistoryService) publishCanonicalBestEffort(c *natsrouter.Context, subj string, evt *model.MessageEvent, messageID, roomID string) {
	payload, err := json.Marshal(evt)
	if err != nil {
		slog.Warn("canonical marshal failed",
			"error", err, "subject", subj, "messageID", messageID, "roomID", roomID)
		return
	}
	if err := s.publisher.Publish(c, subj, payload); err != nil {
		slog.Warn("canonical publish failed",
			"error", err, "subject", subj, "messageID", messageID, "roomID", roomID)
	}
}
