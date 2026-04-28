# Message Union Types Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `type Message = cassandra.Message` alias in `history-service/internal/models` with a closed union interface `Message = BaseMessage | ThreadParentMessage`, so history endpoints can return either type in the same slice while keeping JSON serialization flat.

**Architecture:** `cassandra.Message` remains the DB scan target (unchanged). `BaseMessage` embeds it for plain messages. `ThreadParentMessage` (already exists) also embeds it and gains the `isMessage()` marker. The `Message` interface is the response union — only these two types implement it. No `kind` field is added to JSON; the split is internal only, FE duck-types on presence of `threadCount`.

**Tech Stack:** Go 1.25, `encoding/json`, `go.uber.org/mock`, `stretchr/testify`

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Modify | `history-service/internal/cassrepo/repository.go` | Use `cassandra.Message` directly (drop models alias dep) |
| Modify | `history-service/internal/models/message.go` | Remove alias; add `Message` interface + `BaseMessage` |
| Modify | `history-service/internal/models/threads.go` | Add `isMessage()` to `ThreadParentMessage` |
| Create | `history-service/internal/models/message_test.go` | Interface compliance + JSON marshal tests |
| Modify | `history-service/internal/service/service.go` | `MessageRepository` uses `cassandra.Message` |
| Modify | `history-service/internal/service/utils.go` | `findMessage` → `*cassandra.Message`; add `wrapMessages` |
| Modify | `history-service/internal/service/messages.go` | Handlers wrap `cassandra.Message` in `BaseMessage` |
| Modify | `history-service/internal/service/threads.go` | `msgByID` map type → `cassandra.Message` |
| Regenerate | `history-service/internal/service/mocks/mock_repository.go` | `make generate SERVICE=history-service` |
| Modify | `history-service/internal/service/messages_test.go` | Update helpers + assertions for interface type |
| Modify | `history-service/internal/service/threads_test.go` | Update `makeCassMessages` return type |

---

## Chapter 1 — Decouple `cassrepo` from the `models.Message` alias

**Goal:** Make `cassrepo/repository.go` import `cassandra.Message` directly instead of going through the `models` alias. Purely nominal change — no behaviour differences. Prerequisite for Chapter 2, which repurposes the `Message` name as an interface.

**Files:**
- Modify: `history-service/internal/cassrepo/repository.go`

- [ ] **Step 1 — Baseline check**

```bash
make test SERVICE=history-service
```
Expected: all tests pass.

- [ ] **Step 2 — Rewrite `cassrepo/repository.go`**

Replace the `models` import with `cassandra` and rename every `models.Message` → `cassandra.Message`. Key changes (full diff shown for critical parts):

```go
// OLD import
import "github.com/hmchangw/chat/history-service/internal/models"
// NEW import
import "github.com/hmchangw/chat/pkg/model/cassandra"

// Every occurrence of models.Message → cassandra.Message, e.g.:
func scanMessages(iter *gocql.Iter) []cassandra.Message {
    messages := make([]cassandra.Message, 0)
    for {
        var m cassandra.Message
        if !iter.Scan(baseScanDest(&m)...) { break }
        messages = append(messages, m)
    }
    return messages
}

func (r *Repository) GetMessagesBefore(ctx context.Context, roomID string, before time.Time, q PageRequest) (Page[cassandra.Message], error) {
    var messages []cassandra.Message
    nextCursor, err := NewQueryBuilder(...).Fetch(func(iter *gocql.Iter) {
        messages = scanMessages(iter)
    })
    if err != nil { return Page[cassandra.Message]{}, fmt.Errorf("querying messages before: %w", err) }
    return Page[cassandra.Message]{Data: messages, NextCursor: nextCursor, HasNext: nextCursor != ""}, nil
}
// Apply same pattern to: GetMessagesBetweenDesc, GetMessagesAfter, GetAllMessagesAsc,
// GetMessageByID, GetThreadMessages, GetMessagesByIDs
// and helper functions: baseScanDest, messageByIDScanDest, threadMessageScanDest, scanThreadMessages
```

- [ ] **Step 3 — Build and test**

```bash
make build SERVICE=history-service
make test SERVICE=history-service
```
Expected: build succeeds, all tests pass (alias and direct type are identical at runtime).

- [ ] **Step 4 — Commit**

```bash
git add history-service/internal/cassrepo/repository.go
git commit -m "refactor(cassrepo): use cassandra.Message directly, drop models alias dependency"
```

---

## Chapter 2 — Introduce the Message union type

**Goal:** Remove `type Message = cassandra.Message` alias from `models/message.go`, introduce `Message` interface + `BaseMessage` struct, add `isMessage()` to `ThreadParentMessage`, update the `MessageRepository` interface contract, update all service handlers to wrap DB results in `BaseMessage`, regenerate mocks, and update all unit tests. This chapter is atomic because Go requires all compilation units to be consistent.

**Files:**
- Modify: `history-service/internal/models/message.go`
- Modify: `history-service/internal/models/threads.go`
- Create: `history-service/internal/models/message_test.go`
- Modify: `history-service/internal/service/service.go`
- Modify: `history-service/internal/service/utils.go`
- Modify: `history-service/internal/service/messages.go`
- Modify: `history-service/internal/service/threads.go`
- Regenerate: `history-service/internal/service/mocks/mock_repository.go`
- Modify: `history-service/internal/service/messages_test.go`
- Modify: `history-service/internal/service/threads_test.go`

### 2a — Write model-layer tests (RED)

- [ ] **Step 1 — Create `history-service/internal/models/message_test.go`**

```go
package models_test

import (
    "encoding/json"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/hmchangw/chat/history-service/internal/models"
    "github.com/hmchangw/chat/pkg/model/cassandra"
)

// Compile-time: both concrete types satisfy the interface.
var _ models.Message = models.BaseMessage{}
var _ models.Message = models.ThreadParentMessage{}

func TestBaseMessage_JSONFlat(t *testing.T) {
    bm := models.BaseMessage{Message: cassandra.Message{
        MessageID: "m1",
        Msg:       "hello",
        CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
    }}
    b, err := json.Marshal(bm)
    require.NoError(t, err)
    var got map[string]any
    require.NoError(t, json.Unmarshal(b, &got))
    assert.Equal(t, "m1", got["messageId"])
    assert.Equal(t, "hello", got["msg"])
    _, hasCount := got["threadCount"]
    assert.False(t, hasCount, "BaseMessage must not have threadCount field")
}

func TestThreadParentMessage_JSONFlat(t *testing.T) {
    tpm := models.ThreadParentMessage{
        Message:           cassandra.Message{MessageID: "p1", Msg: "parent"},
        ThreadCount:       5,
        ThreadLastMessage: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
        ReplyAccounts:     []string{"alice"},
    }
    b, err := json.Marshal(tpm)
    require.NoError(t, err)
    var got map[string]any
    require.NoError(t, json.Unmarshal(b, &got))
    assert.Equal(t, "p1", got["messageId"])
    assert.EqualValues(t, 5, got["threadCount"])
    assert.NotEmpty(t, got["threadLastMessage"])
}

func TestMessageSlice_JSONMarshal(t *testing.T) {
    msgs := []models.Message{
        models.BaseMessage{Message: cassandra.Message{MessageID: "m1"}},
        models.ThreadParentMessage{Message: cassandra.Message{MessageID: "p1"}, ThreadCount: 3},
    }
    b, err := json.Marshal(msgs)
    require.NoError(t, err)
    var got []map[string]any
    require.NoError(t, json.Unmarshal(b, &got))
    require.Len(t, got, 2)
    assert.Equal(t, "m1", got[0]["messageId"])
    assert.Equal(t, "p1", got[1]["messageId"])
    assert.EqualValues(t, 3, got[1]["threadCount"])
    _, hasCount := got[0]["threadCount"]
    assert.False(t, hasCount, "BaseMessage element must not have threadCount")
}
```

- [ ] **Step 2 — Verify RED**

```bash
make test SERVICE=history-service
```
Expected: FAIL — `models.BaseMessage` and `models.Message` (interface) undefined.

### 2b — Update model types

- [ ] **Step 3 — Rewrite `history-service/internal/models/message.go`**

```go
package models

import "github.com/hmchangw/chat/pkg/model/cassandra"

// Message is the closed response union for history endpoints.
// Only BaseMessage and ThreadParentMessage implement this interface.
type Message interface{ isMessage() }

// BaseMessage is a plain Cassandra message with no thread enrichment.
type BaseMessage struct {
    cassandra.Message
}

func (BaseMessage) isMessage() {}

// UDT aliases kept for consumers that reference models.Participant etc.
type Participant        = cassandra.Participant
type File              = cassandra.File
type Card              = cassandra.Card
type CardAction        = cassandra.CardAction
type QuotedParentMessage = cassandra.QuotedParentMessage

// --- Request / response DTOs ---

type LoadHistoryRequest struct {
    Before *int64 `json:"before,omitempty"`
    Limit  int    `json:"limit"`
}

type LoadHistoryResponse struct {
    Messages []Message `json:"messages"`
}

type LoadNextMessagesRequest struct {
    After  *int64 `json:"after,omitempty"`
    Limit  int    `json:"limit"`
    Cursor string `json:"cursor"`
}

type LoadNextMessagesResponse struct {
    Messages   []Message `json:"messages"`
    NextCursor string    `json:"nextCursor,omitempty"`
    HasNext    bool      `json:"hasNext"`
}

type LoadSurroundingMessagesRequest struct {
    MessageID string `json:"messageId"`
    Limit     int    `json:"limit"`
}

type LoadSurroundingMessagesResponse struct {
    Messages   []Message `json:"messages"`
    MoreBefore bool      `json:"moreBefore"`
    MoreAfter  bool      `json:"moreAfter"`
}

type GetMessageByIDRequest struct {
    MessageID string `json:"messageId"`
}

type GetThreadMessagesRequest struct {
    ThreadMessageID string `json:"threadMessageId"`
    Cursor          string `json:"cursor,omitempty"`
    Limit           int    `json:"limit"`
}

type GetThreadMessagesResponse struct {
    Messages   []Message `json:"messages"`
    NextCursor string    `json:"nextCursor,omitempty"`
    HasNext    bool      `json:"hasNext"`
}
```

- [ ] **Step 4 — Add `isMessage()` to `ThreadParentMessage` in `history-service/internal/models/threads.go`**

Add one line after the struct definition:

```go
// ThreadParentMessage implements the Message response union.
func (ThreadParentMessage) isMessage() {}
```

### 2c — Update service layer

- [ ] **Step 5 — Rewrite `history-service/internal/service/service.go`**

Change `MessageRepository` to use `cassandra.Message`; drop the `models` import from this file:

```go
package service

import (
    "context"
    "time"

    "github.com/hmchangw/chat/history-service/internal/cassrepo"
    "github.com/hmchangw/chat/history-service/internal/mongorepo"
    pkgmodel "github.com/hmchangw/chat/pkg/model"
    "github.com/hmchangw/chat/pkg/model/cassandra"
    "github.com/hmchangw/chat/pkg/natsrouter"
    "github.com/hmchangw/chat/pkg/subject"
)

//go:generate mockgen -destination=mocks/mock_repository.go -package=mocks . MessageRepository,SubscriptionRepository,ThreadRoomRepository

type MessageRepository interface {
    GetMessagesBefore(ctx context.Context, roomID string, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[cassandra.Message], error)
    GetMessagesBetweenDesc(ctx context.Context, roomID string, since, before time.Time, q cassrepo.PageRequest) (cassrepo.Page[cassandra.Message], error)
    GetMessagesAfter(ctx context.Context, roomID string, after time.Time, q cassrepo.PageRequest) (cassrepo.Page[cassandra.Message], error)
    GetAllMessagesAsc(ctx context.Context, roomID string, q cassrepo.PageRequest) (cassrepo.Page[cassandra.Message], error)
    GetMessageByID(ctx context.Context, messageID string) (*cassandra.Message, error)
    GetThreadMessages(ctx context.Context, roomID, threadRoomID string, q cassrepo.PageRequest) (cassrepo.Page[cassandra.Message], error)
    GetMessagesByIDs(ctx context.Context, messageIDs []string) ([]cassandra.Message, error)
}

type SubscriptionRepository interface {
    GetHistorySharedSince(ctx context.Context, account, roomID string) (*time.Time, bool, error)
    GetSubscriptionForThreads(ctx context.Context, account, roomID string) (*time.Time, []string, bool, error)
}

type ThreadRoomRepository interface {
    GetThreadRooms(ctx context.Context, roomID string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
    GetFollowingThreadRooms(ctx context.Context, roomID, account string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
    GetUnreadThreadRooms(ctx context.Context, roomID string, parentIDs []string, accessSince *time.Time, req mongorepo.OffsetPageRequest) (mongorepo.OffsetPage[pkgmodel.ThreadRoom], error)
}

type HistoryService struct {
    messages      MessageRepository
    subscriptions SubscriptionRepository
    threadRooms   ThreadRoomRepository
}

func New(msgs MessageRepository, subs SubscriptionRepository, threadRooms ThreadRoomRepository) *HistoryService {
    return &HistoryService{messages: msgs, subscriptions: subs, threadRooms: threadRooms}
}

func (s *HistoryService) RegisterHandlers(r *natsrouter.Router, siteID string) {
    natsrouter.Register(r, subject.MsgHistoryPattern(siteID), s.LoadHistory)
    natsrouter.Register(r, subject.MsgNextPattern(siteID), s.LoadNextMessages)
    natsrouter.Register(r, subject.MsgSurroundingPattern(siteID), s.LoadSurroundingMessages)
    natsrouter.Register(r, subject.MsgGetPattern(siteID), s.GetMessageByID)
    natsrouter.Register(r, subject.MsgThreadPattern(siteID), s.GetThreadMessages)
    natsrouter.Register(r, subject.MsgThreadParentPattern(siteID), s.GetThreadsList)
}
```

- [ ] **Step 6 — Update `history-service/internal/service/utils.go`**

Change `findMessage` return type and add `wrapMessages` helper:

```go
package service

import (
    "context"
    "log/slog"
    "time"

    "github.com/hmchangw/chat/history-service/internal/cassrepo"
    "github.com/hmchangw/chat/history-service/internal/models"
    "github.com/hmchangw/chat/pkg/model/cassandra"
    "github.com/hmchangw/chat/pkg/natsrouter"
)

func (s *HistoryService) getAccessSince(ctx context.Context, account, roomID string) (*time.Time, error) {
    accessSince, subscribed, err := s.subscriptions.GetHistorySharedSince(ctx, account, roomID)
    if err != nil {
        slog.Error("checking subscription", "error", err, "account", account, "roomID", roomID)
        return nil, natsrouter.ErrInternal("unable to verify room access")
    }
    if !subscribed {
        return nil, natsrouter.ErrForbidden("not subscribed to room")
    }
    return accessSince, nil
}

func millisToTime(millis *int64) time.Time {
    if millis == nil { return time.Time{} }
    return time.UnixMilli(*millis).UTC()
}

func parsePageRequest(cursor string, limit int) (cassrepo.PageRequest, error) {
    q, err := cassrepo.ParsePageRequest(cursor, limit)
    if err != nil {
        slog.Error("invalid pagination cursor", "error", err, "cursor", cursor)
        return cassrepo.PageRequest{}, natsrouter.ErrBadRequest("invalid pagination cursor")
    }
    return q, nil
}

func (s *HistoryService) findMessage(ctx context.Context, roomID, messageID string) (*cassandra.Message, error) {
    if messageID == "" {
        return nil, natsrouter.ErrBadRequest("messageId is required")
    }
    msg, err := s.messages.GetMessageByID(ctx, messageID)
    if err != nil {
        slog.Error("finding message", "error", err, "messageID", messageID)
        return nil, natsrouter.ErrInternal("failed to retrieve message")
    }
    if msg == nil {
        return nil, natsrouter.ErrNotFound("message not found")
    }
    if msg.RoomID != roomID {
        return nil, natsrouter.ErrNotFound("message not found")
    }
    return msg, nil
}

func derefTime(t *time.Time) time.Time {
    if t == nil { return time.Time{} }
    return *t
}

func timeMax(a, b time.Time) time.Time {
    if a.IsZero() { return b }
    if b.IsZero() { return a }
    if a.After(b) { return a }
    return b
}

// wrapMessages converts Cassandra messages to the response union type.
func wrapMessages(src []cassandra.Message) []models.Message {
    if len(src) == 0 {
        return []models.Message{}
    }
    out := make([]models.Message, len(src))
    for i, m := range src {
        out[i] = models.BaseMessage{Message: m}
    }
    return out
}
```

- [ ] **Step 7 — Update `history-service/internal/service/messages.go`**

Replace `cassrepo.Page[models.Message]` with `cassrepo.Page[cassandra.Message]`; wrap results in `BaseMessage`; change `GetMessageByID` return type to `*models.BaseMessage`:

```go
package service

import (
    "log/slog"
    "time"

    "github.com/hmchangw/chat/history-service/internal/cassrepo"
    "github.com/hmchangw/chat/history-service/internal/models"
    "github.com/hmchangw/chat/pkg/model/cassandra"
    "github.com/hmchangw/chat/pkg/natsrouter"
)

const (
    defaultPageSize     = 20
    surroundingPageSize = 50
    maxPageSize         = 100
)

func (s *HistoryService) LoadHistory(c *natsrouter.Context, req models.LoadHistoryRequest) (*models.LoadHistoryResponse, error) {
    account := c.Param("account")
    roomID := c.Param("roomID")
    accessSince, err := s.getAccessSince(c, account, roomID)
    if err != nil { return nil, err }

    before := millisToTime(req.Before)
    if before.IsZero() { before = time.Now().UTC() }

    limit := req.Limit
    if limit <= 0 { limit = defaultPageSize }
    if limit > maxPageSize { limit = maxPageSize }
    pageReq, err := parsePageRequest("", limit)
    if err != nil { return nil, err }

    var page cassrepo.Page[cassandra.Message]
    if accessSince == nil {
        page, err = s.messages.GetMessagesBefore(c, roomID, before, pageReq)
    } else {
        page, err = s.messages.GetMessagesBetweenDesc(c, roomID, *accessSince, before, pageReq)
    }
    if err != nil {
        slog.Error("loading history", "error", err, "roomID", roomID)
        return nil, natsrouter.ErrInternal("failed to load message history")
    }
    return &models.LoadHistoryResponse{Messages: wrapMessages(page.Data)}, nil
}

func (s *HistoryService) LoadNextMessages(c *natsrouter.Context, req models.LoadNextMessagesRequest) (*models.LoadNextMessagesResponse, error) {
    account := c.Param("account")
    roomID := c.Param("roomID")
    accessSince, err := s.getAccessSince(c, account, roomID)
    if err != nil { return nil, err }

    after := millisToTime(req.After)
    lowerBound := timeMax(after, derefTime(accessSince))

    limit := req.Limit
    if limit <= 0 { limit = defaultPageSize }
    if limit > maxPageSize { limit = maxPageSize }
    pageReq, err := parsePageRequest(req.Cursor, limit)
    if err != nil { return nil, err }

    var page cassrepo.Page[cassandra.Message]
    if lowerBound.IsZero() {
        page, err = s.messages.GetAllMessagesAsc(c, roomID, pageReq)
    } else {
        page, err = s.messages.GetMessagesAfter(c, roomID, lowerBound, pageReq)
    }
    if err != nil {
        slog.Error("loading next messages", "error", err, "roomID", roomID)
        return nil, natsrouter.ErrInternal("failed to load messages")
    }
    return &models.LoadNextMessagesResponse{
        Messages:   wrapMessages(page.Data),
        NextCursor: page.NextCursor,
        HasNext:    page.HasNext,
    }, nil
}

func (s *HistoryService) LoadSurroundingMessages(c *natsrouter.Context, req models.LoadSurroundingMessagesRequest) (*models.LoadSurroundingMessagesResponse, error) {
    account := c.Param("account")
    roomID := c.Param("roomID")
    accessSince, err := s.getAccessSince(c, account, roomID)
    if err != nil { return nil, err }

    centralMsg, err := s.findMessage(c, roomID, req.MessageID)
    if err != nil { return nil, err }
    if accessSince != nil && centralMsg.CreatedAt.Before(*accessSince) {
        return nil, natsrouter.ErrForbidden("message is outside access window")
    }

    limit := req.Limit
    if limit <= 0 { limit = surroundingPageSize }
    if limit > maxPageSize { limit = maxPageSize }

    remaining := limit - 1
    if remaining <= 0 {
        return &models.LoadSurroundingMessagesResponse{
            Messages: []models.Message{models.BaseMessage{Message: *centralMsg}},
        }, nil
    }
    beforeCount := (remaining + 1) / 2
    afterCount := remaining / 2

    beforePageReq, err := parsePageRequest("", beforeCount)
    if err != nil { return nil, err }
    afterPageReq, err := parsePageRequest("", afterCount)
    if err != nil { return nil, err }

    var beforePage cassrepo.Page[cassandra.Message]
    if accessSince == nil {
        beforePage, err = s.messages.GetMessagesBefore(c, roomID, centralMsg.CreatedAt, beforePageReq)
    } else {
        beforePage, err = s.messages.GetMessagesBetweenDesc(c, roomID, *accessSince, centralMsg.CreatedAt, beforePageReq)
    }
    if err != nil {
        slog.Error("loading surrounding messages", "error", err, "roomID", roomID, "direction", "before")
        return nil, natsrouter.ErrInternal("failed to load surrounding messages")
    }

    afterPage, err := s.messages.GetMessagesAfter(c, roomID, centralMsg.CreatedAt, afterPageReq)
    if err != nil {
        slog.Error("loading surrounding messages", "error", err, "roomID", roomID, "direction", "after")
        return nil, natsrouter.ErrInternal("failed to load surrounding messages")
    }

    messages := make([]models.Message, 0, len(beforePage.Data)+1+len(afterPage.Data))
    for i := len(beforePage.Data) - 1; i >= 0; i-- {
        messages = append(messages, models.BaseMessage{Message: beforePage.Data[i]})
    }
    messages = append(messages, models.BaseMessage{Message: *centralMsg})
    for _, m := range afterPage.Data {
        messages = append(messages, models.BaseMessage{Message: m})
    }

    return &models.LoadSurroundingMessagesResponse{
        Messages:   messages,
        MoreBefore: beforePage.HasNext,
        MoreAfter:  afterPage.HasNext,
    }, nil
}

func (s *HistoryService) GetMessageByID(c *natsrouter.Context, req models.GetMessageByIDRequest) (*models.BaseMessage, error) {
    account := c.Param("account")
    roomID := c.Param("roomID")
    accessSince, err := s.getAccessSince(c, account, roomID)
    if err != nil { return nil, err }

    msg, err := s.findMessage(c, roomID, req.MessageID)
    if err != nil { return nil, err }
    if accessSince != nil && msg.CreatedAt.Before(*accessSince) {
        return nil, natsrouter.ErrForbidden("message is outside access window")
    }
    return &models.BaseMessage{Message: *msg}, nil
}

func (s *HistoryService) GetThreadMessages(c *natsrouter.Context, req models.GetThreadMessagesRequest) (*models.GetThreadMessagesResponse, error) {
    account := c.Param("account")
    if req.ThreadMessageID == "" {
        return nil, natsrouter.ErrBadRequest("threadMessageId is required")
    }

    parent, err := s.messages.GetMessageByID(c, req.ThreadMessageID)
    if err != nil {
        slog.Error("finding thread parent", "error", err, "messageID", req.ThreadMessageID)
        return nil, natsrouter.ErrInternal("failed to retrieve message")
    }
    if parent == nil {
        return nil, natsrouter.ErrNotFound("message not found")
    }

    roomID := parent.RoomID
    accessSince, err := s.getAccessSince(c, account, roomID)
    if err != nil { return nil, err }
    if accessSince != nil && parent.CreatedAt.Before(*accessSince) {
        return nil, natsrouter.ErrForbidden("thread is outside access window")
    }

    limit := req.Limit
    if limit <= 0 { limit = defaultPageSize }
    if limit > maxPageSize { limit = maxPageSize }
    pageReq, err := parsePageRequest(req.Cursor, limit)
    if err != nil { return nil, err }

    page, err := s.messages.GetThreadMessages(c, roomID, parent.ThreadRoomID, pageReq)
    if err != nil {
        slog.Error("loading thread messages", "error", err, "roomID", roomID)
        return nil, natsrouter.ErrInternal("failed to load thread messages")
    }
    return &models.GetThreadMessagesResponse{
        Messages:   wrapMessages(page.Data),
        NextCursor: page.NextCursor,
        HasNext:    page.HasNext,
    }, nil
}
```

- [ ] **Step 8 — Update `history-service/internal/service/threads.go`**

Change `cassMessages` and `msgByID` to use `cassandra.Message`:

```go
// In GetThreadsList, change:
cassMessages, err := s.messages.GetMessagesByIDs(c, parentIDs)  // now []cassandra.Message

msgByID := make(map[string]cassandra.Message, len(cassMessages))
for i := range cassMessages {
    msgByID[cassMessages[i].MessageID] = cassMessages[i]
}

// The ThreadParentMessage construction is unchanged:
threads = append(threads, models.ThreadParentMessage{
    Message:           msg,   // cassandra.Message — embeds fine
    ThreadCount:       tr.ThreadCount,
    ThreadLastMessage: tr.LastMsgAt,
    ReplyAccounts:     tr.ReplyAccounts,
})
```

Add `"github.com/hmchangw/chat/pkg/model/cassandra"` to the imports.

- [ ] **Step 9 — Regenerate mocks**

```bash
make generate SERVICE=history-service
```
Expected: `mocks/mock_repository.go` is regenerated with `cassandra.Message` instead of `models.Message`.

- [ ] **Step 10 — Build check**

```bash
make build SERVICE=history-service
```
Expected: build succeeds.

### 2d — Update service unit tests

- [ ] **Step 11 — Update `history-service/internal/service/messages_test.go`**

Key changes:
1. `makePage` now takes `[]cassandra.Message`
2. All inline `models.Message{...}` struct literals → `cassandra.Message{...}`
3. All `cassrepo.Page[models.Message]{}` error stubs → `cassrepo.Page[cassandra.Message]{}`
4. Add `asBase` helper for asserting fields on interface values
5. Field accesses like `resp.Messages[0].MessageID` → `asBase(t, resp.Messages[0]).MessageID`
6. `GetMessageByID` result is `*models.BaseMessage` — `result.MessageID` works directly (embedding)

```go
// Replace makePage:
func makePage(msgs []cassandra.Message, hasNext bool) cassrepo.Page[cassandra.Message] {
    nextCursor := ""
    if hasNext { nextCursor = "fake-next-cursor" }
    return cassrepo.Page[cassandra.Message]{Data: msgs, NextCursor: nextCursor, HasNext: hasNext}
}

// Add helper:
func asBase(t *testing.T, m models.Message) cassandra.Message {
    t.Helper()
    b, ok := m.(models.BaseMessage)
    require.True(t, ok, "expected BaseMessage, got %T", m)
    return b.Message
}

// Replace all models.Message{...} literals with cassandra.Message{...}, e.g.:
messages := []cassandra.Message{
    {MessageID: "m1", RoomID: "r1", CreatedAt: joinTime.Add(1 * time.Minute)},
}

// Replace cassrepo.Page[models.Message]{} with cassrepo.Page[cassandra.Message]{}

// Update field access, e.g. in LoadSurroundingMessages_Success:
assert.Equal(t, "m4", asBase(t, resp.Messages[0]).MessageID)
assert.Equal(t, "m5", asBase(t, resp.Messages[1]).MessageID)
assert.Equal(t, "m6", asBase(t, resp.Messages[2]).MessageID)

// GetMessageByID result has concrete type *models.BaseMessage — direct field access works:
assert.Equal(t, "m1", result.MessageID)  // via embedding, no change needed

// Add cassandra import:
// "github.com/hmchangw/chat/pkg/model/cassandra"
```

- [ ] **Step 12 — Update `history-service/internal/service/threads_test.go`**

Change `makeCassMessages` return type and all inline `models.Message` literals:

```go
// Replace:
func makeCassMessages() []cassandra.Message {
    return []cassandra.Message{
        {MessageID: "p1", RoomID: "r1", Msg: "parent 1"},
        {MessageID: "p2", RoomID: "r1", Msg: "parent 2"},
    }
}

// Replace inline literals in MissingParentIgnored test:
msgs.EXPECT().GetMessagesByIDs(gomock.Any(), gomock.Any()).Return(
    []cassandra.Message{{MessageID: "p1", RoomID: "r1", Msg: "parent 1"}}, nil,
)

// Add cassandra import:
// "github.com/hmchangw/chat/pkg/model/cassandra"
```

Note: assertions on `resp.Threads[0].MessageID` and `resp.Threads[0].ThreadCount` still work unchanged — `ThreadParentMessage` embeds `cassandra.Message`, so fields are promoted.

- [ ] **Step 13 — Run all tests**

```bash
make test SERVICE=history-service
```
Expected: all tests pass (GREEN).

- [ ] **Step 14 — Commit**

```bash
git add \
  history-service/internal/models/message.go \
  history-service/internal/models/threads.go \
  history-service/internal/models/message_test.go \
  history-service/internal/service/service.go \
  history-service/internal/service/utils.go \
  history-service/internal/service/messages.go \
  history-service/internal/service/threads.go \
  history-service/internal/service/mocks/mock_repository.go \
  history-service/internal/service/messages_test.go \
  history-service/internal/service/threads_test.go
git commit -m "feat(models): introduce Message union type (BaseMessage | ThreadParentMessage)"
```

---

## Chapter 3 — Push to remote

- [ ] **Step 1 — Push branch**

```bash
git push -u origin claude/add-history-nats-endpoint-0OVot
```

---

## Self-Review

**Spec coverage:**
- ✅ `type Message interface { isMessage() }` closed union
- ✅ `BaseMessage` embeds `cassandra.Message`
- ✅ `ThreadParentMessage` gets `isMessage()` marker
- ✅ `cassandra.Message` stays as DB scan target (cassrepo unchanged in meaning)
- ✅ No `kind`/`type` discriminator field in JSON output
- ✅ `natsrouter.Register` compatible — handlers return concrete pointer types (`*LoadHistoryResponse`, `*BaseMessage`), `json.Marshal` handles interface slices correctly
- ✅ All existing service handler tests updated

**Placeholder scan:** None — every step has exact code.

**Type consistency:**
- `cassrepo.Page[cassandra.Message]` used consistently in `service.go`, `utils.go`, `messages.go`, mocks
- `models.BaseMessage{Message: m}` wrapping pattern used consistently in `messages.go` and `utils.go/wrapMessages`
- `msgByID map[string]cassandra.Message` in `threads.go` matches `GetMessagesByIDs` return type
