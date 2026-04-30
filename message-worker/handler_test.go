package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
)

func ptrTime(t time.Time) *time.Time { return &t }

func TestHandler_ProcessMessage(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	user := &model.User{
		ID:          "u-1",
		Account:     "alice",
		SiteID:      "site-a",
		EngName:     "Alice Wang",
		ChineseName: "愛麗絲",
	}
	msg := model.Message{
		ID:          "msg-1",
		RoomID:      "r1",
		UserID:      "u-1",
		UserAccount: "alice",
		Content:     "hello",
		CreatedAt:   now,
	}
	evt := model.MessageEvent{Message: msg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	validData, _ := json.Marshal(evt)

	threadMsg := model.Message{
		ID:                    "msg-2",
		RoomID:                "r1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-1",
	}
	threadEvt := model.MessageEvent{Message: threadMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	threadData, _ := json.Marshal(threadEvt)

	bobUser := &model.User{
		ID:          "u-bob",
		Account:     "bob",
		SiteID:      "site-a",
		EngName:     "Bob Chen",
		ChineseName: "鮑勃",
	}

	// Thread reply that mentions @bob (non-participant).
	threadMentionMsg := model.Message{
		ID:                    "msg-thread-mention",
		RoomID:                "r1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply @bob",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-1",
	}
	threadMentionEvt := model.MessageEvent{Message: threadMentionMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	threadMentionData, _ := json.Marshal(threadMentionEvt)

	// Thread reply where sender self-mentions — must be excluded.
	threadSelfMsg := model.Message{
		ID:                    "msg-thread-self",
		RoomID:                "r1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply @alice",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-1",
	}
	threadSelfEvt := model.MessageEvent{Message: threadSelfMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	threadSelfData, _ := json.Marshal(threadSelfEvt)

	// Thread reply with @all only — must be ignored at thread level.
	threadAllMsg := model.Message{
		ID:                    "msg-thread-all",
		RoomID:                "r1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply @all",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-1",
	}
	threadAllEvt := model.MessageEvent{Message: threadAllMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	threadAllData, _ := json.Marshal(threadAllEvt)

	// Thread reply mixing @all + @bob — only bob gets marked.
	threadMixMsg := model.Message{
		ID:                    "msg-thread-mix",
		RoomID:                "r1",
		UserID:                "u-1",
		UserAccount:           "alice",
		Content:               "thread reply @all and @bob",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-1",
	}
	threadMixEvt := model.MessageEvent{Message: threadMixMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	threadMixData, _ := json.Marshal(threadMixEvt)

	// Event with a real user mention — Mentions field is absent in the inbound event
	// and will be populated by resolveMentions.
	evtWithMention := model.MessageEvent{
		Message: model.Message{
			ID: "msg-3", RoomID: "r1", UserID: "u-1", UserAccount: "alice",
			Content:   "hey @bob can you check this?",
			CreatedAt: now,
		},
		SiteID: "site-a", Timestamp: now.UnixMilli(),
	}
	dataWithMention, _ := json.Marshal(evtWithMention)

	// Expected stored message: Mentions resolved to full Participant.
	msgWithMention := model.Message{
		ID: "msg-3", RoomID: "r1", UserID: "u-1", UserAccount: "alice",
		Content:   "hey @bob can you check this?",
		CreatedAt: now,
		Mentions: []model.Participant{{
			UserID: "u-bob", Account: "bob", ChineseName: "鮑勃", EngName: "Bob Chen",
		}},
	}

	// Event with @all — no user lookup should occur.
	evtWithAll := model.MessageEvent{
		Message: model.Message{
			ID: "msg-4", RoomID: "r1", UserID: "u-1", UserAccount: "alice",
			Content:   "hello @all please read",
			CreatedAt: now,
		},
		SiteID: "site-a", Timestamp: now.UnixMilli(),
	}
	dataWithAll, _ := json.Marshal(evtWithAll)

	msgWithAll := model.Message{
		ID: "msg-4", RoomID: "r1", UserID: "u-1", UserAccount: "alice",
		Content:   "hello @all please read",
		CreatedAt: now,
		Mentions:  []model.Participant{{Account: "all", EngName: "all"}},
	}

	expectedSender := cassParticipant{
		ID:          user.ID,
		EngName:     user.EngName,
		CompanyName: user.ChineseName,
		Account:     msg.UserAccount,
	}

	tests := []struct {
		name       string
		data       []byte
		setupMocks func(store *MockStore, userStore *MockUserStore, threadStore *MockThreadStore)
		wantErr    bool
	}{
		{
			name: "happy path — user found and message saved",
			data: validData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				store.EXPECT().SaveMessage(gomock.Any(), &msg, &expectedSender, "site-a").Return(nil)
			},
		},
		{
			name: "user not found — NAK without saving",
			data: validData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").
					Return(nil, errors.New("user not found"))
			},
			wantErr: true,
		},
		{
			name: "user store DB error — NAK without saving",
			data: validData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").
					Return(nil, errors.New("mongo: connection refused"))
			},
			wantErr: true,
		},
		{
			name: "save error — NAK after user lookup",
			data: validData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				store.EXPECT().SaveMessage(gomock.Any(), &msg, &expectedSender, "site-a").
					Return(errors.New("cassandra: write timeout"))
			},
			wantErr: true,
		},
		{
			name:       "malformed JSON — NAK immediately",
			data:       []byte("{invalid"),
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {},
			wantErr:    true,
		},
		{
			name: "thread message — calls SaveThreadMessage not SaveMessage",
			data: threadData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				// handleThreadRoomAndSubscriptions runs first to resolve the threadRoomID.
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-1").
					Return(&model.ThreadRoom{ID: "tr-1"}, nil)
				// Subsequent-reply path: upsert parent and replier subscriptions.
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-1", "msg-2", "alice", now).Return(nil)
				// SaveThreadMessage receives the resolved threadRoomID.
				store.EXPECT().SaveThreadMessage(gomock.Any(), &threadMsg, &expectedSender, "site-a", "tr-1").Return(nil)
			},
		},
		{
			name: "thread message save error — NAK after user lookup",
			data: threadData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				// handleThreadRoomAndSubscriptions runs before SaveThreadMessage.
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-1").
					Return(&model.ThreadRoom{ID: "tr-1"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-1", "msg-2", "alice", now).Return(nil)
				store.EXPECT().SaveThreadMessage(gomock.Any(), &threadMsg, &expectedSender, "site-a", "tr-1").
					Return(errors.New("cassandra: write timeout"))
			},
			wantErr: true,
		},
		{
			name: "mention resolved to Participant and stored",
			data: dataWithMention,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).
					Return([]model.User{*bobUser}, nil)
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				store.EXPECT().SaveMessage(gomock.Any(), &msgWithMention, &expectedSender, "site-a").Return(nil)
			},
		},
		{
			name: "@all stored as special Participant without DB lookup",
			data: dataWithAll,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				store.EXPECT().SaveMessage(gomock.Any(), &msgWithAll, &expectedSender, "site-a").Return(nil)
			},
		},
		{
			name: "mention user lookup error — NAK before sender lookup",
			data: dataWithMention,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).
					Return(nil, errors.New("mongo: connection refused"))
				// FindUserByID and SaveMessage must NOT be called
			},
			wantErr: true,
		},
		{
			name: "system message with unknown user — saved with nil sender",
			data: func() []byte {
				sysMsg := model.Message{
					ID: "msg-sys-1", RoomID: "r1", Content: "added members",
					CreatedAt: now, Type: "members_added",
					SysMsgData: []byte(`{"individuals":["bob"]}`),
				}
				e := model.MessageEvent{Message: sysMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
				d, _ := json.Marshal(e)
				return d
			}(),
			setupMocks: func(store *MockStore, us *MockUserStore, _ *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "").
					Return(nil, errors.New("user not found"))
				expectedMsg := model.Message{
					ID: "msg-sys-1", RoomID: "r1", Content: "added members",
					CreatedAt: now, Type: "members_added",
					SysMsgData: []byte(`{"individuals":["bob"]}`),
				}
				store.EXPECT().SaveMessage(gomock.Any(), &expectedMsg, (*cassParticipant)(nil), "site-a").Return(nil)
			},
		},
		{
			name: "regular message with user lookup error — still returns error",
			data: validData,
			setupMocks: func(_ *MockStore, us *MockUserStore, _ *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").
					Return(nil, errors.New("user not found"))
			},
			wantErr: true,
		},
		{
			name: "thread reply mentioning non-participant — marks that user's subscription",
			data: threadMentionData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).
					Return([]model.User{*bobUser}, nil)
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				// First-reply path: create the thread room.
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				// Parent + replier subscriptions inserted.
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				// Mentionee @bob gets MarkThreadSubscriptionMention — assert sub fields.
				ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-bob", sub.UserID)
						assert.Equal(t, "bob", sub.UserAccount)
						assert.Equal(t, "msg-1", sub.ParentMessageID)
						assert.Equal(t, "r1", sub.RoomID)
						assert.Equal(t, "site-a", sub.SiteID)
						assert.True(t, sub.HasMention)
						assert.Nil(t, sub.LastSeenAt)
						return nil
					})
				store.EXPECT().SaveThreadMessage(gomock.Any(), gomock.Any(), gomock.Any(), "site-a", gomock.Any()).Return(nil)
			},
		},
		{
			name: "thread reply where sender self-mentions — no MarkThreadSubscriptionMention call",
			data: threadSelfData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				// Sender's own account looked up; returns the sender user.
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"alice"}).
					Return([]model.User{*user}, nil)
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				// MarkThreadSubscriptionMention must NOT be called — sender excluded.
				store.EXPECT().SaveThreadMessage(gomock.Any(), gomock.Any(), gomock.Any(), "site-a", gomock.Any()).Return(nil)
			},
		},
		{
			name: "thread reply with @all only — no MarkThreadSubscriptionMention call",
			data: threadAllData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				// No account lookup — @all bypasses the user-by-accounts query.
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				// MarkThreadSubscriptionMention must NOT be called — @all is thread-ignored.
				store.EXPECT().SaveThreadMessage(gomock.Any(), gomock.Any(), gomock.Any(), "site-a", gomock.Any()).Return(nil)
			},
		},
		{
			name: "thread reply with @all + @bob — only bob marked",
			data: threadMixData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).
					Return([]model.User{*bobUser}, nil)
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-bob", sub.UserID)
						assert.True(t, sub.HasMention)
						return nil
					})
				store.EXPECT().SaveThreadMessage(gomock.Any(), gomock.Any(), gomock.Any(), "site-a", gomock.Any()).Return(nil)
			},
		},
		{
			name: "thread reply mentioning non-participant — MarkThreadSubscriptionMention error is propagated",
			data: threadMentionData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).
					Return([]model.User{*bobUser}, nil)
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-1").
					Return(&cassParticipant{ID: "u-parent", Account: "parent-user"}, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().MarkThreadSubscriptionMention(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: write error"))
				// SaveThreadMessage must NOT be called — mention-mark error aborts before save.
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockStore := NewMockStore(ctrl)
			mockUserStore := NewMockUserStore(ctrl)
			mockThreadStore := NewMockThreadStore(ctrl)
			tt.setupMocks(mockStore, mockUserStore, mockThreadStore)

			h := NewHandler(mockStore, mockUserStore, mockThreadStore)
			err := h.processMessage(context.Background(), tt.data)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHandler_HandleThreadRoomAndSubscriptions(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	parentSender := &cassParticipant{
		ID:      "u-parent",
		Account: "parent-user",
	}

	msg := &model.Message{
		ID:                    "msg-reply",
		RoomID:                "r1",
		UserID:                "u-replier",
		UserAccount:           "replier",
		Content:               "thread reply",
		CreatedAt:             now,
		ThreadParentMessageID: "msg-parent",
	}

	tests := []struct {
		name       string
		msg        *model.Message
		siteID     string
		setupMocks func(store *MockStore, ts *MockThreadStore)
		wantErr    bool
	}{
		{
			name:   "first reply — different users — creates room and two subscriptions",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, room *model.ThreadRoom) error {
						assert.Equal(t, "msg-parent", room.ParentMessageID)
						assert.Equal(t, "r1", room.RoomID)
						assert.Equal(t, "site-a", room.SiteID)
						assert.Equal(t, "msg-reply", room.LastMsgID)
						assert.Equal(t, []string{"replier"}, room.ReplyAccounts)
						return nil
					})
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-parent", sub.UserID)
						assert.Equal(t, "parent-user", sub.UserAccount)
						assert.Nil(t, sub.LastSeenAt, "parent's LastSeenAt should be nil on init")
						return nil
					})
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-replier", sub.UserID)
						assert.Equal(t, "replier", sub.UserAccount)
						assert.Nil(t, sub.LastSeenAt, "replier's LastSeenAt should be nil on init")
						return nil
					})
			},
		},
		{
			name:   "first reply — parent message not found — ack-and-skip",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(nil, fmt.Errorf("wrap: %w", errMessageNotFound))
			},
			wantErr: false,
		},
		{
			name: "first reply — same user — creates room and one subscription",
			msg: &model.Message{
				ID:                    "msg-reply",
				RoomID:                "r1",
				UserID:                "u-parent",
				UserAccount:           "parent-user",
				Content:               "self reply",
				CreatedAt:             now,
				ThreadParentMessageID: "msg-parent",
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-parent", sub.UserID)
						return nil
					})
			},
		},
		{
			name:   "first reply — GetMessageSender fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(nil, errors.New("cassandra: read timeout"))
			},
			wantErr: true,
		},
		{
			name:   "first reply — parent InsertThreadSubscription fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: write error"))
			},
			wantErr: true,
		},
		{
			name:   "first reply — replier InsertThreadSubscription fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				// Parent insert succeeds
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				// Replier insert fails
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: write error"))
			},
			wantErr: true,
		},
		{
			name:   "subsequent reply — upserts parent and replier subscriptions and updates last message",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "tr-existing", sub.ThreadRoomID)
						assert.Equal(t, "u-parent", sub.UserID)
						assert.Nil(t, sub.LastSeenAt, "parent's LastSeenAt should be nil on init")
						return nil
					})
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "tr-existing", sub.ThreadRoomID)
						assert.Equal(t, "u-replier", sub.UserID)
						assert.Nil(t, sub.LastSeenAt, "replier's LastSeenAt should be nil on init")
						return nil
					})
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).
					Return(nil)
			},
		},
		{
			name: "subsequent reply — same user as parent — upserts one subscription and updates last message",
			msg: &model.Message{
				ID:                    "msg-reply",
				RoomID:                "r1",
				UserID:                "u-parent",
				UserAccount:           "parent-user",
				Content:               "self reply",
				CreatedAt:             now,
				ThreadParentMessageID: "msg-parent",
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-parent", sub.UserID)
						return nil
					})
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "parent-user", now).
					Return(nil)
			},
		},
		{
			name:   "subsequent reply — parent message not found — skips parent upsert and upserts replier",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(nil, fmt.Errorf("wrap: %w", errMessageNotFound))
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-replier", sub.UserID)
						return nil
					})
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).
					Return(nil)
			},
		},
		{
			name:   "subsequent reply — GetThreadRoomByParentMessageID fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(nil, errors.New("mongo: connection refused"))
			},
			wantErr: true,
		},
		{
			name:   "subsequent reply — GetMessageSender fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(nil, errors.New("cassandra: read timeout"))
			},
			wantErr: true,
		},
		{
			name:   "subsequent reply — UpsertThreadSubscription for parent fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: write error"))
			},
			wantErr: true,
		},
		{
			name:   "subsequent reply — UpsertThreadSubscription for replier fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: write error"))
			},
			wantErr: true,
		},
		{
			name:   "subsequent reply — UpdateThreadRoomLastMessage fails — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).
					Return(errors.New("mongo: write error"))
			},
			wantErr: true,
		},
		{
			name:   "CreateThreadRoom unexpected error — returns error",
			msg:    msg,
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					Return(errors.New("mongo: connection refused"))
			},
			wantErr: true,
		},
		{
			name: "first reply — stamps thread_room_id on parent when parentCreatedAt known",
			msg: &model.Message{
				ID:                           "msg-reply",
				RoomID:                       "r1",
				UserID:                       "u-replier",
				UserAccount:                  "replier",
				CreatedAt:                    now,
				ThreadParentMessageID:        "msg-parent",
				ThreadParentMessageCreatedAt: ptrTime(now.Add(-5 * time.Minute)),
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				var capturedRoomID string
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, room *model.ThreadRoom) error {
						capturedRoomID = room.ID
						return nil
					})
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().UpdateParentMessageThreadRoomID(
					gomock.Any(), "msg-parent", "r1",
					now.Add(-5*time.Minute),
					gomock.Cond(func(x any) bool { s, ok := x.(string); return ok && s == capturedRoomID }),
				).Return(nil)
			},
		},
		{
			name: "first reply — UpdateParentMessageThreadRoomID fails — returns error",
			msg: &model.Message{
				ID:                           "msg-reply",
				RoomID:                       "r1",
				UserID:                       "u-replier",
				UserAccount:                  "replier",
				CreatedAt:                    now,
				ThreadParentMessageID:        "msg-parent",
				ThreadParentMessageCreatedAt: ptrTime(now.Add(-5 * time.Minute)),
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(parentSender, nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().InsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				store.EXPECT().UpdateParentMessageThreadRoomID(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(errors.New("cassandra: write timeout"))
			},
			wantErr: true,
		},
		{
			name: "subsequent reply — stamps thread_room_id on parent when parentCreatedAt known",
			msg: &model.Message{
				ID:                           "msg-reply",
				RoomID:                       "r1",
				UserID:                       "u-replier",
				UserAccount:                  "replier",
				CreatedAt:                    now,
				ThreadParentMessageID:        "msg-parent",
				ThreadParentMessageCreatedAt: ptrTime(now.Add(-5 * time.Minute)),
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).Return(nil)
				store.EXPECT().UpdateParentMessageThreadRoomID(
					gomock.Any(), "msg-parent", "r1",
					now.Add(-5*time.Minute),
					"tr-existing",
				).Return(nil)
			},
		},
		{
			name: "subsequent reply — UpdateParentMessageThreadRoomID fails — returns error",
			msg: &model.Message{
				ID:                           "msg-reply",
				RoomID:                       "r1",
				UserID:                       "u-replier",
				UserAccount:                  "replier",
				CreatedAt:                    now,
				ThreadParentMessageID:        "msg-parent",
				ThreadParentMessageCreatedAt: ptrTime(now.Add(-5 * time.Minute)),
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").Return(parentSender, nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).Return(nil)
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).Return(nil)
				store.EXPECT().UpdateParentMessageThreadRoomID(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(errors.New("cassandra: write timeout"))
			},
			wantErr: true,
		},
		{
			name: "subsequent reply — parent not found but parentCreatedAt known — skips UpdateParentMessageThreadRoomID",
			msg: &model.Message{
				ID:                           "msg-reply",
				RoomID:                       "r1",
				UserID:                       "u-replier",
				UserAccount:                  "replier",
				CreatedAt:                    now,
				ThreadParentMessageID:        "msg-parent",
				ThreadParentMessageCreatedAt: ptrTime(now.Add(-5 * time.Minute)),
			},
			siteID: "site-a",
			setupMocks: func(store *MockStore, ts *MockThreadStore) {
				ts.EXPECT().CreateThreadRoom(gomock.Any(), gomock.Any()).Return(errThreadRoomExists)
				ts.EXPECT().GetThreadRoomByParentMessageID(gomock.Any(), "msg-parent").
					Return(&model.ThreadRoom{ID: "tr-existing"}, nil)
				store.EXPECT().GetMessageSender(gomock.Any(), "msg-parent").
					Return(nil, fmt.Errorf("wrap: %w", errMessageNotFound))
				ts.EXPECT().UpsertThreadSubscription(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, sub *model.ThreadSubscription) error {
						assert.Equal(t, "u-replier", sub.UserID)
						return nil
					})
				ts.EXPECT().UpdateThreadRoomLastMessage(gomock.Any(), "tr-existing", "msg-reply", "replier", now).Return(nil)
				// UpdateParentMessageThreadRoomID must NOT be called — parent doesn't exist
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockStore := NewMockStore(ctrl)
			mockThreadStore := NewMockThreadStore(ctrl)
			mockUserStore := NewMockUserStore(ctrl)
			tt.setupMocks(mockStore, mockThreadStore)

			h := NewHandler(mockStore, mockUserStore, mockThreadStore)
			_, err := h.handleThreadRoomAndSubscriptions(context.Background(), tt.msg, tt.siteID)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// fakeJSMsg is a minimal jetstream.Msg test double that records whether Ack or
// Nak was called so tests can assert on ack/nak behaviour.
type fakeJSMsg struct {
	data  []byte
	acked bool
	naked bool
}

func (m *fakeJSMsg) Data() []byte { return m.data }
func (m *fakeJSMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{}, nil
}
func (m *fakeJSMsg) Headers() nats.Header             { return nil }
func (m *fakeJSMsg) Subject() string                  { return "test.subject" }
func (m *fakeJSMsg) Reply() string                    { return "" }
func (m *fakeJSMsg) Ack() error                       { m.acked = true; return nil }
func (m *fakeJSMsg) DoubleAck(context.Context) error  { m.acked = true; return nil }
func (m *fakeJSMsg) Nak() error                       { m.naked = true; return nil }
func (m *fakeJSMsg) NakWithDelay(time.Duration) error { m.naked = true; return nil }
func (m *fakeJSMsg) InProgress() error                { return nil }
func (m *fakeJSMsg) Term() error                      { return nil }
func (m *fakeJSMsg) TermWithReason(string) error      { return nil }

func TestHandler_HandleJetStreamMsg(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	user := &model.User{
		ID: "u-1", Account: "alice", SiteID: "site-a",
		EngName: "Alice Wang", ChineseName: "愛麗絲",
	}
	msg := model.Message{
		ID: "msg-1", RoomID: "r1", UserID: "u-1", UserAccount: "alice",
		Content: "hello", CreatedAt: now,
	}
	evt := model.MessageEvent{Message: msg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	validData, _ := json.Marshal(evt)
	invalidData := []byte("{invalid")

	expectedSender := cassParticipant{
		ID: user.ID, EngName: user.EngName, CompanyName: user.ChineseName, Account: msg.UserAccount,
	}

	tests := []struct {
		name       string
		msgData    []byte
		setupMocks func(store *MockStore, us *MockUserStore, ts *MockThreadStore)
		wantAck    bool
		wantNak    bool
	}{
		{
			name:    "success — Ack called",
			msgData: validData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {
				us.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
				store.EXPECT().SaveMessage(gomock.Any(), &msg, &expectedSender, "site-a").Return(nil)
			},
			wantAck: true,
		},
		{
			name:       "failure — Nak called",
			msgData:    invalidData,
			setupMocks: func(store *MockStore, us *MockUserStore, ts *MockThreadStore) {},
			wantNak:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockStore := NewMockStore(ctrl)
			mockUserStore := NewMockUserStore(ctrl)
			mockThreadStore := NewMockThreadStore(ctrl)
			tt.setupMocks(mockStore, mockUserStore, mockThreadStore)

			h := NewHandler(mockStore, mockUserStore, mockThreadStore)

			fakeMsg := &fakeJSMsg{data: tt.msgData}
			h.HandleJetStreamMsg(context.Background(), fakeMsg)

			assert.Equal(t, tt.wantAck, fakeMsg.acked, "acked")
			assert.Equal(t, tt.wantNak, fakeMsg.naked, "naked")
		})
	}
}

func TestHandler_ProcessMessage_Quote(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	user := &model.User{
		ID:          "u-1",
		Account:     "alice",
		SiteID:      "site-a",
		EngName:     "Alice Wang",
		ChineseName: "愛麗絲",
	}
	expectedSender := cassParticipant{
		ID:          user.ID,
		EngName:     user.EngName,
		CompanyName: user.ChineseName,
		Account:     "alice",
	}

	snapshot := &cassandra.QuotedParentMessage{
		MessageID:   "parent-msg-uuid",
		RoomID:      "r1",
		Sender:      cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:   time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC),
		Msg:         "the original message",
		MessageLink: "http://localhost:3000/r1/parent-msg-uuid",
	}

	quotedMsg := model.Message{
		ID:                  "msg-quote-1",
		RoomID:              "r1",
		UserID:              "u-1",
		UserAccount:         "alice",
		Content:             "great point!",
		CreatedAt:           now,
		QuotedParentMessage: snapshot,
	}
	quotedEvt := model.MessageEvent{Message: quotedMsg, SiteID: "site-a", Timestamp: now.UnixMilli()}
	quotedData, err := json.Marshal(quotedEvt)
	require.NoError(t, err)

	t.Run("quote snapshot reaches SaveMessage unchanged", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		store := NewMockStore(ctrl)
		userStore := NewMockUserStore(ctrl)
		threadStore := NewMockThreadStore(ctrl)

		userStore.EXPECT().FindUserByID(gomock.Any(), "u-1").Return(user, nil)
		store.EXPECT().
			SaveMessage(gomock.Any(), &quotedMsg, &expectedSender, "site-a").
			DoAndReturn(func(_ context.Context, m *model.Message, _ *cassParticipant, _ string) error {
				require.NotNil(t, m.QuotedParentMessage, "QuotedParentMessage must be forwarded")
				assert.Equal(t, "parent-msg-uuid", m.QuotedParentMessage.MessageID)
				assert.Equal(t, "the original message", m.QuotedParentMessage.Msg)
				return nil
			})

		h := NewHandler(store, userStore, threadStore)
		err := h.processMessage(context.Background(), quotedData)
		require.NoError(t, err)
	})
}
