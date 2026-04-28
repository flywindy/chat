package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/mention"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/userstore"
)

type Handler struct {
	store       Store
	userStore   userstore.UserStore
	threadStore ThreadStore
}

func NewHandler(store Store, userStore userstore.UserStore, threadStore ThreadStore) *Handler {
	return &Handler{store: store, userStore: userStore, threadStore: threadStore}
}

func (h *Handler) HandleJetStreamMsg(ctx context.Context, msg jetstream.Msg) {
	if err := h.processMessage(ctx, msg.Data()); err != nil {
		slog.Error("process message failed", "error", err)
		if nakErr := msg.Nak(); nakErr != nil {
			slog.Error("failed to nack message", "error", nakErr)
		}
		return
	}

	if err := msg.Ack(); err != nil {
		slog.Error("failed to ack message", "err", err)
	}
}

func (h *Handler) processMessage(ctx context.Context, data []byte) error {
	var evt model.MessageEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return fmt.Errorf("unmarshal message event: %w", err)
	}

	resolved, err := mention.Resolve(ctx, evt.Message.Content, h.userStore.FindUsersByAccounts)
	if err != nil {
		return fmt.Errorf("resolve mentions: %w", err)
	}
	evt.Message.Mentions = resolved.Participants

	var sender *cassParticipant
	user, err := h.userStore.FindUserByID(ctx, evt.Message.UserID)
	if err != nil {
		if evt.Message.Type != "" {
			// System messages may have no real user; proceed with nil sender.
			slog.Warn("user not found for system message, using nil sender",
				"userID", evt.Message.UserID, "type", evt.Message.Type)
		} else {
			return fmt.Errorf("lookup user %s: %w", evt.Message.UserID, err)
		}
	} else {
		sender = &cassParticipant{
			ID:          user.ID,
			EngName:     user.EngName,
			CompanyName: user.ChineseName,
			Account:     evt.Message.UserAccount,
		}
	}

	if evt.Message.ThreadParentMessageID != "" {
		// Resolve (or create) the thread room first so we have the threadRoomID
		// before persisting the message to Cassandra.
		threadRoomID, err := h.handleThreadRoomAndSubscriptions(ctx, &evt.Message, evt.SiteID)
		if err != nil {
			return fmt.Errorf("handle thread room and subscriptions: %w", err)
		}
		if err := h.markThreadMentions(ctx, &evt.Message, threadRoomID, evt.SiteID); err != nil {
			return fmt.Errorf("mark thread mentions: %w", err)
		}
		if err := h.store.SaveThreadMessage(ctx, &evt.Message, sender, evt.SiteID, threadRoomID); err != nil {
			return fmt.Errorf("save thread message: %w", err)
		}
	} else {
		if err := h.store.SaveMessage(ctx, &evt.Message, sender, evt.SiteID); err != nil {
			return fmt.Errorf("save message: %w", err)
		}
	}

	return nil
}

func (h *Handler) handleThreadRoomAndSubscriptions(ctx context.Context, msg *model.Message, siteID string) (string, error) {
	now := msg.CreatedAt

	var parentCreatedAt time.Time
	if msg.ThreadParentMessageCreatedAt != nil {
		parentCreatedAt = *msg.ThreadParentMessageCreatedAt
	}
	threadRoom := model.ThreadRoom{
		ID:                    idgen.GenerateID(),
		ParentMessageID:       msg.ThreadParentMessageID,
		ThreadParentCreatedAt: parentCreatedAt,
		RoomID:                msg.RoomID,
		SiteID:                siteID,
		LastMsgAt:             now,
		LastMsgID:             msg.ID,
		ReplyAccounts:         []string{msg.UserAccount},
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	err := h.threadStore.CreateThreadRoom(ctx, &threadRoom)
	switch {
	case err == nil:
		return threadRoom.ID, h.handleFirstThreadReply(ctx, msg, siteID, threadRoom.ID, now)
	case errors.Is(err, errThreadRoomExists):
		return h.handleSubsequentThreadReply(ctx, msg, siteID, now)
	default:
		return "", fmt.Errorf("create thread room: %w", err)
	}
}

func (h *Handler) handleFirstThreadReply(ctx context.Context, msg *model.Message, siteID, threadRoomID string, now time.Time) error {
	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	if err != nil {
		if errors.Is(err, errMessageNotFound) {
			slog.Warn("thread reply parent not found — skipping subscription creation",
				"parentMessageID", msg.ThreadParentMessageID,
				"replyID", msg.ID)
			return nil
		}
		return fmt.Errorf("get parent message sender: %w", err)
	}

	if err := h.threadStore.InsertThreadSubscription(ctx,
		h.buildThreadSubscription(msg, threadRoomID, parentSender.ID, parentSender.Account, siteID, now),
	); err != nil {
		return fmt.Errorf("insert parent author thread subscription: %w", err)
	}

	if msg.UserID != parentSender.ID {
		if err := h.threadStore.InsertThreadSubscription(ctx,
			h.buildThreadSubscription(msg, threadRoomID, msg.UserID, msg.UserAccount, siteID, now),
		); err != nil {
			return fmt.Errorf("insert replier thread subscription: %w", err)
		}
	}

	// Requires ThreadParentMessageCreatedAt to address the Cassandra row;
	// skipped when absent.
	if msg.ThreadParentMessageCreatedAt != nil {
		if err := h.store.UpdateParentMessageThreadRoomID(ctx, msg.ThreadParentMessageID, msg.RoomID, *msg.ThreadParentMessageCreatedAt, threadRoomID); err != nil {
			return fmt.Errorf("stamp thread_room_id on parent message: %w", err)
		}
	}

	return nil
}

// Upserts are idempotent — redeliveries after a partial first attempt never lose
// the parent subscription.
func (h *Handler) handleSubsequentThreadReply(ctx context.Context, msg *model.Message, siteID string, now time.Time) (string, error) {
	existingRoom, err := h.threadStore.GetThreadRoomByParentMessageID(ctx, msg.ThreadParentMessageID)
	if err != nil {
		return "", fmt.Errorf("get existing thread room: %w", err)
	}

	parentFound := true
	parentSender, err := h.store.GetMessageSender(ctx, msg.ThreadParentMessageID)
	switch {
	case err == nil:
		if err := h.threadStore.UpsertThreadSubscription(ctx,
			h.buildThreadSubscription(msg, existingRoom.ID, parentSender.ID, parentSender.Account, siteID, now),
		); err != nil {
			return "", fmt.Errorf("upsert parent author thread subscription: %w", err)
		}
		if msg.UserID != parentSender.ID {
			if err := h.threadStore.UpsertThreadSubscription(ctx,
				h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, siteID, now),
			); err != nil {
				return "", fmt.Errorf("upsert replier thread subscription: %w", err)
			}
		}
	case errors.Is(err, errMessageNotFound):
		parentFound = false
		slog.Warn("thread reply parent not found — skipping parent subscription upsert",
			"parentMessageID", msg.ThreadParentMessageID,
			"replyID", msg.ID)
		if err := h.threadStore.UpsertThreadSubscription(ctx,
			h.buildThreadSubscription(msg, existingRoom.ID, msg.UserID, msg.UserAccount, siteID, now),
		); err != nil {
			return "", fmt.Errorf("upsert replier thread subscription: %w", err)
		}
	default:
		return "", fmt.Errorf("get parent message sender: %w", err)
	}

	if err := h.threadStore.UpdateThreadRoomLastMessage(ctx, existingRoom.ID, msg.ID, msg.UserAccount, now); err != nil {
		return "", fmt.Errorf("update thread room last message: %w", err)
	}

	// Re-stamp handles redelivery: first attempt may have created the thread room
	// but crashed before the stamp landed. IF EXISTS in the store prevents phantom rows.
	if parentFound && msg.ThreadParentMessageCreatedAt != nil {
		if err := h.store.UpdateParentMessageThreadRoomID(ctx, msg.ThreadParentMessageID, msg.RoomID, *msg.ThreadParentMessageCreatedAt, existingRoom.ID); err != nil {
			return "", fmt.Errorf("stamp thread_room_id on parent message: %w", err)
		}
	}

	return existingRoom.ID, nil
}

// lastSeenAt is always nil — subscriptions are insert-only; only the read path
// (client marking the thread as seen) ever sets this field.
func (h *Handler) buildThreadSubscription(msg *model.Message, threadRoomID, userID, userAccount, siteID string, now time.Time) *model.ThreadSubscription {
	return &model.ThreadSubscription{
		ID:              idgen.GenerateID(),
		ParentMessageID: msg.ThreadParentMessageID,
		RoomID:          msg.RoomID,
		ThreadRoomID:    threadRoomID,
		UserID:          userID,
		UserAccount:     userAccount,
		SiteID:          siteID,
		LastSeenAt:      nil,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// Sender and @all are excluded: sender already has a subscription; @all is a
// broadcast that shouldn't trigger per-user mention state in threads.
func (h *Handler) markThreadMentions(ctx context.Context, msg *model.Message, threadRoomID, siteID string) error {
	for i := range msg.Mentions {
		p := &msg.Mentions[i]
		if p.Account == "all" {
			continue
		}
		if p.UserID == msg.UserID {
			continue
		}
		sub := h.buildThreadSubscription(msg, threadRoomID, p.UserID, p.Account, siteID, msg.CreatedAt)
		sub.HasMention = true
		if err := h.threadStore.MarkThreadSubscriptionMention(ctx, sub); err != nil {
			return fmt.Errorf("mark thread subscription mention for user %s: %w", p.UserID, err)
		}
	}
	return nil
}
