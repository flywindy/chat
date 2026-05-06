# Create Room Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the create-room feature per `docs/superpowers/specs/2026-04-28-create-room-design.md`. Clients create `dm` / `botDM` / `channel` rooms via a new NATS request/reply endpoint. Room and `room_members` documents live only on the requester's site; subscriptions are replicated cross-site via a new `room_created` outbox event. Add-member is retrofitted to share the same `X-Request-ID` / async-job-result / `Subscription.Name` infrastructure.

**Architecture:** Mirrors add-member end-to-end. `room-service` does synchronous validation and publishes to the `ROOMS` canonical stream → `room-worker` writes Mongo state, fans out `subscription.update` events, publishes channel-only system messages (`room_created` + `members_added`), and emits cross-site outbox events per remote site → `inbox-worker` writes its slice of subscriptions. Build is incremental: foundational `pkg/` changes first, then add-member retrofit (smaller, validates the new infrastructure), then create-room across the three services.

**Tech Stack:** Go 1.25 · NATS + JetStream · MongoDB (rooms / subscriptions / room_members / apps / users) · `go.uber.org/mock` (mockgen for stores) · `stretchr/testify` (assertions) · `testcontainers-go` (mongodb / nats integration containers) · `pkg/idgen` + `pkg/natsutil` + `pkg/natsrouter` foundations from PR #131.

**Spec:** `docs/superpowers/specs/2026-04-28-create-room-design.md`

**Foundation:** PR #131 ("idgen rework") is merged. `idgen.GenerateUUIDv7`, `idgen.BuildDMRoomID`, `idgen.MessageIDFromRequestID`, `natsutil.NewMsg`, `natsutil.RequestIDFromContext`, `natsrouter` request-ID middleware, and `subject.UserResponse` / `model.AsyncJobResult` are all available in `main`.

---

## File Structure Map

This section lists every file the plan touches and what it owns. Lock decomposition decisions in here before tasks reference them.

### `pkg/model/` (shared types)

| File | Change | What it owns |
|------|--------|--------------|
| `room.go` | modify | `RoomTypeBotDM` constant; `Room.AppCount` field. |
| `subscription.go` | modify | New `Name`, `RoomType`, `SidebarName`, `IsSubscribed` fields. |
| `app.go` | **create** | `App`, `AppAssistant`, `AppSponsor` structs (read-only domain type). |
| `member.go` | modify | `CreateRoomRequest` struct; extend `MemberAddEvent` with `RoomName`. Drop `RequestID` from `AddMembersRequest` if PR #131 added it. |
| `event.go` | modify | Migrate `AsyncJobResult` from `{Job, Success}` to `{Operation, Status, RoomID(omitempty)}`; add all five `AsyncJobOp*` constants; new `RoomCreatedOutbox` payload struct; `ErrorResponse.RoomID` field. |
| `model_test.go` | modify | Round-trip assertions for every new field/struct. |

### `pkg/subject/` (NATS subject builders)

| File | Change | What it owns |
|------|--------|--------------|
| `subject.go` | modify | `RoomCreate(account, siteID)` + `RoomCreateWildcard(siteID)`; `RoomCanonicalOperation(subject) string` extractor (if not already present from PR #131). |
| `subject_test.go` | modify | Round-trip parse/build tests for the new subject. |

### `pkg/pipelines/` (shared aggregation pipelines)

| File | Change | What it owns |
|------|--------|--------------|
| `member.go` | modify | Empty-`roomID` branch in `GetNewMembersPipeline` — drops the "already-subscribed" `$lookup` stages when the room doesn't exist yet. |
| `member_test.go` | modify | Test case for the empty-roomID branch. |

### `room-service/` (validation gateway)

| File | Change | What it owns |
|------|--------|--------------|
| `helper.go` | modify | New sentinel errors (`errEmptyCreateRequest`, `errSelfDM`, `errBotInChannel`, `errBotNotAvailable`, `errInvalidUserData`, `errMissingRequestID`, `errInvalidRequestID`, `errChannelNameRequired`, `errChannelNameTooLong`, `errUserNotFound`); `dmExistsError` typed wrapper; `stripAccount` helper. Extend `sanitizeError` allowlist. **Channels require a client-supplied `Name` validated for both presence and rune-length ≤ 100** — server never truncates or auto-names. |
| `helper_test.go` | modify | Tests for the new helpers and `sanitizeError` allowlist additions. |
| `handler.go` | modify | New `handleCreateRoom`, `natsCreateRoom`, `replyDMExists`. New helpers `determineRoomType`, `validateChannelBranch`, `validateDMBranch`. |
| `handler_test.go` | modify | Table-driven tests for `handleCreateRoom` (≥18 rows per spec §9). |
| `store.go` | modify | `RoomStore` interface: `GetUser`, `GetApp`, `FindDMSubscription`. Modify `CountNewMembers` doc to allow empty `roomID`. |
| `store_mongo.go` | modify | Implement the new methods. Add `EnsureIndexes` calls for `apps.{assistant.name}` and `subscriptions.{u.account, name, roomType}` (partial filter for `dm`/`botDM`). |
| `mock_store_test.go` | regen | `make generate SERVICE=room-service` after `store.go` edits. |
| `main.go` | modify | Subscribe to `subject.RoomCreateWildcard(cfg.SiteID)` with `room-service` queue group. |
| `integration_test.go` | modify | Two new cases: end-to-end create-channel; DM-already-exists. |

### `room-worker/` (writer)

| File | Change | What it owns |
|------|--------|--------------|
| `handler.go` | modify | New `processCreateRoom`, plus dispatcher branch on the `create` operation. New helpers `newSub`, `composeName`, `composeNameOrAccount`, `resolveRoomName`, `createdByForType`, `publishCanonical`, `publishAsyncJobResult`. Extend `processAddMembers` with retrofit changes (deterministic sub IDs; `Subscription.Name`/`RoomType`; async-job notification; `ReconcileMemberCounts` call site). |
| `handler_test.go` | modify | New tests for `processCreateRoom` (DM, botDM, channel happy paths + idempotency + permanent/retryable errors). Update existing add-member tests for the retrofit assertions. |
| `store.go` | modify | Interface gains `CreateRoom`, `GetUser`, `ListNewMembersForNewRoom`, `ReconcileMemberCounts`. Old `ReconcileUserCount` becomes the implementation detail of `ReconcileMemberCounts`. |
| `store_mongo.go` | modify | Implement the new methods. |
| `mock_store_test.go` | regen | `make generate SERVICE=room-worker`. |
| `integration_test.go` | modify | New cases: end-to-end create-channel/DM/botDM; idempotent redelivery. |

### `inbox-worker/` (cross-site receiver)

| File | Change | What it owns |
|------|--------|--------------|
| `handler.go` | modify | New `handleRoomCreated` plus dispatcher routing for the `room_created` event type. Helpers `subscriptionName`, `subscriptionSidebarName`, `subscriptionIsSubscribed`, `rolesForType`. Extend `handleMemberAdded` for the retrofit (populate `Subscription.Name`/`RoomType` from the new `MemberAddEvent.RoomName`). |
| `handler_test.go` | modify | New tests for `handleRoomCreated` (DM, botDM, channel; empty accounts; missing request ID; idempotency). Updated assertions on `handleMemberAdded`. |
| `integration_test.go` | modify | New case: publish a fabricated `room_created` outbox event, observe expected subs created. |

### `pkg/testutil/`, `Makefile`, `deploy/docker-compose.yml`

No changes required. The new feature uses existing streams (`ROOMS`, `OUTBOX`, `INBOX`, `MESSAGES_CANONICAL`) and existing infra.

---

## Implementation Order

The plan executes in nine phases, each phase grouped into self-reviewable parts. Earlier phases unblock later ones; within a phase, tasks should be done in order unless explicitly parallelizable.

| # | Phase | Why this order |
|---|-------|----------------|
| 1 | **`pkg/model` foundation** | Every later task depends on these structs/fields. |
| 2 | **`pkg/subject` + `pkg/pipelines`** | New subject builder and the empty-`roomID` aggregation branch are used by both add-member retrofit and create-room. |
| 3 | **room-worker store split: `ReconcileMemberCounts`** | Replaces `ReconcileUserCount` everywhere. Done early so all subsequent handler edits can use the new name. |
| 4 | **Add-member retrofit** | Smallest end-to-end change. Validates that the new infrastructure (deterministic sub IDs, `Name`/`RoomType` population, async-job notification, `MemberAddEvent.RoomName`) works before we layer create-room on top. |
| 5 | **room-service create-room** | Validation pipeline + new store methods + indices + NATS subscription. |
| 6 | **room-worker create-room** | `processCreateRoom` plus all the new helpers and store methods. |
| 7 | **inbox-worker create-room** | `handleRoomCreated` cross-site receiver. |
| 8 | **Integration tests** | End-to-end across each service with real Mongo + NATS. |
| 9 | **Verification & wiring** | `make lint`, `make test`, `make test-integration`, manual smoke. |

---

## Review Adjustments — Apply Throughout

These rules came out of a code-review pass on this plan. They cut across many tasks; rather than restate them inside each task, the implementer must apply them wherever the relevant code is written.

1. **Request-ID format.** `X-Request-ID` values are 36-char hyphenated UUIDs — any version (v4 or v7) is accepted by `idgen.IsValidUUID`. Unit-test fixtures use a constant like `"0193abcd-0193-7abc-89ab-0193abcd0193"`; integration tests use `idgen.GenerateRequestID()` (which returns a UUIDv7 hyphenated). The 32-char no-hyphen form is exclusively for Mongo entity `_id` fields and must NOT be used for request IDs anywhere.

2. **All JetStream publishes go through `h.publish`.** It sets `Nats-Msg-Id` and is the standard wrapper. Task 36 introduces `publishCanonical` as a thin helper that wraps `h.publish(ctx, subject.MsgCanonicalCreated(siteID), data, msg.ID)` — this is the correct pattern. Tasks 35 and 37 must NOT call `h.js.PublishMsg` directly or build `natsutil.NewMsg` inline anywhere outside of `publishCanonical` / `publishOutbox` helpers.

3. **Extract one `publishOutbox(ctx, destSiteID, eventType, payload, dedupSeed)` helper.** Tasks 35–37 add the fifth call site of the inline `model.OutboxEvent{...}` + `subject.Outbox(...)` + `outboxDedupID(...)` + `h.publish(...)` ritual already duplicated four times in `room-worker/handler.go`. Land the helper in this plan and refactor the existing four call sites in the same task that introduces the fifth.

4. **Reuse, do not redefine, `publishAsyncJobResult`.** `room-worker/handler.go` already has it with three call sites. Task 6 / 16 / 37 must update the existing helper's signature in one place (and update all call sites); they must NOT introduce a parallel function with a different signature.

5. **`stripAccount` is duplicated in both services.** It's a 5-line helper used in one place per service; per CLAUDE.md "three similar lines is better than a premature abstraction" we keep it inlined. The spec rule (channels require client-supplied Name; reject if length > 100) lives entirely in room-service `classifyAndValidate`, so there's no `truncateRunes` / `composeAutoName` to share.

6. **Promote `determineRoomType` / `determineRoomTypeFromPayload` to `pkg/model`.** Task 24 (room-service) and Task 32 (room-worker) currently define identical bodies. Add `model.RoomTypeFor(req)` once and call from both.

7. **Constants, not strings, for status / op / system-message types.** Add to `pkg/model` (in Task 6 alongside `AsyncJobOpRoomCreate`):
   - `AsyncJobStatusOK = "ok"`, `AsyncJobStatusError = "error"`, `CreateRoomReplyAccepted = "accepted"`
   - `MessageTypeRoomCreated = "room_created"`, `MessageTypeMembersAdded = "members_added"`
   - `SubscriptionUpdateActionAdded = "added"`
   Tasks 25, 27, 35, 36, 37 must reference these constants, never the raw strings.

8. **Typed reply struct, not `map[string]string`.** Task 27 must define `model.CreateRoomReply{Status, RoomID, RoomType string}` in `pkg/model` and marshal that. CLAUDE.md forbids `map[string]interface{}` on the wire; the same rule applies to `map[string]string` for typed responses. Update Task 25/27 tests to deserialize into the struct.

9. **Bulk-write helpers belong on the `Store` interface.** `bulkCreateSubsIdempotent` and `bulkCreateMembersIdempotent` in Task 33/34 must be added to `room-worker/store.go` (and implemented in `store_mongo.go` per Task 30), not declared as private handler methods.

10. **Parallelize per-destination outbox publishes (Task 37) and per-sub `subscription.update` fan-out (Task 35).** Use `golang.org/x/sync/errgroup` with bounded concurrency (`SetLimit(8)`). Both loops sit on the request-handling tail and scale with N (sites / members) — sequential publishes turn N×RTT into 1×RTT.

11. **Skip `ReconcileMemberCounts` for `RoomTypeDM` / `RoomTypeBotDM`.** Counts are fixed (1 owner + 1 member). Set `MemberCount`/`UserCount`/`AppCount` at `CreateRoom` insert time in Tasks 33 and call `ReconcileMemberCounts` only for `RoomTypeChannel` in Task 34 / `finishCreateRoom`. Saves a Mongo aggregate per DM.

12. **`AddedUsersCount` is exact — use `len(subs) - 1`.** One subscription per account (including owner); subtracting 1 gives the non-owner member count. This matches the spec exactly and is the expression in scope at `finishCreateRoom` call sites. Do NOT use `len(req.Users) + len(req.Orgs)` — that double-counts orgs against their expanded users.

13. **Drop the room-service pre-check + room-worker pre-check duplication for DM dedup.** Rely on the partial unique index from Task 23. Room-service's `FindDMSubscription` pre-check (Task 25) is a TOCTOU read — keep it ONLY as a fast-path optimization for the common case, and ensure the room-worker insert path also handles `mongo.ErrDuplicateKey` cleanly so a race between two concurrent DM creates produces the same `dmExistsError` either way.

14. **Subscription field denormalization (`Name`, `RoomType`, `IsSubscribed`).** Task 2 adds these to keep them as a deliberate read-optimization for the sidebar query. Document this rationale in Task 2's commit message and add a test that asserts the values stay in sync after a room rename (the rename flow must update all subscriptions). If the rename plumbing is out of scope for this PR, file a follow-up task explicitly.

15. **No `time.Sleep` in non-test code (CLAUDE.md).** Verification step in Task 43: add `! grep -rn 'time\.Sleep' room-service room-worker inbox-worker | grep -v _test.go` to the success criteria.

16. **Subscription and room-member IDs use `GenerateUUIDv7()`.** The spec has an internal contradiction: the summary bullet (line 52) says "deterministic `MessageIDFromRequestID(requestID, 'sub:' + account)`" but the detailed ID-generation section (line 344) says "`idgen.GenerateUUIDv7()` (32-char hex, no hyphens)." The 32-char hex form is correct — consistent with the established convention for `Subscription._id`, `RoomMember._id`, `ThreadRoom._id`, and `ThreadSubscription._id` throughout the codebase. Use `idgen.GenerateUUIDv7()` for both subscription IDs and room-member IDs in all tasks (13, 14, 33, 34, 39). Idempotency on JetStream redelivery is provided by the compound unique index (not by deterministic IDs). The `bulkCreateSubsIdempotent` and `bulkCreateMembersIdempotent` helpers already handle `mongo.IsDuplicateKeyError` → success, which is the correct pattern. Tests that check sub/member IDs must assert `idgen.IsValidUUIDv7(sub.ID)`, not a deterministic value.

---

## Phase 1 — `pkg/model` Foundation

### Task 1: Add `RoomTypeBotDM` constant + `Room.AppCount` field

**Files:**
- Modify: `pkg/model/room.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Read the current Room model**

Run: `cat pkg/model/room.go`

Confirm there's a `RoomType` string-typed constant block with `RoomTypeDM` and `RoomTypeChannel`, and a `Room` struct.

- [ ] **Step 2: Write the failing test**

Append to `pkg/model/model_test.go` near the existing `TestRoomJSON` (or wherever room round-trip tests live):

```go
func TestRoomBotDMRoundtrip(t *testing.T) {
	r := model.Room{
		ID:        "r1",
		Name:      "weather chat",
		Type:      model.RoomTypeBotDM,
		SiteID:    "site-A",
		UserCount: 1,
		AppCount:  1,
		CreatedAt: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
	}

	var dst model.Room
	roundTrip(t, r, &dst)
	assert.Equal(t, model.RoomTypeBotDM, dst.Type)
	assert.Equal(t, "botDM", string(dst.Type))
	assert.Equal(t, 1, dst.AppCount)
}
```

- [ ] **Step 3: Run the test — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -30`

Expected: build error — `model.RoomTypeBotDM` undefined and `dst.AppCount` undefined.

- [ ] **Step 4: Add the constant and field**

Edit `pkg/model/room.go`:

```go
const (
    RoomTypeDM      RoomType = "dm"
    RoomTypeBotDM   RoomType = "botDM"
    RoomTypeChannel RoomType = "channel"
)
```

Add to `Room` struct, immediately after `UserCount`:

```go
AppCount int `json:"appCount" bson:"appCount"`
```

- [ ] **Step 5: Run the test — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: `ok  github.com/.../pkg/model`

- [ ] **Step 6: Commit**

```bash
git add pkg/model/room.go pkg/model/model_test.go
git commit -m "model: add RoomTypeBotDM and Room.AppCount"
```

---

### Task 2: Add new `Subscription` fields (`Name`, `RoomType`, `SidebarName`, `IsSubscribed`)

**Files:**
- Modify: `pkg/model/subscription.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Read the current Subscription struct**

Run: `cat pkg/model/subscription.go`

- [ ] **Step 2: Write the failing test**

Append to `pkg/model/model_test.go`:

```go
func TestSubscriptionNewFields(t *testing.T) {
	t.Run("channel sub round-trips with empty SidebarName/IsSubscribed", func(t *testing.T) {
		sub := model.Subscription{
			ID:       "s1",
			User:     model.SubscriptionUser{ID: "u1", Account: "alice"},
			RoomID:   "r1",
			SiteID:   "site-A",
			Roles:    []model.Role{model.RoleOwner},
			Name:     "deal team",
			RoomType: model.RoomTypeChannel,
			JoinedAt: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		}
		var dst model.Subscription
		raw := roundTrip(t, sub, &dst)
		assert.Equal(t, "deal team", dst.Name)
		assert.Equal(t, model.RoomTypeChannel, dst.RoomType)
		assert.Empty(t, dst.SidebarName)
		assert.False(t, dst.IsSubscribed)
		// SidebarName and IsSubscribed should be omitted from JSON when zero
		assert.NotContains(t, string(raw), "sidebarName")
		assert.NotContains(t, string(raw), "isSubscribed")
	})
	t.Run("botDM human sub round-trips with all fields", func(t *testing.T) {
		sub := model.Subscription{
			ID:           "s2",
			User:         model.SubscriptionUser{ID: "u1", Account: "alice"},
			RoomID:       "r2",
			SiteID:       "site-A",
			Name:         "weather.bot",
			RoomType:     model.RoomTypeBotDM,
			SidebarName:  "Weather Bot",
			IsSubscribed: true,
			JoinedAt:     time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		}
		var dst model.Subscription
		raw := roundTrip(t, sub, &dst)
		assert.Equal(t, "weather.bot", dst.Name)
		assert.Equal(t, model.RoomTypeBotDM, dst.RoomType)
		assert.Equal(t, "Weather Bot", dst.SidebarName)
		assert.True(t, dst.IsSubscribed)
		assert.Contains(t, string(raw), `"sidebarName":"Weather Bot"`)
		assert.Contains(t, string(raw), `"isSubscribed":true`)
	})
}
```

If the existing `roundTrip` helper doesn't return the JSON bytes, change the test to marshal/unmarshal explicitly using `json.Marshal` / `json.Unmarshal` and capture the bytes for the substring asserts.

- [ ] **Step 3: Run the test — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -30`

Expected: build error — `Name`, `RoomType`, `SidebarName`, `IsSubscribed` undefined on `Subscription`.

- [ ] **Step 4: Add the fields**

Edit `pkg/model/subscription.go`. Insert after the existing `Roles` field, in this exact order:

```go
Name         string   `json:"name"                    bson:"name"`
RoomType     RoomType `json:"roomType"                bson:"roomType"`
SidebarName  string   `json:"sidebarName,omitempty"   bson:"sidebarName,omitempty"`
IsSubscribed bool     `json:"isSubscribed,omitempty"  bson:"isSubscribed,omitempty"`
```

- [ ] **Step 5: Run the test — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: `ok  github.com/.../pkg/model`

- [ ] **Step 6: Commit**

```bash
git add pkg/model/subscription.go pkg/model/model_test.go
git commit -m "model: add Subscription.Name/RoomType/SidebarName/IsSubscribed"
```

---

### Task 3: Add `App` / `AppAssistant` / `AppSponsor` model

**Files:**
- Create: `pkg/model/app.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/model/model_test.go`:

```go
func TestAppRoundtrip(t *testing.T) {
	a := model.App{
		ID:          "app1",
		Name:        "Weather Bot",
		Description: "Forecasts and alerts",
		Assistant: &model.AppAssistant{
			Enabled:     true,
			Name:        "weather.bot",
			SettingsURL: "https://example.com/weather/settings",
		},
		Sponsors: []model.AppSponsor{
			{Name: "Alice", Phone: "555-0100"},
		},
	}
	var dst model.App
	roundTrip(t, a, &dst)
	require.NotNil(t, dst.Assistant)
	assert.True(t, dst.Assistant.Enabled)
	assert.Equal(t, "weather.bot", dst.Assistant.Name)
	assert.Equal(t, "Weather Bot", dst.Name)
	assert.Len(t, dst.Sponsors, 1)
	assert.Equal(t, "Alice", dst.Sponsors[0].Name)
}

func TestAppAssistantDisabledRoundtrip(t *testing.T) {
	a := model.App{
		ID:        "app2",
		Name:      "Disabled Bot",
		Assistant: &model.AppAssistant{Enabled: false, Name: "disabled.bot"},
	}
	var dst model.App
	roundTrip(t, a, &dst)
	require.NotNil(t, dst.Assistant)
	assert.False(t, dst.Assistant.Enabled)
}
```

- [ ] **Step 2: Run the test — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -10`

Expected: build error — `model.App`, `model.AppAssistant`, `model.AppSponsor` undefined.

- [ ] **Step 3: Create the file**

Create `pkg/model/app.go`:

```go
package model

// App is the read-only view of a row in the apps collection.
// Provisioning is upstream; chat services only read.
type App struct {
	ID          string         `json:"id"                    bson:"_id"`
	Name        string         `json:"name"                  bson:"name"`
	Description string         `json:"description,omitempty" bson:"description,omitempty"`
	Assistant   *AppAssistant  `json:"assistant,omitempty"   bson:"assistant,omitempty"`
	Sponsors    []AppSponsor   `json:"sponsors,omitempty"    bson:"sponsors,omitempty"`
}

// AppAssistant declares the bot account and whether the assistant is
// active. Assistant.Name is the bot's user account (always ends in
// ".bot"). botDM creation requires Enabled == true.
type AppAssistant struct {
	Enabled     bool   `json:"enabled"               bson:"enabled"`
	Name        string `json:"name"                  bson:"name"`
	SettingsURL string `json:"settingsUrl,omitempty" bson:"settingsUrl,omitempty"`
}

type AppSponsor struct {
	Name  string `json:"name"  bson:"name"`
	Phone string `json:"phone" bson:"phone"`
}
```

- [ ] **Step 4: Run the test — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/app.go pkg/model/model_test.go
git commit -m "model: add App, AppAssistant, AppSponsor"
```

---

### Task 4: Add `CreateRoomRequest` + drop `RequestID` from `AddMembersRequest`

**Files:**
- Modify: `pkg/model/member.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Inspect AddMembersRequest for the RequestID field**

Run: `grep -n "RequestID" pkg/model/member.go`

If the field exists (PR #131 added it as a transitional step), this task removes it. If it doesn't exist, the removal step is a no-op and the test for `AddMembersRequest` round-trip still verifies the post-PR-#131 shape is preserved.

- [ ] **Step 2: Write the failing test**

Append to `pkg/model/model_test.go`:

```go
func TestCreateRoomRequestRoundtrip(t *testing.T) {
	req := model.CreateRoomRequest{
		Name:             "team",
		Users:            []string{"bob", "carol"},
		Orgs:             []string{"org-fx"},
		Channels:         []model.ChannelRef{{RoomID: "r0", SiteID: "site-A"}},
		RoomID:           "r_xyz",
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		AppName:          "",
		Timestamp:        1740000000000,
	}
	var dst model.CreateRoomRequest
	roundTrip(t, req, &dst)
	assert.Equal(t, "team", dst.Name)
	assert.Equal(t, []string{"bob", "carol"}, dst.Users)
	assert.Equal(t, "r_xyz", dst.RoomID)
	assert.Equal(t, "u_alice", dst.RequesterID)
	assert.Equal(t, int64(1740000000000), dst.Timestamp)
}

func TestCreateRoomRequestBotDMHasAppName(t *testing.T) {
	req := model.CreateRoomRequest{
		Users:            []string{"weather.bot"},
		RoomID:           "u_aliceu_wbot",
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		AppName:          "Weather Bot",
		Timestamp:        1,
	}
	var dst model.CreateRoomRequest
	roundTrip(t, req, &dst)
	assert.Equal(t, "Weather Bot", dst.AppName)
}

func TestAddMembersRequestNoRequestIDField(t *testing.T) {
	// AddMembersRequest must not carry RequestID in its payload — the
	// X-Request-ID header is the single source of truth post-PR #131.
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		Timestamp:        1,
	})
	require.NoError(t, err)
	assert.NotContains(t, string(body), "requestId")
}
```

- [ ] **Step 3: Run the tests — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -20`

Expected: build error for `model.CreateRoomRequest` undefined; if `RequestID` exists on `AddMembersRequest`, the substring assert fails.

- [ ] **Step 4: Add `CreateRoomRequest` and remove `RequestID` if present**

Append to `pkg/model/member.go` after the existing `AddMembersRequest`:

```go
// CreateRoomRequest is the canonical event payload for creating a room.
// Client supplies Name/Users/Orgs/Channels; room-service populates the
// RoomID, Requester*, AppName, Timestamp before publishing to the
// ROOMS canonical stream. The X-Request-ID header (not in this struct)
// carries the request correlation ID per PR #131.
type CreateRoomRequest struct {
	Name     string       `json:"name"     bson:"name"`
	Users    []string     `json:"users"    bson:"users"`
	Orgs     []string     `json:"orgs"     bson:"orgs"`
	Channels []ChannelRef `json:"channels" bson:"channels"`

	RoomID           string `json:"roomId"             bson:"roomId"`
	RequesterID      string `json:"requesterId"        bson:"requesterId"`
	RequesterAccount string `json:"requesterAccount"   bson:"requesterAccount"`
	AppName          string `json:"appName,omitempty"  bson:"appName,omitempty"`
	Timestamp        int64  `json:"timestamp"          bson:"timestamp"`
}
```

If `AddMembersRequest` has a `RequestID` field, delete that field from the struct definition. Update any existing test in `model_test.go` that sets `RequestID` on `AddMembersRequest` to remove it.

- [ ] **Step 5: Run the tests — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: PASS.

- [ ] **Step 6: Verify no other code references `AddMembersRequest.RequestID`**

Run: `grep -rn "AddMembersRequest" --include="*.go" | grep -i "requestid"`

Expected: no matches (or only in fixed test files).

- [ ] **Step 7: Commit**

```bash
git add pkg/model/member.go pkg/model/model_test.go
git commit -m "model: add CreateRoomRequest; drop RequestID from AddMembersRequest"
```

---

### Task 5: Extend `MemberAddEvent` with `RoomName`

**Files:**
- Modify: `pkg/model/member.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Locate the struct**

Run: `grep -n "type MemberAddEvent" pkg/model/member.go`

Confirm the existing struct shape.

- [ ] **Step 2: Write the failing test**

Append to `pkg/model/model_test.go`:

```go
func TestMemberAddEventCarriesRoomName(t *testing.T) {
	evt := model.MemberAddEvent{
		Type:      "member_added",
		RoomID:    "r1",
		RoomName:  "deal team",
		Accounts:  []string{"bob"},
		SiteID:    "site-A",
		JoinedAt:  1,
		Timestamp: 1,
	}
	var dst model.MemberAddEvent
	raw := roundTrip(t, evt, &dst)
	assert.Equal(t, "deal team", dst.RoomName)
	assert.Contains(t, string(raw), `"roomName":"deal team"`)
}

func TestMemberAddEventRoomNameOmitemptyOnZero(t *testing.T) {
	// Backward-compat: when RoomName is empty (e.g., consumed from a
	// pre-retrofit producer), marshalling should round-trip to empty
	// without errors. Field is required, not omitempty — empty string
	// is a valid value.
	evt := model.MemberAddEvent{Type: "member_added", RoomID: "r1"}
	var dst model.MemberAddEvent
	roundTrip(t, evt, &dst)
	assert.Empty(t, dst.RoomName)
}
```

- [ ] **Step 3: Run the test — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -10`

Expected: `dst.RoomName` undefined.

- [ ] **Step 4: Add the field**

Edit `pkg/model/member.go`. Insert after `RoomID` in the `MemberAddEvent` struct:

```go
RoomName string `json:"roomName" bson:"roomName"`
```

- [ ] **Step 5: Run the test — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/model/member.go pkg/model/model_test.go
git commit -m "model: add RoomName to MemberAddEvent for cross-site sub naming"
```

---

### Task 6: Add `RoomCreatedOutbox` event payload + extend `ErrorResponse` + add `AsyncJobResult` op constants

**Files:**
- Modify: `pkg/model/event.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Read the existing event.go**

Run: `cat pkg/model/event.go`

Confirm `ErrorResponse`, `OutboxEvent`, and (per PR #131) `AsyncJobResult` shapes. Locate where existing operation-name constants live, if any.

- [ ] **Step 2: Write the failing test**

Append to `pkg/model/model_test.go`:

```go
func TestRoomCreatedOutboxRoundtrip(t *testing.T) {
	out := model.RoomCreatedOutbox{
		RoomID:               "r1",
		RoomType:             model.RoomTypeChannel,
		RoomName:             "deal team",
		HomeSiteID:           "site-A",
		Accounts:             []string{"bob", "ian"},
		RequesterAccount:     "alice",
		RequesterEngName:     "Alice",
		RequesterChineseName: "爱丽丝",
		Timestamp:            1740000000000,
	}
	var dst model.RoomCreatedOutbox
	raw := roundTrip(t, out, &dst)
	assert.Equal(t, model.RoomTypeChannel, dst.RoomType)
	assert.Equal(t, []string{"bob", "ian"}, dst.Accounts)
	assert.NotContains(t, string(raw), "appName") // omitempty when not set
}

func TestRoomCreatedOutboxBotDMHasAppName(t *testing.T) {
	out := model.RoomCreatedOutbox{
		RoomID:               "r2",
		RoomType:             model.RoomTypeBotDM,
		HomeSiteID:           "site-A",
		Accounts:             []string{"weather.bot"},
		RequesterAccount:     "alice",
		RequesterEngName:     "Alice",
		RequesterChineseName: "爱丽丝",
		AppName:              "Weather Bot",
		Timestamp:            1,
	}
	var dst model.RoomCreatedOutbox
	raw := roundTrip(t, out, &dst)
	assert.Equal(t, "Weather Bot", dst.AppName)
	assert.Contains(t, string(raw), `"appName":"Weather Bot"`)
}

func TestErrorResponseRoomIDOmitempty(t *testing.T) {
	er := model.ErrorResponse{Error: "internal"}
	body, err := json.Marshal(er)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "roomId")

	er2 := model.ErrorResponse{Error: "dm already exists", RoomID: "r1"}
	body2, err := json.Marshal(er2)
	require.NoError(t, err)
	assert.Contains(t, string(body2), `"roomId":"r1"`)
}

func TestAsyncJobResultShape(t *testing.T) {
	// Verify the migrated struct uses Operation/Status (not Job/Success).
	r := model.AsyncJobResult{
		RequestID: "req-1",
		Operation: model.AsyncJobOpRoomCreate,
		Status:    "ok",
		RoomID:    "r1",
		Timestamp: 1,
	}
	var dst model.AsyncJobResult
	raw := roundTrip(t, r, &dst)
	assert.Equal(t, "ok", dst.Status)
	assert.Equal(t, model.AsyncJobOpRoomCreate, dst.Operation)
	assert.Equal(t, "r1", dst.RoomID)
	assert.NotContains(t, string(raw), `"job"`)
	assert.NotContains(t, string(raw), `"success"`)

	// Error path — RoomID omitted when empty.
	r2 := model.AsyncJobResult{Operation: model.AsyncJobOpRoomMemberAdd, Status: "error", Error: "failed"}
	raw2, _ := json.Marshal(r2)
	assert.NotContains(t, string(raw2), `"roomId"`)
}

func TestAsyncJobResultOpConstants(t *testing.T) {
	assert.Equal(t, "room.create", model.AsyncJobOpRoomCreate)
	assert.Equal(t, "room.member.add", model.AsyncJobOpRoomMemberAdd)
	assert.Equal(t, "room.member.remove", model.AsyncJobOpRoomMemberRemove)
	assert.Equal(t, "room.member.remove_org", model.AsyncJobOpRoomMemberRemoveOrg)
	assert.Equal(t, "room.member.role_update", model.AsyncJobOpRoomMemberRoleUpdate)
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=pkg/model 2>&1 | head -25`

Expected: `Operation`/`Status` fields undefined on `AsyncJobResult`; undefined `RoomCreatedOutbox`; undefined `ErrorResponse.RoomID`; undefined op constants.

- [ ] **Step 4: Migrate `AsyncJobResult`, add `RoomCreatedOutbox`, extend `ErrorResponse`, add constants**

Edit `pkg/model/event.go`:

**4a. Replace the existing `AsyncJobResult` struct** (currently has `Job string` and `Success bool`) with the canonical shape:

```go
type AsyncJobResult struct {
	RequestID string `json:"requestId"`
	Operation string `json:"operation"`         // see AsyncJobOp* constants
	Status    string `json:"status"`             // "ok" | "error"
	RoomID    string `json:"roomId,omitempty"`   // populated for room.create
	Error     string `json:"error,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

const (
	AsyncJobOpRoomCreate           = "room.create"
	AsyncJobOpRoomMemberAdd        = "room.member.add"
	AsyncJobOpRoomMemberRemove     = "room.member.remove"
	AsyncJobOpRoomMemberRemoveOrg  = "room.member.remove_org"
	AsyncJobOpRoomMemberRoleUpdate = "room.member.role_update"
)
```

**4b. Update the three existing call sites in `room-worker/handler.go`** that pass `job string` and `jobErr error` to `publishAsyncJobResult`. Replace each with the typed constant and `Status` field:

| Old | New |
|-----|-----|
| `"add_members"` | `AsyncJobOpRoomMemberAdd` |
| `"remove_member"` | `AsyncJobOpRoomMemberRemove` |
| `"remove_org"` | `AsyncJobOpRoomMemberRemoveOrg` |

Also update `publishAsyncJobResult`'s signature if it still takes a bare `job string` — change it to accept `model.AsyncJobResult` directly (or keep `(account, op string, err error)` and build the struct internally using `Status: "ok"/"error"`).

**4c. Add `RoomID` to `ErrorResponse`:**

```go
type ErrorResponse struct {
	Error  string `json:"error"`
	RoomID string `json:"roomId,omitempty"`
}
```

**4d. Append `RoomCreatedOutbox` to the file:**

```go
// RoomCreatedOutbox is the cross-site payload published by room-worker
// on the home site whenever a new room is created and at least one
// member lives on the destination site. Wrapped in OutboxEvent.
type RoomCreatedOutbox struct {
	RoomID               string   `json:"roomId"`
	RoomType             RoomType `json:"roomType"`
	RoomName             string   `json:"roomName"`
	HomeSiteID           string   `json:"homeSiteId"`
	Accounts             []string `json:"accounts"`
	RequesterAccount     string   `json:"requesterAccount"`
	RequesterEngName     string   `json:"requesterEngName"`
	RequesterChineseName string   `json:"requesterChineseName"`
	AppName              string   `json:"appName,omitempty"`
	Timestamp            int64    `json:"timestamp"`
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=pkg/model`

Expected: PASS.

Run: `make test SERVICE=room-worker` — confirm existing tests still pass after the `AsyncJobResult` caller updates.

- [ ] **Step 6: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go room-worker/handler.go
git commit -m "model: migrate AsyncJobResult to Operation/Status; add all AsyncJobOp constants; add RoomCreatedOutbox + ErrorResponse.RoomID"
```

---

## Phase 2 — `pkg/subject` and `pkg/pipelines`

### Task 7: Add `RoomCreate` and `RoomCreateWildcard` subject builders

**Files:**
- Modify: `pkg/subject/subject.go`
- Test: `pkg/subject/subject_test.go`

- [ ] **Step 1: Inspect existing builders**

Run: `grep -n "func RoomCanonical\|func MemberAdd\|func MemberAddWildcard" pkg/subject/subject.go`

Confirm pattern: each builder has a `func Foo(...) string` and a `func FooWildcard(...) string`.

- [ ] **Step 2: Write the failing test**

Append to `pkg/subject/subject_test.go`:

```go
func TestRoomCreate(t *testing.T) {
	got := subject.RoomCreate("alice", "site-A")
	assert.Equal(t, "chat.user.alice.request.room.site-A.create", got)
}

func TestRoomCreateWildcard(t *testing.T) {
	got := subject.RoomCreateWildcard("site-A")
	assert.Equal(t, "chat.user.*.request.room.site-A.create", got)
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=pkg/subject 2>&1 | head -10`

Expected: undefined `subject.RoomCreate` / `subject.RoomCreateWildcard`.

- [ ] **Step 4: Add the builders**

Append to `pkg/subject/subject.go` near `MemberAdd` / `MemberAddWildcard`:

```go
// RoomCreate is the request/reply subject the client posts to in order
// to create a new room. The siteID segment is the requester's site —
// the room will be created there. NATS gateways route cross-site
// requests transparently.
func RoomCreate(account, siteID string) string {
	return fmt.Sprintf("chat.user.%s.request.room.%s.create", account, siteID)
}

// RoomCreateWildcard is the queue-subscription pattern room-service
// uses for create-room requests on its own site.
func RoomCreateWildcard(siteID string) string {
	return fmt.Sprintf("chat.user.*.request.room.%s.create", siteID)
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=pkg/subject`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "subject: add RoomCreate / RoomCreateWildcard builders"
```

---

### Task 8: Add `RoomCanonicalOperation` extractor (if missing)

**Files:**
- Modify: `pkg/subject/subject.go`
- Test: `pkg/subject/subject_test.go`

- [ ] **Step 1: Check whether the extractor already exists**

Run: `grep -n "func RoomCanonicalOperation" pkg/subject/subject.go`

If it returns a hit, skip this task entirely. Continue to Task 9.

- [ ] **Step 2: Write the failing test**

Append to `pkg/subject/subject_test.go`:

```go
func TestRoomCanonicalOperation(t *testing.T) {
	tests := map[string]struct {
		subject string
		want    string
		ok      bool
	}{
		"member.add":    {"chat.room.canonical.site-A.member.add", "member.add", true},
		"create":        {"chat.room.canonical.site-A.create", "create", true},
		"unrelated":     {"chat.user.alice.request.room.site-A.create", "", false},
		"too short":     {"chat.room.canonical.site-A", "", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			op, ok := subject.RoomCanonicalOperation(tc.subject)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, op)
		})
	}
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=pkg/subject 2>&1 | head -10`

Expected: undefined `subject.RoomCanonicalOperation`.

- [ ] **Step 4: Add the extractor**

Append to `pkg/subject/subject.go`:

```go
// RoomCanonicalOperation extracts the trailing operation token from a
// canonical room subject like "chat.room.canonical.{siteID}.{op}". It
// returns ("", false) when the subject does not match the expected
// shape. The operation may itself contain dots (e.g., "member.add"),
// so the implementation joins everything after the siteID segment.
func RoomCanonicalOperation(s string) (string, bool) {
	const prefix = "chat.room.canonical."
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(s, prefix)
	dot := strings.IndexByte(rest, '.')
	if dot == -1 {
		return "", false
	}
	op := rest[dot+1:]
	if op == "" {
		return "", false
	}
	return op, true
}
```

Verify `strings` is already imported at the top of `subject.go`. If not, add it.

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=pkg/subject`

Expected: PASS for all four sub-cases.

- [ ] **Step 6: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "subject: add RoomCanonicalOperation extractor"
```

---

### Task 9: Empty-`roomID` branch in `GetNewMembersPipeline`

**Files:**
- Modify: `pkg/pipelines/member.go`
- Test: `pkg/pipelines/member_test.go`

This unblocks room-service's capacity check at create time, when no room exists yet.

- [ ] **Step 1: Read the current pipeline**

Run: `cat pkg/pipelines/member.go`

Confirm the existing function signature `GetNewMembersPipeline(orgIDs, directAccounts []string, roomID string) bson.A` and the structure (filter by org/account, regex bot exclude, $lookup against subscriptions to filter "already subscribed").

- [ ] **Step 2: Write the failing test**

Append to `pkg/pipelines/member_test.go` (create the file if it doesn't exist; mirror the package layout used by other `*_test.go` files in `pkg/`):

```go
package pipelines_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"github.com/<your-org>/chat/pkg/pipelines"
)

func TestGetNewMembersPipelineEmptyRoomID(t *testing.T) {
	pipe := pipelines.GetNewMembersPipeline([]string{"org-fx"}, []string{"bob"}, "")
	require.NotEmpty(t, pipe)

	// When roomID is empty, the pipeline must NOT contain a $lookup
	// into the subscriptions collection. Walk the stages and assert.
	hasLookup := false
	for _, stage := range pipe {
		m, ok := stage.(bson.M)
		if !ok {
			continue
		}
		if _, found := m["$lookup"]; found {
			hasLookup = true
		}
	}
	assert.False(t, hasLookup, "empty-roomID branch must drop the subscriptions $lookup")
}

func TestGetNewMembersPipelineWithRoomIDStillHasLookup(t *testing.T) {
	pipe := pipelines.GetNewMembersPipeline([]string{"org-fx"}, []string{"bob"}, "r1")
	require.NotEmpty(t, pipe)

	hasLookup := false
	for _, stage := range pipe {
		m, ok := stage.(bson.M)
		if !ok {
			continue
		}
		if _, found := m["$lookup"]; found {
			hasLookup = true
		}
	}
	assert.True(t, hasLookup, "non-empty roomID must keep the subscriptions $lookup")
}
```

(Replace `<your-org>` with the actual go.mod module path — check `head -1 go.mod`.)

- [ ] **Step 3: Run — confirm one assertion fails**

Run: `make test SERVICE=pkg/pipelines 2>&1 | head -20`

Expected: `TestGetNewMembersPipelineEmptyRoomID` fails because the current code unconditionally adds the lookup.

- [ ] **Step 4: Modify the pipeline**

Edit `pkg/pipelines/member.go`. Wrap the lookup + final `$match` stages so they only run when `roomID != ""`:

```go
func GetNewMembersPipeline(orgIDs, directAccounts []string, roomID string) bson.A {
	orFilter := bson.A{}
	if len(orgIDs) > 0 {
		orFilter = append(orFilter, bson.M{"sectId": bson.M{"$in": orgIDs}})
	}
	if len(directAccounts) > 0 {
		orFilter = append(orFilter, bson.M{"account": bson.M{"$in": directAccounts}})
	}

	stages := bson.A{
		bson.M{"$match": bson.M{
			"$or":     orFilter,
			"account": bson.M{"$not": bson.Regex{Pattern: `(\.bot$|^p_)`, Options: ""}},
		}},
	}

	if roomID != "" {
		stages = append(stages,
			bson.M{"$lookup": bson.M{
				"from": "subscriptions",
				"let":  bson.M{"userAccount": "$account"},
				"pipeline": bson.A{
					bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
						bson.M{"$eq": bson.A{"$roomId", roomID}},
						bson.M{"$eq": bson.A{"$u.account", "$$userAccount"}},
					}}}},
					bson.M{"$limit": 1},
				},
				"as": "existingSub",
			}},
			bson.M{"$match": bson.M{"existingSub": bson.M{"$eq": bson.A{}}}},
		)
	}

	return stages
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=pkg/pipelines`

Expected: both pipeline tests pass.

- [ ] **Step 6: Commit**

```bash
git add pkg/pipelines/member.go pkg/pipelines/member_test.go
git commit -m "pipelines: empty-roomID branch in GetNewMembersPipeline"
```

---

## Phase 3 — `ReconcileMemberCounts` (room-worker store split)

### Task 10: Replace `ReconcileUserCount` with `ReconcileMemberCounts` in the interface

**Files:**
- Modify: `room-worker/store.go`
- Modify: `room-worker/handler.go` (the existing add-member call site)
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Locate current callers and the interface method**

Run:
```
grep -n "ReconcileUserCount" room-worker/
```

Note all sites — there should be one in `store.go` (interface), one in `store_mongo.go` (impl), and one in `handler.go` (called by `processAddMembers`).

- [ ] **Step 2: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestReconcileMemberCountsCalledByAddMembers(t *testing.T) {
	// Spec: add-member must call ReconcileMemberCounts (the split
	// version) instead of the pre-split ReconcileUserCount.
	t.Skip("placeholder — replaced by full processAddMembers test in Phase 4")
}
```

This is a placeholder; the real assertion lives in the Phase 4 retrofit task. We use it here as a tripwire so the package compiles after we rename the interface method.

- [ ] **Step 3: Rename in the interface**

Edit `room-worker/store.go`. Replace the old method line:

```go
ReconcileUserCount(ctx context.Context, roomID string) error
```

with:

```go
// ReconcileMemberCounts recomputes Room.UserCount (non-bot subs) and
// Room.AppCount (bot subs) by scanning the subscriptions collection,
// then writes both back to the rooms collection in a single update.
ReconcileMemberCounts(ctx context.Context, roomID string) error
```

- [ ] **Step 4: Update handler.go call sites**

In `room-worker/handler.go`, replace every `h.store.ReconcileUserCount(` with `h.store.ReconcileMemberCounts(`. Update the surrounding error-wrap message to say `reconcile member counts` instead of `reconcile user count`.

- [ ] **Step 5: Run — confirm package still builds**

Run: `make build SERVICE=room-worker`

Expected: success (the interface, impl, and call site agree on the new name).

If `store_mongo.go` still references the old method name and breaks the build, that's fine — Task 11 fixes the implementation. To unblock now, either delete the old method body or rename it tentatively in `store_mongo.go`.

- [ ] **Step 6: Regenerate mocks**

Run: `make generate SERVICE=room-worker`

Expected: `room-worker/mock_store_test.go` regenerated with the new method.

- [ ] **Step 7: Commit (allow tests to be temporarily broken until Task 11)**

```bash
git add room-worker/store.go room-worker/handler.go room-worker/mock_store_test.go room-worker/handler_test.go
git commit -m "room-worker: rename ReconcileUserCount → ReconcileMemberCounts in interface + call sites"
```

---

### Task 11: Implement `ReconcileMemberCounts` in `store_mongo.go`

**Files:**
- Modify: `room-worker/store_mongo.go`
- Test: `room-worker/integration_test.go` (later; this task only adds an integration assertion)

- [ ] **Step 1: Read current implementation**

Run: `grep -n "ReconcileUserCount" room-worker/store_mongo.go`

Note the existing aggregation/update logic.

- [ ] **Step 2: Replace the implementation**

In `room-worker/store_mongo.go`, replace the old method with:

```go
// ReconcileMemberCounts counts the room's subscriptions, splitting on
// the ".bot" account suffix to produce both UserCount (non-bot) and
// AppCount (bot). Writes both fields to the rooms collection in a
// single updateOne.
func (s *MongoStore) ReconcileMemberCounts(ctx context.Context, roomID string) error {
	subs := s.db.Collection("subscriptions")

	userCount, err := subs.CountDocuments(ctx, bson.M{
		"roomId":     roomID,
		"u.account":  bson.M{"$not": bson.Regex{Pattern: `\.bot$`, Options: ""}},
	})
	if err != nil {
		return fmt.Errorf("count user subs: %w", err)
	}

	appCount, err := subs.CountDocuments(ctx, bson.M{
		"roomId":     roomID,
		"u.account":  bson.Regex{Pattern: `\.bot$`, Options: ""},
	})
	if err != nil {
		return fmt.Errorf("count app subs: %w", err)
	}

	rooms := s.db.Collection("rooms")
	if _, err := rooms.UpdateOne(ctx, bson.M{"_id": roomID}, bson.M{
		"$set": bson.M{
			"userCount": userCount,
			"appCount":  appCount,
			"updatedAt": time.Now().UTC(),
		},
	}); err != nil {
		return fmt.Errorf("update room counts: %w", err)
	}
	return nil
}
```

Verify imports include `"fmt"`, `"time"`, and `"go.mongodb.org/mongo-driver/v2/bson"`.

- [ ] **Step 3: Run unit tests — confirm package builds**

Run: `make test SERVICE=room-worker`

Expected: existing add-member unit tests still pass (mock-based; this implementation isn't exercised). PASS.

- [ ] **Step 4: Add an integration test**

Append to `room-worker/integration_test.go`:

```go
//go:build integration

func TestReconcileMemberCountsSplitsBots(t *testing.T) {
	ctx := context.Background()
	store, db := setupMongo(t)

	// Seed: 3 user subs and 1 bot sub for room r1.
	mustInsertSub(t, db, &model.Subscription{
		ID: "s1", User: model.SubscriptionUser{Account: "alice"}, RoomID: "r1",
	})
	mustInsertSub(t, db, &model.Subscription{
		ID: "s2", User: model.SubscriptionUser{Account: "bob"}, RoomID: "r1",
	})
	mustInsertSub(t, db, &model.Subscription{
		ID: "s3", User: model.SubscriptionUser{Account: "carol"}, RoomID: "r1",
	})
	mustInsertSub(t, db, &model.Subscription{
		ID: "s4", User: model.SubscriptionUser{Account: "weather.bot"}, RoomID: "r1",
	})
	mustInsertRoom(t, db, &model.Room{ID: "r1", Type: model.RoomTypeChannel})

	require.NoError(t, store.ReconcileMemberCounts(ctx, "r1"))

	got, err := store.GetRoom(ctx, "r1")
	require.NoError(t, err)
	assert.Equal(t, 3, got.UserCount)
	assert.Equal(t, 1, got.AppCount)
}
```

(`mustInsertSub` and `mustInsertRoom` are simple test helpers — declare them at the top of `integration_test.go` if not already there: `func mustInsertSub(t *testing.T, db *mongo.Database, sub *model.Subscription) { ... }` calling `db.Collection("subscriptions").InsertOne`.)

- [ ] **Step 5: Run integration test**

Run: `make test-integration SERVICE=room-worker -- -run TestReconcileMemberCountsSplitsBots`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add room-worker/store_mongo.go room-worker/integration_test.go
git commit -m "room-worker: implement ReconcileMemberCounts (UserCount/AppCount split)"
```

---

## Phase 4 — Add-Member Retrofit

The retrofit lands in two batches: 4a covers the room-worker write-path changes (deterministic sub IDs, `Subscription.Name`/`RoomType` population). 4b covers the cross-site outbox extension (`MemberAddEvent.RoomName`), the async-job notification, the inbox-worker counterpart, and updated tests.

### Task 12: Verify `X-Request-ID` is mandatory in `processAddMembers`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

PR #131 wired `X-Request-ID` propagation but may not yet hard-fail on missing header. This task makes that policy explicit.

- [ ] **Step 1: Inspect current behavior**

Run:
```
grep -n "RequestIDFromContext\|X-Request-ID" room-worker/handler.go
```

Note where the request ID is read in `processAddMembers`. If there's no rejection on empty value, this task adds it.

- [ ] **Step 2: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersRequiresRequestID(t *testing.T) {
	h, _ := newTestHandler(t)
	body, err := json.Marshal(model.AddMembersRequest{
		RoomID:           "r1",
		Users:            []string{"bob"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		Timestamp:        time.Now().UnixMilli(),
	})
	require.NoError(t, err)

	// Pass a context with NO request ID set.
	err = h.processAddMembers(context.Background(), body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing X-Request-ID")
}
```

(`newTestHandler` is the existing helper in `handler_test.go`; if absent, declare a small constructor that builds a `Handler` with a mock store and the no-op publisher.)

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersRequiresRequestID`

Expected: FAIL — handler doesn't currently reject missing header.

- [ ] **Step 4: Add the guard at the top of `processAddMembers`**

In `room-worker/handler.go`, immediately after unmarshalling the request:

```go
requestID, _ := natsutil.RequestIDFromContext(ctx)
if requestID == "" {
    return fmt.Errorf("missing X-Request-ID: %w", errPermanent)
}
```

If `errPermanent` doesn't exist yet in this package, add it as a package-level sentinel:

```go
var errPermanent = errors.New("permanent")
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersRequiresRequestID`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: require X-Request-ID in processAddMembers"
```

---

### Task 13: Switch subscription IDs to `GenerateUUIDv7()` in `processAddMembers`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Locate the current sub-ID generation**

Run: `grep -n "GenerateID()" room-worker/handler.go`

Confirm `processAddMembers` currently uses `idgen.GenerateID()` for `Subscription.ID`.

- [ ] **Step 2: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersSubIDsAreUUIDv7(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "01970a4f-8c2d-7c9a-abcd-e0123456789f"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectAddMembersHappyPath(t, mocks, []string{"bob", "carol"}) // helper sketched below

	captured := captureBulkSubsArg(mocks)
	body := mustMarshalAddMembers(t, "r1", []string{"bob", "carol"})

	require.NoError(t, h.processAddMembers(ctx, body))

	got := captured.Args()
	require.Len(t, got, 2)
	// IDs must be 32-char hex (UUIDv7 without hyphens), non-empty, and differ from each other.
	assert.Len(t, got[0].ID, 32)
	assert.Len(t, got[1].ID, 32)
	assert.NotEqual(t, got[0].ID, got[1].ID)
}
```

Add the small helpers near the top of the test file, or in a `_helpers_test.go`:

```go
func expectAddMembersHappyPath(t *testing.T, mocks *handlerMocks, accounts []string) {
	t.Helper()
	mocks.store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mocks.store.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return(accounts, nil)
	users := make([]model.User, len(accounts))
	for i, a := range accounts {
		users[i] = model.User{ID: "u_" + a, Account: a, SiteID: "site-A", EngName: "X", ChineseName: "X"}
	}
	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), accounts).Return(users, nil)
	mocks.store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).
		DoAndReturn(captureBulkSubs).Times(1)
	mocks.store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)
}

// captureBulkSubs is a DoAndReturn func that records the slice arg so
// tests can inspect the constructed Subscription values.
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersSubIDsAreUUIDv7`

Expected: FAIL — IDs are 17-char base62 from old `GenerateID()`, not 32-char hex.

- [ ] **Step 4: Switch the ID generation**

In `room-worker/handler.go` `processAddMembers`, locate the sub-construction loop:

```go
sub := &model.Subscription{
    ID:       idgen.GenerateID(),
    ...
}
```

Replace with:

```go
sub := &model.Subscription{
    ID:       idgen.GenerateUUIDv7(),
    ...
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersSubIDsAreUUIDv7`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: use GenerateUUIDv7 for sub IDs in processAddMembers"
```

---

### Task 14: Populate `Subscription.Name` and `Subscription.RoomType` in `processAddMembers`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersPopulatesNameAndRoomType(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectAddMembersHappyPath(t, mocks, []string{"bob"})
	captured := captureBulkSubsArg(mocks)
	body := mustMarshalAddMembers(t, "r1", []string{"bob"})

	require.NoError(t, h.processAddMembers(ctx, body))

	got := captured.Args()
	require.Len(t, got, 1)
	assert.Equal(t, "deal team", got[0].Name) // Room.Name from happy-path stub
	assert.Equal(t, model.RoomTypeChannel, got[0].RoomType)
	assert.Empty(t, got[0].SidebarName)
	assert.False(t, got[0].IsSubscribed)
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersPopulatesNameAndRoomType`

Expected: FAIL — `sub.Name` and `sub.RoomType` are still zero.

- [ ] **Step 3: Set the new fields in the sub-construction loop**

In `room-worker/handler.go` inside `processAddMembers`, in the same loop that builds `sub`:

```go
sub := &model.Subscription{
    ID:       idgen.GenerateUUIDv7(),
    User:     model.SubscriptionUser{ID: user.ID, Account: user.Account},
    RoomID:   req.RoomID,
    SiteID:   room.SiteID,
    Roles:    []model.Role{model.RoleMember},
    Name:     room.Name,
    RoomType: room.Type, (always model.RoomTypeChannel for add-member)
    JoinedAt: acceptedAt,
}
```

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersPopulatesNameAndRoomType`

Expected: PASS.

- [ ] **Step 5: Run the rest of the room-worker test suite to catch regressions**

Run: `make test SERVICE=room-worker`

Expected: PASS for everything that was passing before.

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: populate Sub.Name + Sub.RoomType in processAddMembers"
```

---

### Task 14b: History timestamp fallback in `processAddMembers`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

When the add-members request specifies history config `"none"` but omits the `since` timestamp, use `time.Now().UTC()` rather than leaving `HistorySharedSince` nil (which would grant full history access) or returning an error.

- [ ] **Step 1: Write the failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersHistoryNoneWithTimestamp(t *testing.T) {
	h, mocks := newTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "01970a4f-8c2d-7c9a-abcd-e0123456789f")

	ts := int64(1740000000000)
	expectAddMembersHappyPath(t, mocks, []string{"bob"})
	captured := captureBulkSubsArg(mocks)
	body := mustMarshalAddMembersWithHistory(t, "r1", []string{"bob"}, "none", &ts)

	require.NoError(t, h.processAddMembers(ctx, body))

	got := captured.Args()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].HistorySharedSince)
	assert.Equal(t, time.UnixMilli(ts).UTC(), *got[0].HistorySharedSince)
}

func TestProcessAddMembersHistoryNoneWithoutTimestamp(t *testing.T) {
	h, mocks := newTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "01970a4f-8c2d-7c9a-abcd-e0123456789f")

	before := time.Now().UTC()
	expectAddMembersHappyPath(t, mocks, []string{"bob"})
	captured := captureBulkSubsArg(mocks)
	body := mustMarshalAddMembersWithHistory(t, "r1", []string{"bob"}, "none", nil)

	require.NoError(t, h.processAddMembers(ctx, body))

	got := captured.Args()
	require.Len(t, got, 1)
	// HistorySharedSince must be set (non-nil) and close to now.
	require.NotNil(t, got[0].HistorySharedSince)
	assert.True(t, !got[0].HistorySharedSince.Before(before),
		"HistorySharedSince should be >= time before handler ran")
}

func TestProcessAddMembersNoHistoryConfigLeavesNil(t *testing.T) {
	h, mocks := newTestHandler(t)
	ctx := natsutil.WithRequestID(context.Background(), "01970a4f-8c2d-7c9a-abcd-e0123456789f")

	expectAddMembersHappyPath(t, mocks, []string{"bob"})
	captured := captureBulkSubsArg(mocks)
	body := mustMarshalAddMembers(t, "r1", []string{"bob"}) // no history config

	require.NoError(t, h.processAddMembers(ctx, body))

	got := captured.Args()
	require.Len(t, got, 1)
	assert.Nil(t, got[0].HistorySharedSince)
}
```

(`mustMarshalAddMembersWithHistory` marshals an `AddMembersRequest` that includes a `HistoryConfig` and optional `HistorySharedSince` field.)

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run "TestProcessAddMembersHistory"`

Expected: FAIL — no fallback logic yet; `HistorySharedSince` is nil when timestamp is absent.

- [ ] **Step 3: Implement the fallback**

In `room-worker/handler.go`, in the sub-construction logic of `processAddMembers`:

```go
if req.HistoryConfig == "none" {
    since := now  // fall back to current time
    if req.HistorySharedSince != nil {
        since = time.UnixMilli(*req.HistorySharedSince).UTC()
    }
    sub.HistorySharedSince = &since
}
```

Apply the same fallback when populating `MemberAddEvent.HistorySharedSince` for the outbox publish:

```go
var historySharedSincePtr *int64
if req.HistoryConfig == "none" {
    var ts int64
    if req.HistorySharedSince != nil {
        ts = *req.HistorySharedSince
    } else {
        ts = now.UnixMilli()
    }
    historySharedSincePtr = &ts
}
```

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run "TestProcessAddMembersHistory"`

Expected: all three cases PASS.

Run full suite: `make test SERVICE=room-worker`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: fall back to current timestamp when history config is none but timestamp absent"
```

---

### Task 15: Propagate `RoomName` through `MemberAddEvent` to outbox

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

`MemberAddEvent.RoomName` was added to the model in Task 5; this task wires the producer side so cross-site subs can be named correctly by inbox-worker.

- [ ] **Step 1: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersOutboxCarriesRoomName(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	// Cross-site member: bob lives on site-B.
	mocks.store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Name: "deal team", Type: model.RoomTypeChannel, SiteID: "site-A",
	}, nil)
	mocks.store.EXPECT().ListNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "r1").
		Return([]string{"bob"}, nil)
	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-B", EngName: "Bob", ChineseName: "鲍勃"},
	}, nil)
	mocks.store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mocks.store.EXPECT().HasOrgRoomMembers(gomock.Any(), "r1").Return(false, nil)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r1").Return(nil)

	body := mustMarshalAddMembers(t, "r1", []string{"bob"})
	require.NoError(t, h.processAddMembers(ctx, body))

	// Inspect captured publish messages on the outbox subject.
	outbox := mocks.publisher.OutboxFor("site-B", "member_added")
	require.Len(t, outbox, 1)
	var envelope model.OutboxEvent
	require.NoError(t, json.Unmarshal(outbox[0].Data, &envelope))
	var evt model.MemberAddEvent
	require.NoError(t, json.Unmarshal(envelope.Payload, &evt))
	assert.Equal(t, "deal team", evt.RoomName)
}
```

The test depends on `mocks.publisher.OutboxFor(destSite, eventType)` returning all captured outbox publishes. If the existing publisher mock doesn't expose this, add a thin wrapper like:

```go
func (p *capturingPublisher) OutboxFor(destSiteID, eventType string) []capturedMsg {
	prefix := fmt.Sprintf("outbox.site-A.to.%s.%s", destSiteID, eventType)
	out := []capturedMsg{}
	for _, m := range p.all {
		if m.Subject == prefix {
			out = append(out, m)
		}
	}
	return out
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersOutboxCarriesRoomName`

Expected: FAIL — `evt.RoomName` is empty.

- [ ] **Step 3: Set `RoomName` in the outbox `MemberAddEvent`**

In `room-worker/handler.go` `processAddMembers`, find the loop that builds the per-destination-site `MemberAddEvent`. Add `RoomName: room.Name`:

```go
siteEvt := model.MemberAddEvent{
    Type:               "member_added",
    RoomID:             req.RoomID,
    RoomName:           room.Name,
    Accounts:           accounts,
    SiteID:             room.SiteID,
    JoinedAt:           req.Timestamp,
    HistorySharedSince: historySharedSincePtr,
    Timestamp:          now.UnixMilli(),
}
```

If there's also a same-site `MemberAddEvent` published on the room's broadcast subject, add `RoomName` there too — keeping every emission of `MemberAddEvent` consistent.

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersOutboxCarriesRoomName`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: emit MemberAddEvent.RoomName for cross-site Sub.Name"
```

---

### Task 16: Async-job notification at end of `processAddMembers`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Check whether `publishAsyncJobResult` already exists**

Run: `grep -rn "publishAsyncJobResult\|AsyncJobResult" room-worker/`

If a helper exists from PR #131, reuse it. If not, this task adds it.

- [ ] **Step 2: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembersPublishesAsyncJobOnSuccess(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectAddMembersHappyPath(t, mocks, []string{"bob"})
	body := mustMarshalAddMembers(t, "r1", []string{"bob"})
	require.NoError(t, h.processAddMembers(ctx, body))

	jobs := mocks.publisher.UserResponseFor("alice")
	require.Len(t, jobs, 1)
	var got model.AsyncJobResult
	require.NoError(t, json.Unmarshal(jobs[0].Data, &got))
	assert.Equal(t, reqID, got.RequestID)
	assert.Equal(t, model.AsyncJobOpRoomMemberAdd, got.Operation)
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "r1", got.RoomID)
}
```

Add a `UserResponseFor(account string)` accessor to `capturingPublisher` mirroring `OutboxFor`, matching subject `chat.user.<account>.event.async-job-result` (or whatever PR #131 named it — check `pkg/subject` for `UserResponse`).

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersPublishesAsyncJobOnSuccess`

Expected: FAIL — no async-job result published.

- [ ] **Step 4: Add the publish call**

In `room-worker/handler.go`, immediately before `processAddMembers` returns `nil`:

```go
if err := h.publishAsyncJobResult(ctx, model.AsyncJobResult{
    RequestID: requestID,
    Operation: model.AsyncJobOpRoomMemberAdd,
    Status:    "ok",
    RoomID:    req.RoomID,
    Timestamp: now.UnixMilli(),
}); err != nil {
    slog.Error("publish async-job result failed",
        "error", err, "requestId", requestID, "operation", model.AsyncJobOpRoomMemberAdd)
}
return nil
```

If `publishAsyncJobResult` doesn't exist yet, add it as a method on `Handler`:

```go
func (h *Handler) publishAsyncJobResult(ctx context.Context, r model.AsyncJobResult) error {
    data, err := json.Marshal(r)
    if err != nil {
        return fmt.Errorf("marshal async-job result: %w", err)
    }
    msg := natsutil.NewMsg(ctx, subject.UserResponse(r.requesterAccountFromRoutingCtx()), data)
    // requesterAccount must be available in scope — pass it explicitly:
    return h.nc.PublishMsg(msg)
}
```

Cleaner shape — pass the account explicitly so the helper is reusable across operations:

```go
func (h *Handler) publishAsyncJobResult(ctx context.Context, account string, r model.AsyncJobResult) error {
    data, err := json.Marshal(r)
    if err != nil {
        return fmt.Errorf("marshal async-job result: %w", err)
    }
    msg := natsutil.NewMsg(ctx, subject.UserResponse(account), data)
    return h.nc.PublishMsg(msg)
}
```

Update the call site to pass `req.RequesterAccount` as the second argument.

- [ ] **Step 5: Add the error-path counterpart**

The dispatcher pattern in spec §8 splits errors into permanent vs retryable. For this retrofit, add a wrapper at the handler dispatcher (in `room-worker/main.go` or wherever the consumer loop lives):

```go
err := h.processAddMembers(ctx, msg.Data())
switch {
case err == nil:
    msg.Ack()
case errors.Is(err, errPermanent):
    requestID, _ := natsutil.RequestIDFromContext(ctx)
    var req model.AddMembersRequest
    _ = json.Unmarshal(msg.Data(), &req) // best-effort; account is the key field
    _ = h.publishAsyncJobResult(ctx, req.RequesterAccount, model.AsyncJobResult{
        RequestID: requestID,
        Operation: model.AsyncJobOpRoomMemberAdd,
        Status:    "error",
        RoomID:    req.RoomID,
        Error:     sanitizeErrorString(err),
        Timestamp: time.Now().UnixMilli(),
    })
    msg.Ack()
default:
    msg.Nak()
}
```

`sanitizeErrorString` is the existing error-sanitization helper from room-worker (or a new wrapper around the room-service `sanitizeError` if not already shared). If the helper lives only in room-service today, copy a minimal version into room-worker — small string-allowlist filter so we don't leak internal Mongo errors back to clients.

- [ ] **Step 6: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessAddMembersPublishesAsyncJobOnSuccess`

Expected: PASS.

Run the full handler suite: `make test SERVICE=room-worker`. Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add room-worker/handler.go room-worker/main.go room-worker/handler_test.go
git commit -m "room-worker: emit AsyncJobResult on processAddMembers success/permanent error"
```

---

### Task 17: `inbox-worker.handleMemberAdded` populates `Subscription.Name` + `RoomType`

**Files:**
- Modify: `inbox-worker/handler.go`
- Test: `inbox-worker/handler_test.go`

- [ ] **Step 1: Locate the existing handler**

Run: `grep -n "handleMemberAdded" inbox-worker/handler.go`

Find the loop that builds `*model.Subscription` from `data.Accounts`.

- [ ] **Step 2: Write the failing test**

Append to `inbox-worker/handler_test.go`:

```go
func TestHandleMemberAddedSetsNameAndRoomType(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-B"},
	}, nil)
	captured := captureBulkSubsArg(mocks)

	evtBytes, err := json.Marshal(model.MemberAddEvent{
		Type:      "member_added",
		RoomID:    "r1",
		RoomName:  "deal team",
		Accounts:  []string{"bob"},
		SiteID:    "site-A",
		JoinedAt:  1740000000000,
		Timestamp: 1740000000000,
	})
	require.NoError(t, err)

	require.NoError(t, h.handleMemberAdded(ctx, model.OutboxEvent{
		Type:    "member_added",
		Payload: evtBytes,
	}))

	got := captured.Args()
	require.Len(t, got, 1)
	assert.Equal(t, "deal team", got[0].Name)
	assert.Equal(t, model.RoomTypeChannel, got[0].RoomType)
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=inbox-worker -- -run TestHandleMemberAddedSetsNameAndRoomType`

Expected: FAIL — `Name` and `RoomType` are zero on the constructed sub.

- [ ] **Step 4: Update `handleMemberAdded`**

In `inbox-worker/handler.go`, in the sub-construction loop:

```go
sub := &model.Subscription{
    ID:       idgen.GenerateUUIDv7(),
    User:     model.SubscriptionUser{ID: u.ID, Account: u.Account},
    RoomID:   data.RoomID,
    SiteID:   data.SiteID,
    Roles:    []model.Role{model.RoleMember},
    Name:     data.RoomName,
    RoomType: model.RoomTypeChannel,   // member_added events only fire for channels today
    JoinedAt: time.UnixMilli(data.JoinedAt).UTC(),
}
```

Redelivery idempotency is provided by the unique compound index `(roomId, u.account)` on the `subscriptions` collection — duplicate-key on that index is treated as success.

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=inbox-worker -- -run TestHandleMemberAddedSetsNameAndRoomType`

Expected: PASS.

Run the full inbox-worker test suite: `make test SERVICE=inbox-worker`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go
git commit -m "inbox-worker: handleMemberAdded populates Sub.Name + Sub.RoomType from MemberAddEvent.RoomName"
```

---

## Phase 5 — room-service create-room

Phase 5 lands across three batches: 5a (sentinels, helpers, sanitize-error), 5b (store interface + Mongo + indices), 5c (`handleCreateRoom` validation pipeline + main.go subscription + handler tests).

### Task 18: Sentinel errors + `dmExistsError` typed wrapper

**Files:**
- Modify: `room-service/helper.go`
- Test: `room-service/helper_test.go`

- [ ] **Step 1: Read existing sentinels**

Run: `grep -n "errors.New\|errNotRoomMember" room-service/helper.go`

Find the existing block of sentinel `var (...)` declarations.

- [ ] **Step 2: Write the failing test**

Append to `room-service/helper_test.go`:

```go
func TestNewSentinelErrorsExist(t *testing.T) {
	// Existence checks — make sure the imports compile and the sentinels
	// have the expected message.
	assert.Equal(t, "request must include at least one of users, orgs, channels, or name", errEmptyCreateRequest.Error())
	assert.Equal(t, "cannot create a DM with yourself", errSelfDM.Error())
	assert.Equal(t, "bots cannot be added to a channel during creation", errBotInChannel.Error())
	assert.Equal(t, "bot not available", errBotNotAvailable.Error())
	assert.Equal(t, "user is missing required name fields", errInvalidUserData.Error())
	assert.Equal(t, "missing X-Request-ID header", errMissingRequestID.Error())
	assert.Equal(t, "user not found", errUserNotFound.Error())
}

func TestDMExistsErrorWrapsCorrectly(t *testing.T) {
	e := newDMExistsError("r_existing")
	assert.Equal(t, "dm already exists", e.Error())
	assert.Equal(t, "r_existing", e.RoomID())

	// errors.Is should match against any *dmExistsError.
	var sentinel *dmExistsError
	assert.True(t, errors.Is(e, sentinel))

	// Wrapping it via fmt.Errorf still allows .RoomID via errors.As.
	wrapped := fmt.Errorf("validation failed: %w", e)
	var target *dmExistsError
	require.True(t, errors.As(wrapped, &target))
	assert.Equal(t, "r_existing", target.RoomID())
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=room-service -- -run "TestNewSentinelErrorsExist|TestDMExistsErrorWrapsCorrectly"`

Expected: undefined identifiers.

- [ ] **Step 4: Add the sentinels and the typed wrapper**

In `room-service/helper.go`, add to the existing sentinel `var (...)` block:

```go
var (
	// ...existing sentinels...
	errEmptyCreateRequest = errors.New("request must include at least one of users, orgs, channels, or name")
	errSelfDM             = errors.New("cannot create a DM with yourself")
	errBotInChannel       = errors.New("bots cannot be added to a channel during creation")
	errBotNotAvailable    = errors.New("bot not available")
	errInvalidUserData    = errors.New("user is missing required name fields")
	errMissingRequestID   = errors.New("missing X-Request-ID header")
	errUserNotFound       = errors.New("user not found")
)
```

Append the typed wrapper:

```go
// dmExistsError carries the existing DM/botDM room ID through the error
// chain so the natsCreateRoom handler can populate the special
// "dm already exists" reply with the existing roomId.
type dmExistsError struct{ existingRoomID string }

func newDMExistsError(roomID string) *dmExistsError {
	return &dmExistsError{existingRoomID: roomID}
}

func (e *dmExistsError) Error() string  { return "dm already exists" }
func (e *dmExistsError) RoomID() string { return e.existingRoomID }
func (e *dmExistsError) Is(target error) bool {
	_, ok := target.(*dmExistsError)
	return ok
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestNewSentinelErrorsExist|TestDMExistsErrorWrapsCorrectly"`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add room-service/helper.go room-service/helper_test.go
git commit -m "room-service: sentinel errors + dmExistsError wrapper for create-room"
```

---

### Task 19: `stripAccount`, `composeAutoName`, `truncateRunes` helpers

**Files:**
- Modify: `room-service/helper.go`
- Test: `room-service/helper_test.go`

- [ ] **Step 1: Write the failing test**

Append to `room-service/helper_test.go`:

```go
func TestStripAccount(t *testing.T) {
	tests := map[string]struct {
		in      []string
		account string
		want    []string
	}{
		"present":           {[]string{"alice", "bob", "carol"}, "bob", []string{"alice", "carol"}},
		"absent":            {[]string{"alice", "carol"}, "bob", []string{"alice", "carol"}},
		"first":             {[]string{"alice", "bob"}, "alice", []string{"bob"}},
		"only-element":      {[]string{"alice"}, "alice", []string{}},
		"multiple-occur":    {[]string{"alice", "alice", "bob"}, "alice", []string{"bob"}},
		"empty":             {[]string{}, "alice", []string{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := stripAccount(tc.in, tc.account)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "hello", truncateRunes("hello", 100))
	assert.Equal(t, "hel", truncateRunes("hello", 3))
	// Multi-byte runes — Chinese characters count as 1 rune each.
	assert.Equal(t, "你好", truncateRunes("你好世界", 2))
	got := truncateRunes("你好世界", 100)
	assert.Equal(t, "你好世界", got)
}

func TestComposeAutoName(t *testing.T) {
	tests := map[string]struct {
		users    []string
		orgs     []string
		channels []model.ChannelRef
		want     string
	}{
		"users only":           {[]string{"alice", "bob"}, nil, nil, "alice, bob"},
		"users + orgs":         {[]string{"alice"}, []string{"org-fx"}, nil, "alice, org-fx"},
		"users + orgs + chans": {[]string{"alice"}, []string{"org-fx"}, []model.ChannelRef{{RoomID: "r0"}}, "alice, org-fx, r0"},
		"channels only":        {nil, nil, []model.ChannelRef{{RoomID: "r0"}, {RoomID: "r1"}}, "r0, r1"},
		"empty everything":     {nil, nil, nil, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := composeAutoName(tc.users, tc.orgs, tc.channels)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestComposeAutoNameTruncatesAt100Runes(t *testing.T) {
	users := make([]string, 0)
	for i := 0; i < 50; i++ {
		users = append(users, "userwithlongaccountname")
	}
	got := composeAutoName(users, nil, nil)
	assert.Equal(t, 100, utf8.RuneCountInString(got))
}
```

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-service -- -run "TestStripAccount|TestTruncateRunes|TestComposeAutoName"`

Expected: undefined helpers.

- [ ] **Step 3: Implement the helpers**

Append to `room-service/helper.go`:

```go
func stripAccount(slice []string, account string) []string {
	out := make([]string, 0, len(slice))
	for _, s := range slice {
		if s != account {
			out = append(out, s)
		}
	}
	return out
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	out := make([]rune, 0, max)
	for _, r := range s {
		out = append(out, r)
		if len(out) == max {
			break
		}
	}
	return string(out)
}

func composeAutoName(users, orgs []string, channels []model.ChannelRef) string {
	parts := make([]string, 0, len(users)+len(orgs)+len(channels))
	parts = append(parts, users...)
	parts = append(parts, orgs...)
	for _, c := range channels {
		parts = append(parts, c.RoomID)
	}
	joined := strings.Join(parts, ", ")
	return truncateRunes(joined, 100)
}
```

Verify imports include `"strings"`, `"unicode/utf8"`, and the model package.

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestStripAccount|TestTruncateRunes|TestComposeAutoName"`

Expected: PASS for all subcases.

- [ ] **Step 5: Commit**

```bash
git add room-service/helper.go room-service/helper_test.go
git commit -m "room-service: stripAccount, truncateRunes, composeAutoName helpers"
```

---

### Task 20: Extend `sanitizeError` allowlist for the new sentinels

**Files:**
- Modify: `room-service/helper.go`
- Test: `room-service/helper_test.go`

- [ ] **Step 1: Read current allowlist**

Run: `grep -n "sanitizeError" room-service/helper.go`

Find the function. Note the existing allowlist of safe substrings (the spec calls them out under §8 Error Handling).

- [ ] **Step 2: Write the failing test**

Append to `room-service/helper_test.go`:

```go
func TestSanitizeErrorPassesThroughCreateRoomSentinels(t *testing.T) {
	cases := []error{
		errEmptyCreateRequest,
		errSelfDM,
		errBotInChannel,
		errBotNotAvailable,
		errInvalidUserData,
		errMissingRequestID,
		errUserNotFound,
		newDMExistsError("r_existing"),
		fmt.Errorf("validation: %w", errSelfDM),
	}
	for _, e := range cases {
		t.Run(e.Error(), func(t *testing.T) {
			assert.Equal(t, e.Error(), sanitizeError(e))
		})
	}
}

func TestSanitizeErrorCollapsesUnknown(t *testing.T) {
	got := sanitizeError(errors.New("mongo: connection refused: tcp 127.0.0.1:27017"))
	assert.Equal(t, "internal error", got)
}
```

- [ ] **Step 3: Run — confirm failure**

Run: `make test SERVICE=room-service -- -run TestSanitizeError`

Expected: at least one new sentinel collapses to "internal error".

- [ ] **Step 4: Extend the allowlist**

In `room-service/helper.go` `sanitizeError`, add the new sentinel checks before the default fallthrough:

```go
case errors.Is(err, errEmptyCreateRequest),
     errors.Is(err, errSelfDM),
     errors.Is(err, errBotInChannel),
     errors.Is(err, errBotNotAvailable),
     errors.Is(err, errInvalidUserData),
     errors.Is(err, errMissingRequestID),
     errors.Is(err, errUserNotFound),
     errors.Is(err, &dmExistsError{}):
    return err.Error()
```

If the existing function uses `strings.Contains`-style allowlisting instead of `errors.Is`, add these substrings to the allowlist:

```go
"request must include at least one of",
"cannot create a DM with yourself",
"bots cannot be added",
"bot not available",
"user is missing required",
"missing X-Request-ID",
"user not found",
"dm already exists",
```

Pick whichever style matches the existing function — don't mix.

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run TestSanitizeError`

Expected: PASS for all subcases.

- [ ] **Step 6: Commit**

```bash
git add room-service/helper.go room-service/helper_test.go
git commit -m "room-service: extend sanitizeError allowlist with create-room sentinels"
```

---

### Task 21: Store interface — add `GetUser`, `GetApp`, `FindDMSubscription`

**Files:**
- Modify: `room-service/store.go`
- Modify: `room-service/mock_store_test.go` (regenerated)

- [ ] **Step 1: Read existing interface**

Run: `grep -n "type RoomStore interface" room-service/store.go`

Find the interface and the `//go:generate mockgen` directive.

- [ ] **Step 2: Add the new methods to the interface**

In `room-service/store.go`, append three methods:

```go
type RoomStore interface {
	// ...existing methods...

	// GetUser returns the user document by account, or ErrUserNotFound
	// when no document matches. Used by create-room to resolve the
	// requester and the DM/botDM counterpart.
	GetUser(ctx context.Context, account string) (*model.User, error)

	// GetApp returns the apps doc whose Assistant.Name equals
	// botAccount, or ErrAppNotFound. Used to gate botDM creation on
	// App.Assistant.Enabled.
	GetApp(ctx context.Context, botAccount string) (*model.App, error)

	// FindDMSubscription returns the requester's existing dm/botDM
	// subscription whose Name == targetName, or ErrSubscriptionNotFound.
	// The query filters on Subscription.RoomType to avoid false
	// positives where a channel happens to be named after an account.
	FindDMSubscription(ctx context.Context, account, targetName string) (*model.Subscription, error)
}
```

Add the matching error sentinels in the same file (or wherever existing store sentinels live):

```go
var (
	// ErrUserNotFound is returned by store.GetUser when no users doc
	// matches the supplied account.
	ErrUserNotFound = errors.New("user not found")

	// ErrAppNotFound is returned by store.GetApp when no apps doc
	// matches the supplied bot account.
	ErrAppNotFound = errors.New("app not found")
)
```

(`ErrSubscriptionNotFound` should already exist from add-member.)

- [ ] **Step 3: Regenerate the mock**

Run: `make generate SERVICE=room-service`

Expected: `room-service/mock_store_test.go` rewritten to expose `GetUser`, `GetApp`, `FindDMSubscription`.

- [ ] **Step 4: Verify compile**

Run: `make build SERVICE=room-service`

Expected: success — `store.go` compiles even though no caller invokes the new methods yet.

- [ ] **Step 5: Commit**

```bash
git add room-service/store.go room-service/mock_store_test.go
git commit -m "room-service: store interface adds GetUser, GetApp, FindDMSubscription"
```

---

### Task 22: Mongo implementations of `GetUser`, `GetApp`, `FindDMSubscription`

**Files:**
- Modify: `room-service/store_mongo.go`

These are exercised end-to-end via integration tests in Phase 8. Phase 5 keeps unit tests on the handler-side mocks.

- [ ] **Step 1: Locate the existing impl block**

Run: `grep -n "func (s \*MongoStore)" room-service/store_mongo.go | head -10`

Confirm the receiver type and the convention used by existing methods.

- [ ] **Step 2: Add `GetUser`**

Append to `room-service/store_mongo.go`:

```go
func (s *MongoStore) GetUser(ctx context.Context, account string) (*model.User, error) {
	var u model.User
	err := s.db.Collection("users").FindOne(ctx, bson.M{"account": account}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user %q: %w", account, err)
	}
	return &u, nil
}
```

- [ ] **Step 3: Add `GetApp`**

```go
func (s *MongoStore) GetApp(ctx context.Context, botAccount string) (*model.App, error) {
	var a model.App
	err := s.db.Collection("apps").FindOne(ctx, bson.M{"assistant.name": botAccount}).Decode(&a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get app for bot %q: %w", botAccount, err)
	}
	return &a, nil
}
```

- [ ] **Step 4: Add `FindDMSubscription`**

```go
func (s *MongoStore) FindDMSubscription(ctx context.Context, account, targetName string) (*model.Subscription, error) {
	var sub model.Subscription
	err := s.db.Collection("subscriptions").FindOne(ctx, bson.M{
		"u.account": account,
		"name":      targetName,
		"roomType":  bson.M{"$in": []model.RoomType{model.RoomTypeDM, model.RoomTypeBotDM}},
	}).Decode(&sub)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, model.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find dm subscription: %w", err)
	}
	return &sub, nil
}
```

If `model.ErrSubscriptionNotFound` doesn't exist, use the local sentinel name in `store.go`.

- [ ] **Step 5: Run unit tests — confirm package builds**

Run: `make test SERVICE=room-service`

Expected: PASS (existing tests; new methods are unreferenced until Task 24).

- [ ] **Step 6: Commit**

```bash
git add room-service/store_mongo.go
git commit -m "room-service: implement GetUser, GetApp, FindDMSubscription"
```

---

### Task 23: `EnsureIndexes` — apps + subscriptions DM-dedup partial index

**Files:**
- Modify: `room-service/store_mongo.go`
- Test: `room-service/integration_test.go`

- [ ] **Step 1: Inspect existing index ensure logic**

Run: `grep -n "EnsureIndexes\|CreateOne\|IndexModel" room-service/store_mongo.go`

If `EnsureIndexes` already exists, this task adds two new index models to its body. If not, this task introduces the method and `main.go` must call it at startup.

- [ ] **Step 2: Add the indices**

In `room-service/store_mongo.go` `EnsureIndexes` (creating the function if absent):

```go
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	// ...existing index creations...

	appsIndex := mongo.IndexModel{
		Keys:    bson.D{{Key: "assistant.name", Value: 1}},
		Options: options.Index().SetName("assistant_name_idx"),
	}
	if _, err := s.db.Collection("apps").Indexes().CreateOne(ctx, appsIndex); err != nil {
		return fmt.Errorf("ensure apps index: %w", err)
	}

	dmDedupIndex := mongo.IndexModel{
		Keys: bson.D{
			{Key: "u.account", Value: 1},
			{Key: "name", Value: 1},
			{Key: "roomType", Value: 1},
		},
		Options: options.Index().
			SetName("u_account_name_roomtype_dm_idx").
			SetPartialFilterExpression(bson.M{
				"roomType": bson.M{"$in": bson.A{model.RoomTypeDM, model.RoomTypeBotDM}},
			}),
	}
	if _, err := s.db.Collection("subscriptions").Indexes().CreateOne(ctx, dmDedupIndex); err != nil {
		return fmt.Errorf("ensure dm-dedup subscription index: %w", err)
	}

	// Idempotency index for create-room JetStream redelivery (spec §5.3).
	subUniqueIndex := mongo.IndexModel{
		Keys:    bson.D{{Key: "roomId", Value: 1}, {Key: "u.account", Value: 1}},
		Options: options.Index().SetName("roomId_account_unique_idx").SetUnique(true),
	}
	if _, err := s.db.Collection("subscriptions").Indexes().CreateOne(ctx, subUniqueIndex); err != nil {
		return fmt.Errorf("ensure subscription unique index: %w", err)
	}

	memberUniqueIndex := mongo.IndexModel{
		Keys:    bson.D{{Key: "roomId", Value: 1}, {Key: "member.id", Value: 1}},
		Options: options.Index().SetName("roomId_memberid_unique_idx").SetUnique(true),
	}
	if _, err := s.db.Collection("room_members").Indexes().CreateOne(ctx, memberUniqueIndex); err != nil {
		return fmt.Errorf("ensure room_members unique index: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Verify `main.go` calls `EnsureIndexes`**

Run: `grep -n "EnsureIndexes" room-service/main.go`

If absent, add a call right after the store is constructed and before NATS subscriptions are wired:

```go
if err := store.EnsureIndexes(ctx); err != nil {
	slog.Error("ensure indexes failed", "error", err)
	os.Exit(1)
}
```

- [ ] **Step 4: Add an integration test**

Append to `room-service/integration_test.go`:

```go
//go:build integration

func TestEnsureIndexesCreatesAppsAndDMDedup(t *testing.T) {
	ctx := context.Background()
	_, db := setupMongo(t)
	store := NewMongoStore(db)

	require.NoError(t, store.EnsureIndexes(ctx))

	appsIdx, err := db.Collection("apps").Indexes().List(ctx)
	require.NoError(t, err)
	hasAppsIndex := false
	for appsIdx.Next(ctx) {
		var doc bson.M
		require.NoError(t, appsIdx.Decode(&doc))
		if doc["name"] == "assistant_name_idx" {
			hasAppsIndex = true
		}
	}
	assert.True(t, hasAppsIndex)

	subsIdx, err := db.Collection("subscriptions").Indexes().List(ctx)
	require.NoError(t, err)
	var subsIndex bson.M
	for subsIdx.Next(ctx) {
		var doc bson.M
		require.NoError(t, subsIdx.Decode(&doc))
		if doc["name"] == "u_account_name_roomtype_dm_idx" {
			subsIndex = doc
		}
	}
	require.NotNil(t, subsIndex, "expected DM-dedup partial index to exist")
	assert.Contains(t, subsIndex, "partialFilterExpression")
}
```

- [ ] **Step 5: Run integration test**

Run: `make test-integration SERVICE=room-service -- -run TestEnsureIndexesCreatesAppsAndDMDedup`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add room-service/store_mongo.go room-service/main.go room-service/integration_test.go
git commit -m "room-service: ensure apps + DM-dedup partial subscription indices"
```

---

### Task 24: `handleCreateRoom` skeleton — subject parse, request-ID guard, payload unmarshal, requester lookup, strip, type determination

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

This task introduces the handler shell with steps 1–6 of the validation pipeline (spec §3). DM/botDM and channel branches land in Tasks 25–26.

- [ ] **Step 1: Add parser helper to `pkg/subject` if absent**

Run: `grep -n "ParseRoomCreateSubject\|ParseUserRoomSubject" pkg/subject/subject.go`

If `ParseRoomCreateSubject(subject) (account string, ok bool)` is missing, add it and a corresponding test (mirroring whatever `ParseUserRoomSubject` looks like). Commit separately:

```go
// ParseRoomCreateSubject extracts the account from a
// chat.user.{account}.request.room.{siteID}.create subject.
func ParseRoomCreateSubject(s string) (account string, ok bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 7 {
		return "", false
	}
	if parts[0] != "chat" || parts[1] != "user" || parts[3] != "request" || parts[4] != "room" || parts[6] != "create" {
		return "", false
	}
	return parts[2], true
}
```

Test:

```go
func TestParseRoomCreateSubject(t *testing.T) {
	acct, ok := subject.ParseRoomCreateSubject("chat.user.alice.request.room.site-A.create")
	assert.True(t, ok)
	assert.Equal(t, "alice", acct)

	_, ok = subject.ParseRoomCreateSubject("chat.user.alice.request.room.site-A.member.add")
	assert.False(t, ok)
}
```

Commit: `git commit -m "subject: ParseRoomCreateSubject helper"` after running `make test SERVICE=pkg/subject`.

- [ ] **Step 2: Write the failing handler tests**

Append to `room-service/handler_test.go`:

```go
func TestHandleCreateRoomRejectsMissingRequestID(t *testing.T) {
	h, _ := newTestHandler(t)
	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	_, err := h.handleCreateRoom(context.Background(),
		"chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errMissingRequestID))
}

func TestHandleCreateRoomRejectsEmptyPayload(t *testing.T) {
	h, _ := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)
	body, _ := json.Marshal(model.CreateRoomRequest{})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errEmptyCreateRequest))
}

func TestHandleCreateRoomRequesterNotFound(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)
	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(nil, ErrUserNotFound)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errUserNotFound))
}

func TestHandleCreateRoomSelfDM(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)
	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").
		Return(&model.User{ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A"}, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"alice"}})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errSelfDM))
}
```

- [ ] **Step 3: Run — confirm failures**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoom"`

Expected: undefined `h.handleCreateRoom`.

- [ ] **Step 4: Add the handler skeleton**

Add to `room-service/handler.go`:

```go
func (h *Handler) handleCreateRoom(ctx context.Context, subj string, data []byte) ([]byte, error) {
	requesterAccount, ok := subject.ParseRoomCreateSubject(subj)
	if !ok {
		return nil, fmt.Errorf("invalid create-room subject: %s", subj)
	}

	requestID, _ := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return nil, errMissingRequestID
	}
	if !idgen.IsValidUUID(requestID) {
		return nil, fmt.Errorf("invalid X-Request-ID format: %w", errMissingRequestID)
	}

	var req model.CreateRoomRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	originalUsers := append([]string(nil), req.Users...)

	if req.Name == "" && len(req.Users) == 0 && len(req.Orgs) == 0 && len(req.Channels) == 0 {
		return nil, errEmptyCreateRequest
	}

	requester, err := h.store.GetUser(ctx, requesterAccount)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("get requester: %w", err)
	}
	if requester.EngName == "" || requester.ChineseName == "" {
		return nil, errInvalidUserData
	}

	// Self-DM detection BEFORE strip — the original payload form is what
	// the client tried to do, so the sentinel reflects that intent.
	if req.Name == "" && len(req.Orgs) == 0 && len(req.Channels) == 0 &&
		len(originalUsers) == 1 && originalUsers[0] == requesterAccount {
		return nil, errSelfDM
	}

	req.Users = stripAccount(dedup(req.Users), requesterAccount)

	roomType := determineRoomType(req)
	switch roomType {
	case model.RoomTypeChannel:
		// TODO: Task 25 (channel branch).
		return nil, fmt.Errorf("channel branch not yet implemented")
	case model.RoomTypeDM, model.RoomTypeBotDM:
		// TODO: Task 26 (dm/botDM branch).
		return nil, fmt.Errorf("dm/botDM branch not yet implemented")
	default:
		return nil, fmt.Errorf("unknown room type: %s", roomType)
	}
}
```

Add `determineRoomType` to `helper.go`:

```go
// determineRoomType inspects the post-strip request and decides what
// room type should be created. Empty payload is the caller's
// responsibility — this function assumes at least one of users / orgs /
// channels / name is non-empty.
func determineRoomType(req model.CreateRoomRequest) model.RoomType {
	if req.Name == "" && len(req.Orgs) == 0 && len(req.Channels) == 0 && len(req.Users) == 1 {
		if strings.HasSuffix(req.Users[0], ".bot") {
			return model.RoomTypeBotDM
		}
		return model.RoomTypeDM
	}
	return model.RoomTypeChannel
}
```

`dedup` already exists from add-member (`helper.go`).

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoomRejectsMissingRequestID|TestHandleCreateRoomRejectsEmptyPayload|TestHandleCreateRoomRequesterNotFound|TestHandleCreateRoomSelfDM"`

Expected: PASS for all four. The unimplemented-branch placeholders below those won't be exercised by these tests.

- [ ] **Step 6: Commit**

```bash
git add room-service/handler.go room-service/helper.go room-service/handler_test.go
git commit -m "room-service: handleCreateRoom skeleton — request-ID guard, requester lookup, type determination"
```

---

### Task 25: DM / botDM branch in `handleCreateRoom`

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-service/handler_test.go`:

```go
func TestHandleCreateRoomDMHappyPath(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "bob").Return(&model.User{
		ID: "u_bob", Account: "bob", EngName: "Bob", ChineseName: "鲍勃", SiteID: "site-B",
	}, nil)
	mocks.store.EXPECT().FindDMSubscription(gomock.Any(), "alice", "bob").
		Return(nil, model.ErrSubscriptionNotFound)

	deterministicID := idgen.BuildDMRoomID("u_alice", "u_bob")
	captured := capturePublishArg(mocks)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	resp, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.NoError(t, err)

	var got model.CreateRoomReply
	require.NoError(t, json.Unmarshal(resp, &got))
	assert.Equal(t, "accepted", got["status"])
	assert.Equal(t, deterministicID, got["roomId"])
	assert.Equal(t, "dm", got["roomType"])

	require.Len(t, captured.All(), 1)
	assert.Equal(t, "chat.room.canonical.site-A.create", captured.All()[0].Subject)
	var pub model.CreateRoomRequest
	require.NoError(t, json.Unmarshal(captured.All()[0].Data, &pub))
	assert.Equal(t, deterministicID, pub.RoomID)
	assert.Equal(t, "u_alice", pub.RequesterID)
	assert.Empty(t, pub.AppName)
}

func TestHandleCreateRoomDMAlreadyExists(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "bob").Return(&model.User{
		ID: "u_bob", Account: "bob", EngName: "B", ChineseName: "B",
	}, nil)
	mocks.store.EXPECT().FindDMSubscription(gomock.Any(), "alice", "bob").
		Return(&model.Subscription{RoomID: "u_aliceu_bob"}, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	var dmExists *dmExistsError
	require.True(t, errors.As(err, &dmExists))
	assert.Equal(t, "u_aliceu_bob", dmExists.RoomID())
}

func TestHandleCreateRoomBotDMHappyPath(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "weather.bot").Return(&model.User{
		ID: "u_wbot", Account: "weather.bot", EngName: "Weather", ChineseName: "Weather",
	}, nil)
	mocks.store.EXPECT().GetApp(gomock.Any(), "weather.bot").Return(&model.App{
		ID: "app1", Name: "Weather Bot",
		Assistant: &model.AppAssistant{Enabled: true, Name: "weather.bot"},
	}, nil)
	mocks.store.EXPECT().FindDMSubscription(gomock.Any(), "alice", "weather.bot").
		Return(nil, model.ErrSubscriptionNotFound)

	captured := capturePublishArg(mocks)
	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"weather.bot"}})
	resp, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.NoError(t, err)

	var got model.CreateRoomReply
	require.NoError(t, json.Unmarshal(resp, &got))
	assert.Equal(t, "botDM", got["roomType"])

	var pub model.CreateRoomRequest
	require.NoError(t, json.Unmarshal(captured.All()[0].Data, &pub))
	assert.Equal(t, "Weather Bot", pub.AppName)
}

func TestHandleCreateRoomBotDMAppDisabled(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "weather.bot").Return(&model.User{
		ID: "u_wbot", Account: "weather.bot", EngName: "W", ChineseName: "W",
	}, nil)
	mocks.store.EXPECT().GetApp(gomock.Any(), "weather.bot").Return(&model.App{
		ID: "app1", Name: "Weather Bot",
		Assistant: &model.AppAssistant{Enabled: false, Name: "weather.bot"},
	}, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"weather.bot"}})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBotNotAvailable))
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoomDM|TestHandleCreateRoomBotDM"`

Expected: FAIL — branch is still stubbed.

- [ ] **Step 3: Implement the DM/botDM branch**

In `room-service/handler.go`, replace the DM/botDM placeholder with:

```go
case model.RoomTypeDM, model.RoomTypeBotDM:
    otherAccount := req.Users[0]
    other, err := h.store.GetUser(ctx, otherAccount)
    if err != nil {
        if errors.Is(err, ErrUserNotFound) {
            return nil, errUserNotFound
        }
        return nil, fmt.Errorf("get counterpart: %w", err)
    }
    if other.EngName == "" || other.ChineseName == "" {
        return nil, errInvalidUserData
    }

    if roomType == model.RoomTypeBotDM {
        app, err := h.store.GetApp(ctx, other.Account)
        if err != nil {
            if errors.Is(err, ErrAppNotFound) {
                return nil, errBotNotAvailable
            }
            return nil, fmt.Errorf("get app: %w", err)
        }
        if app.Assistant == nil || !app.Assistant.Enabled {
            return nil, errBotNotAvailable
        }
        req.AppName = app.Name
    }

    req.RoomID = idgen.BuildDMRoomID(requester.ID, other.ID)

    existing, err := h.store.FindDMSubscription(ctx, requester.Account, other.Account)
    if err == nil && existing != nil {
        return nil, newDMExistsError(existing.RoomID)
    }
    if err != nil && !errors.Is(err, model.ErrSubscriptionNotFound) {
        return nil, fmt.Errorf("dm dedup check: %w", err)
    }

    return h.publishCreateRoom(ctx, &req, requester, roomType)
```

Add the `publishCreateRoom` helper at the bottom of `handler.go`:

```go
func (h *Handler) publishCreateRoom(ctx context.Context, req *model.CreateRoomRequest, requester *model.User, roomType model.RoomType) ([]byte, error) {
    req.RequesterID = requester.ID
    req.RequesterAccount = requester.Account
    req.Timestamp = time.Now().UTC().UnixMilli()

    payload, err := json.Marshal(req)
    if err != nil {
        return nil, fmt.Errorf("marshal canonical event: %w", err)
    }
    if err := h.publishToStream(ctx, subject.RoomCanonical(h.siteID, "create"), payload); err != nil {
        return nil, fmt.Errorf("publish canonical: %w", err)
    }
    return json.Marshal(model.CreateRoomReply{
        Status:   model.CreateRoomReplyAccepted,
        RoomID:   req.RoomID,
        RoomType: string(roomType),
    })
}
```

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoomDM|TestHandleCreateRoomBotDM"`

Expected: PASS for all four DM/botDM cases.

- [ ] **Step 5: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "room-service: handleCreateRoom DM/botDM branch with dedup + app validation"
```

---

### Task 26: Channel branch in `handleCreateRoom`

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-service/handler_test.go`:

```go
func TestHandleCreateRoomChannelHappyPath(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	mocks.store.EXPECT().CountNewMembers(gomock.Any(), []string{}, []string{"bob", "carol"}, "").Return(2, nil)

	captured := capturePublishArg(mocks)
	body, _ := json.Marshal(model.CreateRoomRequest{
		Name: "deal team", Users: []string{"bob", "carol"},
	})
	resp, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.NoError(t, err)

	var got model.CreateRoomReply
	require.NoError(t, json.Unmarshal(resp, &got))
	assert.Equal(t, "accepted", got["status"])
	assert.NotEmpty(t, got["roomId"])
	assert.Equal(t, "channel", got["roomType"])

	require.Len(t, captured.All(), 1)
	var pub model.CreateRoomRequest
	require.NoError(t, json.Unmarshal(captured.All()[0].Data, &pub))
	assert.Equal(t, got["roomId"], pub.RoomID)
	assert.Equal(t, []string{"bob", "carol"}, pub.Users)
}

func TestHandleCreateRoomChannelRejectsBot(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)
	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{
		Name: "team", Users: []string{"bob", "weather.bot"},
	})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBotInChannel))
}

func TestHandleCreateRoomChannelExceedsCapacity(t *testing.T) {
	h, mocks := newTestHandler(t)
	h.maxRoomSize = 5 // override on the test handler
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)
	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().CountNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "").Return(99, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{
		Name: "huge", Users: []string{"bob"},
	})
	_, err := h.handleCreateRoom(ctx, "chat.user.alice.request.room.site-A.create", body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum capacity")
}
```

(Add a `capturePublishArg` helper if not present — same shape as the `captureBulkSubsArg` from earlier tasks but for the publish-to-stream injected func.)

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoomChannel"`

Expected: FAIL — channel branch is still stubbed.

- [ ] **Step 3: Implement the channel branch**

In `room-service/handler.go`, replace the channel placeholder:

```go
case model.RoomTypeChannel:
    for _, a := range req.Users {
        if strings.HasSuffix(a, ".bot") {
            return nil, errBotInChannel
        }
    }

    channelOrgIDs, channelAccounts, err := h.expandChannelRefs(ctx, requester.Account, req.Channels)
    if err != nil {
        return nil, fmt.Errorf("expand channels: %w", err)
    }
    allOrgs := dedup(append(append([]string{}, req.Orgs...), channelOrgIDs...))
    allUsers := stripAccount(dedup(append(append([]string{}, req.Users...), channelAccounts...)), requesterAccount)

    if req.Name == "" && len(allUsers) == 0 && len(allOrgs) == 0 {
        return nil, errEmptyCreateRequest
    }

    newCount, err := h.store.CountNewMembers(ctx, allOrgs, allUsers, "")
    if err != nil {
        return nil, fmt.Errorf("count new members: %w", err)
    }
    if newCount > h.maxRoomSize {
        return nil, fmt.Errorf("exceeds maximum capacity (%d): would add %d", h.maxRoomSize, newCount)
    }

    req.Users = allUsers
    req.Orgs = allOrgs
    req.RoomID = idgen.GenerateID()
    return h.publishCreateRoom(ctx, &req, requester, roomType)
```

The `expandChannelRefs` helper already exists from add-member; reuse as-is.

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestHandleCreateRoomChannel"`

Expected: PASS for all three channel cases.

- [ ] **Step 5: Run the full handler suite**

Run: `make test SERVICE=room-service`

Expected: PASS for everything (including the existing add-member tests).

- [ ] **Step 6: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "room-service: handleCreateRoom channel branch with channelRef expansion and capacity check"
```

---

### Task 27: `natsCreateRoom` wrapper + `replyDMExists` helper

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

This task wires the NATS adapter that calls into `handleCreateRoom`, including the special-case reply for `dmExistsError`.

- [ ] **Step 1: Write the failing test**

Append to `room-service/handler_test.go`:

```go
func TestNatsCreateRoomDMExistsRepliesWithRoomID(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "bob").Return(&model.User{
		ID: "u_bob", Account: "bob", EngName: "B", ChineseName: "B",
	}, nil)
	mocks.store.EXPECT().FindDMSubscription(gomock.Any(), "alice", "bob").
		Return(&model.Subscription{RoomID: "u_aliceu_bob"}, nil)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	msg := newTestNatsMsg(t,
		"chat.user.alice.request.room.site-A.create", body, reqID)

	h.natsCreateRoom(otelnats.Msg{Msg: msg, Ctx: natsutil.WithRequestID(context.Background(), reqID)})

	require.Len(t, msg.Replies, 1)
	var got model.ErrorResponse
	require.NoError(t, json.Unmarshal(msg.Replies[0], &got))
	assert.Equal(t, "dm already exists", got.Error)
	assert.Equal(t, "u_aliceu_bob", got.RoomID)
}

func TestNatsCreateRoomGenericErrorReplyShape(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(nil, ErrUserNotFound)

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	msg := newTestNatsMsg(t,
		"chat.user.alice.request.room.site-A.create", body, reqID)
	h.natsCreateRoom(otelnats.Msg{Msg: msg, Ctx: natsutil.WithRequestID(context.Background(), reqID)})

	require.Len(t, msg.Replies, 1)
	var got model.ErrorResponse
	require.NoError(t, json.Unmarshal(msg.Replies[0], &got))
	assert.Equal(t, "user not found", got.Error)
	assert.Empty(t, got.RoomID)
}
```

`newTestNatsMsg` is a small helper that constructs a `*nats.Msg` with the given subject/data and stubs out `Respond` to capture replies into a `Replies [][]byte` slice — copy the pattern from existing add-member handler tests (`grep -n "natsAddMembers\|otelnats.Msg" room-service/handler_test.go`).

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-service -- -run "TestNatsCreateRoom"`

Expected: undefined `h.natsCreateRoom`.

- [ ] **Step 3: Add the wrapper**

Add to `room-service/handler.go`:

```go
func (h *Handler) natsCreateRoom(m otelnats.Msg) {
    resp, err := h.handleCreateRoom(m.Context(), m.Msg.Subject, m.Msg.Data)
    if err != nil {
        var dmExists *dmExistsError
        if errors.As(err, &dmExists) {
            h.replyDMExists(m.Msg, dmExists.RoomID())
            return
        }
        slog.Error("create-room failed",
            "error", err,
            "subject", m.Msg.Subject,
        )
        natsutil.ReplyError(m.Msg, sanitizeError(err))
        return
    }
    if err := m.Msg.Respond(resp); err != nil {
        slog.Error("failed to respond to create-room", "error", err)
    }
}

func (h *Handler) replyDMExists(msg *nats.Msg, existingRoomID string) {
    body, err := json.Marshal(model.ErrorResponse{
        Error:  "dm already exists",
        RoomID: existingRoomID,
    })
    if err != nil {
        natsutil.ReplyError(msg, "internal error")
        return
    }
    if err := msg.Respond(body); err != nil {
        slog.Error("failed to respond DM exists", "error", err)
    }
}
```

Imports: ensure `"github.com/nats-io/nats.go"` and the project's `otelnats` are imported (they likely already are, mirroring `natsAddMembers`).

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-service -- -run "TestNatsCreateRoom"`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "room-service: natsCreateRoom adapter + replyDMExists helper"
```

---

### Task 28: Wire NATS subscription in `main.go`

**Files:**
- Modify: `room-service/main.go`

- [ ] **Step 1: Locate the existing add-member subscribe**

Run: `grep -n "MemberAddWildcard\|QueueSubscribe" room-service/main.go`

Note the registration call and the queue group name (`"room-service"`).

- [ ] **Step 2: Add the create-room subscription**

Right after the `MemberAddWildcard` registration in `room-service/main.go`:

```go
if _, err := nc.QueueSubscribe(
    subject.RoomCreateWildcard(cfg.SiteID),
    "room-service",
    natsrouter.WithMiddleware(handler.natsCreateRoom, requestIDMW, traceMW),
); err != nil {
    slog.Error("subscribe room.create failed", "error", err)
    os.Exit(1)
}
```

Match the exact middleware-application style used by the surrounding `MemberAddWildcard` registration. If the file uses a different idiom (e.g., `natsrouter.RegisterHandlers`), follow that instead.

- [ ] **Step 3: Verify compile**

Run: `make build SERVICE=room-service`

Expected: success.

- [ ] **Step 4: Add an assertion via integration test**

This step is captured by the Phase 8 end-to-end integration test (Task 38) — no separate unit test here.

- [ ] **Step 5: Commit**

```bash
git add room-service/main.go
git commit -m "room-service: subscribe room.create wildcard with room-service queue group"
```

---

## Phase 6 — room-worker create-room

Phase 6 lands across four batches: 6a (store interface + Mongo + helpers), 6b (`processCreateRoom` DM/botDM branches), 6c (channel branch + `room_members` write + counts reconcile), 6d (events + sys-messages + outbox + dispatcher wiring).

### Task 29: Store interface — `CreateRoom`, `GetUser`, `ListNewMembersForNewRoom`

**Files:**
- Modify: `room-worker/store.go`
- Modify: `room-worker/mock_store_test.go` (regenerated)

`ReconcileMemberCounts` was already added in Task 10. This task layers on the remaining new methods.

- [ ] **Step 1: Add the methods**

Append to the `Store` interface in `room-worker/store.go`:

```go
type Store interface {
    // ...existing methods including ReconcileMemberCounts from Task 10...

    // CreateRoom inserts the room doc. Returns mongo.ErrDuplicateKey
    // when the _id collides; the handler's idempotency logic handles
    // matching-existing-room as success-on-redelivery.
    CreateRoom(ctx context.Context, room *model.Room) error

    // GetUser returns the user doc by account, or ErrUserNotFound.
    GetUser(ctx context.Context, account string) (*model.User, error)

    // ListNewMembersForNewRoom is the empty-roomID variant of
    // ListNewMembers — same dedup + bot filter, no "already-subscribed"
    // pruning since the room doesn't exist yet.
    ListNewMembersForNewRoom(ctx context.Context, orgIDs, accounts []string) ([]string, error)
}

var ErrUserNotFound = errors.New("user not found")
```

- [ ] **Step 2: Regenerate mocks**

Run: `make generate SERVICE=room-worker`

Expected: `mock_store_test.go` rewritten.

- [ ] **Step 3: Verify compile**

Run: `make build SERVICE=room-worker`

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add room-worker/store.go room-worker/mock_store_test.go
git commit -m "room-worker: store interface adds CreateRoom, GetUser, ListNewMembersForNewRoom"
```

---

### Task 30: Mongo implementations of the new methods

**Files:**
- Modify: `room-worker/store_mongo.go`

- [ ] **Step 1: Add `CreateRoom`**

Append to `room-worker/store_mongo.go`:

```go
func (s *MongoStore) CreateRoom(ctx context.Context, room *model.Room) error {
    if _, err := s.db.Collection("rooms").InsertOne(ctx, room); err != nil {
        return fmt.Errorf("insert room: %w", err)
    }
    return nil
}
```

- [ ] **Step 2: Add `GetUser`**

```go
func (s *MongoStore) GetUser(ctx context.Context, account string) (*model.User, error) {
    var u model.User
    err := s.db.Collection("users").FindOne(ctx, bson.M{"account": account}).Decode(&u)
    if errors.Is(err, mongo.ErrNoDocuments) {
        return nil, ErrUserNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("get user %q: %w", account, err)
    }
    return &u, nil
}
```

- [ ] **Step 3: Add `ListNewMembersForNewRoom`**

This re-uses the shared aggregation from `pkg/pipelines.GetNewMembersPipeline` with `roomID == ""` (per Task 9):

```go
func (s *MongoStore) ListNewMembersForNewRoom(ctx context.Context, orgIDs, accounts []string) ([]string, error) {
    pipe := pipelines.GetNewMembersPipeline(orgIDs, accounts, "")
    pipe = append(pipe, bson.M{"$group": bson.M{
        "_id":      nil,
        "accounts": bson.M{"$addToSet": "$account"},
    }})
    cur, err := s.db.Collection("users").Aggregate(ctx, pipe)
    if err != nil {
        return nil, fmt.Errorf("list new members for new room: %w", err)
    }
    defer cur.Close(ctx)
    if !cur.Next(ctx) {
        return nil, nil
    }
    var doc struct {
        Accounts []string `bson:"accounts"`
    }
    if err := cur.Decode(&doc); err != nil {
        return nil, fmt.Errorf("decode aggregation result: %w", err)
    }
    return doc.Accounts, nil
}
```

- [ ] **Step 4: Run unit tests**

Run: `make test SERVICE=room-worker`

Expected: PASS (existing tests).

- [ ] **Step 5: Commit**

```bash
git add room-worker/store_mongo.go
git commit -m "room-worker: implement CreateRoom, GetUser, ListNewMembersForNewRoom"
```

---

### Task 31: Helpers — `composeName`, `composeNameOrAccount`, `resolveRoomName`, `createdByForType`, `newSub`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestComposeName(t *testing.T) {
	tests := map[string]struct {
		eng, ch string
		want    string
	}{
		"distinct": {"Alice", "爱丽丝", "Alice 爱丽丝"},
		"equal":    {"Alice", "Alice", "Alice"},
		"empty eng": {"", "爱丽丝", ""},
		"empty ch":  {"Alice", "", ""},
		"both empty": {"", "", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, composeName(tc.eng, tc.ch))
		})
	}
}

func TestComposeNameOrAccount(t *testing.T) {
	u := &model.User{Account: "alice", EngName: "Alice", ChineseName: "爱丽丝"}
	assert.Equal(t, "Alice 爱丽丝", composeNameOrAccount(u))

	bare := &model.User{Account: "alice"}
	assert.Equal(t, "alice", composeNameOrAccount(bare))
}

func TestResolveRoomName(t *testing.T) {
	tests := map[string]struct {
		req      model.CreateRoomRequest
		roomType model.RoomType
		want     string
	}{
		"dm uses roomID":     {model.CreateRoomRequest{RoomID: "u_a|u_b"}, model.RoomTypeDM, "u_a|u_b"},
		"botDM uses roomID":  {model.CreateRoomRequest{RoomID: "u_a|u_w"}, model.RoomTypeBotDM, "u_a|u_w"},
		"channel given name": {model.CreateRoomRequest{Name: "deal team", RoomID: "r1"}, model.RoomTypeChannel, "deal team"},
		"channel auto-name":  {model.CreateRoomRequest{Users: []string{"bob"}, Orgs: []string{"org-fx"}, RoomID: "r2"}, model.RoomTypeChannel, "bob, org-fx"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveRoomName(tc.req, tc.roomType))
		})
	}
}

func TestCreatedByForType(t *testing.T) {
	assert.Equal(t, "u_alice", createdByForType("u_alice", model.RoomTypeChannel))
	assert.Empty(t, createdByForType("u_alice", model.RoomTypeDM))
	assert.Empty(t, createdByForType("u_alice", model.RoomTypeBotDM))
}

func TestNewSubSetsAllFields(t *testing.T) {
	user := &model.User{ID: "u1", Account: "alice"}
	room := &model.Room{ID: "r1", SiteID: "site-A", Type: model.RoomTypeChannel}
	now := time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC)

	sub := newSub("s1", user, room, []model.Role{model.RoleOwner},
		"deal team", "", false, now)

	assert.Equal(t, "s1", sub.ID)
	assert.Equal(t, "u1", sub.User.ID)
	assert.Equal(t, "alice", sub.User.Account)
	assert.Equal(t, "r1", sub.RoomID)
	assert.Equal(t, "site-A", sub.SiteID)
	assert.Equal(t, []model.Role{model.RoleOwner}, sub.Roles)
	assert.Equal(t, "deal team", sub.Name)
	assert.Equal(t, model.RoomTypeChannel, sub.RoomType)
	assert.Empty(t, sub.SidebarName)
	assert.False(t, sub.IsSubscribed)
	assert.Equal(t, now, sub.JoinedAt)
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-worker -- -run "TestComposeName|TestResolveRoomName|TestCreatedByForType|TestNewSub"`

Expected: undefined helpers.

- [ ] **Step 3: Add the helpers**

Append to `room-worker/handler.go`:

```go
func composeName(eng, ch string) string {
	if eng == "" || ch == "" {
		return ""
	}
	if eng == ch {
		return eng
	}
	return eng + " " + ch
}

func composeNameOrAccount(u *model.User) string {
	if name := composeName(u.EngName, u.ChineseName); name != "" {
		return name
	}
	slog.Warn("composeNameOrAccount: missing eng/chinese name; falling back to account",
		"account", u.Account)
	return u.Account
}

func resolveRoomName(req model.CreateRoomRequest, roomType model.RoomType) string {
	if roomType == model.RoomTypeDM || roomType == model.RoomTypeBotDM {
		return req.RoomID
	}
	if req.Name != "" {
		return truncateRunes(req.Name, 100)
	}
	return composeAutoName(req.Users, req.Orgs, req.Channels)
}

func createdByForType(requesterID string, roomType model.RoomType) string {
	if roomType == model.RoomTypeChannel {
		return requesterID
	}
	return ""
}

func newSub(id string, user *model.User, room *model.Room, roles []model.Role,
	name, sidebarName string, isSubscribed bool, joinedAt time.Time) *model.Subscription {
	return &model.Subscription{
		ID:           id,
		User:         model.SubscriptionUser{ID: user.ID, Account: user.Account},
		RoomID:       room.ID,
		SiteID:       room.SiteID,
		Roles:        roles,
		Name:         name,
		RoomType:     room.Type,
		SidebarName:  sidebarName,
		IsSubscribed: isSubscribed,
		JoinedAt:     joinedAt,
	}
}
```

`truncateRunes` and `composeAutoName` are added to `room-worker/handler.go` as well — copy the implementations from `room-service/helper.go` (Task 19). (The helpers are pure and lightweight; sharing across services via a `pkg/` would require its own task — defer that to a future cleanup.)

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run "TestComposeName|TestResolveRoomName|TestCreatedByForType|TestNewSub"`

Expected: PASS for all subcases.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: add composeName, resolveRoomName, createdByForType, newSub helpers"
```

---

### Task 32: `processCreateRoom` skeleton — unmarshal + Room insert

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

The skeleton lands enough of the handler that subsequent tasks (DM/botDM/channel sub-construction, events, outbox) can layer on top without restructuring the function.

- [ ] **Step 1: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomRequiresRequestID(t *testing.T) {
	h, _ := newTestHandler(t)
	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", RequesterID: "u_a", RequesterAccount: "alice",
		Users: []string{"bob"}, Timestamp: 1,
	})
	err := h.processCreateRoom(context.Background(), body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent))
	assert.Contains(t, err.Error(), "missing X-Request-ID")
}

func TestProcessCreateRoomInsertsRoom(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	expectedRoom := &model.Room{
		ID: "r_xyz", Name: "deal team", Type: model.RoomTypeChannel,
		CreatedBy: "u_alice", SiteID: "site-A",
	}
	captureRoom := captureCreateRoomArg(mocks)
	// ... downstream expectations are filled in by Task 34 (channel branch).
	// For now, the test asserts only the Room insert and accepts the
	// handler may return an error in subsequent steps.

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID:           "r_xyz",
		Name:             "deal team",
		Users:            []string{"bob"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		Timestamp:        1740000000000,
	})
	_ = h.processCreateRoom(ctx, body) // expected: error from un-stubbed channel branch

	got := captureRoom.Arg()
	require.NotNil(t, got)
	assert.Equal(t, expectedRoom.ID, got.ID)
	assert.Equal(t, expectedRoom.Name, got.Name)
	assert.Equal(t, expectedRoom.Type, got.Type)
	assert.Equal(t, expectedRoom.CreatedBy, got.CreatedBy)
}
```

`captureCreateRoomArg` is a small helper that records the `*model.Room` passed to `mocks.store.EXPECT().CreateRoom(...)`. Mirror the existing capture helpers in the file.

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoom"`

Expected: undefined `h.processCreateRoom`.

- [ ] **Step 3: Add the skeleton**

Append to `room-worker/handler.go`:

```go
func (h *Handler) processCreateRoom(ctx context.Context, data []byte) error {
	requestID, _ := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return fmt.Errorf("missing X-Request-ID: %w", errPermanent)
	}

	var req model.CreateRoomRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("unmarshal: %w: %w", err, errPermanent)
	}

	requester, err := h.store.GetUser(ctx, req.RequesterAccount)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return fmt.Errorf("requester not found: %w", errPermanent)
		}
		return fmt.Errorf("get requester: %w", err)
	}

	roomType := determineRoomTypeFromPayload(req)
	acceptedAt := time.UnixMilli(req.Timestamp).UTC()
	now := time.Now().UTC()

	room := &model.Room{
		ID:        req.RoomID,
		Name:      resolveRoomName(req, roomType),
		Type:      roomType,
		CreatedBy: createdByForType(requester.ID, roomType),
		SiteID:    h.siteID,
		CreatedAt: acceptedAt,
		UpdatedAt: acceptedAt,
	}
	if err := h.store.CreateRoom(ctx, room); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			existing, fetchErr := h.store.GetRoom(ctx, room.ID)
			if fetchErr != nil {
				return fmt.Errorf("fetch on duplicate-key: %w", fetchErr)
			}
			if existing.Type != room.Type || existing.SiteID != room.SiteID {
				return fmt.Errorf("room ID collision (existing type=%s site=%s; want %s/%s): %w",
					existing.Type, existing.SiteID, room.Type, room.SiteID, errPermanent)
			}
			// Otherwise treat as redelivery and continue with the existing room.
			room = existing
		} else {
			return fmt.Errorf("create room: %w", err)
		}
	}

	switch roomType {
	case model.RoomTypeDM:
		return h.processCreateRoomDM(ctx, &req, room, requester, requestID, acceptedAt, now)
	case model.RoomTypeBotDM:
		return h.processCreateRoomBotDM(ctx, &req, room, requester, requestID, acceptedAt, now)
	case model.RoomTypeChannel:
		return h.processCreateRoomChannel(ctx, &req, room, requester, requestID, acceptedAt, now)
	default:
		return fmt.Errorf("unknown room type %q: %w", roomType, errPermanent)
	}
}

// determineRoomTypeFromPayload mirrors room-service's determineRoomType
// but operates on the canonical (post-strip, server-populated) payload.
func determineRoomTypeFromPayload(req model.CreateRoomRequest) model.RoomType {
	if req.Name == "" && len(req.Orgs) == 0 && len(req.Channels) == 0 && len(req.Users) == 1 {
		if strings.HasSuffix(req.Users[0], ".bot") {
			return model.RoomTypeBotDM
		}
		return model.RoomTypeDM
	}
	return model.RoomTypeChannel
}
```

Stub the three branch methods so the package compiles:

```go
func (h *Handler) processCreateRoomDM(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	return fmt.Errorf("dm branch unimplemented")
}
func (h *Handler) processCreateRoomBotDM(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	return fmt.Errorf("botDM branch unimplemented")
}
func (h *Handler) processCreateRoomChannel(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	return fmt.Errorf("channel branch unimplemented")
}
```

- [ ] **Step 4: Run — confirm test 1 passes; test 2 reaches CreateRoom and then errors**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoom"`

Expected: `TestProcessCreateRoomRequiresRequestID` passes. `TestProcessCreateRoomInsertsRoom` passes too — it ignores the post-`CreateRoom` error.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: processCreateRoom skeleton — request-ID guard + Room insert + branch dispatch"
```

---

### Task 33: DM and botDM branches in `processCreateRoom`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomDMBuildsTwoSubs(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "bob").Return(&model.User{
		ID: "u_bob", Account: "bob", EngName: "Bob", ChineseName: "鲍勃", SiteID: "site-B",
	}, nil)
	mocks.store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	captured := captureBulkSubsArg(mocks)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), "u_aliceu_bob").Return(nil)

	roomID := idgen.BuildDMRoomID("u_alice", "u_bob")
	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID:           roomID,
		Users:            []string{"bob"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		Timestamp:        1740000000000,
	})

	require.NoError(t, h.processCreateRoom(ctx, body))

	subs := captured.Args()
	require.Len(t, subs, 2)

	// Order: requester first, then otherUser
	assert.Equal(t, "alice", subs[0].User.Account)
	assert.Equal(t, "bob", subs[0].Name)
	assert.Equal(t, "Bob 鲍勃", subs[0].SidebarName)
	assert.False(t, subs[0].IsSubscribed)
	assert.Nil(t, subs[0].Roles)
	assert.Equal(t, model.RoomTypeDM, subs[0].RoomType)

	assert.Equal(t, "bob", subs[1].User.Account)
	assert.Equal(t, "alice", subs[1].Name)
	assert.Equal(t, "Alice 爱丽丝", subs[1].SidebarName)
}

func TestProcessCreateRoomBotDMHasIsSubscribedAndAppName(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "weather.bot").Return(&model.User{
		ID: "u_wbot", Account: "weather.bot", EngName: "Weather", ChineseName: "Weather",
	}, nil)
	mocks.store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	captured := captureBulkSubsArg(mocks)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), gomock.Any()).Return(nil)

	roomID := idgen.BuildDMRoomID("u_alice", "u_wbot")
	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID:           roomID,
		Users:            []string{"weather.bot"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		AppName:          "Weather Bot",
		Timestamp:        1740000000000,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	subs := captured.Args()
	require.Len(t, subs, 2)

	assert.Equal(t, "alice", subs[0].User.Account)
	assert.Equal(t, "weather.bot", subs[0].Name)
	assert.Equal(t, "Weather Bot", subs[0].SidebarName)
	assert.True(t, subs[0].IsSubscribed)
	assert.Equal(t, model.RoomTypeBotDM, subs[0].RoomType)

	assert.Equal(t, "weather.bot", subs[1].User.Account)
	assert.Equal(t, "alice", subs[1].Name)
	assert.Equal(t, "Alice 爱丽丝", subs[1].SidebarName)
	assert.False(t, subs[1].IsSubscribed)
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomDM|TestProcessCreateRoomBotDM"`

Expected: branch unimplemented.

- [ ] **Step 3: Implement the DM branch**

Replace the stub:

```go
func (h *Handler) processCreateRoomDM(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	other, err := h.store.GetUser(ctx, req.Users[0])
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return fmt.Errorf("counterpart not found: %w", errPermanent)
		}
		return fmt.Errorf("get counterpart: %w", err)
	}

	subs := []*model.Subscription{
		newSub(idgen.GenerateUUIDv7(),
			requester, room, nil, other.Account, composeNameOrAccount(other), false, acceptedAt),
		newSub(idgen.GenerateUUIDv7(),
			other, room, nil, requester.Account, composeNameOrAccount(requester), false, acceptedAt),
	}
	if err := h.bulkCreateSubsIdempotent(ctx, subs); err != nil {
		return err
	}
	return h.finishCreateRoom(ctx, req, room, requester, []*model.User{requester, other}, subs, requestID, now)
}
```

- [ ] **Step 4: Implement the botDM branch**

```go
func (h *Handler) processCreateRoomBotDM(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	bot, err := h.store.GetUser(ctx, req.Users[0])
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return fmt.Errorf("bot user not found: %w", errPermanent)
		}
		return fmt.Errorf("get bot user: %w", err)
	}

	subs := []*model.Subscription{
		newSub(idgen.GenerateUUIDv7(),
			requester, room, nil, bot.Account, req.AppName, true, acceptedAt),
		newSub(idgen.GenerateUUIDv7(),
			bot, room, nil, requester.Account, composeNameOrAccount(requester), false, acceptedAt),
	}
	if err := h.bulkCreateSubsIdempotent(ctx, subs); err != nil {
		return err
	}
	return h.finishCreateRoom(ctx, req, room, requester, []*model.User{requester, bot}, subs, requestID, now)
}
```

- [ ] **Step 5: Add the shared helpers**

```go
func (h *Handler) bulkCreateSubsIdempotent(ctx context.Context, subs []*model.Subscription) error {
	if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // success-on-redelivery
		}
		return fmt.Errorf("bulk create subs: %w", err)
	}
	return nil
}

// finishCreateRoom is the post-write tail: ReconcileMemberCounts +
// publishes (subscription.update for every sub, sys-messages for
// channels, outbox per remote site, async-job result). Tasks 34
// (channel) and 35 (events/outbox) flesh this out; for now it
// handles only ReconcileMemberCounts.
func (h *Handler) finishCreateRoom(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, allUsers []*model.User, subs []*model.Subscription, requestID string, now time.Time) error {
	if err := h.store.ReconcileMemberCounts(ctx, room.ID); err != nil {
		return fmt.Errorf("reconcile member counts: %w", err)
	}
	return nil // Tasks 35-37 extend this body with fan-out, sys-messages, outbox, and async-job result
}
```

- [ ] **Step 6: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomDM|TestProcessCreateRoomBotDM"`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: processCreateRoom DM and botDM branches"
```

---

### Task 34: Channel branch in `processCreateRoom`

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomChannelBuildsSubsAndMembers(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝", SiteID: "site-A",
	}, nil)
	mocks.store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)

	mocks.store.EXPECT().ListNewMembersForNewRoom(gomock.Any(), []string{"org-fx"}, []string{"bob"}).
		Return([]string{"bob", "carol"}, nil)
	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "carol"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", EngName: "Bob", ChineseName: "鲍勃", SiteID: "site-A"},
		{ID: "u_carol", Account: "carol", EngName: "Carol", ChineseName: "卡罗尔", SiteID: "site-A"},
	}, nil)

	capturedSubs := captureBulkSubsArg(mocks)
	capturedMembers := captureBulkRoomMembersArg(mocks)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), "r_xyz").Return(nil)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID:           "r_xyz",
		Name:             "deal team",
		Users:            []string{"bob"},
		Orgs:             []string{"org-fx"},
		RequesterID:      "u_alice",
		RequesterAccount: "alice",
		Timestamp:        1740000000000,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	subs := capturedSubs.Args()
	require.Len(t, subs, 3) // bob, carol, alice (owner)

	for _, s := range subs[:2] {
		assert.Equal(t, []model.Role{model.RoleMember}, s.Roles)
		assert.Equal(t, "deal team", s.Name)
		assert.Equal(t, model.RoomTypeChannel, s.RoomType)
	}
	owner := subs[2]
	assert.Equal(t, "alice", owner.User.Account)
	assert.Equal(t, []model.Role{model.RoleOwner}, owner.Roles)

	members := capturedMembers.Args()
	// 2 individuals + 1 org row + 1 owner individual = 4
	require.Len(t, members, 4)

	hasOrgRow := false
	hasOwner := false
	for _, m := range members {
		if m.Member.Type == model.RoomMemberOrg && m.Member.ID == "org-fx" {
			hasOrgRow = true
		}
		if m.Member.Account == "alice" {
			hasOwner = true
		}
	}
	assert.True(t, hasOrgRow)
	assert.True(t, hasOwner)
}

func TestProcessCreateRoomChannelNoOrgsSkipsRoomMembers(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mocks.store.EXPECT().ListNewMembersForNewRoom(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]string{"bob"}, nil)
	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", EngName: "B", ChineseName: "B"},
	}, nil)

	captured := captureBulkSubsArg(mocks)
	captureRM := captureBulkRoomMembersArg(mocks)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), gomock.Any()).Return(nil)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", Name: "team", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	require.Len(t, captured.Args(), 2)
	// orgs empty → only the owner room_members row should be written.
	require.Len(t, captureRM.Args(), 1)
	assert.Equal(t, "alice", captureRM.Args()[0].Member.Account)
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomChannel"`

Expected: branch unimplemented.

- [ ] **Step 3: Implement the channel branch**

Replace the channel stub:

```go
func (h *Handler) processCreateRoomChannel(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, requestID string, acceptedAt, now time.Time) error {
	accounts, err := h.store.ListNewMembersForNewRoom(ctx, req.Orgs, req.Users)
	if err != nil {
		return fmt.Errorf("list new members: %w", err)
	}
	accounts = stripAccount(accounts, requester.Account)

	users, err := h.store.FindUsersByAccounts(ctx, accounts)
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}
	for i := range users {
		if users[i].EngName == "" || users[i].ChineseName == "" {
			return fmt.Errorf("user %s missing required name fields: %w", users[i].Account, errPermanent)
		}
	}

	subs := make([]*model.Subscription, 0, len(users)+1)
	for i := range users {
		u := &users[i]
		subs = append(subs, newSub(
			idgen.GenerateUUIDv7(),
			u, room,
			[]model.Role{model.RoleMember},
			room.Name, "", false, acceptedAt))
	}
	subs = append(subs, newSub(
		idgen.GenerateUUIDv7(),
		requester, room,
		[]model.Role{model.RoleOwner},
		room.Name, "", false, acceptedAt))

	if err := h.bulkCreateSubsIdempotent(ctx, subs); err != nil {
		return err
	}

	members := make([]*model.RoomMember, 0, len(subs)+len(req.Orgs))
	if len(req.Orgs) > 0 {
		for _, sub := range subs[:len(subs)-1] {
			members = append(members, &model.RoomMember{
				ID:     idgen.GenerateUUIDv7(),
				RoomID: room.ID,
				Ts:     acceptedAt,
				Member: model.RoomMemberEntry{
					ID: sub.User.ID, Type: model.RoomMemberIndividual, Account: sub.User.Account,
				},
			})
		}
		for _, org := range req.Orgs {
			members = append(members, &model.RoomMember{
				ID:     idgen.GenerateUUIDv7(),
				RoomID: room.ID,
				Ts:     acceptedAt,
				Member: model.RoomMemberEntry{ID: org, Type: model.RoomMemberOrg},
			})
		}
	}
	// Owner row always written for channels.
	members = append(members, &model.RoomMember{
		ID:     idgen.GenerateUUIDv7(),
		RoomID: room.ID,
		Ts:     acceptedAt,
		Member: model.RoomMemberEntry{
			ID: requester.ID, Type: model.RoomMemberIndividual, Account: requester.Account,
		},
	})

	if err := h.bulkCreateMembersIdempotent(ctx, members); err != nil {
		return err
	}

	allUsers := make([]*model.User, 0, len(users)+1)
	for i := range users {
		allUsers = append(allUsers, &users[i])
	}
	allUsers = append(allUsers, requester)

	return h.finishCreateRoom(ctx, req, room, requester, allUsers, subs, requestID, now)
}

func (h *Handler) bulkCreateMembersIdempotent(ctx context.Context, members []*model.RoomMember) error {
	if len(members) == 0 {
		return nil
	}
	if err := h.store.BulkCreateRoomMembers(ctx, members); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return fmt.Errorf("bulk create room members: %w", err)
	}
	return nil
}
```

`stripAccount` is already in `room-worker/handler.go` from Task 31 (or copy from `room-service/helper.go` if not).

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomChannel"`

Expected: PASS for both.

- [ ] **Step 5: Run the full handler suite to catch retrofit regressions**

Run: `make test SERVICE=room-worker`

Expected: PASS overall — including the retrofit add-member tests from Phase 4.

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: processCreateRoom channel branch with member subs + room_members"
```

---

### Task 35: `subscription.update` fan-out for every sub

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomFiresSubscriptionUpdateForEverySub(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectChannelHappyPath(t, mocks, []string{"bob"}) // helper sketched below

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", Name: "team", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	subUpdates := mocks.publisher.SubscriptionUpdates()
	require.Len(t, subUpdates, 2) // bob + owner alice
	subjects := []string{subUpdates[0].Subject, subUpdates[1].Subject}
	assert.ElementsMatch(t, []string{
		"chat.user.bob.event.subscription.update",
		"chat.user.alice.event.subscription.update",
	}, subjects)
}
```

`expectChannelHappyPath(t, mocks, accounts)` sets up the same mock expectations as `TestProcessCreateRoomChannelBuildsSubsAndMembers` from Task 34 — extract it into a helper if not already. `mocks.publisher.SubscriptionUpdates()` returns every captured publish whose subject matches `chat.user.*.event.subscription.update`.

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run TestProcessCreateRoomFiresSubscriptionUpdateForEverySub`

Expected: 0 publishes captured.

- [ ] **Step 3: Add the fan-out to `finishCreateRoom`**

Replace the placeholder body of `finishCreateRoom` with the start of the publish pipeline. Insert this block after the `ReconcileMemberCounts` call:

```go
for _, sub := range subs {
    evt := model.SubscriptionUpdateEvent{
        UserID:       sub.User.ID,
        Subscription: *sub,
        Action:       "added",
        Timestamp:    now.UnixMilli(),
    }
    data, err := json.Marshal(evt)
    if err != nil {
        slog.Error("marshal subscription.update failed",
            "error", err, "account", sub.User.Account)
        continue
    }
    msg := natsutil.NewMsg(ctx, subject.SubscriptionUpdate(sub.User.Account), data)
    if err := h.nc.PublishMsg(msg); err != nil {
        slog.Error("publish subscription.update failed",
            "error", err, "account", sub.User.Account)
    }
}
```

Errors here are logged but not returned — subs are durable in Mongo; UI can recover via reconnect.

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run TestProcessCreateRoomFiresSubscriptionUpdateForEverySub`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "room-worker: subscription.update fan-out per sub in finishCreateRoom"
```

---

### Task 36: Channel-only system messages (`room_created` + `members_added`)

**Files:**
- Modify: `room-worker/handler.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomChannelEmitsSysMessages(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectChannelHappyPath(t, mocks, []string{"bob"})

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", Name: "team", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	sys := mocks.publisher.MessagesCanonical("site-A")
	require.Len(t, sys, 2)

	var first, second model.MessageEvent
	require.NoError(t, json.Unmarshal(sys[0].Data, &first))
	require.NoError(t, json.Unmarshal(sys[1].Data, &second))
	assert.Equal(t, "room_created", first.Message.Type)
	assert.Equal(t, idgen.MessageIDFromRequestID(reqID, "room_created"), first.Message.ID)
	assert.Equal(t, "members_added", second.Message.Type)
	assert.Equal(t, idgen.MessageIDFromRequestID(reqID, "members_added"), second.Message.ID)
	assert.True(t, second.Message.CreatedAt.After(first.Message.CreatedAt))
}

func TestProcessCreateRoomDMEmitsNoSysMessages(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u_alice", Account: "alice", EngName: "A", ChineseName: "A",
	}, nil)
	mocks.store.EXPECT().GetUser(gomock.Any(), "bob").Return(&model.User{
		ID: "u_bob", Account: "bob", EngName: "B", ChineseName: "B",
	}, nil)
	mocks.store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
	mocks.store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	mocks.store.EXPECT().ReconcileMemberCounts(gomock.Any(), gomock.Any()).Return(nil)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: idgen.BuildDMRoomID("u_alice", "u_bob"),
		Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	assert.Empty(t, mocks.publisher.MessagesCanonical("site-A"))
}
```

`mocks.publisher.MessagesCanonical(siteID)` filters captured publishes by subject `chat.msg.canonical.{siteID}.created`.

- [ ] **Step 2: Run — confirm failure**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomChannelEmitsSysMessages|TestProcessCreateRoomDMEmitsNoSysMessages"`

Expected: first test fails (no sys-messages); second already passes.

- [ ] **Step 3: Implement sys-message publishing**

In `room-worker/handler.go` `finishCreateRoom`, after the subscription.update loop, add a channel-only block:

```go
if room.Type == model.RoomTypeChannel {
    if err := h.publishChannelSysMessages(ctx, req, room, requester, len(subs)-1, requestID, now); err != nil {
        return fmt.Errorf("publish sys messages: %w", err)
    }
}
```

Add the helper. `addedUsersCount` is `len(subs) - 1` (subscriptions minus owner — one sub per account, matching the spec exactly).

```go
func (h *Handler) publishChannelSysMessages(ctx context.Context, req *model.CreateRoomRequest, room *model.Room, requester *model.User, addedUsersCount int, requestID string, now time.Time) error {
    acceptedAt := time.UnixMilli(req.Timestamp).UTC()

    sysData1, err := json.Marshal(model.RoomCreated{
        Name:            room.Name,
        Users:           req.Users,
        Orgs:            req.Orgs,
        Channels:        req.Channels,
        AddedUsersCount: addedUsersCount,
    })
    if err != nil {
        return fmt.Errorf("marshal room_created sys data: %w", err)
    }
    msg1 := model.Message{
        ID:          idgen.MessageIDFromRequestID(requestID, "room_created"),
        RoomID:      room.ID,
        UserID:      requester.ID,
        UserAccount: requester.Account,
        Type:        "room_created",
        Content:     "a new room has been created",
        SysMsgData:  sysData1,
        CreatedAt:   acceptedAt,
    }
    if err := h.publishCanonical(ctx, msg1, room.SiteID, now); err != nil {
        return fmt.Errorf("publish room_created: %w", err)
    }

    sysData2, err := json.Marshal(model.MembersAdded{
        Individuals:     req.Users,
        Orgs:            req.Orgs,
        Channels:        req.Channels,
        AddedUsersCount: addedUsersCount,
    })
    if err != nil {
        return fmt.Errorf("marshal members_added sys data: %w", err)
    }
    msg2 := model.Message{
        ID:          idgen.MessageIDFromRequestID(requestID, "members_added"),
        RoomID:      room.ID,
        UserID:      requester.ID,
        UserAccount: requester.Account,
        Type:        "members_added",
        SysMsgData:  sysData2,
        CreatedAt:   acceptedAt.Add(time.Millisecond),
    }
    if err := h.publishCanonical(ctx, msg2, room.SiteID, now); err != nil {
        return fmt.Errorf("publish members_added: %w", err)
    }
    return nil
}

func (h *Handler) publishCanonical(ctx context.Context, msg model.Message, siteID string, now time.Time) error {
    evt := model.MessageEvent{
        Event:     model.EventCreated,
        Message:   msg,
        SiteID:    siteID,
        Timestamp: now.UnixMilli(),
    }
    data, err := json.Marshal(evt)
    if err != nil {
        return fmt.Errorf("marshal MessageEvent: %w", err)
    }
    return h.publish(ctx, subject.MsgCanonicalCreated(siteID), data, msg.ID)
}
```

If `model.RoomCreated` doesn't yet exist as a sys-message payload struct, add it next to `MembersAdded` in `pkg/model/member.go`:

```go
type RoomCreated struct {
    Name            string       `json:"name"`
    Users           []string     `json:"users"`
    Orgs            []string     `json:"orgs"`
    Channels        []ChannelRef `json:"channels"`
    AddedUsersCount int          `json:"addedUsersCount"`
}
```

(Plus a round-trip test in `model_test.go`.)

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomChannelEmitsSysMessages|TestProcessCreateRoomDMEmitsNoSysMessages"`

Expected: PASS for both.

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go pkg/model/member.go pkg/model/model_test.go
git commit -m "room-worker: publish room_created + members_added sys-messages on channel create"
```

---

### Task 37: Outbox per remote site + async-job result + dispatcher wiring

**Files:**
- Modify: `room-worker/handler.go`
- Modify: `room-worker/main.go`
- Test: `room-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoomOutboxPerRemoteSite(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectChannelHappyPathMixed(t, mocks)
	// helper that returns 1 site-A user (carol) and 2 site-B users (bob, ian)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", Name: "team",
		Users: []string{"bob", "carol", "ian"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	outboxB := mocks.publisher.OutboxFor("site-B", "room_created")
	require.Len(t, outboxB, 1)
	var envelope model.OutboxEvent
	require.NoError(t, json.Unmarshal(outboxB[0].Data, &envelope))
	var payload model.RoomCreatedOutbox
	require.NoError(t, json.Unmarshal(envelope.Payload, &payload))
	assert.ElementsMatch(t, []string{"bob", "ian"}, payload.Accounts)
	assert.Equal(t, "Alice", payload.RequesterEngName)
	assert.Equal(t, model.RoomTypeChannel, payload.RoomType)
	assert.Equal(t, reqID+":site-B", outboxB[0].Header.Get("Nats-Msg-Id"))

	assert.Empty(t, mocks.publisher.OutboxFor("site-A", "room_created"))
}

func TestProcessCreateRoomEmitsAsyncJobOk(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	expectChannelHappyPath(t, mocks, []string{"bob"})

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r1", Name: "team", Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice", Timestamp: 1,
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	jobs := mocks.publisher.UserResponseFor("alice")
	require.Len(t, jobs, 1)
	var got model.AsyncJobResult
	require.NoError(t, json.Unmarshal(jobs[0].Data, &got))
	assert.Equal(t, model.AsyncJobOpRoomCreate, got.Operation)
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "r1", got.RoomID)
	assert.Equal(t, reqID, got.RequestID)
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=room-worker -- -run "TestProcessCreateRoomOutboxPerRemoteSite|TestProcessCreateRoomEmitsAsyncJobOk"`

Expected: zero outbox / async-job publishes captured.

- [ ] **Step 3: Implement outbox + async-job in `finishCreateRoom`**

After the sys-messages block, add:

```go
remoteSiteAccounts := map[string][]string{}
for _, u := range allUsers {
    if u.SiteID == h.siteID {
        continue
    }
    remoteSiteAccounts[u.SiteID] = append(remoteSiteAccounts[u.SiteID], u.Account)
}
for destSiteID, accounts := range remoteSiteAccounts {
    payload := model.RoomCreatedOutbox{
        RoomID:               room.ID,
        RoomType:             room.Type,
        RoomName:             room.Name,
        HomeSiteID:           room.SiteID,
        Accounts:             accounts,
        RequesterAccount:     requester.Account,
        RequesterEngName:     requester.EngName,
        RequesterChineseName: requester.ChineseName,
        AppName:              req.AppName,
        Timestamp:            req.Timestamp,
    }
    pData, err := json.Marshal(payload)
    if err != nil {
        return fmt.Errorf("marshal room_created outbox payload: %w", err)
    }
    envelope := model.OutboxEvent{
        Type: model.MessageTypeRoomCreated, SiteID: room.SiteID, DestSiteID: destSiteID,
        Payload: pData, Timestamp: now.UnixMilli(),
    }
    eData, err := json.Marshal(envelope)
    if err != nil {
        return fmt.Errorf("marshal outbox envelope: %w", err)
    }
    if err := h.publish(ctx, subject.Outbox(room.SiteID, destSiteID, model.MessageTypeRoomCreated), eData, requestID+":"+destSiteID); err != nil {
        return fmt.Errorf("publish room_created outbox to %s: %w", destSiteID, err)
    }
}

if err := h.publishAsyncJobResult(ctx, requester.Account, model.AsyncJobResult{
    RequestID: requestID,
    Operation: model.AsyncJobOpRoomCreate,
    Status:    model.AsyncJobStatusOK,
    RoomID:    room.ID,
    Timestamp: now.UnixMilli(),
}); err != nil {
    slog.Error("publish async-job result failed",
        "error", err, "requestId", requestID, "operation", model.AsyncJobOpRoomCreate)
}
return nil
```

- [ ] **Step 4: Wire dispatcher in `main.go`**

In `room-worker/main.go`, find the existing dispatcher branch on `subject.RoomCanonicalOperation`:

```go
case "member.add":
    err = h.processAddMembers(ctx, msg.Data())
```

Add:

```go
case "create":
    err = h.processCreateRoom(ctx, msg.Data())
```

Wrap the dispatcher with the same retryable/permanent dispatch as Task 16:

```go
switch {
case err == nil:
    msg.Ack()
case errors.Is(err, errPermanent):
    requestID, _ := natsutil.RequestIDFromContext(ctx)
    var req model.CreateRoomRequest
    _ = json.Unmarshal(msg.Data(), &req)
    _ = h.publishAsyncJobResult(ctx, req.RequesterAccount, model.AsyncJobResult{
        RequestID: requestID,
        Operation: opForSubject(msg.Subject()),
        Status:    "error",
        RoomID:    req.RoomID,
        Error:     sanitizeErrorString(err),
        Timestamp: time.Now().UnixMilli(),
    })
    msg.Ack()
default:
    msg.Nak()
}
```

Helper:

```go
func opForSubject(subj string) string {
    op, _ := subject.RoomCanonicalOperation(subj)
    switch op {
    case "create":
        return model.AsyncJobOpRoomCreate
    case "member.add":
        return model.AsyncJobOpRoomMemberAdd
    default:
        return op
    }
}
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=room-worker`

Expected: PASS overall, including the new outbox + async-job tests.

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/main.go room-worker/handler_test.go
git commit -m "room-worker: room_created outbox per remote site + async-job result + dispatcher wiring"
```

---

## Phase 7 — inbox-worker create-room

### Task 38: Helpers for `handleRoomCreated`

**Files:**
- Modify: `inbox-worker/handler.go`
- Test: `inbox-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `inbox-worker/handler_test.go`:

```go
func TestRolesForType(t *testing.T) {
	assert.Equal(t, []model.Role{model.RoleMember}, rolesForType(model.RoomTypeChannel))
	assert.Nil(t, rolesForType(model.RoomTypeDM))
	assert.Nil(t, rolesForType(model.RoomTypeBotDM))
}

func TestSubscriptionName(t *testing.T) {
	d := model.RoomCreatedOutbox{
		RoomType: model.RoomTypeChannel,
		RoomName: "deal team",
		RequesterAccount: "alice",
	}
	assert.Equal(t, "deal team", subscriptionName(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeDM
	assert.Equal(t, "alice", subscriptionName(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeBotDM
	assert.Equal(t, "alice", subscriptionName(d, model.User{Account: "weather.bot"}))
}

func TestSubscriptionSidebarName(t *testing.T) {
	d := model.RoomCreatedOutbox{
		RoomType: model.RoomTypeChannel,
		RequesterEngName: "Alice", RequesterChineseName: "爱丽丝",
	}
	assert.Empty(t, subscriptionSidebarName(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeDM
	assert.Equal(t, "Alice 爱丽丝", subscriptionSidebarName(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeBotDM
	d.AppName = "Weather Bot"
	// Bot side gets human's name
	assert.Equal(t, "Alice 爱丽丝", subscriptionSidebarName(d, model.User{Account: "weather.bot"}))
}

func TestSubscriptionIsSubscribed(t *testing.T) {
	d := model.RoomCreatedOutbox{RoomType: model.RoomTypeChannel}
	assert.False(t, subscriptionIsSubscribed(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeDM
	assert.False(t, subscriptionIsSubscribed(d, model.User{Account: "bob"}))

	d.RoomType = model.RoomTypeBotDM
	assert.False(t, subscriptionIsSubscribed(d, model.User{Account: "weather.bot"})) // bot side
	assert.True(t, subscriptionIsSubscribed(d, model.User{Account: "alice"}))         // human
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=inbox-worker -- -run "TestRolesForType|TestSubscriptionName|TestSubscriptionSidebarName|TestSubscriptionIsSubscribed"`

Expected: undefined helpers.

- [ ] **Step 3: Add the helpers**

Append to `inbox-worker/handler.go`:

```go
func rolesForType(t model.RoomType) []model.Role {
	if t == model.RoomTypeChannel {
		return []model.Role{model.RoleMember}
	}
	return nil
}

func subscriptionName(d model.RoomCreatedOutbox, u model.User) string {
	switch d.RoomType {
	case model.RoomTypeChannel:
		return d.RoomName
	case model.RoomTypeDM, model.RoomTypeBotDM:
		// The user being processed on this remote site is by definition
		// not the requester (the home site holds the requester's sub).
		// Therefore the "other party" from u's perspective is the
		// requester.
		return d.RequesterAccount
	}
	return ""
}

func subscriptionSidebarName(d model.RoomCreatedOutbox, u model.User) string {
	switch d.RoomType {
	case model.RoomTypeChannel:
		return ""
	case model.RoomTypeDM:
		return composeName(d.RequesterEngName, d.RequesterChineseName)
	case model.RoomTypeBotDM:
		if strings.HasSuffix(u.Account, ".bot") {
			return composeName(d.RequesterEngName, d.RequesterChineseName)
		}
		return d.AppName
	}
	return ""
}

func subscriptionIsSubscribed(d model.RoomCreatedOutbox, u model.User) bool {
	if d.RoomType != model.RoomTypeBotDM {
		return false
	}
	return !strings.HasSuffix(u.Account, ".bot")
}
```

`composeName` is added to `inbox-worker/handler.go` mirroring the room-worker version (Task 31). Copy the implementation.

- [ ] **Step 4: Run — confirm pass**

Run: `make test SERVICE=inbox-worker -- -run "TestRolesForType|TestSubscriptionName|TestSubscriptionSidebarName|TestSubscriptionIsSubscribed"`

Expected: PASS for all subcases.

- [ ] **Step 5: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go
git commit -m "inbox-worker: rolesForType, subscriptionName/SidebarName/IsSubscribed helpers"
```

---

### Task 39: `handleRoomCreated` handler + dispatcher routing

**Files:**
- Modify: `inbox-worker/handler.go`
- Test: `inbox-worker/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `inbox-worker/handler_test.go`:

```go
func TestHandleRoomCreatedRequiresRequestID(t *testing.T) {
	h, _ := newTestHandler(t)
	payload, _ := json.Marshal(model.RoomCreatedOutbox{
		RoomID: "r1", RoomType: model.RoomTypeChannel,
		Accounts: []string{"bob"},
	})
	err := h.handleRoomCreated(context.Background(), model.OutboxEvent{Payload: payload})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing X-Request-ID")
}

func TestHandleRoomCreatedEmptyAccountsAcksWithWarn(t *testing.T) {
	h, _ := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	payload, _ := json.Marshal(model.RoomCreatedOutbox{
		RoomID: "r1", RoomType: model.RoomTypeChannel, Accounts: []string{},
	})
	require.NoError(t, h.handleRoomCreated(ctx, model.OutboxEvent{Payload: payload}))
}

func TestHandleRoomCreatedDMBuildsRemoteSub(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-B"},
	}, nil)
	captured := captureBulkSubsArg(mocks)

	payload, _ := json.Marshal(model.RoomCreatedOutbox{
		RoomID:               "u_aliceu_bob",
		RoomType:             model.RoomTypeDM,
		RoomName:             "u_aliceu_bob",
		HomeSiteID:           "site-A",
		Accounts:             []string{"bob"},
		RequesterAccount:     "alice",
		RequesterEngName:     "Alice",
		RequesterChineseName: "爱丽丝",
		Timestamp:            1740000000000,
	})
	require.NoError(t, h.handleRoomCreated(ctx, model.OutboxEvent{Payload: payload}))

	subs := captured.Args()
	require.Len(t, subs, 1)
	assert.True(t, idgen.IsValidUUIDv7(subs[0].ID)) // random GenerateUUIDv7(), not deterministic
	assert.Equal(t, "u_aliceu_bob", subs[0].RoomID)
	assert.Equal(t, "site-A", subs[0].SiteID) // room's home, not bob's
	assert.Equal(t, "alice", subs[0].Name)
	assert.Equal(t, "Alice 爱丽丝", subs[0].SidebarName)
	assert.Nil(t, subs[0].Roles)
	assert.False(t, subs[0].IsSubscribed)
	assert.Equal(t, model.RoomTypeDM, subs[0].RoomType)
}

func TestHandleRoomCreatedChannelBulkInsert(t *testing.T) {
	h, mocks := newTestHandler(t)
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx := natsutil.WithRequestID(context.Background(), reqID)

	mocks.store.EXPECT().FindUsersByAccounts(gomock.Any(), []string{"bob", "ian"}).Return([]model.User{
		{ID: "u_bob", Account: "bob", SiteID: "site-B"},
		{ID: "u_ian", Account: "ian", SiteID: "site-B"},
	}, nil)
	captured := captureBulkSubsArg(mocks)

	payload, _ := json.Marshal(model.RoomCreatedOutbox{
		RoomID: "r1", RoomType: model.RoomTypeChannel,
		RoomName: "deal team", HomeSiteID: "site-A",
		Accounts: []string{"bob", "ian"},
		RequesterAccount: "alice",
		Timestamp: 1,
	})
	require.NoError(t, h.handleRoomCreated(ctx, model.OutboxEvent{Payload: payload}))

	subs := captured.Args()
	require.Len(t, subs, 2)
	for _, s := range subs {
		assert.Equal(t, "deal team", s.Name)
		assert.Empty(t, s.SidebarName)
		assert.Equal(t, []model.Role{model.RoleMember}, s.Roles)
		assert.Equal(t, model.RoomTypeChannel, s.RoomType)
		assert.Equal(t, "site-A", s.SiteID)
	}
}
```

- [ ] **Step 2: Run — confirm failures**

Run: `make test SERVICE=inbox-worker -- -run "TestHandleRoomCreated"`

Expected: undefined `h.handleRoomCreated`.

- [ ] **Step 3: Add the handler**

Append to `inbox-worker/handler.go`:

```go
func (h *Handler) handleRoomCreated(ctx context.Context, evt model.OutboxEvent) error {
	requestID, _ := natsutil.RequestIDFromContext(ctx)
	if requestID == "" {
		return fmt.Errorf("missing X-Request-ID: %w", errPermanent)
	}

	var data model.RoomCreatedOutbox
	if err := json.Unmarshal(evt.Payload, &data); err != nil {
		return fmt.Errorf("unmarshal room_created payload: %w: %w", err, errPermanent)
	}
	if len(data.Accounts) == 0 {
		slog.Warn("room_created event with empty Accounts list",
			"requestId", requestID, "roomId", data.RoomID)
		return nil
	}

	users, err := h.store.FindUsersByAccounts(ctx, data.Accounts)
	if err != nil {
		return fmt.Errorf("find users by accounts: %w", err)
	}

	acceptedAt := time.UnixMilli(data.Timestamp).UTC()
	subs := make([]*model.Subscription, 0, len(users))
	for _, u := range users {
		sub := &model.Subscription{
			ID:           idgen.GenerateUUIDv7(),
			User:         model.SubscriptionUser{ID: u.ID, Account: u.Account},
			RoomID:       data.RoomID,
			SiteID:       data.HomeSiteID,
			Roles:        rolesForType(data.RoomType),
			Name:         subscriptionName(data, u),
			RoomType:     data.RoomType,
			SidebarName:  subscriptionSidebarName(data, u),
			IsSubscribed: subscriptionIsSubscribed(data, u),
			JoinedAt:     acceptedAt,
		}
		subs = append(subs, sub)
	}

	if len(subs) == 0 {
		return nil
	}
	if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // success-on-redelivery
		}
		return fmt.Errorf("bulk create subs: %w", err)
	}
	return nil
}
```

If `errPermanent` doesn't exist in `inbox-worker/handler.go`, add it as a sentinel:

```go
var errPermanent = errors.New("permanent")
```

- [ ] **Step 4: Wire dispatcher routing**

Find the inbox-worker event-type switch (it dispatches `member_added`, `room_sync`, etc.) and add:

```go
case model.MessageTypeRoomCreated:
    err = h.handleRoomCreated(ctx, evt)
```

- [ ] **Step 5: Run — confirm pass**

Run: `make test SERVICE=inbox-worker -- -run "TestHandleRoomCreated"`

Expected: PASS for all four subcases.

Run the full suite: `make test SERVICE=inbox-worker`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go
git commit -m "inbox-worker: handleRoomCreated builds remote-side subs from outbox payload"
```

---

## Phase 8 — Integration Tests

Each integration test uses real Mongo + NATS containers via `testcontainers-go`. Tests are tagged `//go:build integration` and run via `make test-integration SERVICE=<name>`.

### Task 40: room-service end-to-end create-channel + DM dedup

**Files:**
- Modify: `room-service/integration_test.go`

- [ ] **Step 1: Add helpers if missing**

Confirm `setupNATS(t)` and `setupMongo(t)` already exist in `integration_test.go`. If not, copy their pattern from existing add-member integration tests.

- [ ] **Step 2: Add the create-channel happy-path test**

Append to `room-service/integration_test.go`:

```go
//go:build integration

func TestCreateRoomChannelEndToEnd(t *testing.T) {
	ctx := context.Background()
	nc, _ := setupNATS(t)
	store, db := setupMongo(t)

	require.NoError(t, store.EnsureIndexes(ctx))
	mustInsertUser(t, db, &model.User{
		ID: "u_alice", Account: "alice", SiteID: "site-A",
		EngName: "Alice", ChineseName: "爱丽丝",
	})
	mustInsertUser(t, db, &model.User{
		ID: "u_bob", Account: "bob", SiteID: "site-A",
		EngName: "Bob", ChineseName: "鲍勃",
	})

	startRoomService(t, nc, store, "site-A")

	body, _ := json.Marshal(model.CreateRoomRequest{
		Name:  "deal team",
		Users: []string{"bob"},
	})
	reqID := idgen.GenerateRequestID()
	req := natsutil.NewMsg(natsutil.WithRequestID(ctx, reqID),
		"chat.user.alice.request.room.site-A.create", body)
	reply, err := nc.RequestMsg(req, 5*time.Second)
	require.NoError(t, err)

	var got model.CreateRoomReply
	require.NoError(t, json.Unmarshal(reply.Data, &got))
	assert.Equal(t, "accepted", got["status"])
	assert.Equal(t, "channel", got["roomType"])
	assert.NotEmpty(t, got["roomId"])

	// Canonical event published on the ROOMS stream.
	events := waitForCanonicalCreate(t, nc, "site-A", 5*time.Second)
	require.Len(t, events, 1)
	var canonical model.CreateRoomRequest
	require.NoError(t, json.Unmarshal(events[0].Data, &canonical))
	assert.Equal(t, got["roomId"], canonical.RoomID)
	assert.Equal(t, "alice", canonical.RequesterAccount)
}
```

`mustInsertUser`, `startRoomService`, `waitForCanonicalCreate` are small helpers — reuse or add at the top of `integration_test.go` mirroring existing patterns.

- [ ] **Step 3: Add the DM-dedup integration test**

Append:

```go
func TestCreateRoomDMAlreadyExists(t *testing.T) {
	ctx := context.Background()
	nc, _ := setupNATS(t)
	store, db := setupMongo(t)
	require.NoError(t, store.EnsureIndexes(ctx))

	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice",
		EngName: "Alice", ChineseName: "爱丽丝", SiteID: "site-A"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob",
		EngName: "Bob", ChineseName: "鲍勃", SiteID: "site-A"})

	roomID := idgen.BuildDMRoomID("u_alice", "u_bob")
	mustInsertRoom(t, db, &model.Room{ID: roomID, Type: model.RoomTypeDM, SiteID: "site-A"})
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-A",
		User:     model.SubscriptionUser{ID: "u_alice", Account: "alice"},
		Name:     "bob", RoomType: model.RoomTypeDM,
	})

	startRoomService(t, nc, store, "site-A")

	body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"bob"}})
	reqID := idgen.GenerateRequestID()
	req := natsutil.NewMsg(natsutil.WithRequestID(ctx, reqID),
		"chat.user.alice.request.room.site-A.create", body)
	reply, err := nc.RequestMsg(req, 5*time.Second)
	require.NoError(t, err)

	var got model.ErrorResponse
	require.NoError(t, json.Unmarshal(reply.Data, &got))
	assert.Equal(t, "dm already exists", got.Error)
	assert.Equal(t, roomID, got.RoomID)
}
```

- [ ] **Step 4: Run integration tests**

Run: `make test-integration SERVICE=room-service -- -run "TestCreateRoomChannelEndToEnd|TestCreateRoomDMAlreadyExists"`

Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add room-service/integration_test.go
git commit -m "room-service: integration tests for create-room channel + DM dedup"
```

---

### Task 41: room-worker integration tests (create-channel, create-DM, idempotency)

**Files:**
- Modify: `room-worker/integration_test.go`

- [ ] **Step 1: Add the create-channel test**

Append to `room-worker/integration_test.go`:

```go
//go:build integration

func TestProcessCreateRoomChannelPersistsAllState(t *testing.T) {
	ctx := context.Background()
	store, db := setupMongo(t)
	mustInsertUser(t, db, &model.User{
		ID: "u_alice", Account: "alice", SiteID: "site-A",
		EngName: "Alice", ChineseName: "爱丽丝",
	})
	mustInsertUser(t, db, &model.User{
		ID: "u_bob", Account: "bob", SiteID: "site-A",
		EngName: "Bob", ChineseName: "鲍勃",
	})

	h := newIntegrationHandler(t, store, "site-A")
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx = natsutil.WithRequestID(ctx, reqID)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r_xyz", Name: "deal team",
		Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: time.Now().UTC().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	room, err := store.GetRoom(ctx, "r_xyz")
	require.NoError(t, err)
	assert.Equal(t, "deal team", room.Name)
	assert.Equal(t, model.RoomTypeChannel, room.Type)
	assert.Equal(t, 2, room.UserCount)
	assert.Equal(t, 0, room.AppCount)

	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": "r_xyz"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), subCount)

	rmCount, err := db.Collection("room_members").CountDocuments(ctx, bson.M{"rid": "r_xyz"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), rmCount) // owner-only since no orgs in payload
}
```

- [ ] **Step 2: Add the create-DM test**

```go
func TestProcessCreateRoomDMPersistsTwoSubsAndZeroMembers(t *testing.T) {
	ctx := context.Background()
	store, db := setupMongo(t)
	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice",
		EngName: "A", ChineseName: "A", SiteID: "site-A"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob",
		EngName: "B", ChineseName: "B", SiteID: "site-B"})

	h := newIntegrationHandler(t, store, "site-A")
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx = natsutil.WithRequestID(ctx, reqID)

	roomID := idgen.BuildDMRoomID("u_alice", "u_bob")
	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: roomID,
		Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: time.Now().UTC().UnixMilli(),
	})
	require.NoError(t, h.processCreateRoom(ctx, body))

	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": roomID})
	require.NoError(t, err)
	assert.Equal(t, int64(2), subCount)

	rmCount, err := db.Collection("room_members").CountDocuments(ctx, bson.M{"rid": roomID})
	require.NoError(t, err)
	assert.Equal(t, int64(0), rmCount)

	room, err := store.GetRoom(ctx, roomID)
	require.NoError(t, err)
	assert.Equal(t, model.RoomTypeDM, room.Type)
	assert.Empty(t, room.CreatedBy)
}
```

- [ ] **Step 3: Add the idempotent-redelivery test**

```go
func TestProcessCreateRoomIdempotentRedelivery(t *testing.T) {
	ctx := context.Background()
	store, db := setupMongo(t)
	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice",
		EngName: "A", ChineseName: "A", SiteID: "site-A"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob",
		EngName: "B", ChineseName: "B", SiteID: "site-A"})

	h := newIntegrationHandler(t, store, "site-A")
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx = natsutil.WithRequestID(ctx, reqID)

	body, _ := json.Marshal(model.CreateRoomRequest{
		RoomID: "r_idem", Name: "team",
		Users: []string{"bob"},
		RequesterID: "u_alice", RequesterAccount: "alice",
		Timestamp: time.Now().UTC().UnixMilli(),
	})

	require.NoError(t, h.processCreateRoom(ctx, body))
	require.NoError(t, h.processCreateRoom(ctx, body)) // redelivery

	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": "r_idem"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), subCount, "redelivery must not create duplicate subs")
}
```

- [ ] **Step 4: Run integration tests**

Run: `make test-integration SERVICE=room-worker -- -run "TestProcessCreateRoomChannelPersistsAllState|TestProcessCreateRoomDMPersistsTwoSubsAndZeroMembers|TestProcessCreateRoomIdempotentRedelivery"`

Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add room-worker/integration_test.go
git commit -m "room-worker: integration tests for processCreateRoom (channel, DM, idempotency)"
```

---

### Task 42: inbox-worker integration test for `handleRoomCreated`

**Files:**
- Modify: `inbox-worker/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `inbox-worker/integration_test.go`:

```go
//go:build integration

func TestHandleRoomCreatedPersistsRemoteSubs(t *testing.T) {
	ctx := context.Background()
	store, db := setupMongo(t)
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob",
		SiteID: "site-B", EngName: "Bob", ChineseName: "鲍勃"})
	mustInsertUser(t, db, &model.User{ID: "u_ian", Account: "ian",
		SiteID: "site-B", EngName: "Ian", ChineseName: "伊恩"})

	h := newIntegrationHandler(t, store, "site-B")
	const reqID = "0193abcd-0193-7abc-89ab-0193abcd0193"
	ctx = natsutil.WithRequestID(ctx, reqID)

	payload, _ := json.Marshal(model.RoomCreatedOutbox{
		RoomID: "r_xyz", RoomType: model.RoomTypeChannel,
		RoomName: "deal team", HomeSiteID: "site-A",
		Accounts:             []string{"bob", "ian"},
		RequesterAccount:     "alice",
		RequesterEngName:     "Alice",
		RequesterChineseName: "爱丽丝",
		Timestamp:            time.Now().UTC().UnixMilli(),
	})
	require.NoError(t, h.handleRoomCreated(ctx, model.OutboxEvent{Payload: payload}))

	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": "r_xyz"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), subCount)

	// Inbox-worker must NOT have created the room doc on site-B.
	roomCount, err := db.Collection("rooms").CountDocuments(ctx, bson.M{"_id": "r_xyz"})
	require.NoError(t, err)
	assert.Equal(t, int64(0), roomCount, "inbox-worker must not create room mirror")

	// Verify subscription fields.
	var bobSub model.Subscription
	require.NoError(t, db.Collection("subscriptions").FindOne(ctx,
		bson.M{"roomId": "r_xyz", "u.account": "bob"}).Decode(&bobSub))
	assert.Equal(t, "deal team", bobSub.Name)
	assert.Equal(t, "site-A", bobSub.SiteID)
	assert.Equal(t, model.RoomTypeChannel, bobSub.RoomType)
}
```

- [ ] **Step 2: Run integration test**

Run: `make test-integration SERVICE=inbox-worker -- -run TestHandleRoomCreatedPersistsRemoteSubs`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add inbox-worker/integration_test.go
git commit -m "inbox-worker: integration test for handleRoomCreated remote-sub persistence"
```

---

## Phase 9 — Verification & Wiring

### Task 43: Full lint + unit + integration sweep

**Files:** none (verification only)

- [ ] **Step 1: Run formatter**

Run: `make fmt`

Expected: clean tree (no diffs after).

- [ ] **Step 2: Run linter**

Run: `make lint`

Expected: zero issues.

- [ ] **Step 3: Run all unit tests with race**

Run: `make test`

Expected: PASS for every package, including `pkg/model`, `pkg/subject`, `pkg/pipelines`, `room-service`, `room-worker`, `inbox-worker`.

- [ ] **Step 4: Run integration tests for every touched service**

Run each in turn:

```
make test-integration SERVICE=room-service
make test-integration SERVICE=room-worker
make test-integration SERVICE=inbox-worker
```

Expected: PASS for each.

- [ ] **Step 5: Verify no stale references**

Run:
```
grep -rn "ReconcileUserCount" --include="*.go" .
grep -rn "AddMembersRequest.*RequestID" --include="*.go" .
grep -rn "FindSubscriptionByName\|FindExistingDMByRoomID" --include="*.go" .
```

Expected: zero matches in production code (the first should only match historical rename evidence in commit messages — not in `.go` files; the latter two are abandoned spec naming and should not exist anywhere in the tree).

- [ ] **Step 6: Verify all new sentinels are reachable**

Run:
```
grep -n "errEmptyCreateRequest\|errSelfDM\|errBotInChannel\|errBotNotAvailable\|errInvalidUserData\|errMissingRequestID\|errUserNotFound" room-service/handler.go
```

Expected: every sentinel referenced at least once.

- [ ] **Step 7: No commit (verification only)**

If any step failed, fix the underlying issue and re-run from Step 1. Do NOT skip steps.

---

### Task 44: Manual smoke test against docker-compose

**Files:** none (manual verification)

- [ ] **Step 1: Bring up the local stack**

Run: `docker compose -f room-service/deploy/docker-compose.yml up -d`

(Or whichever compose entry covers room-service + room-worker + inbox-worker + NATS + Mongo.)

Wait until all containers are `healthy`.

- [ ] **Step 2: Send a create-channel request via the `nats` CLI**

```
nats request \
  "chat.user.alice.request.room.site-local.create" \
  --header "X-Request-ID: 0193abcd-0193-7abc-89ab-0193abcd0193" \
  '{"name":"smoke-test","users":["bob"]}'
```

Expected: `{"status":"accepted","roomId":"<id>","roomType":"channel"}` reply within ~1s.

- [ ] **Step 3: Verify Mongo state**

```
mongosh --eval 'db.rooms.findOne({name:"smoke-test"})'
mongosh --eval 'db.subscriptions.find({roomId: "<id from step 2>"}).pretty()'
mongosh --eval 'db.room_members.find({rid: "<id>"}).pretty()'
```

Expected:
- One room doc with `userCount: 2`, `appCount: 0`, `type: "channel"`.
- Two subscription docs (alice + bob) with `name: "smoke-test"`, `roomType: "channel"`. Alice's `roles: ["owner"]`; Bob's `roles: ["member"]`.
- One room_members doc for the owner.

- [ ] **Step 4: Verify sys-messages on the canonical stream**

```
nats stream view MESSAGES_CANONICAL_site-local --count 10
```

Expected: two new messages with `Type: "room_created"` and `Type: "members_added"` for the new room.

- [ ] **Step 5: Send a botDM request**

(Requires an `apps` doc and a `weather.bot` user pre-seeded — add a tiny seed step if not already in docker-compose init scripts.)

```
nats request \
  "chat.user.alice.request.room.site-local.create" \
  --header "X-Request-ID: 0193abcd-0193-7abc-89ab-0193abcd0193" \
  '{"users":["weather.bot"]}'
```

Expected: `{"status":"accepted","roomId":"<deterministic>","roomType":"botDM"}`.

Verify the human's subscription has `isSubscribed: true` and `sidebarName: "Weather Bot"`.

- [ ] **Step 6: Send the same DM request twice — observe dedup**

```
nats request "chat.user.alice.request.room.site-local.create" \
  --header "X-Request-ID: <fresh-uuid-1>" \
  '{"users":["bob"]}'

nats request "chat.user.alice.request.room.site-local.create" \
  --header "X-Request-ID: <fresh-uuid-2>" \
  '{"users":["bob"]}'
```

Expected: first reply `{"status":"accepted",...}`; second reply `{"error":"dm already exists","roomId":"<same id>"}`.

- [ ] **Step 7: Tear down**

Run: `docker compose -f room-service/deploy/docker-compose.yml down -v`

- [ ] **Step 8: No commit (manual smoke is verification only)**

Record any unexpected behavior in a follow-up ticket. If any step failed, debug at the affected service and re-run.

---

## Self-Review Notes

**Spec coverage check (run mentally before declaring done):**

- ✅ Section 2 (NATS Subjects) — Tasks 7, 8, 24 (parser), 28 (subscription), 39 (inbox routing).
- ✅ Section 2 (Data Models) — Tasks 1–6 cover every model addition; Task 36 adds `RoomCreated` sys-message struct.
- ✅ Section 2 (Reply Contract) — Tasks 25, 26 (synchronous reply), Task 27 (DM-exists special reply), Task 37 (async-job).
- ✅ Section 3 (room-service Request Handling) — Tasks 18–28 cover sentinels, helpers, store, indices, every step of the validation pipeline, and main.go subscription.
- ✅ Section 4 (room-worker Handler) — Tasks 29–37 cover store, helpers, skeleton, three branches, fan-out, sys-messages, outbox, dispatcher.
- ✅ Section 5 (inbox-worker Handler) — Tasks 38–39.
- ✅ Section 6 (requestId & async-job) — propagated through Tasks 12, 16, 24, 32, 37, 39.
- ✅ Section 7 (Cross-Site Scenarios) — covered by Tasks 41 (room-worker e2e channel + DM), 42 (inbox-worker e2e), 44 (manual smoke).
- ✅ Section 8 (Error Handling) — Task 18 (sentinels), Task 20 (sanitizeError), Task 32 (errPermanent), Task 37 (dispatcher).
- ✅ Section 9 (Testing Strategy) — All unit, integration, and verification tasks.
- ✅ Add-member retrofit — Tasks 12–17 cover X-Request-ID enforcement, deterministic sub IDs, Subscription.Name/RoomType population, MemberAddEvent.RoomName, async-job notification, and inbox-worker counterpart.
- ✅ Mongo indices — Task 23.
- ✅ ReconcileMemberCounts split — Tasks 10, 11.

**Type / signature consistency:**

- `errPermanent` introduced in Task 12 and reused throughout (Tasks 32, 37, 39, etc.).
- `newSub` constructor introduced in Task 31 and used by Tasks 33, 34.
- `bulkCreateSubsIdempotent` and `bulkCreateMembersIdempotent` introduced in Task 33 and used by Task 34.
- `finishCreateRoom` declared as a stub in Task 33, fleshed out in Tasks 35, 36, 37.
- `composeName` lives in both room-worker (Task 31) and inbox-worker (Task 38) — duplicated, not yet shared via `pkg/`. Acceptable per the spec; a future cleanup could extract.
- `composeAutoName` and `truncateRunes` defined in Task 19 (room-service) and copied into Task 31 (room-worker). Same caveat.
- `model.AsyncJobOpRoomCreate` and `model.AsyncJobOpRoomMemberAdd` defined in Task 6 and used in Tasks 16, 37 (and the dispatcher in Task 37).

**No placeholders found.** Every step's code block is complete. Every task has a final commit step.

---

## Execution

The plan is complete and saved to `docs/superpowers/plans/2026-04-28-create-room.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration. Uses the `superpowers:subagent-driven-development` skill.

2. **Inline Execution** — Execute tasks in this session using the `superpowers:executing-plans` skill, batch execution with checkpoints for review.

Which approach?

