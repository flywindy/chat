# Thread Unread Summary RPC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a server-to-server NATS RPC in room-service that, given a `userAccount`, returns a per-site rollup of that user's thread unread state (`unread`, `unreadDirectMessage`, `unreadMention`, `lastMessageAt`).

**Architecture:** A single MongoDB aggregation over `thread_subscriptions` (matched by `userAccount`+`siteId`), joined to `thread_rooms` (for `lastMsgAt`) and `rooms` (for DM type), collapsed with `$group`/`$max`. Computation lives entirely in room-service's store; the handler is a thin validate-call-map shim registered on `chat.server.request.room.{siteID}.thread.unread.summary`. No write-path changes, no backfill.

**Tech Stack:** Go 1.25, `nats.go` + `pkg/natsrouter`, `mongo-driver/v2`, `go.uber.org/mock`, `testify`, `testcontainers-go` (via `pkg/testutil`).

**Spec:** `docs/superpowers/specs/2026-06-09-thread-unread-summary-rpc-design.md`

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `pkg/subject/subject.go` | Subject builders | Add `ThreadUnreadSummary` / `ThreadUnreadSummarySubscribe` |
| `pkg/subject/subject_test.go` | Subject builder tests | Add cases |
| `pkg/model/threadsubscription.go` | Thread sub model + RPC DTOs | Add `ThreadUnreadSummaryRequest` / `ThreadUnreadSummaryResponse` |
| `pkg/model/model_test.go` | Model roundtrip tests | Add roundtrip for the response DTO |
| `room-service/store.go` | `RoomStore` interface + result structs | Add `ThreadUnreadSummary` struct + interface method |
| `room-service/store_mongo.go` | Mongo implementation | Add `threadRooms` handle, index, `GetThreadUnreadSummary` |
| `room-service/mock_store_test.go` | Generated mock | Regenerate (never hand-edit) |
| `room-service/handler.go` | NATS handler + registration | Add `threadUnreadSummary` + register |
| `room-service/handler_test.go` | Handler unit tests | Add `TestHandler_threadUnreadSummary` |
| `room-service/integration_test.go` | Store integration tests | Add `TestMongoStore_GetThreadUnreadSummary_Integration` |

---

# Chapter 1 — Wire contract (`pkg`)

Defines the subject and the request/response DTOs that both caller and server share. No room-service dependency, so it builds and tests in isolation first.

### Task 1.1: Subject builders

**Files:**
- Modify: `pkg/subject/subject.go` (near `RoomsInfoBatch`, ~line 218)
- Test: `pkg/subject/subject_test.go` (add to `TestSubjectBuilders` table ~line 56, and `TestWildcardPatterns` ~line 271)

- [ ] **Step 1: Write the failing test**

In `pkg/subject/subject_test.go`, add to the `TestSubjectBuilders` table (alongside the `RoomsInfoBatch` case at line 56):

```go
		{"ThreadUnreadSummary", subject.ThreadUnreadSummary("site-a"),
			"chat.server.request.room.site-a.thread.unread.summary"},
```

And add to the `TestWildcardPatterns` table (alongside `RoomsInfoBatchSubscribe` at line 271):

```go
		{"ThreadUnreadSummarySubscribe", subject.ThreadUnreadSummarySubscribe("site-a"),
			"chat.server.request.room.site-a.thread.unread.summary"},
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/subject/ -run TestSubjectBuilders -v`
Expected: FAIL — `subject.ThreadUnreadSummary` undefined (compile error).

- [ ] **Step 3: Write minimal implementation**

In `pkg/subject/subject.go`, after the `RoomsInfoBatch` function (~line 221):

```go
// ThreadUnreadSummary is the server-to-server request subject for a per-site
// thread unread rollup for a single user.
func ThreadUnreadSummary(siteID string) string {
	return fmt.Sprintf("chat.server.request.room.%s.thread.unread.summary", siteID)
}
```

And after `RoomsInfoBatchSubscribe` (~line 355):

```go
// ThreadUnreadSummarySubscribe is the per-site subscription subject for the
// thread unread summary RPC.
func ThreadUnreadSummarySubscribe(siteID string) string {
	return fmt.Sprintf("chat.server.request.room.%s.thread.unread.summary", siteID)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/subject/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "Add thread unread summary subject builders"
```

### Task 1.2: Request/response DTOs

**Files:**
- Modify: `pkg/model/threadsubscription.go` (append after the `ThreadSubscription` struct)
- Test: `pkg/model/model_test.go` (add a roundtrip test)

- [ ] **Step 1: Write the failing test**

In `pkg/model/model_test.go`, add (after `TestThreadSubscriptionJSON` ~line 196):

```go
func TestThreadUnreadSummaryResponseJSON(t *testing.T) {
	ms := int64(1717000000000)
	r := model.ThreadUnreadSummaryResponse{
		Unread:              true,
		UnreadDirectMessage: false,
		UnreadMention:       true,
		LastMessageAt:       &ms,
	}
	roundTrip(t, &r, &model.ThreadUnreadSummaryResponse{})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/model/ -run TestThreadUnreadSummaryResponseJSON -v`
Expected: FAIL — `model.ThreadUnreadSummaryResponse` undefined (compile error).

- [ ] **Step 3: Write minimal implementation**

In `pkg/model/threadsubscription.go`, append at end of file:

```go
// ThreadUnreadSummaryRequest is the NATS request body for the per-site thread
// unread summary RPC. Filtered on userAccount (the key every thread_subscriptions
// query and index uses).
type ThreadUnreadSummaryRequest struct {
	UserAccount string `json:"userAccount"`
}

// ThreadUnreadSummaryResponse is the per-site thread unread rollup for one user.
// LastMessageAt is UnixMilli; nil when the user has no threads on this site.
type ThreadUnreadSummaryResponse struct {
	Unread              bool   `json:"unread"`
	UnreadDirectMessage bool   `json:"unreadDirectMessage"`
	UnreadMention       bool   `json:"unreadMention"`
	LastMessageAt       *int64 `json:"lastMessageAt,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/model/ -run TestThreadUnreadSummaryResponseJSON -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/threadsubscription.go pkg/model/model_test.go
git commit -m "Add thread unread summary request/response models"
```

---

# Chapter 2 — Store layer (`room-service`)

The aggregation that does the real work, plus the supporting collection handle and index. Tested with a real Mongo via testcontainers.

### Task 2.1: Store interface + result struct + collection handle + index

**Files:**
- Modify: `room-service/store.go` (add struct after `RoomCounts` ~line 36; add method to `RoomStore` interface ~line 54)
- Modify: `room-service/store_mongo.go` (add `threadRooms` field ~line 31, constructor ~line 42, index in `EnsureIndexes` ~line 127)

- [ ] **Step 1: Add the result struct and interface method**

In `room-service/store.go`, after the `RoomCounts` struct (line 36):

```go
// ThreadUnreadSummary is the result of GetThreadUnreadSummary — a per-site
// rollup of a single user's thread unread state, computed in one aggregation.
type ThreadUnreadSummary struct {
	Unread              bool
	UnreadDirectMessage bool
	UnreadMention       bool
	LastMessageAt       *time.Time
}
```

In the `RoomStore` interface (after `ListRoomsByIDs` at line 54):

```go
	GetThreadUnreadSummary(ctx context.Context, account, siteID string) (*ThreadUnreadSummary, error)
```

- [ ] **Step 2: Add the collection handle and index (no implementation yet)**

In `room-service/store_mongo.go`, add to the `MongoStore` struct (after line 31):

```go
	threadRooms         *mongo.Collection
```

In `NewMongoStore` (after line 42):

```go
		threadRooms:         db.Collection("thread_rooms"),
```

In `EnsureIndexes`, after the existing `(parentMessageId,userAccount)` thread_subscriptions index block (line 132), add:

```go
	// Backs GetThreadUnreadSummary's $match {userAccount, siteId}. No existing
	// thread_subscriptions index has userAccount as a prefix.
	if _, err := s.threadSubscriptions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "userAccount", Value: 1}, {Key: "siteId", Value: 1}},
	}); err != nil {
		return fmt.Errorf("ensure thread_subscriptions (userAccount,siteId) index: %w", err)
	}
```

- [ ] **Step 3: Regenerate the mock and verify the build fails on the missing implementation**

Run: `make generate SERVICE=room-service`
Then: `make build SERVICE=room-service`
Expected: FAIL — `*MongoStore` does not implement `RoomStore` (missing method `GetThreadUnreadSummary`). The mock is regenerated with the new method.

- [ ] **Step 4: Commit**

```bash
git add room-service/store.go room-service/store_mongo.go room-service/mock_store_test.go
git commit -m "Add thread unread summary store interface, handle, and index"
```

### Task 2.2: Aggregation implementation + integration test

**Files:**
- Modify: `room-service/store_mongo.go` (implement `GetThreadUnreadSummary`)
- Test: `room-service/integration_test.go` (add `TestMongoStore_GetThreadUnreadSummary_Integration`)

- [ ] **Step 1: Write the failing integration test**

In `room-service/integration_test.go`, add (follow the `TestMongoStore_CountMembersAndOwners_Integration` style at line 224):

```go
func TestMongoStore_GetThreadUnreadSummary_Integration(t *testing.T) {
	db := setupMongo(t)
	store := NewMongoStore(db)
	ctx := context.Background()

	const account = "alice@example.com"
	const site = "site-a"
	seen := time.Now().UTC().Add(-time.Hour)
	newer := time.Now().UTC()
	older := time.Now().UTC().Add(-2 * time.Hour)

	// Rooms: one channel, one DM.
	_, err := db.Collection("rooms").InsertMany(ctx, []interface{}{
		model.Room{ID: "room-chan", Type: model.RoomTypeChannel, SiteID: site},
		model.Room{ID: "room-dm", Type: model.RoomTypeDM, SiteID: site},
	})
	require.NoError(t, err)

	// Thread rooms: tr1 (channel, newer msg), tr2 (dm, newer msg), tr3 (channel, older msg).
	_, err = db.Collection("thread_rooms").InsertMany(ctx, []interface{}{
		model.ThreadRoom{ID: "tr1", RoomID: "room-chan", SiteID: site, LastMsgAt: newer},
		model.ThreadRoom{ID: "tr2", RoomID: "room-dm", SiteID: site, LastMsgAt: newer},
		model.ThreadRoom{ID: "tr3", RoomID: "room-chan", SiteID: site, LastMsgAt: older},
	})
	require.NoError(t, err)

	// Thread subscriptions for alice:
	//  tr1: lastSeenAt=seen < newer  -> unread (channel)
	//  tr2: lastSeenAt=seen < newer  -> unread + DM, hasMention=true
	//  tr3: lastSeenAt=newer > older -> read
	seenCopy := seen
	_, err = db.Collection("thread_subscriptions").InsertMany(ctx, []interface{}{
		model.ThreadSubscription{ID: "ts1", ThreadRoomID: "tr1", RoomID: "room-chan", UserAccount: account, SiteID: site, LastSeenAt: &seenCopy},
		model.ThreadSubscription{ID: "ts2", ThreadRoomID: "tr2", RoomID: "room-dm", UserAccount: account, SiteID: site, LastSeenAt: &seenCopy, HasMention: true},
		model.ThreadSubscription{ID: "ts3", ThreadRoomID: "tr3", RoomID: "room-chan", UserAccount: account, SiteID: site, LastSeenAt: &newer},
	})
	require.NoError(t, err)

	t.Run("rolls up unread, dm, mention, and max lastMessageAt", func(t *testing.T) {
		got, err := store.GetThreadUnreadSummary(ctx, account, site)
		require.NoError(t, err)
		assert.True(t, got.Unread)
		assert.True(t, got.UnreadDirectMessage)
		assert.True(t, got.UnreadMention)
		require.NotNil(t, got.LastMessageAt)
		assert.WithinDuration(t, newer, got.LastMessageAt.UTC(), time.Millisecond)
	})

	t.Run("no subscriptions yields zero-value summary", func(t *testing.T) {
		got, err := store.GetThreadUnreadSummary(ctx, "nobody@example.com", site)
		require.NoError(t, err)
		assert.False(t, got.Unread)
		assert.False(t, got.UnreadDirectMessage)
		assert.False(t, got.UnreadMention)
		assert.Nil(t, got.LastMessageAt)
	})

	t.Run("other site is filtered out", func(t *testing.T) {
		got, err := store.GetThreadUnreadSummary(ctx, account, "site-b")
		require.NoError(t, err)
		assert.False(t, got.Unread)
		assert.Nil(t, got.LastMessageAt)
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test-integration SERVICE=room-service`
Expected: FAIL — `GetThreadUnreadSummary` returns the not-implemented error / panics, or assertions fail (method body not written yet).

- [ ] **Step 3: Implement the aggregation**

In `room-service/store_mongo.go`, add (near the other aggregation methods):

```go
// GetThreadUnreadSummary rolls up a single user's thread unread state on this
// site. Unread = subscribed AND threadRoom.lastMsgAt > lastSeenAt (nil lastSeenAt
// = never seen = unread, via BSON null being the smallest value). The booleans
// are an OR across the user's threads, expressed as $max over per-row booleans
// (BSON orders false < true). lastMessageAt is the newest thread message time.
func (s *MongoStore) GetThreadUnreadSummary(ctx context.Context, account, siteID string) (*ThreadUnreadSummary, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"userAccount": account, "siteId": siteID}}},
		{{Key: "$lookup", Value: bson.M{
			"from":         "thread_rooms",
			"localField":   "threadRoomId",
			"foreignField": "_id",
			"as":           "tr",
		}}},
		{{Key: "$unwind", Value: "$tr"}},
		{{Key: "$lookup", Value: bson.M{
			"from":         "rooms",
			"localField":   "roomId",
			"foreignField": "_id",
			"as":           "room",
		}}},
		{{Key: "$unwind", Value: bson.M{"path": "$room", "preserveNullAndEmptyArrays": true}}},
		{{Key: "$addFields", Value: bson.M{
			"isUnread": bson.M{"$gt": bson.A{"$tr.lastMsgAt", "$lastSeenAt"}},
			"isDMUnread": bson.M{"$and": bson.A{
				bson.M{"$gt": bson.A{"$tr.lastMsgAt", "$lastSeenAt"}},
				bson.M{"$eq": bson.A{"$room.type", string(model.RoomTypeDM)}},
			}},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":                 nil,
			"unread":              bson.M{"$max": "$isUnread"},
			"unreadDirectMessage": bson.M{"$max": "$isDMUnread"},
			"unreadMention":       bson.M{"$max": "$hasMention"},
			"lastMessageAt":       bson.M{"$max": "$tr.lastMsgAt"},
		}}},
	}

	cursor, err := s.threadSubscriptions.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregate thread unread summary: %w", err)
	}
	defer cursor.Close(ctx)

	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return nil, fmt.Errorf("iterate thread unread summary: %w", err)
		}
		return &ThreadUnreadSummary{}, nil
	}

	var result struct {
		Unread              bool       `bson:"unread"`
		UnreadDirectMessage bool       `bson:"unreadDirectMessage"`
		UnreadMention       bool       `bson:"unreadMention"`
		LastMessageAt       *time.Time `bson:"lastMessageAt"`
	}
	if err := cursor.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode thread unread summary: %w", err)
	}
	return &ThreadUnreadSummary{
		Unread:              result.Unread,
		UnreadDirectMessage: result.UnreadDirectMessage,
		UnreadMention:       result.UnreadMention,
		LastMessageAt:       result.LastMessageAt,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test-integration SERVICE=room-service`
Expected: PASS — all three subtests green.

- [ ] **Step 5: Commit**

```bash
git add room-service/store_mongo.go room-service/integration_test.go
git commit -m "Implement GetThreadUnreadSummary aggregation"
```

---

# Chapter 3 — Handler + registration (`room-service`)

The thin NATS shim: validate, call the store, map `*time.Time` → `*int64` ms. Unit-tested against the regenerated mock.

### Task 3.1: Handler method + registration

**Files:**
- Modify: `room-service/handler.go` (add handler after `roomsInfoBatch` ~line 1077; register after line 100)

- [ ] **Step 1: Add the handler and registration**

In `room-service/handler.go`, register after the `RoomsInfoBatchSubscribe` line (line 100):

```go
	natsrouter.Register(r, subject.ThreadUnreadSummarySubscribe(h.siteID), h.threadUnreadSummary)
```

Add the handler after `roomsInfoBatch` ends (line 1077):

```go
func (h *Handler) threadUnreadSummary(c *natsrouter.Context, req model.ThreadUnreadSummaryRequest) (*model.ThreadUnreadSummaryResponse, error) {
	var ctx context.Context = c
	start := time.Now()
	if req.UserAccount == "" {
		return nil, errcode.BadRequest("userAccount must not be empty")
	}

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attribute.String("site_id", h.siteID))
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	summary, err := h.store.GetThreadUnreadSummary(ctx, req.UserAccount, h.siteID)
	if err != nil {
		return nil, fmt.Errorf("get thread unread summary: %w", err)
	}

	resp := &model.ThreadUnreadSummaryResponse{
		Unread:              summary.Unread,
		UnreadDirectMessage: summary.UnreadDirectMessage,
		UnreadMention:       summary.UnreadMention,
	}
	if summary.LastMessageAt != nil && !summary.LastMessageAt.IsZero() {
		ms := summary.LastMessageAt.UTC().UnixMilli()
		resp.LastMessageAt = &ms
	}

	slog.Debug("thread unread summary handled",
		"site_id", h.siteID,
		"unread", resp.Unread,
		"latency_ms", time.Since(start).Milliseconds(),
	)
	return resp, nil
}
```

- [ ] **Step 2: Verify the build compiles**

Run: `make build SERVICE=room-service`
Expected: PASS (compiles). No `slog`/`trace`/`attribute`/`errcode` import additions needed — all already used in `handler.go`.

- [ ] **Step 3: Commit**

```bash
git add room-service/handler.go
git commit -m "Add threadUnreadSummary handler and registration"
```

### Task 3.2: Handler unit tests

**Files:**
- Test: `room-service/handler_test.go` (add `TestHandler_threadUnreadSummary`, mirroring `TestHandler_handleRoomsInfoBatch` at line 1927)

- [ ] **Step 1: Write the failing test**

In `room-service/handler_test.go`, add:

```go
func TestHandler_threadUnreadSummary(t *testing.T) {
	ms := time.UnixMilli(1717000000000).UTC()

	tests := []struct {
		name       string
		req        model.ThreadUnreadSummaryRequest
		setupStore func(*MockRoomStore)
		wantErr    string
		assertResp func(t *testing.T, resp model.ThreadUnreadSummaryResponse)
	}{
		{
			name: "happy path maps all fields",
			req:  model.ThreadUnreadSummaryRequest{UserAccount: "alice@example.com"},
			setupStore: func(s *MockRoomStore) {
				s.EXPECT().GetThreadUnreadSummary(gomock.Any(), "alice@example.com", "site-a").
					Return(&ThreadUnreadSummary{
						Unread:              true,
						UnreadDirectMessage: true,
						UnreadMention:       false,
						LastMessageAt:       &ms,
					}, nil)
			},
			assertResp: func(t *testing.T, resp model.ThreadUnreadSummaryResponse) {
				assert.True(t, resp.Unread)
				assert.True(t, resp.UnreadDirectMessage)
				assert.False(t, resp.UnreadMention)
				require.NotNil(t, resp.LastMessageAt)
				assert.Equal(t, int64(1717000000000), *resp.LastMessageAt)
			},
		},
		{
			name: "nil lastMessageAt maps to nil",
			req:  model.ThreadUnreadSummaryRequest{UserAccount: "bob@example.com"},
			setupStore: func(s *MockRoomStore) {
				s.EXPECT().GetThreadUnreadSummary(gomock.Any(), "bob@example.com", "site-a").
					Return(&ThreadUnreadSummary{}, nil)
			},
			assertResp: func(t *testing.T, resp model.ThreadUnreadSummaryResponse) {
				assert.False(t, resp.Unread)
				assert.Nil(t, resp.LastMessageAt)
			},
		},
		{
			name:    "empty userAccount is rejected",
			req:     model.ThreadUnreadSummaryRequest{UserAccount: ""},
			wantErr: "userAccount must not be empty",
		},
		{
			name: "store error propagates",
			req:  model.ThreadUnreadSummaryRequest{UserAccount: "alice@example.com"},
			setupStore: func(s *MockRoomStore) {
				s.EXPECT().GetThreadUnreadSummary(gomock.Any(), "alice@example.com", "site-a").
					Return(nil, errors.New("boom"))
			},
			wantErr: "get thread unread summary",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockRoomStore(ctrl)
			if tc.setupStore != nil {
				tc.setupStore(store)
			}
			h := &Handler{store: store, siteID: "site-a"}

			resp, err := h.threadUnreadSummary(ctxParams(map[string]string{}), tc.req)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			if tc.assertResp != nil {
				tc.assertResp(t, *resp)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails (then passes)**

Run: `make test SERVICE=room-service`
Expected: PASS — the handler from Task 3.1 already satisfies these. (If Task 3.1 and 3.2 are done together, this is the green confirmation; the test is written to exercise the just-added handler.)

> Note: `ctxParams`, `MockRoomStore`, and the `errors`/`gomock`/`time` imports already exist in `handler_test.go` (used by `TestHandler_handleRoomsInfoBatch`). No new imports needed.

- [ ] **Step 3: Commit**

```bash
git add room-service/handler_test.go
git commit -m "Add threadUnreadSummary handler unit tests"
```

---

# Chapter 4 — Finalization

### Task 4.1: Full lint + test sweep

- [ ] **Step 1: Lint**

Run: `make lint`
Expected: PASS (no new findings).

- [ ] **Step 2: Unit tests with race detector**

Run: `make test SERVICE=room-service` and `go test ./pkg/subject/ ./pkg/model/`
Expected: PASS.

- [ ] **Step 3: Integration tests**

Run: `make test-integration SERVICE=room-service`
Expected: PASS.

- [ ] **Step 4: SAST (blocking CI gate)**

Run: `make sast`
Expected: PASS (no medium+ findings).

- [ ] **Step 5: Push the branch**

```bash
git push -u origin claude/trusting-pascal-3rz68c
```

---

## Notes for the implementer

- **No `docs/client-api.md` change.** The subject is `chat.server.*`, not `chat.user.*`; the client-api guardrail applies only to `chat.user.` RPCs.
- **Why `$max` for booleans:** each boolean field is "is there ANY thread where …?" — an OR across the user's thread rows. `$group` has no `$or` accumulator, and BSON orders `false < true`, so `$max` over a boolean column yields `true` iff any row is true.
- **Why `lastSeenAt` has no `omitempty`** (in `model.ThreadSubscription`): the `$gt` unread check relies on a never-seen subscription encoding as explicit BSON `null` (the smallest value), so `lastMsgAt > null` is always true. Do not add `omitempty` to that field.
- **`$unwind tr` drops orphans** (no `preserveNullAndEmptyArrays`): a thread sub whose `thread_rooms` doc was deleted is excluded. **`$unwind room` preserves** (`preserveNullAndEmptyArrays: true`): a missing parent room keeps the row for unread/mention; the DM check just resolves false.
- **Mock regeneration:** after editing `store.go`'s interface, always `make generate SERVICE=room-service` before building/testing. Never hand-edit `mock_store_test.go`.
