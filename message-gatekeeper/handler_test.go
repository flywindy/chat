package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/subject"
)

func makePublishFunc(published *[]publishedMsg, returnErr error) publishFunc {
	return func(_ context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		if published != nil {
			*published = append(*published, publishedMsg{subject: msg.Subject, data: msg.Data})
		}
		if returnErr != nil {
			return nil, returnErr
		}
		return &jetstream.PubAck{}, nil
	}
}

type publishedMsg struct {
	subject string
	data    []byte
}

func TestHandler_ProcessMessage(t *testing.T) {
	validID := idgen.GenerateMessageID()
	validContent := "hello world"
	validSiteID := "site-a"
	validRoomID := "room-1"
	validAccount := "alice"

	sub := &model.Subscription{
		User:   model.SubscriptionUser{ID: "u1", Account: validAccount},
		RoomID: validRoomID,
		Roles:  []model.Role{model.RoleMember},
	}

	tests := []struct {
		name        string
		account     string
		roomID      string
		siteID      string
		buildData   func() []byte
		setupStore  func(s *MockStore)
		setupPub    func() (publishFunc, *[]publishedMsg)
		wantErr     bool
		wantInfra   bool
		checkResult func(t *testing.T, data []byte, published []publishedMsg)
	}{
		{
			name:    "happy path",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().
					GetSubscription(gomock.Any(), validAccount, validRoomID).
					Return(sub, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			wantErr: false,
			checkResult: func(t *testing.T, data []byte, published []publishedMsg) {
				require.NotNil(t, data)
				var msg model.Message
				err := json.Unmarshal(data, &msg)
				require.NoError(t, err)
				assert.Equal(t, validContent, msg.Content)
				assert.Equal(t, "u1", msg.UserID)
				assert.Equal(t, validRoomID, msg.RoomID)
				assert.Equal(t, validAccount, msg.UserAccount)
				assert.NotEmpty(t, msg.ID)
				assert.Len(t, published, 1)
				assert.Equal(t, subject.MsgCanonicalCreated(validSiteID), published[0].subject)
				// Verify MessageEvent has Timestamp set
				var evt model.MessageEvent
				err = json.Unmarshal(published[0].data, &evt)
				require.NoError(t, err)
				assert.Greater(t, evt.Timestamp, int64(0))
			},
		},
		{
			name:    "happy path with thread parent",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				parentID := idgen.GenerateMessageID()
				parentMillis := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d}`,
					validID, validContent, parentID, parentMillis,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().
					GetSubscription(gomock.Any(), validAccount, validRoomID).
					Return(sub, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			wantErr: false,
			checkResult: func(t *testing.T, data []byte, published []publishedMsg) {
				parentTS := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
				require.NotNil(t, data)
				var msg model.Message
				require.NoError(t, json.Unmarshal(data, &msg))
				require.NotEmpty(t, msg.ThreadParentMessageID)
				assert.Len(t, msg.ThreadParentMessageID, 20)
				require.NotNil(t, msg.ThreadParentMessageCreatedAt)
				assert.Equal(t, parentTS, msg.ThreadParentMessageCreatedAt.UTC())

				require.Len(t, published, 1)
				var evt model.MessageEvent
				require.NoError(t, json.Unmarshal(published[0].data, &evt))
				assert.NotEmpty(t, evt.Message.ThreadParentMessageID)
				assert.Len(t, evt.Message.ThreadParentMessageID, 20)
				require.NotNil(t, evt.Message.ThreadParentMessageCreatedAt)
				assert.Equal(t, parentTS, evt.Message.ThreadParentMessageCreatedAt.UTC())
				assert.Greater(t, evt.Timestamp, int64(0))
			},
		},
		{
			name:    "thread parent ID without timestamp",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					ThreadParentMessageID: "parent-msg-uuid",
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "invalid UUID",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: "not-a-uuid", Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "empty content",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: ""}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "content exceeds 20KB",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: strings.Repeat("x", 20*1024+1)}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "user not in room",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().
					GetSubscription(gomock.Any(), validAccount, validRoomID).
					Return(nil, fmt.Errorf("user alice not subscribed to room room-1: %w", errNotSubscribed))
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "store infra error",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().
					GetSubscription(gomock.Any(), validAccount, validRoomID).
					Return(nil, fmt.Errorf("connection refused"))
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: true,
		},
		{
			name:    "publish fails",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().
					GetSubscription(gomock.Any(), validAccount, validRoomID).
					Return(sub, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, fmt.Errorf("nats publish error")), nil
			},
			wantErr:   true,
			wantInfra: true,
		},
		{
			name:    "malformed JSON",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  validSiteID,
			buildData: func() []byte {
				return []byte("{not valid json}")
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
		{
			name:    "siteID mismatch",
			account: validAccount,
			roomID:  validRoomID,
			siteID:  "site-b",
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr:   true,
			wantInfra: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockStore(ctrl)
			tc.setupStore(store)

			pub, publishedPtr := tc.setupPub()

			h := &Handler{
				store:   store,
				publish: pub,
				siteID:  validSiteID,
			}

			data, err := h.processMessage(context.Background(), tc.account, tc.roomID, tc.siteID, tc.buildData())

			if tc.wantErr {
				require.Error(t, err)
				if tc.wantInfra {
					var ie *infraError
					assert.True(t, errors.As(err, &ie), "expected infraError, got %T: %v", err, err)
				} else {
					var ie *infraError
					assert.False(t, errors.As(err, &ie), "expected non-infra error, got infraError: %v", err)
				}
			} else {
				require.NoError(t, err)
				if tc.checkResult != nil {
					var published []publishedMsg
					if publishedPtr != nil {
						published = *publishedPtr
					}
					tc.checkResult(t, data, published)
				}
			}
		})
	}
}

func TestHandler_processMessage_RejectsInvalidThreadParentMessageID(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	// No store expectations: validation must fail before any store call.

	pub := func(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		return &jetstream.PubAck{}, nil
	}
	reply := func(ctx context.Context, msg *nats.Msg) error { return nil }
	h := NewHandler(store, pub, reply, "site1", nil)

	parentTs := int64(1000)
	req := model.SendMessageRequest{
		ID:                           idgen.GenerateMessageID(),
		Content:                      "reply",
		ThreadParentMessageID:        "not-a-valid-msg-id",
		ThreadParentMessageCreatedAt: &parentTs,
	}
	data, _ := json.Marshal(req)
	_, err := h.processMessage(context.Background(), "alice", "room-1", "site1", data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid thread parent message ID")
}

func TestHandler_processMessage_PropagatesRequestIDOnCanonicalPublish(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().GetSubscription(gomock.Any(), "alice", "room-1").
		Return(&model.Subscription{User: model.SubscriptionUser{ID: "u-alice", Account: "alice"}}, nil)

	var capturedHeader nats.Header
	pub := func(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
		capturedHeader = msg.Header
		return &jetstream.PubAck{}, nil
	}
	reply := func(ctx context.Context, msg *nats.Msg) error { return nil }

	h := NewHandler(store, pub, reply, "site1", nil)

	ctx := natsutil.WithRequestID(context.Background(), "req-mg-test-id")
	req := model.SendMessageRequest{ID: idgen.GenerateMessageID(), Content: "hello"}
	data, _ := json.Marshal(req)

	_, err := h.processMessage(ctx, "alice", "room-1", "site1", data)
	require.NoError(t, err)
	require.NotNil(t, capturedHeader, "publish must propagate header from ctx")
	assert.Equal(t, "req-mg-test-id", capturedHeader.Get(natsutil.RequestIDHeader))
}

func TestHandler_ProcessMessage_WithQuote(t *testing.T) {
	validID := idgen.GenerateMessageID()
	validContent := "great point!"
	validSiteID := "site-a"
	validRoomID := "room-1"
	validAccount := "alice"
	parentMessageID := idgen.GenerateMessageID()

	sub := &model.Subscription{
		User:   model.SubscriptionUser{ID: "u1", Account: validAccount},
		RoomID: validRoomID,
		Roles:  []model.Role{model.RoleMember},
	}

	threadID := idgen.GenerateMessageID()
	threadParentTS := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	// mainRoomSnapshot represents a parent that lives in the main room
	// (not inside any thread): ThreadParentID == "".
	mainRoomSnapshot := &cassandra.QuotedParentMessage{
		MessageID:   parentMessageID,
		RoomID:      validRoomID,
		Sender:      cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:   time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:         "the original message",
		MessageLink: "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
	}

	// inThreadSnapshot represents a parent that is itself a reply inside thread T.
	inThreadSnapshot := &cassandra.QuotedParentMessage{
		MessageID:             parentMessageID,
		RoomID:                validRoomID,
		Sender:                cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:             time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:                   "a reply inside thread T",
		MessageLink:           "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
		ThreadParentID:        threadID,
		ThreadParentCreatedAt: &threadParentTS,
	}

	// inDifferentThreadSnapshot is in thread T'.
	inDifferentThreadSnapshot := &cassandra.QuotedParentMessage{
		MessageID:             parentMessageID,
		RoomID:                validRoomID,
		Sender:                cassandra.Participant{ID: "u-bob", Account: "bob", EngName: "Bob Chen"},
		CreatedAt:             time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Msg:                   "a reply inside thread T'",
		MessageLink:           "http://localhost:3000/" + validRoomID + "/" + parentMessageID,
		ThreadParentID:        "different-thread-uuid",
		ThreadParentCreatedAt: &threadParentTS,
	}

	tests := []struct {
		name          string
		buildData     func() []byte
		setupStore    func(s *MockStore)
		setupFetcher  func(f *MockParentMessageFetcher)
		setupPub      func() (publishFunc, *[]publishedMsg)
		wantErr       bool
		assertMessage func(t *testing.T, msg model.Message)
	}{
		{
			name: "main-room msg quoting main-room parent — snapshot embedded",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(mainRoomSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				require.NotNil(t, msg.QuotedParentMessage)
				assert.Equal(t, parentMessageID, msg.QuotedParentMessage.MessageID)
				assert.Equal(t, "the original message", msg.QuotedParentMessage.Msg)
				assert.Equal(t, "bob", msg.QuotedParentMessage.Sender.Account)
				assert.Equal(t, mainRoomSnapshot.MessageLink, msg.QuotedParentMessage.MessageLink)
				assert.Empty(t, msg.QuotedParentMessage.ThreadParentID)
			},
		},
		{
			name: "fetcher error — request fails",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(nil, fmt.Errorf("history response error: not found"))
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr: true,
		},
		{
			name: "thread T msg quoting another reply in thread T — snapshot embedded",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Equal(t, threadID, msg.ThreadParentMessageID)
				require.NotNil(t, msg.QuotedParentMessage)
				assert.Equal(t, parentMessageID, msg.QuotedParentMessage.MessageID)
				assert.Equal(t, threadID, msg.QuotedParentMessage.ThreadParentID)
			},
		},
		{
			name: "main-room msg quoting a thread reply — request fails (cross-thread)",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr: true,
		},
		{
			name: "thread T msg quoting main-room parent — request fails (strict)",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(mainRoomSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr: true,
		},
		{
			name: "thread T msg quoting reply in different thread T' — request fails",
			buildData: func() []byte {
				return []byte(fmt.Sprintf(
					`{"id":%q,"content":%q,"requestId":"req-1","threadParentMessageId":%q,"threadParentMessageCreatedAt":%d,"quotedParentMessageId":%q}`,
					validID, validContent, threadID, threadParentTS.UnixMilli(), parentMessageID,
				))
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(inDifferentThreadSnapshot, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr: true,
		},
		{
			name: "no quote field — fetcher not called",
			buildData: func() []byte {
				req := model.SendMessageRequest{ID: validID, Content: validContent}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				// no EXPECT — gomock will fail the test if FetchQuotedParent is called
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				var published []publishedMsg
				return makePublishFunc(&published, nil), &published
			},
			assertMessage: func(t *testing.T, msg model.Message) {
				assert.Nil(t, msg.QuotedParentMessage)
			},
		},
		{
			name: "fetcher returns (nil, nil) — request fails (defensive guard)",
			buildData: func() []byte {
				req := model.SendMessageRequest{
					ID:                    validID,
					Content:               validContent,
					QuotedParentMessageID: parentMessageID,
				}
				data, _ := json.Marshal(req)
				return data
			},
			setupStore: func(s *MockStore) {
				s.EXPECT().GetSubscription(gomock.Any(), validAccount, validRoomID).Return(sub, nil)
			},
			setupFetcher: func(f *MockParentMessageFetcher) {
				f.EXPECT().
					FetchQuotedParent(gomock.Any(), validAccount, validRoomID, validSiteID, parentMessageID).
					Return(nil, nil)
			},
			setupPub: func() (publishFunc, *[]publishedMsg) {
				return makePublishFunc(nil, nil), nil
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockStore(ctrl)
			fetcher := NewMockParentMessageFetcher(ctrl)
			tc.setupStore(store)
			tc.setupFetcher(fetcher)

			pub, publishedPtr := tc.setupPub()

			h := &Handler{
				store:         store,
				publish:       pub,
				siteID:        validSiteID,
				parentFetcher: fetcher,
			}

			data, err := h.processMessage(context.Background(), validAccount, validRoomID, validSiteID, tc.buildData())

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, data)

			var msg model.Message
			require.NoError(t, json.Unmarshal(data, &msg))
			tc.assertMessage(t, msg)

			// Also verify the snapshot reaches the canonical event.
			require.NotNil(t, publishedPtr)
			require.Len(t, *publishedPtr, 1)
			var evt model.MessageEvent
			require.NoError(t, json.Unmarshal((*publishedPtr)[0].data, &evt))
			if msg.QuotedParentMessage != nil {
				require.NotNil(t, evt.Message.QuotedParentMessage)
				assert.Equal(t, msg.QuotedParentMessage.MessageID, evt.Message.QuotedParentMessage.MessageID)
			} else {
				assert.Nil(t, evt.Message.QuotedParentMessage)
			}
		})
	}
}
