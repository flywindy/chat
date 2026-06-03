# Channel Rename and Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Post-merge update (PR #224 review):** The shipped design diverged from this plan in two places. (1) The visibility RPC was renamed to `room.restricted` and migrated from the user-facing `chat.user.>` async-job path to a **synchronous** server-to-server request/reply on `chat.server.request.room.{siteID}.restricted`. room-service now does the work in-handler (Mongo writes + sys message publish + outbox fan-out) and replies `{"status":"ok"}` once the work is committed; no `AsyncJobResult` is emitted and the canonical `ROOMS_` flow is no longer used for restricted changes. (2) For both rename and restricted, the per-subscription `SubscriptionUpdateEvent` fan-out described below was replaced with a **single room-scoped sys message** on `chat.room.{roomID}.event` — clients drive their local state off that one event. See `docs/client-api.md` for the live shape.

**Goal:** Add two admin RPCs for channel rooms: `room.rename` (owner or platform admin) and `room.visibility` (platform admin only, sets `Restricted` + `ExternalAccess`, rewrites roles on the unrestricted→restricted transition).

**Architecture:** Two-service split, mirroring the existing role-update flow. `room-service` validates synchronously and publishes a canonical event to `ROOMS_{siteID}`. `room-worker` (home site) persists Mongo writes, emits a sys message for rename, fans `SubscriptionUpdateEvent` per affected subscription, publishes one outbox event per remote site with federated members. `inbox-worker` (remote sites) mirrors the subscription mutation locally — no event fan-out (supercluster routes per-account events from home site).

**Tech stack:** Go 1.25, NATS + JetStream, MongoDB, `go.uber.org/mock`, `stretchr/testify`, `testcontainers-go`.

**Spec:** `docs/superpowers/specs/2026-05-22-channel-rename-and-visibility-design.md`

---

## Conventions

- One task = one commit. Don't squash mid-implementation.
- Commands: `make test SERVICE=<dir>` (unit, race), `make test-integration SERVICE=<dir>` (Docker), `make generate SERVICE=<dir>` (mocks), `make lint`, `make sast`.
- `SERVICE=` accepts `pkg/model`, `pkg/subject`, or any service dir at repo root.
- Never hand-edit `mock_store_test.go` — always `make generate`.
- Every new handler logs `slog.Info` at entry with `op`, `requester`, `roomID`, `requestID`. Validation rejects in `room-service` log INFO with `reason`. The code snippets below omit these for brevity — add them when implementing.

---

## File map

| File | What changes |
|------|--------------|
| `pkg/model/user.go` | `UserRole` type, `UserRoleAdmin`/`UserRoleUser` consts, `User.Roles` field |
| `pkg/model/room.go` | `Room.ExternalAccess` field; `RenameRoomRequest` and `RoomVisibilityRequest` |
| `pkg/model/subscription.go` | `Subscription.Restricted`, `Subscription.ExternalAccess` |
| `pkg/model/message.go` | `RoomRenamedSysData` type |
| `pkg/model/event.go` | `MessageTypeRoomRenamed`; `OutboxRoomRenamed` + `OutboxRoomVisibilityChanged` consts; `RoomRenamedOutboxPayload` + `RoomVisibilityOutboxPayload`; `AsyncJobOpRoomRename` + `AsyncJobOpRoomVisibility`; `SubscriptionUpdateEvent.Action` doc-comment |
| `pkg/model/model_test.go` | Round-trip tests for everything above |
| `pkg/subject/subject.go` + `_test.go` | Four new builders (`RoomRename`, `RoomRenameWildcard`, `RoomVisibility`, `RoomVisibilityWildcard`) |
| `room-worker/store.go` | 4 new methods on `SubscriptionStore` interface |
| `room-worker/store_mongo.go` | Implement the 4 methods |
| `room-worker/integration_test.go` | Integration tests for the 4 store methods + the 2 processors |
| `room-worker/handler.go` | `findRemoteSitesForAccounts` + `publishSubscriptionEvents` helpers; `processRoomRename` + `processRoomVisibility`; extend `HandleJetStreamMsg` switch; retrofit `processAddMembers` to use the helper |
| `room-worker/handler_test.go` | Unit tests for new helpers, processors, dispatch |
| `inbox-worker/handler.go` | `errPermanent` + `permanentError` + `newPermanent`; 2 new methods on `InboxStore`; `handleRoomRenamed` + `handleRoomVisibilityChanged`; extend `HandleEvent` switch |
| `inbox-worker/main.go` | `dispatchAckPolicy` helper; ack-on-permanent in `cons.Consume`; implement the 2 new store methods on `mongoInboxStore` |
| `inbox-worker/handler_test.go` | Unit tests |
| `inbox-worker/integration_test.go` | Integration tests for store methods + handlers |
| `inbox-worker/main_test.go` | Test for `dispatchAckPolicy` |
| `room-service/helper.go` + `_test.go` | `isPlatformAdmin`; new sentinels; extend `sanitizeError` |
| `room-service/handler.go` + `_test.go` | `handleRoomRename` + `handleRoomVisibility` + their nats wrappers; register in `RegisterCRUD` |
| `room-service/integration_test.go` | End-to-end NATS request/reply with stream-publish assertions |
| `room-service/main.go` | `RESTRICTED_ROOM_MIN_MEMBERS` env (default `5`); plumb into Handler |
| `mock-user-service/handler.go` + `_test.go` | `admin1` → admin role |
| `docs/client-api.md` | Document both new RPCs under §3.1 |

Auto-regenerated (never hand-edit): `room-worker/mock_store_test.go`, `inbox-worker/mock_store_test.go`.

---

## Phase 1: Data Models (`pkg/model`)

### Task 1: Add all model types and round-trip tests

**Files:** `pkg/model/user.go`, `pkg/model/room.go`, `pkg/model/subscription.go`, `pkg/model/message.go`, `pkg/model/event.go`, `pkg/model/model_test.go`.

- [ ] **1.1 Write all failing round-trip tests**

Append the following to `pkg/model/model_test.go`:

```go
// --- User.Roles ---

func TestUserJSON_WithRoles(t *testing.T) {
	u := model.User{ID: "u1", Account: "admin1", SiteID: "site-a",
		Roles: []model.UserRole{model.UserRoleAdmin}}
	roundTrip(t, &u, &model.User{})
}

func TestUserRoleConstants(t *testing.T) {
	if model.UserRoleAdmin != "admin" || model.UserRoleUser != "user" {
		t.Fatalf("UserRole consts: admin=%q user=%q", model.UserRoleAdmin, model.UserRoleUser)
	}
}

// --- Room.ExternalAccess ---

func TestRoomJSON_RestrictedAndExternalAccess(t *testing.T) {
	r := model.Room{
		ID: "r1", Name: "x", Type: model.RoomTypeChannel, SiteID: "site-a", UserCount: 5,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Restricted: true, ExternalAccess: true,
	}
	roundTrip(t, &r, &model.Room{})
}

func TestRoomJSON_ExternalAccessOmittedWhenFalse(t *testing.T) {
	r := model.Room{ID: "r1", Name: "x", Type: model.RoomTypeChannel, SiteID: "site-a",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(&r)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	_, has := raw["externalAccess"]
	assert.False(t, has, "false ExternalAccess must omit")
}

// --- Subscription.Restricted / ExternalAccess ---

func TestSubscriptionJSON_RestrictedAndExternalAccess(t *testing.T) {
	s := model.Subscription{
		ID: "s1", User: model.SubscriptionUser{ID: "u1", Account: "alice"},
		RoomID: "r1", SiteID: "site-a",
		Roles: []model.Role{model.RoleMember}, Name: "x", RoomType: model.RoomTypeChannel,
		JoinedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Restricted: true, ExternalAccess: true,
	}
	roundTrip(t, &s, &model.Subscription{})
}

// --- Request types ---

func TestRenameRoomRequestJSON(t *testing.T) {
	r := model.RenameRoomRequest{RoomID: "r1", NewName: "new", Account: "alice", Timestamp: 1700000000000}
	roundTrip(t, &r, &model.RenameRoomRequest{})
}

func TestRoomVisibilityRequestJSON(t *testing.T) {
	r := model.RoomVisibilityRequest{
		RoomID: "r1", Restricted: true, ExternalAccess: false,
		OwnerAccount: "alice", Account: "admin1", Timestamp: 1700000000000,
	}
	roundTrip(t, &r, &model.RoomVisibilityRequest{})
}

func TestRoomVisibilityRequest_OwnerOmittedWhenEmpty(t *testing.T) {
	r := model.RoomVisibilityRequest{RoomID: "r1", Account: "admin1", Timestamp: 1700000000000}
	data, err := json.Marshal(&r)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	_, has := raw["ownerAccount"]
	assert.False(t, has)
}

// --- Sys message + outbox payloads ---

func TestRoomRenamedSysDataJSON(t *testing.T) {
	d := model.RoomRenamedSysData{NewName: "renamed", ByAccount: "alice"}
	roundTrip(t, &d, &model.RoomRenamedSysData{})
}

func TestRoomRenamedOutboxPayloadJSON(t *testing.T) {
	p := model.RoomRenamedOutboxPayload{RoomID: "r1", NewName: "x", Timestamp: 1700000000000}
	roundTrip(t, &p, &model.RoomRenamedOutboxPayload{})
}

func TestRoomVisibilityOutboxPayloadJSON(t *testing.T) {
	p := model.RoomVisibilityOutboxPayload{
		RoomID: "r1", Restricted: true, ExternalAccess: false,
		OwnerAccount: "alice", Timestamp: 1700000000000,
	}
	roundTrip(t, &p, &model.RoomVisibilityOutboxPayload{})
}

// --- Constants ---

func TestMessageAndOutboxAndAsyncOpConstants(t *testing.T) {
	assert.Equal(t, "room_renamed", model.MessageTypeRoomRenamed)
	assert.Equal(t, "room_renamed", string(model.OutboxRoomRenamed))
	assert.Equal(t, "room_visibility_changed", string(model.OutboxRoomVisibilityChanged))
	assert.Equal(t, "room.rename", model.AsyncJobOpRoomRename)
	assert.Equal(t, "room.visibility", model.AsyncJobOpRoomVisibility)
}
```

- [ ] **1.2 Run, confirm RED**

`make test SERVICE=pkg/model` — expect undefined identifiers.

- [ ] **1.3 Implement model additions**

`pkg/model/user.go` — replace file:

```go
package model

// UserRole is a platform-level role flag on the User record.
// Empty Roles reads as ["user"]; only positive marker is "admin".
type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

type User struct {
	ID          string     `json:"id"           bson:"_id"`
	Account     string     `json:"account"      bson:"account"`
	SiteID      string     `json:"siteId"       bson:"siteId"`
	SectID      string     `json:"sectId"       bson:"sectId"`
	SectName    string     `json:"sectName"     bson:"sectName"`
	SectTCName  string     `json:"sectTCName"   bson:"sectTCName"`
	DeptID      string     `json:"deptId"       bson:"deptId"`
	DeptName    string     `json:"deptName"     bson:"deptName"`
	DeptTCName  string     `json:"deptTCName"   bson:"deptTCName"`
	EngName     string     `json:"engName"      bson:"engName"`
	ChineseName string     `json:"chineseName"  bson:"chineseName"`
	EmployeeID  string     `json:"employeeId"   bson:"employeeId"`
	Roles       []UserRole `json:"roles"        bson:"roles"`
}
```

`pkg/model/room.go` — add `ExternalAccess` to the `Room` struct (immediately after `Restricted`):

```go
	Restricted     bool `json:"restricted,omitempty"     bson:"restricted,omitempty"`
	ExternalAccess bool `json:"externalAccess,omitempty" bson:"externalAccess,omitempty"`
```

Append the request types at the end of `room.go`:

```go
// RenameRoomRequest is the canonical event for renaming a channel room.
// Account and Timestamp are server-set by room-service before publishing.
type RenameRoomRequest struct {
	RoomID    string `json:"roomId"    bson:"roomId"`
	NewName   string `json:"newName"   bson:"newName"`
	Account   string `json:"account"   bson:"account"`
	Timestamp int64  `json:"timestamp" bson:"timestamp"`
}

// RoomVisibilityRequest is the canonical event for setting Restricted and
// ExternalAccess on a channel room. When Restricted=true and OwnerAccount is
// non-empty, that account becomes sole owner regardless of prior role.
// Account and Timestamp are server-set by room-service.
type RoomVisibilityRequest struct {
	RoomID         string `json:"roomId"                 bson:"roomId"`
	Restricted     bool   `json:"restricted"             bson:"restricted"`
	ExternalAccess bool   `json:"externalAccess"         bson:"externalAccess"`
	OwnerAccount   string `json:"ownerAccount,omitempty" bson:"ownerAccount,omitempty"`
	Account        string `json:"account"                bson:"account"`
	Timestamp      int64  `json:"timestamp"              bson:"timestamp"`
}
```

`pkg/model/subscription.go` — append after `Muted`:

```go
	// Denormalized from Room.Restricted / Room.ExternalAccess so a single
	// SubscriptionUpdateEvent carries every client-facing room field.
	// Readers MUST treat missing as false. Room remains the access-control
	// source of truth.
	Restricted     bool `json:"restricted,omitempty"     bson:"restricted,omitempty"`
	ExternalAccess bool `json:"externalAccess,omitempty" bson:"externalAccess,omitempty"`
```

`pkg/model/message.go` — append:

```go
// RoomRenamedSysData is the JSON payload stored in Message.SysMsgData
// for a room_renamed system message.
type RoomRenamedSysData struct {
	NewName   string `json:"newName"   bson:"newName"`
	ByAccount string `json:"byAccount" bson:"byAccount"`
}
```

`pkg/model/event.go` — three edits:

1. Extend the `MessageType*` const block (currently 4 entries):

```go
	// MessageTypeRoomRenamed is the system-message type emitted when a channel is renamed.
	MessageTypeRoomRenamed = "room_renamed"
```

2. Extend the `OutboxEventType` const block:

```go
	OutboxRoomRenamed           OutboxEventType = "room_renamed"
	OutboxRoomVisibilityChanged OutboxEventType = "room_visibility_changed"
```

3. Extend the `AsyncJobOp*` const block:

```go
	AsyncJobOpRoomRename     = "room.rename"
	AsyncJobOpRoomVisibility = "room.visibility"
```

4. Update `SubscriptionUpdateEvent.Action` doc-comment to include the new values:

```go
	Action string `json:"action"` // "added" | "removed" | "role_updated" | "mute_toggled" | "renamed" | "visibility_changed"
```

5. Append the outbox payload types at end of file:

```go
// RoomRenamedOutboxPayload is wrapped in OutboxEvent.Payload for OutboxRoomRenamed.
type RoomRenamedOutboxPayload struct {
	RoomID    string `json:"roomId"    bson:"roomId"`
	NewName   string `json:"newName"   bson:"newName"`
	Timestamp int64  `json:"timestamp" bson:"timestamp"`
}

// RoomVisibilityOutboxPayload is wrapped in OutboxEvent.Payload for
// OutboxRoomVisibilityChanged. When OwnerAccount is non-empty AND Restricted
// is true, the destination $cond promotes that account to sole owner.
type RoomVisibilityOutboxPayload struct {
	RoomID         string `json:"roomId"                 bson:"roomId"`
	Restricted     bool   `json:"restricted"             bson:"restricted"`
	ExternalAccess bool   `json:"externalAccess"         bson:"externalAccess"`
	OwnerAccount   string `json:"ownerAccount,omitempty" bson:"ownerAccount,omitempty"`
	Timestamp      int64  `json:"timestamp"              bson:"timestamp"`
}
```

- [ ] **1.4 Run, confirm GREEN**

`make test SERVICE=pkg/model` — all pass.

- [ ] **1.5 Commit**

```bash
git add pkg/model/
git commit -m "feat(model): add types for channel rename and visibility"
```

---

## Phase 2: Subject Builders

### Task 2: Add four subject builders

**Files:** `pkg/subject/subject.go`, `pkg/subject/subject_test.go`.

- [ ] **2.1 Append failing test cases** to the `TestSubjectBuilders` table in `pkg/subject/subject_test.go`:

```go
		{"RoomRename", subject.RoomRename("alice", "r1", "site-a"),
			"chat.user.alice.request.room.r1.site-a.room.rename"},
		{"RoomRenameWildcard", subject.RoomRenameWildcard("site-a"),
			"chat.user.*.request.room.*.site-a.room.rename"},
		{"RoomVisibility", subject.RoomVisibility("alice", "r1", "site-a"),
			"chat.user.alice.request.room.r1.site-a.room.visibility"},
		{"RoomVisibilityWildcard", subject.RoomVisibilityWildcard("site-a"),
			"chat.user.*.request.room.*.site-a.room.visibility"},
```

- [ ] **2.2 Run, confirm RED** — `make test SERVICE=pkg/subject`.

- [ ] **2.3 Add four builders** to `pkg/subject/subject.go` next to the existing `MemberRoleUpdate` / `MemberRoleUpdateWildcard`:

```go
// RoomRename is the request/reply subject for the rename RPC (owner or admin).
func RoomRename(account, roomID, siteID string) string {
	return fmt.Sprintf("chat.user.%s.request.room.%s.%s.room.rename", account, roomID, siteID)
}

// RoomVisibility is the request/reply subject for the visibility RPC (admin only).
func RoomVisibility(account, roomID, siteID string) string {
	return fmt.Sprintf("chat.user.%s.request.room.%s.%s.room.visibility", account, roomID, siteID)
}

// RoomRenameWildcard is the queue-subscribe pattern on a site.
func RoomRenameWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.*.%s.room.rename", siteID)
}

// RoomVisibilityWildcard is the queue-subscribe pattern on a site.
func RoomVisibilityWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.*.%s.room.visibility", siteID)
}
```

- [ ] **2.4 Run, confirm GREEN. Commit.**

```bash
git add pkg/subject/
git commit -m "feat(subject): add room.rename and room.visibility subject builders"
```

---

## Phase 3: room-worker store layer

### Task 3: Extend `SubscriptionStore` interface and regenerate mocks

**Files:** `room-worker/store.go`, `room-worker/mock_store_test.go` (auto).

- [ ] **3.1 Add four method signatures** to the `SubscriptionStore` interface (end of block):

```go
	// Rename/visibility operations.

	// UpdateRoomName sets {name, updatedAt} on the channel-typed room doc.
	// Returns ErrRoomNotFound or ErrNotChannelRoom; both are package sentinels in store.go.
	UpdateRoomName(ctx context.Context, roomID, newName string) error

	// UpdateRoomVisibility sets {restricted, externalAccess, updatedAt}; same
	// not-found / wrong-type semantics as UpdateRoomName.
	UpdateRoomVisibility(ctx context.Context, roomID string, restricted, externalAccess bool) error

	// UpdateSubscriptionNamesForRoom updateMany on subscriptions matching {roomId: roomID}.
	UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error

	// ApplySubscriptionVisibility updateMany matching {roomId: roomID}. Three branches:
	//   restricted=true && ownerAccount!="" → aggregation pipeline rewriting roles
	//     ($cond: u.account == ownerAccount → ["owner"] else ["member"]) and flipping flags.
	//   restricted=true && ownerAccount=="" → $set flags only.
	//   restricted=false → $set flags only (ownerAccount ignored).
	// All branches idempotent on retry.
	ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error
```

If `ErrRoomNotFound` and `ErrNotChannelRoom` don't exist in `room-worker/store.go`, add them at the top:

```go
var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrNotChannelRoom = errors.New("not a channel room")
)
```

- [ ] **3.2 Regenerate mocks** — `make generate SERVICE=room-worker`. The build will fail (`MongoStore does not implement SubscriptionStore`); that's Task 4.

- [ ] **3.3 Commit**

```bash
git add room-worker/store.go room-worker/mock_store_test.go
git commit -m "feat(room-worker): extend SubscriptionStore with rename/visibility methods"
```

---

### Task 4: Implement four store methods with integration tests

**Files:** `room-worker/store_mongo.go`, `room-worker/integration_test.go`.

- [ ] **4.1 Append integration tests** to `room-worker/integration_test.go`. Use the existing `testutil.MongoDB` harness already used elsewhere in the file:

```go
func TestMongoStore_UpdateRoomName(t *testing.T) {
	ctx := context.Background()
	db := testutil.MongoDB(t, "room-worker-rename")
	store := NewMongoStore(db)

	_, err := db.Collection("rooms").InsertOne(ctx, model.Room{
		ID: "r1", Name: "old", Type: model.RoomTypeChannel, SiteID: "site-a",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateRoomName(ctx, "r1", "new"))
	got, err := store.GetRoom(ctx, "r1")
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)

	assert.ErrorIs(t, store.UpdateRoomName(ctx, "missing", "x"), ErrRoomNotFound)

	_, err = db.Collection("rooms").InsertOne(ctx, model.Room{
		ID: "dm1", Type: model.RoomTypeDM, SiteID: "site-a",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.ErrorIs(t, store.UpdateRoomName(ctx, "dm1", "x"), ErrNotChannelRoom)
}

func TestMongoStore_UpdateRoomVisibility(t *testing.T) {
	ctx := context.Background()
	db := testutil.MongoDB(t, "room-worker-visibility")
	store := NewMongoStore(db)
	_, err := db.Collection("rooms").InsertOne(ctx, model.Room{
		ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	require.NoError(t, store.UpdateRoomVisibility(ctx, "r1", true, true))
	got, _ := store.GetRoom(ctx, "r1")
	assert.True(t, got.Restricted)
	assert.True(t, got.ExternalAccess)

	// Idempotent + flip
	require.NoError(t, store.UpdateRoomVisibility(ctx, "r1", true, true))
	require.NoError(t, store.UpdateRoomVisibility(ctx, "r1", false, false))
	got, _ = store.GetRoom(ctx, "r1")
	assert.False(t, got.Restricted)
	assert.False(t, got.ExternalAccess)
}

func TestMongoStore_UpdateSubscriptionNamesForRoom(t *testing.T) {
	ctx := context.Background()
	db := testutil.MongoDB(t, "room-worker-sub-name")
	store := NewMongoStore(db)

	_, err := db.Collection("subscriptions").InsertMany(ctx, []any{
		newSubFixture("s1", "u1", "alice", "r1", "old"),
		newSubFixture("s2", "u2", "bob", "r1", "old"),
		newSubFixture("s3", "u3", "carol", "other", "untouched"),
	})
	require.NoError(t, err)

	require.NoError(t, store.UpdateSubscriptionNamesForRoom(ctx, "r1", "new"))
	all, _ := store.ListByRoom(ctx, "r1")
	for _, sub := range all {
		assert.Equal(t, "new", sub.Name, "sub %s", sub.ID)
	}
	other, _ := store.ListByRoom(ctx, "other")
	require.Len(t, other, 1)
	assert.Equal(t, "untouched", other[0].Name)
}

func TestMongoStore_ApplySubscriptionVisibility(t *testing.T) {
	seed := func(t *testing.T, db *mongo.Database) {
		t.Helper()
		_, err := db.Collection("subscriptions").InsertMany(context.Background(), []any{
			newSubFixtureWithRoles("s1", "u1", "alice", "r1", []model.Role{model.RoleOwner}),
			newSubFixtureWithRoles("s2", "u2", "bob", "r1", []model.Role{model.RoleMember}),
			newSubFixtureWithRoles("s3", "u3", "carol", "r1", []model.Role{model.RoleMember}),
		})
		require.NoError(t, err)
	}
	rolesByAccount := func(t *testing.T, store *MongoStore) map[string][]model.Role {
		t.Helper()
		subs, _ := store.ListByRoom(context.Background(), "r1")
		out := map[string][]model.Role{}
		for _, sub := range subs {
			out[sub.User.Account] = sub.Roles
		}
		return out
	}

	t.Run("restrict transition with ownerAccount rewrites roles", func(t *testing.T) {
		db := testutil.MongoDB(t, "room-worker-visibility-restrict")
		store := NewMongoStore(db)
		seed(t, db)
		require.NoError(t, store.ApplySubscriptionVisibility(context.Background(), "r1", true, false, "bob"))
		roles := rolesByAccount(t, store)
		assert.Equal(t, []model.Role{model.RoleOwner}, roles["bob"])
		assert.Equal(t, []model.Role{model.RoleMember}, roles["alice"])
		assert.Equal(t, []model.Role{model.RoleMember}, roles["carol"])
		// Idempotent
		require.NoError(t, store.ApplySubscriptionVisibility(context.Background(), "r1", true, false, "bob"))
		assert.Equal(t, roles, rolesByAccount(t, store))
	})

	t.Run("flags only when ownerAccount empty (roles untouched)", func(t *testing.T) {
		db := testutil.MongoDB(t, "room-worker-visibility-flags")
		store := NewMongoStore(db)
		seed(t, db)
		require.NoError(t, store.ApplySubscriptionVisibility(context.Background(), "r1", true, true, ""))
		roles := rolesByAccount(t, store)
		assert.Equal(t, []model.Role{model.RoleOwner}, roles["alice"])
	})

	t.Run("unrestrict ignores ownerAccount", func(t *testing.T) {
		db := testutil.MongoDB(t, "room-worker-visibility-unrestrict")
		store := NewMongoStore(db)
		seed(t, db)
		require.NoError(t, store.ApplySubscriptionVisibility(context.Background(), "r1", false, false, "bob"))
		roles := rolesByAccount(t, store)
		assert.Equal(t, []model.Role{model.RoleOwner}, roles["alice"])
	})
}
```

Helper fixtures (add once in the integration test file if not present):

```go
func newSubFixture(id, userID, account, roomID, name string) model.Subscription {
	return newSubFixtureWithRoles(id, userID, account, roomID, []model.Role{model.RoleMember})
}
func newSubFixtureWithRoles(id, userID, account, roomID string, roles []model.Role) model.Subscription {
	return model.Subscription{
		ID:       id,
		User:     model.SubscriptionUser{ID: userID, Account: account},
		RoomID:   roomID,
		SiteID:   "site-a",
		Name:     "n",
		Roles:    roles,
		RoomType: model.RoomTypeChannel,
		JoinedAt: time.Now().UTC(),
	}
}
```

- [ ] **4.2 Run, confirm RED** — `make test-integration SERVICE=room-worker`.

- [ ] **4.3 Implement the four methods** in `room-worker/store_mongo.go`.

`UpdateRoomName` and `UpdateRoomVisibility` share a probe pattern for not-found vs wrong-type:

```go
func (s *MongoStore) UpdateRoomName(ctx context.Context, roomID, newName string) error {
	return s.updateChannelRoom(ctx, roomID, bson.M{
		"$set": bson.M{"name": newName, "updatedAt": time.Now().UTC()},
	})
}

func (s *MongoStore) UpdateRoomVisibility(ctx context.Context, roomID string, restricted, externalAccess bool) error {
	return s.updateChannelRoom(ctx, roomID, bson.M{
		"$set": bson.M{
			"restricted":     restricted,
			"externalAccess": externalAccess,
			"updatedAt":      time.Now().UTC(),
		},
	})
}

// updateChannelRoom enforces type=channel and disambiguates not-found vs wrong-type.
func (s *MongoStore) updateChannelRoom(ctx context.Context, roomID string, update bson.M) error {
	res, err := s.rooms.UpdateOne(ctx,
		bson.M{"_id": roomID, "type": model.RoomTypeChannel}, update)
	if err != nil {
		return fmt.Errorf("update channel room %s: %w", roomID, err)
	}
	if res.MatchedCount > 0 {
		return nil
	}
	var probe model.Room
	if probeErr := s.rooms.FindOne(ctx, bson.M{"_id": roomID}).Decode(&probe); probeErr != nil {
		if errors.Is(probeErr, mongo.ErrNoDocuments) {
			return ErrRoomNotFound
		}
		return fmt.Errorf("probe room type: %w", probeErr)
	}
	return ErrNotChannelRoom
}

func (s *MongoStore) UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error {
	if _, err := s.subscriptions.UpdateMany(ctx,
		bson.M{"roomId": roomID},
		bson.M{"$set": bson.M{"name": newName}}); err != nil {
		return fmt.Errorf("update subscription names for room %s: %w", roomID, err)
	}
	return nil
}

func (s *MongoStore) ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error {
	filter := bson.M{"roomId": roomID}

	if restricted && ownerAccount != "" {
		pipeline := mongo.Pipeline{
			bson.D{{Key: "$set", Value: bson.M{
				"restricted":     true,
				"externalAccess": externalAccess,
				"roles": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$u.account", ownerAccount}},
					"then": bson.A{string(model.RoleOwner)},
					"else": bson.A{string(model.RoleMember)},
				}},
			}}},
		}
		if _, err := s.subscriptions.UpdateMany(ctx, filter, pipeline); err != nil {
			return fmt.Errorf("apply visibility (restrict+rewrite): %w", err)
		}
		return nil
	}

	if _, err := s.subscriptions.UpdateMany(ctx, filter, bson.M{
		"$set": bson.M{"restricted": restricted, "externalAccess": externalAccess},
	}); err != nil {
		return fmt.Errorf("apply visibility (flags only): %w", err)
	}
	return nil
}
```

(Use whatever field names the existing `MongoStore` uses for `rooms`/`subscriptions` collections — match the file's existing style.)

- [ ] **4.4 Run, confirm GREEN. Commit.**

```bash
git add room-worker/store_mongo.go room-worker/integration_test.go
git commit -m "feat(room-worker): implement rename/visibility store methods"
```

---

## Phase 4: inbox-worker store layer

### Task 5: Extend `InboxStore` interface and regenerate mocks

**Files:** `inbox-worker/handler.go`, `inbox-worker/mock_store_test.go` (auto).

> **Layout note:** the inbox-worker's Mongo store implementation lives in `main.go` as `mongoInboxStore`. Implementation in Task 6 will add methods to that struct in `main.go`.

- [ ] **5.1 Append to `InboxStore`** in `inbox-worker/handler.go`:

```go
	// UpdateSubscriptionNamesForRoom mass-renames subscription mirrors on this site.
	UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error

	// ApplySubscriptionVisibility mirrors room-worker's counterpart (same 3 branches).
	// On a remote site this only updates mirrored subs whose users are homed here;
	// OwnerAccount is load-bearing so $cond can promote the chosen owner's local mirror.
	ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error
```

- [ ] **5.2 Regenerate and commit**

```bash
make generate SERVICE=inbox-worker
git add inbox-worker/handler.go inbox-worker/mock_store_test.go
git commit -m "feat(inbox-worker): extend InboxStore with rename/visibility methods"
```

---

### Task 6: Implement two `mongoInboxStore` methods with integration tests

**Files:** `inbox-worker/main.go`, `inbox-worker/integration_test.go`.

- [ ] **6.1 Append integration tests** to `inbox-worker/integration_test.go` — mirror the room-worker shape from Task 4.1 but call methods on `mongoInboxStore`. Same `newSubFixture` helpers (re-define locally, or pull into a shared `_test.go`). Cover:

- `UpdateSubscriptionNamesForRoom` — updates matching subs, leaves others untouched.
- `ApplySubscriptionVisibility` — all 3 branches (restrict-with-owner, flags-only, unrestrict).

(Implementation parallels Task 4.1; use the same assertion patterns.)

- [ ] **6.2 Run, confirm RED.**

- [ ] **6.3 Implement** by appending to `inbox-worker/main.go`:

```go
func (s *mongoInboxStore) UpdateSubscriptionNamesForRoom(ctx context.Context, roomID, newName string) error {
	if _, err := s.subs.UpdateMany(ctx,
		bson.M{"roomId": roomID},
		bson.M{"$set": bson.M{"name": newName}}); err != nil {
		return fmt.Errorf("update subscription names for room %s: %w", roomID, err)
	}
	return nil
}

func (s *mongoInboxStore) ApplySubscriptionVisibility(ctx context.Context, roomID string, restricted, externalAccess bool, ownerAccount string) error {
	filter := bson.M{"roomId": roomID}
	if restricted && ownerAccount != "" {
		pipeline := mongo.Pipeline{
			bson.D{{Key: "$set", Value: bson.M{
				"restricted":     true,
				"externalAccess": externalAccess,
				"roles": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$u.account", ownerAccount}},
					"then": bson.A{string(model.RoleOwner)},
					"else": bson.A{string(model.RoleMember)},
				}},
			}}},
		}
		if _, err := s.subs.UpdateMany(ctx, filter, pipeline); err != nil {
			return fmt.Errorf("apply visibility (restrict+rewrite): %w", err)
		}
		return nil
	}
	if _, err := s.subs.UpdateMany(ctx, filter, bson.M{
		"$set": bson.M{"restricted": restricted, "externalAccess": externalAccess},
	}); err != nil {
		return fmt.Errorf("apply visibility (flags only): %w", err)
	}
	return nil
}
```

(If the subscription-collection field is `s.subscriptions` instead of `s.subs`, adjust.)

- [ ] **6.4 Run, confirm GREEN. Commit.**

```bash
git add inbox-worker/main.go inbox-worker/integration_test.go
git commit -m "feat(inbox-worker): implement rename/visibility store methods"
```

---

## Phase 5: inbox-worker permanent-error refactor

### Task 7: Port `errPermanent` and Ack-on-permanent dispatch

**Files:** `inbox-worker/handler.go`, `inbox-worker/main.go`, `inbox-worker/handler_test.go`, `inbox-worker/main_test.go`.

- [ ] **7.1 Write failing tests** for both pieces.

`inbox-worker/handler_test.go`:

```go
func TestNewPermanent_WrapsErrPermanent(t *testing.T) {
	err := newPermanent("bad payload: %s", "boom")
	assert.True(t, errors.Is(err, errPermanent))
	assert.Equal(t, "bad payload: boom", err.Error())
}

func TestPermanentError_NestedIs(t *testing.T) {
	err := fmt.Errorf("outer: %w", newPermanent("a"))
	assert.True(t, errors.Is(err, errPermanent))
}
```

`inbox-worker/main_test.go`:

```go
func TestDispatchAckPolicy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ackAction
	}{
		{"nil → ack", nil, ackActionAck},
		{"permanent → ack", newPermanent("bad"), ackActionAck},
		{"wrapped permanent → ack", fmt.Errorf("ctx: %w", newPermanent("bad")), ackActionAck},
		{"transient → nak", errors.New("mongo timeout"), ackActionNak},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dispatchAckPolicy(tt.err))
		})
	}
}
```

- [ ] **7.2 Run, confirm RED.**

- [ ] **7.3 Add the sentinel + type + constructor + methods** to `inbox-worker/handler.go` (top, after imports). Add `errors` and `fmt` to imports.

```go
// errPermanent marks non-retryable errors (caller Acks instead of Nak).
var errPermanent = errors.New("permanent")

type permanentError struct {
	msg   string
	cause error
}

func newPermanent(format string, args ...any) error {
	return &permanentError{msg: fmt.Sprintf(format, args...)}
}

func (e *permanentError) Error() string { return e.msg }
func (e *permanentError) Unwrap() error { return e.cause }
func (e *permanentError) Is(target error) bool {
	if target == errPermanent {
		return true
	}
	_, ok := target.(*permanentError)
	return ok
}
```

Add the dispatch helper + replace the `cons.Consume` callback in `inbox-worker/main.go`. Place above `main`:

```go
type ackAction int

const (
	ackActionNak ackAction = iota
	ackActionAck
)

// dispatchAckPolicy decides Ack (poison; stop redelivering) vs Nak (transient; retry).
func dispatchAckPolicy(err error) ackAction {
	if err == nil || errors.Is(err, errPermanent) {
		return ackActionAck
	}
	return ackActionNak
}
```

Inside the `cons.Consume` callback (currently at lines ~298-308), replace the inner handle-and-Nak with:

```go
handlerErr := handler.HandleEvent(handlerCtx, m.Data())
if handlerErr != nil {
	slog.Error("handle event failed",
		"error", handlerErr,
		"request_id", natsutil.RequestIDFromContext(handlerCtx))
}
switch dispatchAckPolicy(handlerErr) {
case ackActionAck:
	if err := m.Ack(); err != nil {
		slog.Error("failed to ack message", "error", err)
	}
case ackActionNak:
	if err := m.Nak(); err != nil {
		slog.Error("failed to nak message", "error", err)
	}
}
```

- [ ] **7.4 Run, confirm GREEN. Commit.**

```bash
git add inbox-worker/handler.go inbox-worker/main.go inbox-worker/handler_test.go inbox-worker/main_test.go
git commit -m "refactor(inbox-worker): port errPermanent and ack-on-permanent dispatch"
```

---

## Phase 6: room-service

### Task 8: Helpers, sentinels, sanitizeError, config

**Files:** `room-service/helper.go`, `room-service/helper_test.go`, `room-service/handler.go`, `room-service/main.go`.

- [ ] **8.1 Write failing tests** in `room-service/helper_test.go`:

```go
func TestIsPlatformAdmin(t *testing.T) {
	tests := []struct {
		name string
		user *model.User
		want bool
	}{
		{"nil", nil, false},
		{"empty roles", &model.User{Account: "alice"}, false},
		{"user only", &model.User{Account: "a", Roles: []model.UserRole{model.UserRoleUser}}, false},
		{"admin", &model.User{Account: "a", Roles: []model.UserRole{model.UserRoleAdmin}}, true},
		{"mixed", &model.User{Account: "a", Roles: []model.UserRole{model.UserRoleUser, model.UserRoleAdmin}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, isPlatformAdmin(tt.user)) })
	}
}

func TestSanitizeError_NewSentinels(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errOnlyOwnersOrAdmins, "only owners or admins can rename a channel"},
		{errOnlyAdmins, "only admins can change room visibility"},
		{errOwnerNotMember, "owner account is not a member of this room"},
		{errOwnerAccountRequired, "owner account is required when restricting a room"},
		{errNotEnoughMembers, "not enough members to restrict"},
		{errInvalidName, "invalid name"},
		{errRenameChannelOnly, "rename is only allowed in channel rooms"},
		{errVisibilityChannelOnly, "visibility change is only allowed in channel rooms"},
		{errRoomNotFound, "room not found"},
	}
	for _, tt := range cases {
		t.Run(tt.err.Error(), func(t *testing.T) { assert.Equal(t, tt.want, sanitizeError(tt.err)) })
	}
}
```

- [ ] **8.2 Run, confirm RED.**

- [ ] **8.3 Implement** in `room-service/helper.go`. Append to the `var (...)` sentinel block:

```go
	errOnlyOwnersOrAdmins    = errors.New("only owners or admins can rename a channel")
	errOnlyAdmins            = errors.New("only admins can change room visibility")
	errOwnerNotMember        = errors.New("owner account is not a member of this room")
	errOwnerAccountRequired  = errors.New("owner account is required when restricting a room")
	errNotEnoughMembers      = errors.New("not enough members to restrict")
	errInvalidName           = errors.New("invalid name")
	errRenameChannelOnly     = errors.New("rename is only allowed in channel rooms")
	errVisibilityChannelOnly = errors.New("visibility change is only allowed in channel rooms")
	errRoomNotFound          = errors.New("room not found")
```

Add the helper after `dedup`:

```go
// isPlatformAdmin returns true when u has the UserRoleAdmin role. Nil-safe.
func isPlatformAdmin(u *model.User) bool {
	if u == nil {
		return false
	}
	for _, r := range u.Roles {
		if r == model.UserRoleAdmin {
			return true
		}
	}
	return false
}
```

Extend `sanitizeError`'s sentinel list (the big `errors.Is(...)` switch case) — add each new sentinel:

```go
		errors.Is(err, errOnlyOwnersOrAdmins),
		errors.Is(err, errOnlyAdmins),
		errors.Is(err, errOwnerNotMember),
		errors.Is(err, errOwnerAccountRequired),
		errors.Is(err, errNotEnoughMembers),
		errors.Is(err, errInvalidName),
		errors.Is(err, errRenameChannelOnly),
		errors.Is(err, errVisibilityChannelOnly),
		errors.Is(err, errRoomNotFound),
```

Add `RestrictedRoomMinMembers` to the config struct in `room-service/main.go`:

```go
RestrictedRoomMinMembers int `env:"RESTRICTED_ROOM_MIN_MEMBERS" envDefault:"5"`
```

In `room-service/handler.go`, add a field on `Handler`:

```go
restrictedRoomMinMembers int
```

Extend `NewHandler` to accept and assign it. Update the call site in `main.go` to pass it.

- [ ] **8.4 Run, confirm GREEN. Commit.**

```bash
git add room-service/
git commit -m "feat(room-service): add isPlatformAdmin, visibility sentinels, RESTRICTED_ROOM_MIN_MEMBERS"
```

---

### Task 9: Implement `handleRoomRename` and `handleRoomVisibility` with registration

**Files:** `room-service/handler.go`, `room-service/handler_test.go`.

- [ ] **9.1 Write failing table-driven tests** in `room-service/handler_test.go`. Use existing mocks (`MockRoomStore` from `mock_store_test.go`); seed via `gomock.EXPECT()`. Two parallel tables — one per handler.

Rename validation cases:

- invalid subject → `errInvalidRenameSubject`
- blank name (after trim) → `errInvalidName`
- name > 100 chars → `errInvalidName`
- room not found (GetRoom → `mongo.ErrNoDocuments`) → `errRoomNotFound`
- wrong room type → `errRenameChannelOnly`
- non-admin + no owner subscription → `errOnlyOwnersOrAdmins`
- owner (subscription) → success
- admin without subscription → success (no `GetSubscription` call)
- error-then-ok request ID guards already covered by `errMissingRequestID` / `errInvalidRequestID` paths in the existing handler base patterns; replicate one case each.

Sample table entry:

```go
{
	name: "admin allowed without subscription",
	subj: subject.RoomRename("admin1", "r1", "site-a"),
	body: mustJSON(t, model.RenameRoomRequest{RoomID: "r1", NewName: "new"}),
	setupStore: func(s *MockRoomStore) {
		s.EXPECT().GetUser(gomock.Any(), "admin1").Return(&model.User{Account: "admin1", Roles: []model.UserRole{model.UserRoleAdmin}}, nil)
		s.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a"}, nil)
	},
	wantErr: nil,
},
```

Visibility validation cases:

- invalid subject → `errInvalidVisibilitySubject`
- non-admin requester → `errOnlyAdmins`
- room not found → `errRoomNotFound`
- non-channel room → `errVisibilityChannelOnly`
- restricted=true + ownerAccount given + owner not a member → `errOwnerNotMember`
- transition false→true without ownerAccount → `errOwnerAccountRequired`
- transition with `UserCount < 5` → `errNotEnoughMembers`
- transition success (admin + ownerAccount + UserCount=5) → success
- unrestrict (no owner/threshold checks) → success
- already-restricted owner change → success

Set `restrictedRoomMinMembers=5` on the test Handler.

- [ ] **9.2 Run, confirm RED.**

- [ ] **9.3 Implement** in `room-service/handler.go`:

```go
var (
	errInvalidRenameSubject     = errors.New("invalid rename subject")
	errInvalidVisibilitySubject = errors.New("invalid visibility subject")
)

func (h *Handler) natsRoomRename(m otelnats.Msg) {
	ctx := wrappedCtx(m)
	resp, err := h.handleRoomRename(ctx, m.Msg.Subject, m.Msg.Data)
	if err != nil {
		slog.Error("rename failed", "error", err)
		natsutil.ReplyError(m.Msg, sanitizeError(err))
		return
	}
	if err := m.Msg.Respond(resp); err != nil {
		slog.Error("respond rename failed", "error", err)
	}
}

func (h *Handler) handleRoomRename(ctx context.Context, subj string, data []byte) ([]byte, error) {
	account, roomID, ok := subject.ParseUserRoomSubject(subj)
	if !ok {
		return nil, fmt.Errorf("%w: %s", errInvalidRenameSubject, subj)
	}
	requestID := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return nil, errMissingRequestID
	}
	if !idgen.IsValidUUID(requestID) {
		return nil, errInvalidRequestID
	}

	var req model.RenameRoomRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if req.RoomID != "" && req.RoomID != roomID {
		return nil, fmt.Errorf("invalid request: room ID mismatch")
	}
	if req.Account != "" && req.Account != account {
		return nil, fmt.Errorf("invalid request: account mismatch")
	}
	req.RoomID, req.Account = roomID, account

	name := strings.TrimSpace(req.NewName)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return nil, errInvalidName
	}
	req.NewName = name

	requesterUser, err := h.store.GetUser(ctx, account)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return nil, fmt.Errorf("get user: %w", err)
	}

	room, err := h.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errRoomNotFound
		}
		return nil, fmt.Errorf("get room: %w", err)
	}
	if room.Type != model.RoomTypeChannel {
		return nil, errRenameChannelOnly
	}

	if !isPlatformAdmin(requesterUser) {
		sub, subErr := h.store.GetSubscription(ctx, account, roomID)
		if subErr != nil || !hasRole(sub.Roles, model.RoleOwner) {
			return nil, errOnlyOwnersOrAdmins
		}
	}

	req.Timestamp = time.Now().UTC().UnixMilli()
	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal rename request: %w", err)
	}
	if err := h.publishToStream(ctx, subject.RoomCanonical(h.siteID, "room.rename"), out); err != nil {
		return nil, fmt.Errorf("publish to stream: %w", err)
	}
	return json.Marshal(map[string]string{"status": "accepted", "requestId": requestID})
}

func (h *Handler) natsRoomVisibility(m otelnats.Msg) {
	ctx := wrappedCtx(m)
	resp, err := h.handleRoomVisibility(ctx, m.Msg.Subject, m.Msg.Data)
	if err != nil {
		slog.Error("visibility failed", "error", err)
		natsutil.ReplyError(m.Msg, sanitizeError(err))
		return
	}
	if err := m.Msg.Respond(resp); err != nil {
		slog.Error("respond visibility failed", "error", err)
	}
}

func (h *Handler) handleRoomVisibility(ctx context.Context, subj string, data []byte) ([]byte, error) {
	account, roomID, ok := subject.ParseUserRoomSubject(subj)
	if !ok {
		return nil, fmt.Errorf("%w: %s", errInvalidVisibilitySubject, subj)
	}
	requestID := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return nil, errMissingRequestID
	}
	if !idgen.IsValidUUID(requestID) {
		return nil, errInvalidRequestID
	}

	var req model.RoomVisibilityRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if req.RoomID != "" && req.RoomID != roomID {
		return nil, fmt.Errorf("invalid request: room ID mismatch")
	}
	if req.Account != "" && req.Account != account {
		return nil, fmt.Errorf("invalid request: account mismatch")
	}
	req.RoomID, req.Account = roomID, account

	requesterUser, err := h.store.GetUser(ctx, account)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if !isPlatformAdmin(requesterUser) {
		return nil, errOnlyAdmins
	}

	room, err := h.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errRoomNotFound
		}
		return nil, fmt.Errorf("get room: %w", err)
	}
	if room.Type != model.RoomTypeChannel {
		return nil, errVisibilityChannelOnly
	}

	isTransition := req.Restricted && !room.Restricted

	if req.Restricted && req.OwnerAccount != "" {
		if _, subErr := h.store.GetSubscription(ctx, req.OwnerAccount, roomID); subErr != nil {
			if errors.Is(subErr, mongo.ErrNoDocuments) || errors.Is(subErr, model.ErrSubscriptionNotFound) {
				return nil, errOwnerNotMember
			}
			return nil, fmt.Errorf("get owner subscription: %w", subErr)
		}
	}
	if isTransition {
		if req.OwnerAccount == "" {
			return nil, errOwnerAccountRequired
		}
		if room.UserCount < h.restrictedRoomMinMembers {
			return nil, fmt.Errorf("%w (need at least %d)", errNotEnoughMembers, h.restrictedRoomMinMembers)
		}
	}

	req.Timestamp = time.Now().UTC().UnixMilli()
	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal visibility request: %w", err)
	}
	if err := h.publishToStream(ctx, subject.RoomCanonical(h.siteID, "room.visibility"), out); err != nil {
		return nil, fmt.Errorf("publish to stream: %w", err)
	}
	return json.Marshal(map[string]string{"status": "accepted", "requestId": requestID})
}
```

Add to `RegisterCRUD` (immediately before the closing `return nil`):

```go
	if _, err := nc.QueueSubscribe(subject.RoomRenameWildcard(h.siteID), queue, h.natsRoomRename); err != nil {
		return fmt.Errorf("subscribe room rename: %w", err)
	}
	if _, err := nc.QueueSubscribe(subject.RoomVisibilityWildcard(h.siteID), queue, h.natsRoomVisibility); err != nil {
		return fmt.Errorf("subscribe room visibility: %w", err)
	}
```

- [ ] **9.4 Run, confirm GREEN. Commit.**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "feat(room-service): add room.rename and room.visibility handlers"
```

---

### Task 10: room-service integration test

**Files:** `room-service/integration_test.go`.

- [ ] **10.1 Add end-to-end NATS tests** modeled on the existing role-update integration test (find `TestIntegration_*` for `MemberRoleUpdate` in this file).

For each RPC:
- Send a NATS request with `X-Request-ID` header.
- Assert reply body = `{"status":"accepted","requestId":"<uuid>"}`.
- Assert a JetStream message landed on `chat.room.canonical.<siteID>.room.{rename|visibility}` with `Account`/`Timestamp` populated.

Include one negative case per RPC (e.g., non-admin requesting visibility → reply contains sanitized error).

- [ ] **10.2 Run `make test-integration SERVICE=room-service`, confirm GREEN. Commit.**

```bash
git add room-service/integration_test.go
git commit -m "test(room-service): integration tests for room.rename and room.visibility"
```

---

## Phase 7: room-worker processors

### Task 11: Add `findRemoteSitesForAccounts` and `publishSubscriptionEvents` helpers

**Files:** `room-worker/handler.go`, `room-worker/handler_test.go`.

- [ ] **11.1 Write failing unit tests** in `room-worker/handler_test.go`:

```go
func TestFindRemoteSitesForAccounts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockSubscriptionStore(ctrl)

	t.Run("dedupes remote, drops local, preserves siteIDs", func(t *testing.T) {
		store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"alice", "bob", "carol", "dave"}).Return([]model.User{
			{Account: "alice", SiteID: "site-a"}, // local
			{Account: "bob", SiteID: "site-b"},   // remote
			{Account: "carol", SiteID: "site-c"}, // remote
			{Account: "dave", SiteID: "site-b"},  // dup of bob's site
		}, nil)
		h := &Handler{store: store, siteID: "site-a"}
		got, err := h.findRemoteSitesForAccounts(context.Background(), []string{"alice", "bob", "carol", "dave"})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"site-b", "site-c"}, got)
	})

	t.Run("empty input returns empty slice", func(t *testing.T) {
		h := &Handler{store: store, siteID: "site-a"}
		got, err := h.findRemoteSitesForAccounts(context.Background(), nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestPublishSubscriptionEvents(t *testing.T) {
	var got []model.SubscriptionUpdateEvent
	publish := func(_ context.Context, _ string, data []byte, _ string) error {
		var evt model.SubscriptionUpdateEvent
		require.NoError(t, json.Unmarshal(data, &evt))
		got = append(got, evt)
		return nil
	}
	h := &Handler{siteID: "site-a", publish: publish}
	subs := []model.Subscription{
		{User: model.SubscriptionUser{ID: "u1", Account: "alice"}, RoomID: "r1"},
		{User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: "r1"},
	}
	h.publishSubscriptionEvents(context.Background(), subs, "renamed")
	require.Len(t, got, 2)
	assert.Equal(t, "renamed", got[0].Action)
	assert.Equal(t, "alice", got[0].Subscription.User.Account)
}
```

- [ ] **11.2 Run, confirm RED.**

- [ ] **11.3 Implement** in `room-worker/handler.go` (near `messageDedupSeed` and `publishSubscriptionUpdates`):

```go
// findRemoteSitesForAccounts looks up the home site of each account and returns
// the deduplicated set of remote sites (siteID != h.siteID). Empty in → empty out.
func (h *Handler) findRemoteSitesForAccounts(ctx context.Context, accounts []string) ([]string, error) {
	if len(accounts) == 0 {
		return []string{}, nil
	}
	users, err := h.store.FindUsersByAccounts(ctx, accounts)
	if err != nil {
		return nil, fmt.Errorf("find users by accounts: %w", err)
	}
	seen := make(map[string]struct{}, len(users))
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u.SiteID == h.siteID {
			continue
		}
		if _, dup := seen[u.SiteID]; dup {
			continue
		}
		seen[u.SiteID] = struct{}{}
		out = append(out, u.SiteID)
	}
	return out, nil
}

// publishSubscriptionEvents fans out one SubscriptionUpdateEvent per subscription.
// Best-effort: errors are logged, not returned.
func (h *Handler) publishSubscriptionEvents(ctx context.Context, subs []model.Subscription, action string) {
	now := time.Now().UTC().UnixMilli()
	for i := range subs {
		evt := model.SubscriptionUpdateEvent{
			UserID:       subs[i].User.ID,
			Subscription: subs[i],
			Action:       action,
			Timestamp:    now,
		}
		data, err := json.Marshal(evt)
		if err != nil {
			slog.Error("marshal subscription update", "error", err, "account", subs[i].User.Account, "action", action)
			continue
		}
		if err := h.publish(ctx, subject.SubscriptionUpdate(subs[i].User.Account), data, ""); err != nil {
			slog.Error("publish subscription update", "error", err, "account", subs[i].User.Account, "action", action)
		}
	}
}
```

- [ ] **11.4 Retrofit `processAddMembers`** (around lines 1117-1124). Replace the inline `accounts/users/remoteSites map` block with:

```go
	remoteSites, err := h.findRemoteSitesForAccounts(ctx, accounts)
	if err != nil {
		return fmt.Errorf("find remote sites for outbox fan-out: %w", err)
	}
```

Then change `for destSiteID := range remoteSites {` → `for _, destSiteID := range remoteSites {`.

(Skip `processRemoveOrg` — its loop body iterates `accounts` per site, which the helper doesn't return. Document as future cleanup in commit message.)

- [ ] **11.5 Run, confirm GREEN. Commit.**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): add findRemoteSitesForAccounts + publishSubscriptionEvents helpers"
```

---

### Task 12: Implement `processRoomRename`

**Files:** `room-worker/handler.go`, `room-worker/handler_test.go`.

- [ ] **12.1 Write failing unit tests.** Cover:

1. Missing X-Request-ID → permanent, no store calls.
2. Invalid UUID → permanent.
3. Unmarshal failure → permanent + AsyncJobResult does NOT publish (empty `requesterAccount`).
4. `UpdateRoomName` returns `ErrRoomNotFound` → permanent + AsyncJobResult error.
5. `UpdateRoomName` returns `ErrNotChannelRoom` → permanent + AsyncJobResult error.
6. Transient error on `UpdateSubscriptionNamesForRoom` → non-permanent error returned.
7. Happy path no remote sites: store calls succeed, sys message published with deterministic Nats-Msg-Id, two SubscriptionUpdateEvent publishes, no outbox, AsyncJobResult ok.
8. Happy path with one remote site: same as 7 plus one outbox publish carrying `RoomRenamedOutboxPayload`.
9. **Error-then-ok retry sequence** (spec §Processing requirement): two sequential calls, first transient-error then success → asserts two AsyncJobResults with `Status:"error"` then `Status:"ok"`.

Template for case 7 (model the others on this):

```go
func TestProcessRoomRename_HappyPathNoRemoteSites(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockSubscriptionStore(ctrl)

	const roomID, newName = "r1", "renamed"
	requestID := "01970a4f-8c2d-7c9a-abcd-e0123456789f"

	subs := []model.Subscription{
		{ID: "s1", User: model.SubscriptionUser{ID: "u1", Account: "alice"}, RoomID: roomID},
		{ID: "s2", User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: roomID},
	}

	store.EXPECT().UpdateRoomName(gomock.Any(), roomID, newName).Return(nil)
	store.EXPECT().UpdateSubscriptionNamesForRoom(gomock.Any(), roomID, newName).Return(nil)
	store.EXPECT().ListByRoom(gomock.Any(), roomID).Return(subs, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return([]model.User{
		{Account: "alice", SiteID: "site-a"}, {Account: "bob", SiteID: "site-a"},
	}, nil)

	var publishedSubjects []string
	publish := func(_ context.Context, subj string, _ []byte, _ string) error {
		publishedSubjects = append(publishedSubjects, subj)
		return nil
	}

	h := &Handler{store: store, siteID: "site-a", publish: publish}
	ctx := natsutil.ContextWithRequestID(context.Background(), requestID)
	body, _ := json.Marshal(model.RenameRoomRequest{
		RoomID: roomID, NewName: newName, Account: "alice", Timestamp: time.Now().UTC().UnixMilli(),
	})

	require.NoError(t, h.processRoomRename(ctx, body))

	assert.Contains(t, publishedSubjects, subject.MsgCanonicalCreated("site-a"))
	assert.Contains(t, publishedSubjects, subject.SubscriptionUpdate("alice"))
	assert.Contains(t, publishedSubjects, subject.SubscriptionUpdate("bob"))
	assert.Contains(t, publishedSubjects, subject.UserResponse("alice", requestID))
	for _, subj := range publishedSubjects {
		assert.NotContains(t, subj, "outbox.")
	}
}
```

Retry sequence template:

```go
func TestProcessRoomRename_ErrorThenOkRetrySequence(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockSubscriptionStore(ctrl)
	requestID := "01970a4f-8c2d-7c9a-abcd-e0123456789f"

	store.EXPECT().UpdateRoomName(gomock.Any(), "r1", "x").Return(errors.New("mongo timeout"))
	store.EXPECT().UpdateRoomName(gomock.Any(), "r1", "x").Return(nil)
	store.EXPECT().UpdateSubscriptionNamesForRoom(gomock.Any(), "r1", "x").Return(nil)
	store.EXPECT().ListByRoom(gomock.Any(), "r1").Return([]model.Subscription{}, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return([]model.User{}, nil)

	var asyncResults []model.AsyncJobResult
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		if subj == subject.UserResponse("alice", requestID) {
			var r model.AsyncJobResult
			require.NoError(t, json.Unmarshal(data, &r))
			asyncResults = append(asyncResults, r)
		}
		return nil
	}

	h := &Handler{store: store, siteID: "site-a", publish: publish}
	ctx := natsutil.ContextWithRequestID(context.Background(), requestID)
	body, _ := json.Marshal(model.RenameRoomRequest{RoomID: "r1", NewName: "x", Account: "alice", Timestamp: 1700000000000})

	err := h.processRoomRename(ctx, body)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errPermanent))

	require.NoError(t, h.processRoomRename(ctx, body))

	require.Len(t, asyncResults, 2)
	assert.Equal(t, model.AsyncJobStatusError, asyncResults[0].Status)
	assert.Equal(t, model.AsyncJobStatusOK, asyncResults[1].Status)
}
```

- [ ] **12.2 Run, confirm RED.**

- [ ] **12.3 Implement** in `room-worker/handler.go`:

```go
func (h *Handler) processRoomRename(ctx context.Context, data []byte) (err error) {
	var requesterAccount, roomID string
	defer func() {
		h.publishAsyncJobResult(ctx, requesterAccount, model.AsyncJobOpRoomRename, roomID, err)
	}()

	requestID := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return newPermanent("missing X-Request-ID")
	}
	if !idgen.IsValidUUID(requestID) {
		return newPermanent("invalid X-Request-ID: must be a hyphenated UUID")
	}

	var req model.RenameRoomRequest
	if err = json.Unmarshal(data, &req); err != nil {
		return newPermanent("unmarshal rename request: %s", err.Error())
	}
	requesterAccount, roomID = req.Account, req.RoomID

	if err = h.store.UpdateRoomName(ctx, req.RoomID, req.NewName); err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return newPermanent("room not found")
		}
		if errors.Is(err, ErrNotChannelRoom) {
			return newPermanent("rename is only allowed in channel rooms")
		}
		return fmt.Errorf("update room name: %w", err)
	}
	if err = h.store.UpdateSubscriptionNamesForRoom(ctx, req.RoomID, req.NewName); err != nil {
		return fmt.Errorf("update subscription names: %w", err)
	}

	sysData, err := json.Marshal(model.RoomRenamedSysData{NewName: req.NewName, ByAccount: req.Account})
	if err != nil {
		return fmt.Errorf("marshal sys data: %w", err)
	}
	msg := model.Message{
		ID:          idgen.MessageIDFromRequestID(requestID, "room_renamed"),
		RoomID:      req.RoomID,
		UserAccount: req.Account,
		Type:        model.MessageTypeRoomRenamed,
		Content:     fmt.Sprintf("%s renamed the channel to %q", req.Account, req.NewName),
		SysMsgData:  sysData,
		CreatedAt:   time.UnixMilli(req.Timestamp).UTC(),
	}
	if err = h.publishCanonical(ctx, &msg, h.siteID, time.Now().UTC()); err != nil {
		return fmt.Errorf("publish room_renamed sys message: %w", err)
	}

	subs, err := h.store.ListByRoom(ctx, req.RoomID)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	h.publishSubscriptionEvents(ctx, subs, "renamed")

	accounts := make([]string, 0, len(subs))
	for _, sub := range subs {
		accounts = append(accounts, sub.User.Account)
	}
	remoteSites, err := h.findRemoteSitesForAccounts(ctx, accounts)
	if err != nil {
		return fmt.Errorf("find remote sites for outbox fan-out: %w", err)
	}
	for _, remoteSiteID := range remoteSites {
		payload, mErr := json.Marshal(model.RoomRenamedOutboxPayload{
			RoomID: req.RoomID, NewName: req.NewName, Timestamp: req.Timestamp,
		})
		if mErr != nil {
			return fmt.Errorf("marshal rename outbox payload: %w", mErr)
		}
		evt := model.OutboxEvent{
			Type: model.OutboxRoomRenamed, SiteID: h.siteID, DestSiteID: remoteSiteID,
			Payload: payload, Timestamp: time.Now().UTC().UnixMilli(),
		}
		evtData, mErr := json.Marshal(evt)
		if mErr != nil {
			return fmt.Errorf("marshal rename outbox event: %w", mErr)
		}
		seed := fmt.Sprintf("%s:%s:%d", req.RoomID, req.NewName, req.Timestamp)
		if err = h.publish(ctx, subject.Outbox(h.siteID, remoteSiteID, model.OutboxRoomRenamed),
			evtData, natsutil.OutboxDedupID(ctx, remoteSiteID, seed)); err != nil {
			return fmt.Errorf("publish rename outbox to %s: %w", remoteSiteID, err)
		}
	}
	return nil
}
```

- [ ] **12.4 Run, confirm GREEN. Commit.**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): implement processRoomRename"
```

---

### Task 13: Implement `processRoomVisibility`, dispatch wiring, and integration test

**Files:** `room-worker/handler.go`, `room-worker/handler_test.go`, `room-worker/integration_test.go`.

- [ ] **13.1 Write failing unit tests for `processRoomVisibility`.** Same shape as Task 12.1 but cover visibility-specific cases:

- Missing/invalid request ID → permanent.
- Unmarshal failure → permanent + no stale ok.
- `UpdateRoomVisibility` returns `ErrRoomNotFound` → permanent + AsyncJobResult error.
- Happy path no remote sites: `UpdateRoomVisibility`, `ApplySubscriptionVisibility(ownerAccount="bob")`, `ListByRoom`, two `publishSubscriptionEvents`, AsyncJobResult ok. NO sys message published.
- Happy path with remote site: above plus outbox publish carrying `RoomVisibilityOutboxPayload` (assert decoded `OwnerAccount` field).
- Error-then-ok retry sequence (mirror Task 12).

Template for the remote-site case:

```go
func TestProcessRoomVisibility_HappyPathWithRemoteSite(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockSubscriptionStore(ctrl)

	const roomID, owner = "r1", "bob"
	requestID := "01970a4f-8c2d-7c9a-abcd-e0123456789f"

	subs := []model.Subscription{
		{User: model.SubscriptionUser{ID: "u1", Account: "alice"}, RoomID: roomID},
		{User: model.SubscriptionUser{ID: "u2", Account: "bob"}, RoomID: roomID},
	}

	store.EXPECT().UpdateRoomVisibility(gomock.Any(), roomID, true, false).Return(nil)
	store.EXPECT().ApplySubscriptionVisibility(gomock.Any(), roomID, true, false, owner).Return(nil)
	store.EXPECT().ListByRoom(gomock.Any(), roomID).Return(subs, nil)
	store.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return([]model.User{
		{Account: "alice", SiteID: "site-a"}, {Account: "bob", SiteID: "site-b"},
	}, nil)

	var subjects []string
	var outboxPayloads [][]byte
	publish := func(_ context.Context, subj string, data []byte, _ string) error {
		subjects = append(subjects, subj)
		if strings.HasPrefix(subj, "outbox.") {
			outboxPayloads = append(outboxPayloads, data)
		}
		return nil
	}

	h := &Handler{store: store, siteID: "site-a", publish: publish}
	ctx := natsutil.ContextWithRequestID(context.Background(), requestID)
	body, _ := json.Marshal(model.RoomVisibilityRequest{
		RoomID: roomID, Restricted: true, ExternalAccess: false,
		OwnerAccount: owner, Account: "admin1", Timestamp: time.Now().UTC().UnixMilli(),
	})

	require.NoError(t, h.processRoomVisibility(ctx, body))

	assert.Contains(t, subjects, subject.Outbox("site-a", "site-b", model.OutboxRoomVisibilityChanged))
	require.Len(t, outboxPayloads, 1)

	var outboxEvt model.OutboxEvent
	require.NoError(t, json.Unmarshal(outboxPayloads[0], &outboxEvt))
	var payload model.RoomVisibilityOutboxPayload
	require.NoError(t, json.Unmarshal(outboxEvt.Payload, &payload))
	assert.Equal(t, owner, payload.OwnerAccount)
	assert.True(t, payload.Restricted)
}
```

- [ ] **13.2 Run, confirm RED.**

- [ ] **13.3 Implement `processRoomVisibility`** in `room-worker/handler.go`:

```go
func (h *Handler) processRoomVisibility(ctx context.Context, data []byte) (err error) {
	var requesterAccount, roomID string
	defer func() {
		h.publishAsyncJobResult(ctx, requesterAccount, model.AsyncJobOpRoomVisibility, roomID, err)
	}()

	requestID := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return newPermanent("missing X-Request-ID")
	}
	if !idgen.IsValidUUID(requestID) {
		return newPermanent("invalid X-Request-ID: must be a hyphenated UUID")
	}

	var req model.RoomVisibilityRequest
	if err = json.Unmarshal(data, &req); err != nil {
		return newPermanent("unmarshal visibility request: %s", err.Error())
	}
	requesterAccount, roomID = req.Account, req.RoomID

	if err = h.store.UpdateRoomVisibility(ctx, req.RoomID, req.Restricted, req.ExternalAccess); err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return newPermanent("room not found")
		}
		if errors.Is(err, ErrNotChannelRoom) {
			return newPermanent("visibility change is only allowed in channel rooms")
		}
		return fmt.Errorf("update room visibility: %w", err)
	}
	if err = h.store.ApplySubscriptionVisibility(ctx, req.RoomID, req.Restricted, req.ExternalAccess, req.OwnerAccount); err != nil {
		return fmt.Errorf("apply subscription visibility: %w", err)
	}

	subs, err := h.store.ListByRoom(ctx, req.RoomID)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	h.publishSubscriptionEvents(ctx, subs, "visibility_changed")

	accounts := make([]string, 0, len(subs))
	for _, sub := range subs {
		accounts = append(accounts, sub.User.Account)
	}
	remoteSites, err := h.findRemoteSitesForAccounts(ctx, accounts)
	if err != nil {
		return fmt.Errorf("find remote sites for outbox fan-out: %w", err)
	}
	for _, remoteSiteID := range remoteSites {
		payload, mErr := json.Marshal(model.RoomVisibilityOutboxPayload{
			RoomID:         req.RoomID,
			Restricted:     req.Restricted,
			ExternalAccess: req.ExternalAccess,
			OwnerAccount:   req.OwnerAccount,
			Timestamp:      req.Timestamp,
		})
		if mErr != nil {
			return fmt.Errorf("marshal visibility outbox payload: %w", mErr)
		}
		evt := model.OutboxEvent{
			Type: model.OutboxRoomVisibilityChanged, SiteID: h.siteID, DestSiteID: remoteSiteID,
			Payload: payload, Timestamp: time.Now().UTC().UnixMilli(),
		}
		evtData, mErr := json.Marshal(evt)
		if mErr != nil {
			return fmt.Errorf("marshal visibility outbox event: %w", mErr)
		}
		seed := fmt.Sprintf("%s:%t:%t:%d", req.RoomID, req.Restricted, req.ExternalAccess, req.Timestamp)
		if err = h.publish(ctx, subject.Outbox(h.siteID, remoteSiteID, model.OutboxRoomVisibilityChanged),
			evtData, natsutil.OutboxDedupID(ctx, remoteSiteID, seed)); err != nil {
			return fmt.Errorf("publish visibility outbox to %s: %w", remoteSiteID, err)
		}
	}
	return nil
}
```

- [ ] **13.4 Wire dispatch** in `HandleJetStreamMsg` switch (add before `default`):

```go
	case strings.HasSuffix(subj, ".room.rename"):
		err = h.processRoomRename(ctx, msg.Data())
	case strings.HasSuffix(subj, ".room.visibility"):
		err = h.processRoomVisibility(ctx, msg.Data())
```

- [ ] **13.5 Add integration tests** in `room-worker/integration_test.go` (one per processor). Use the existing test harness: testcontainers Mongo + NATS (`testutil`). For rename:

1. Seed `rooms` (channel) + `subscriptions` (2 local, 1 remote) + `users` collection.
2. Subscribe to `chat.msg.canonical.<siteID>.created`, `chat.user.*.event.subscription.update`, and `outbox.<siteID>.to.<remoteSite>.room_renamed` (collect with a `sync.WaitGroup`).
3. Publish `RenameRoomRequest` to the canonical stream subject with an `X-Request-ID` header.
4. Assert: room.Name updated, all subs renamed, one sys message, two SubscriptionUpdateEvents, one outbox publish.

For visibility (restrict transition): mirror the above; additionally assert role rewrite, `Restricted` + `ExternalAccess` flags on room and all subs, outbox payload carries `OwnerAccount`.

- [ ] **13.6 Run `make test SERVICE=room-worker` and `make test-integration SERVICE=room-worker`, confirm GREEN. Commit.**

```bash
git add room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go
git commit -m "feat(room-worker): implement processRoomVisibility, wire dispatch, integration tests"
```

---

## Phase 8: inbox-worker handlers

### Task 14: Implement both inbox handlers with dispatch

**Files:** `inbox-worker/handler.go`, `inbox-worker/handler_test.go`.

- [ ] **14.1 Write failing unit tests**:

```go
func TestHandleRoomRenamed_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockInboxStore(ctrl)
	store.EXPECT().UpdateSubscriptionNamesForRoom(gomock.Any(), "r1", "new").Return(nil)

	h := NewHandler(store)
	payload, _ := json.Marshal(model.RoomRenamedOutboxPayload{RoomID: "r1", NewName: "new", Timestamp: 1700000000000})
	data, _ := json.Marshal(model.OutboxEvent{Type: model.OutboxRoomRenamed, Payload: payload, Timestamp: 1700000000000})
	require.NoError(t, h.HandleEvent(context.Background(), data))
}

func TestHandleRoomRenamed_PermanentOnUnmarshal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockInboxStore(ctrl)
	h := NewHandler(store)
	data, _ := json.Marshal(model.OutboxEvent{Type: model.OutboxRoomRenamed, Payload: []byte("not json")})
	err := h.HandleEvent(context.Background(), data)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent))
}

func TestHandleRoomVisibilityChanged_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockInboxStore(ctrl)
	store.EXPECT().ApplySubscriptionVisibility(gomock.Any(), "r1", true, false, "bob").Return(nil)

	h := NewHandler(store)
	payload, _ := json.Marshal(model.RoomVisibilityOutboxPayload{
		RoomID: "r1", Restricted: true, ExternalAccess: false, OwnerAccount: "bob", Timestamp: 1700000000000,
	})
	data, _ := json.Marshal(model.OutboxEvent{Type: model.OutboxRoomVisibilityChanged, Payload: payload, Timestamp: 1700000000000})
	require.NoError(t, h.HandleEvent(context.Background(), data))
}

func TestHandleRoomVisibilityChanged_PermanentOnUnmarshal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store := NewMockInboxStore(ctrl)
	h := NewHandler(store)
	data, _ := json.Marshal(model.OutboxEvent{Type: model.OutboxRoomVisibilityChanged, Payload: []byte("not json")})
	err := h.HandleEvent(context.Background(), data)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent))
}
```

- [ ] **14.2 Run, confirm RED.**

- [ ] **14.3 Implement** in `inbox-worker/handler.go`. Append:

```go
func (h *Handler) handleRoomRenamed(ctx context.Context, evt *model.OutboxEvent) error {
	var payload model.RoomRenamedOutboxPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return newPermanent("unmarshal room_renamed payload: %s", err.Error())
	}
	if err := h.store.UpdateSubscriptionNamesForRoom(ctx, payload.RoomID, payload.NewName); err != nil {
		return fmt.Errorf("update subscription names for room %s: %w", payload.RoomID, err)
	}
	return nil
}

func (h *Handler) handleRoomVisibilityChanged(ctx context.Context, evt *model.OutboxEvent) error {
	var payload model.RoomVisibilityOutboxPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return newPermanent("unmarshal room_visibility_changed payload: %s", err.Error())
	}
	if err := h.store.ApplySubscriptionVisibility(ctx, payload.RoomID, payload.Restricted, payload.ExternalAccess, payload.OwnerAccount); err != nil {
		return fmt.Errorf("apply visibility for room %s: %w", payload.RoomID, err)
	}
	return nil
}
```

Add two cases to the `HandleEvent` switch:

```go
	case "room_renamed":
		return h.handleRoomRenamed(ctx, &evt)
	case "room_visibility_changed":
		return h.handleRoomVisibilityChanged(ctx, &evt)
```

- [ ] **14.4 Run, confirm GREEN. Commit.**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go
git commit -m "feat(inbox-worker): implement room_renamed and room_visibility_changed handlers"
```

---

### Task 15: inbox-worker integration tests

**Files:** `inbox-worker/integration_test.go`.

- [ ] **15.1 Add end-to-end tests** using the existing inbox-worker integration harness:

1. **`TestIntegration_HandleRoomRenamed`** — seed local subscription mirrors for `r1`, publish an outbox event onto the inbox subject for `room_renamed`, assert subscription names update.
2. **`TestIntegration_HandleRoomVisibilityChanged`** — seed mirrors (alice=owner, bob=member, carol=member), publish visibility event with `OwnerAccount=bob`, assert bob promoted to owner, alice demoted, flags set on all mirrors.

- [ ] **15.2 Run `make test-integration SERVICE=inbox-worker`, confirm GREEN. Commit.**

```bash
git add inbox-worker/integration_test.go
git commit -m "test(inbox-worker): integration tests for rename and visibility handlers"
```

---

## Phase 9: Mock + Docs + Final

### Task 16: `mock-user-service` admin1 seeding

**Files:** `mock-user-service/handler.go`, `mock-user-service/handler_test.go`.

> **Discovery first.** Today `mock-user-service` does not return `model.User` records — it serves status/profile/subscription endpoints. The spec calls for `admin1` → `[UserRoleAdmin]`. Before coding, decide the seeding mechanism:
>
> ```bash
> grep -rn "model.User\|users.InsertOne" mock-user-service/
> ```
>
> Options: (a) add a `userGet` RPC that returns `model.User` (with Roles), (b) extend an existing endpoint, (c) seed `users` collection via a startup script. Whichever path: keep the helper below as the single source of truth.

- [ ] **16.1 Write failing test** in `mock-user-service/handler_test.go`:

```go
func TestBuildMockUser_AdminRole(t *testing.T) {
	admin := buildMockUser("admin1", "site-a")
	assert.Equal(t, []model.UserRole{model.UserRoleAdmin}, admin.Roles)

	normal := buildMockUser("alice", "site-a")
	assert.Equal(t, []model.UserRole{model.UserRoleUser}, normal.Roles)
}
```

- [ ] **16.2 Run, confirm RED.**

- [ ] **16.3 Implement** `buildMockUser` in `mock-user-service/handler.go`:

```go
// buildMockUser returns a User with Roles seeded. admin1 → admin; others → user.
func buildMockUser(account, siteID string) model.User {
	roles := []model.UserRole{model.UserRoleUser}
	if account == "admin1" {
		roles = []model.UserRole{model.UserRoleAdmin}
	}
	return model.User{
		ID:      "mock-user-" + account,
		Account: account,
		SiteID:  siteID,
		Roles:   roles,
	}
}
```

Wire it into the surfacing endpoint chosen during discovery.

- [ ] **16.4 Run, confirm GREEN. Commit.**

```bash
git add mock-user-service/
git commit -m "feat(mock-user-service): seed admin1 with admin role"
```

---

### Task 17: Update `docs/client-api.md`

**Files:** `docs/client-api.md`.

- [ ] **17.1 Add two RPC sections under §3.1 (room-service)**, following the existing "Update Member Role" format (find that section as the template).

**Rename Room**
- **Subject:** `chat.user.{account}.request.room.{roomID}.{siteID}.room.rename`
- **Request body:** `roomId` (string, required, matches subject), `newName` (string, required, 1-100 chars after trim).
- **Success:** `{"status":"accepted","requestId":"<uuid>"}`.
- **Errors:** `invalid name`, `room not found`, `rename is only allowed in channel rooms`, `only owners or admins can rename a channel`, `invalid request`, `missing X-Request-ID header`, `invalid X-Request-ID format`.
- **Triggered events:** canonical `room_renamed` sys-message → broadcast-worker fans to `chat.room.{roomID}.event`; one `SubscriptionUpdateEvent{Action:"renamed"}` per subscription; one outbox event per remote site with federated members.
- **AsyncJobResult:** `room.rename` op on `chat.user.{account}.response.{requestId}`.

**Set Room Visibility**
- **Subject:** `chat.user.{account}.request.room.{roomID}.{siteID}.room.visibility`
- **Request body:** `roomId` (string, required), `restricted` (bool, required), `externalAccess` (bool, required), `ownerAccount` (string, conditional — required when transitioning false→true; when supplied alongside `restricted=true`, becomes sole owner).
- **Success:** `{"status":"accepted","requestId":"<uuid>"}`.
- **Errors:** `only admins can change room visibility`, `room not found`, `visibility change is only allowed in channel rooms`, `owner account is required when restricting a room`, `owner account is not a member of this room`, `not enough members to restrict (need at least N)`, `invalid request`, request-ID errors.
- **Triggered events:** one `SubscriptionUpdateEvent{Action:"visibility_changed"}` per subscription (carries new flags + roles); one outbox event per remote site. **No sys message.**
- **AsyncJobResult:** `room.visibility` op.

- [ ] **17.2 Commit.**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): document room.rename and room.visibility RPCs"
```

---

### Task 18: Final validation and push

- [ ] **18.1** `make lint` — fix any.
- [ ] **18.2** `make test` — full unit run.
- [ ] **18.3** `make test-integration` — full integration run (Docker).
- [ ] **18.4** `make sast` — must pass (no medium+).
- [ ] **18.5** Push: `git push -u origin <current-branch>`.

---

## Notes for Implementor

- **Don't combine commits mid-implementation.** Each task = one commit, even when they're small.
- **Spec line numbers drift.** Use section headings (`§Processing`, `§Validation Rules`) as anchors, not line refs.
- **Match existing patterns.** When adapting code (variable names, helper presence, mock generator output), check 2-3 sibling handlers first.
- **Per-test isolation in integration tests** is the caller's responsibility — `testutil.MongoDB(t, "...")` hashes `t.Name()` for isolated DBs. Don't drop the helper.
- **Stop on RED-when-expected-GREEN.** A surprising failure means investigate, not move on.
- **Observability (spec §Observability).** Every new handler logs `slog.Info` at entry with `op`, `requester`, `roomID`, `requestID`. The implementation snippets above omit these for brevity; add when coding. Validation rejects log INFO with `reason`.
- **Federation race (spec §Known Limitations).** A member added between a rename/visibility event and federation reaching their site sees stale data. Out of scope here; mitigations are a future spec.
