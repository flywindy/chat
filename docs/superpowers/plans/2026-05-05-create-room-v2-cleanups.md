# Create Room v2 Cleanups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Apply the round-3 review cleanups (mliu) to the create-room feature already shipped on PR #142. Drops `Subscription.SidebarName` and the eng/chinese-name composition pathway, simplifies validation, sets `Room.CreatedBy` consistently, and tightens worker code structure. No external behavior change beyond persisted-field shape — frontend now resolves display names from the locally-replicated `users`/`apps` collections.

**Architecture:** All edits are in-place to existing files. The change is a coordinated drop of one persisted field (`SidebarName`) plus the three outbox helper fields (`RequesterEngName`, `RequesterChineseName`, `AppName`) and the `CreateRoomRequest.AppName`/`Subscription` ripples that follow. The model layer must change first; downstream code that referenced the removed fields must change in the same commit chain to avoid breaking compilation between commits.

**Tech Stack:** Go 1.25 · NATS + JetStream · MongoDB · `go.uber.org/mock` · `stretchr/testify` · `testcontainers-go`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-04-28-create-room-design.md` (v2 update marker dated 2026-05-05).

**Foundation:** PR #142 has shipped to `claude/implement-create-room-ePJUK`. The original plan `docs/superpowers/plans/2026-04-28-create-room.md` is fully executed; this addendum only covers deltas. The PR is in draft so CI does not run on each push during this work.

---

## File Structure Map

### `pkg/model/` (shared types — change first to fail compile downstream until everything is updated)

| File | Change | What it owns |
|------|--------|--------------|
| `subscription.go` | modify | Drop `Subscription.SidebarName` field (line 31). |
| `member.go` | modify | Drop `CreateRoomRequest.AppName` field (line 150). |
| `event.go` | modify | Drop `RoomCreatedOutbox.RequesterEngName`, `RequesterChineseName`, `AppName` (lines 222–224). |
| `model_test.go` | modify | Update affected round-trip tests; drop `TestCreateRoomRequestBotDMHasAppName`, `TestRoomCreatedOutboxBotDMHasAppName`; assert removed fields are absent (no struct field by name) where reasonable. |

### `room-service/` (synchronous validation gateway)

| File | Change | What it owns |
|------|--------|--------------|
| `handler.go` | modify | Skip EngName/ChineseName check on counterpart for botDM (line 237). Drop `req.AppName = app.Name` (line 267, field no longer exists). Fold pre-strip self-DM check into the post-strip length check (lines 197–203 + 205). |
| `helper.go` | modify | Merge the two `err.Error()` pass-through case groups in `sanitizeError` into one branch (line 172 group + line 182 group). |
| `store_mongo.go` | modify | Replace `db *mongo.Database` field with `apps *mongo.Collection`; bind in `NewMongoStore`; update `EnsureIndexes` and `GetApp` call sites. |
| `handler_test.go` | modify | Drop assertions on removed fields. Add `TestHandleCreateRoom_BotDM_AppCounterpartNoNameFields` (botDM proceeds when counterpart has empty EngName/ChineseName). |
| `helper_test.go` | modify | Existing sanitize tests still pass; no new tests required (behavior unchanged, only structural refactor). |

### `room-worker/` (async finalize)

| File | Change | What it owns |
|------|--------|--------------|
| `handler.go` | modify | (a) `Room.CreatedBy = requester.ID` for **all** room types — drop `createdByForType` helper (line 843). (b) `resolveRoomName` returns `""` for DM/botDM. (c) `newSub` signature loses `sidebarName string` parameter (line 853); all 5 call sites updated. (d) DM branch: `Subscription.Name` = `other.Account` / `requester.Account` (no `composeNameOrAccount`). (e) botDM branch: human's sub `Name = bot.Account`, bot's sub `Name = requester.Account` (no `req.AppName`, no `composeNameOrAccount`). (f) Channel branch: build owner sub in the same loop as members; drop the trailing append. (g) Drop `composeName` and `composeNameOrAccount` helpers entirely. (h) Outbox publish drops `RequesterEngName`/`RequesterChineseName`/`AppName` fields (line 1136 area). (i) Drop redundant `len()` guards before `BulkCreateRoomMembers` and the subscription-update publish loop. (j) Verify `acceptedAt` is derived once at the top of `processCreateRoom` (it already is — lines 396/589/etc check). |
| `handler_test.go` | modify | Update DM/botDM/channel happy-path assertions: `Room.CreatedBy = requester.ID`, `Room.Name == ""` for DM/botDM, `Subscription.Name == counterpart account`, no `SidebarName` references. Add `TestProcessCreateRoom_DM_RoomCreatedByRequester`, `TestProcessCreateRoom_BotDM_RoomCreatedByRequester`, `TestProcessCreateRoom_BotDM_HumanSubNameIsBotAccount`. |
| `integration_test.go` | modify | Same field-shape updates in DM/botDM/channel happy-path integration tests. |

### `inbox-worker/` (cross-site receipt)

| File | Change | What it owns |
|------|--------|--------------|
| `handler.go` | modify | Drop `subscriptionSidebarName` helper (line 231). Drop `composeName` helper (line 197). `subscriptionName` for botDM no longer needs to inspect `isBot` for the rare cross-site case — bot's sub name is `RequesterAccount` (only the home site holds humans' subs in botDM). Sub construction drops the `SidebarName` field set at line 299. |
| `handler_test.go` | modify | Drop assertions on `SidebarName` in `TestHandleRoomCreatedDMBuildsRemoteSub` and `TestHandleRoomCreatedChannelBulkInsert`. |
| `integration_test.go` | modify | Same — drop SidebarName assertions in `TestHandleRoomCreatedPersistsRemoteSubs`. |

---

## Build Order

The model layer must change before any service that imports it (otherwise the intermediate commits fail to compile and break `git bisect`). Sequence:

1. Tasks 1–2 — model field drops + test updates (compilation breaks downstream services until Tasks 3+).
2. Tasks 3–6 — room-worker downstream cleanups (resolves the compile errors from Task 1).
3. Tasks 7–9 — inbox-worker downstream cleanups (parallel to Tasks 3–6, but listed sequentially).
4. Tasks 10–13 — room-service cleanups (independent of model field drops; can interleave).
5. Task 14 — MongoStore field refactor.
6. Task 16 — add-member bot rejection parity (independent of model changes; only touches `room-service`).
7. Task 15 — final verification + commit + push.

Each task is self-contained and ends with a green `make lint && make test SERVICE=<service>` and a commit. Cross-service compile errors only span Tasks 1→6 — keep that window short.

---

## Task 1: Drop Subscription.SidebarName and outbox/request fields (model)

**Files:**
- Modify: `pkg/model/subscription.go:31`
- Modify: `pkg/model/event.go:222-224`
- Modify: `pkg/model/member.go:150`
- Modify: `pkg/model/model_test.go` (tests around `TestCreateRoomRequestBotDMHasAppName`, `TestRoomCreatedOutboxBotDMHasAppName`, any sidebar-related round-trip test)

> **Note for the executor:** This task intentionally leaves the tree non-compiling for downstream services. The plan resolves that in Tasks 3–9. Do not push between Task 1 and Task 9 unless the build is green.

- [ ] **Step 1: Read the existing `Subscription` struct**

```bash
sed -n '23,40p' pkg/model/subscription.go
```

Expected: line 31 has `SidebarName string ...` between `RoomType` and `IsSubscribed`.

- [ ] **Step 2: Drop `SidebarName` from `Subscription`**

Edit `pkg/model/subscription.go` and remove this single line:

```go
    SidebarName        string           `json:"sidebarName,omitempty"   bson:"sidebarName,omitempty"`
```

- [ ] **Step 3: Drop `RequesterEngName` / `RequesterChineseName` / `AppName` from `RoomCreatedOutbox`**

Edit `pkg/model/event.go`. The `RoomCreatedOutbox` struct should end up exactly:

```go
type RoomCreatedOutbox struct {
    RoomID           string   `json:"roomId"`
    RoomType         RoomType `json:"roomType"`
    RoomName         string   `json:"roomName"`
    HomeSiteID       string   `json:"homeSiteId"`
    Accounts         []string `json:"accounts"`
    RequesterAccount string   `json:"requesterAccount"`
    Timestamp        int64    `json:"timestamp"`
}
```

- [ ] **Step 4: Drop `AppName` from `CreateRoomRequest`**

Edit `pkg/model/member.go`. Remove the line at ~150:

```go
    AppName          string `json:"appName,omitempty" bson:"appName,omitempty"`
```

- [ ] **Step 5: Update model tests**

Edit `pkg/model/model_test.go`:

  - Delete `TestCreateRoomRequestBotDMHasAppName` (the field no longer exists).
  - Delete `TestRoomCreatedOutboxBotDMHasAppName`.
  - In `TestCreateRoomRequestBotDM` (or whichever DM/botDM round-trip test sets `AppName`), drop the `AppName` field assignment.
  - In any subscription round-trip test that sets `SidebarName`, drop the assignment.
  - Add a small assertion that `Subscription` JSON does NOT contain a `sidebarName` key:

```go
func TestSubscriptionJSON_NoSidebarName(t *testing.T) {
    s := model.Subscription{
        ID:       "s1",
        User:     model.SubscriptionUser{ID: "u1", Account: "alice"},
        RoomID:   "r1",
        RoomType: model.RoomTypeChannel,
        Name:     "deal team",
    }
    data, err := json.Marshal(&s)
    require.NoError(t, err)
    var raw map[string]any
    require.NoError(t, json.Unmarshal(data, &raw))
    _, hasSidebar := raw["sidebarName"]
    assert.False(t, hasSidebar, "sidebarName must not appear in Subscription JSON")
}
```

- [ ] **Step 6: Run model tests in isolation**

Run: `go test ./pkg/model/... -run 'TestSubscription|TestCreateRoomRequest|TestRoomCreatedOutbox' -count=1`

Expected: PASS.

- [ ] **Step 7: Confirm full repo does NOT compile yet (sanity check)**

Run: `go build ./... 2>&1 | head -20`

Expected: compile errors in `room-worker`, `inbox-worker`, `room-service` referencing removed fields. This is intentional — Tasks 3–13 fix these.

- [ ] **Step 8: Stage but do not commit yet**

Run: `git add pkg/model/subscription.go pkg/model/event.go pkg/model/member.go pkg/model/model_test.go`

Do not commit — Task 14 commits the consolidated change once the tree compiles end-to-end.

---

## Task 2: Drop helper.composeName and composeNameOrAccount in room-worker

**Files:**
- Modify: `room-worker/handler.go:814-840` (delete `composeName` and `composeNameOrAccount`)

- [ ] **Step 1: Locate the helpers**

Run: `grep -n "func composeName\|func composeNameOrAccount" room-worker/handler.go`

Expected: two definitions around lines 815 and 826.

- [ ] **Step 2: Delete both helpers**

Edit `room-worker/handler.go`. Remove both functions and their leading comments (lines ~814–840). After this edit, references in `processCreateRoomDM` and `processCreateRoomBotDM` will fail to compile until Task 3 rewrites them.

- [ ] **Step 3: Confirm compile errors point at processCreateRoomDM/BotDM**

Run: `go build ./room-worker/... 2>&1 | head -10`

Expected: errors mention `composeNameOrAccount` undefined inside `processCreateRoomDM` / `processCreateRoomBotDM`. This is what Task 3 fixes.

---

## Task 3: Update room-worker DM/botDM branches to use raw counterpart accounts

**Files:**
- Modify: `room-worker/handler.go` — `processCreateRoomDM`, `processCreateRoomBotDM`, `newSub` signature, all callers.

- [ ] **Step 1: Read the current `newSub` signature and call sites**

Run:
```bash
grep -n "func newSub\|newSub(" room-worker/handler.go
```

Expected: helper at ~line 853 with signature `(id, user, room, roles, name, sidebarName, isSubscribed, joinedAt)`. Callers exist in `processCreateRoomDM`, `processCreateRoomBotDM`, and `processCreateRoomChannel`.

- [ ] **Step 2: Drop the `sidebarName` parameter from `newSub`**

Edit `room-worker/handler.go`. Replace the helper block exactly:

```go
func newSub(id string, user *model.User, room *model.Room, roles []model.Role,
    name string, isSubscribed bool, joinedAt time.Time) *model.Subscription {
    return &model.Subscription{
        ID:           id,
        User:         model.SubscriptionUser{ID: user.ID, Account: user.Account},
        RoomID:       room.ID,
        SiteID:       room.SiteID,
        Roles:        roles,
        Name:         name,
        RoomType:     room.Type,
        IsSubscribed: isSubscribed,
        JoinedAt:     joinedAt,
    }
}
```

- [ ] **Step 3: Update `processCreateRoomDM` call sites**

Find the function and replace its sub construction. The two `newSub` calls become:

```go
subs := []*model.Subscription{
    newSub(idgen.GenerateUUIDv7(), requester, room, nil,
        other.Account, false, acceptedAt),
    newSub(idgen.GenerateUUIDv7(), other, room, nil,
        requester.Account, false, acceptedAt),
}
```

(No `composeNameOrAccount`, no sidebarName parameter.)

- [ ] **Step 4: Update `processCreateRoomBotDM` call sites**

Replace the two `newSub` calls in `processCreateRoomBotDM`:

```go
subs := []*model.Subscription{
    // Human's sub: Name = bot account; IsSubscribed = true
    newSub(idgen.GenerateUUIDv7(), requester, room, nil,
        bot.Account, true, acceptedAt),
    // Bot's sub: Name = requester's account; IsSubscribed = false
    newSub(idgen.GenerateUUIDv7(), bot, room, nil,
        requester.Account, false, acceptedAt),
}
```

(No `req.AppName` — that field no longer exists.)

- [ ] **Step 5: Update `processCreateRoomChannel` call sites**

Find the channel branch's `newSub` call (the one inside the per-user loop) and the trailing owner append. Replace the loop body and drop the trailing append (we'll consolidate in Task 5). For now, just drop the `sidebarName` (`""`) argument from the existing two `newSub` calls so the file compiles. Task 5 collapses the loop.

The two `newSub` calls become (without sidebarName):

```go
// inside the per-user loop:
subs = append(subs, newSub(
    idgen.GenerateUUIDv7(), u, room,
    []model.Role{model.RoleMember},
    room.Name,
    false, acceptedAt))
```

```go
// trailing owner append:
subs = append(subs, newSub(
    idgen.GenerateUUIDv7(), requester, room,
    []model.Role{model.RoleOwner},
    room.Name,
    false, acceptedAt))
```

- [ ] **Step 6: Build room-worker**

Run: `go build ./room-worker/... 2>&1 | head -10`

Expected: success (no output).

- [ ] **Step 7: Run room-worker unit tests (some will fail — those are fixed in Task 4)**

Run: `go test ./room-worker/... -run 'TestProcessCreateRoom' -count=1 2>&1 | tail -20`

Expected: tests reference `SidebarName` field assertions or `composeNameOrAccount` — failures are expected. Note which tests fail; Task 4 covers them.

---

## Task 4: Update room-worker tests for new sub shape

**Files:**
- Modify: `room-worker/handler_test.go`
- Modify: `room-worker/integration_test.go`

- [ ] **Step 1: Find SidebarName references in tests**

Run: `grep -n "SidebarName" room-worker/handler_test.go room-worker/integration_test.go`

- [ ] **Step 2: Drop every `SidebarName` assertion**

For each match, remove the assertion line (e.g. `assert.Equal(t, "Bob 鲍勃", got.SidebarName)`) and any setup line that produces it (`req.AppName = "Weather Bot"` etc.).

- [ ] **Step 3: Update DM happy-path assertion**

Find `TestProcessCreateRoom_DM_*` (handler_test.go) and update the asserted `Subscription.Name` values:

```go
// alice's sub
assert.Equal(t, "bob", aliceSub.Name)        // counterpart account, not "Bob 鲍勃"
assert.Empty(t, aliceSub.SidebarName)        // DELETE this line — field is gone
// bob's sub
assert.Equal(t, "alice", bobSub.Name)
```

- [ ] **Step 4: Update botDM happy-path assertions**

Find `TestProcessCreateRoom_BotDM_*` and update the human's sub:

```go
// human's sub
assert.Equal(t, "weather.bot", humanSub.Name)   // bot's account, not "Weather Bot"
assert.True(t, humanSub.IsSubscribed)
// bot's sub
assert.Equal(t, "alice", botSub.Name)            // requester's account
```

- [ ] **Step 5: Run room-worker unit tests**

Run: `make test SERVICE=room-worker`

Expected: PASS.

- [ ] **Step 6: Stage**

Run: `git add room-worker/handler.go room-worker/handler_test.go`

Do not commit yet — Tasks 5/6 land in the same commit chain.

---

## Task 5: Collapse channel sub building into a single loop

**Files:**
- Modify: `room-worker/handler.go` — `processCreateRoomChannel`

- [ ] **Step 1: Read the current channel branch**

Run:
```bash
sed -n '/func.*processCreateRoomChannel/,/^}/p' room-worker/handler.go | head -80
```

Expected: a per-user loop appending to `subs`, followed by a trailing append for the owner.

- [ ] **Step 2: Write the single-loop version**

Replace the sub-building section with this exact shape:

```go
allUsers := append([]*model.User{requester}, users...)
subs := make([]*model.Subscription, 0, len(allUsers))
for _, u := range allUsers {
    roles := []model.Role{model.RoleMember}
    if u.ID == requester.ID {
        roles = []model.Role{model.RoleOwner}
    }
    subs = append(subs, newSub(
        idgen.GenerateUUIDv7(), u, room, roles,
        room.Name, false, acceptedAt))
}
```

- [ ] **Step 3: Update the room_members section to walk subs directly**

Find the room_members building section in the same function. Replace the `if writeIndividuals` branch + trailing owner append with a single loop:

```go
members := make([]*model.RoomMember, 0, len(subs)+len(req.Orgs))
for _, sub := range subs {
    members = append(members, &model.RoomMember{
        ID:     idgen.GenerateUUIDv7(),
        RoomID: room.ID,
        Ts:     acceptedAt,
        Member: model.RoomMemberEntry{
            ID:      sub.User.ID,
            Type:    model.RoomMemberIndividual,
            Account: sub.User.Account,
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
if err := h.store.BulkCreateRoomMembers(ctx, members); err != nil {
    return fmt.Errorf("bulk create room_members: %w", err)
}
```

(No `len(members) > 0` guard — `members` is always non-empty for a channel because the owner is always present.)

- [ ] **Step 4: Drop the system-message `AddedUsersCount: len(subs) - 1`**

Search for `AddedUsersCount` in `processCreateRoomChannel`:

```bash
grep -n "AddedUsersCount" room-worker/handler.go
```

The `- 1` (excluding owner) becomes `len(users)` (members only, since `users` is the slice without the requester). Or compute `len(subs) - 1` — both are equivalent because owner is now at index 0. Use whichever the surrounding code already uses; the value must remain "non-owner subs count".

- [ ] **Step 5: Run room-worker tests**

Run: `make test SERVICE=room-worker`

Expected: PASS. Channel happy-path tests assert `len(subs) == invitees + 1` and `RoomMember` count — both unchanged.

- [ ] **Step 6: Stage**

Run: `git add room-worker/handler.go room-worker/handler_test.go`

---

## Task 6: Set Room.CreatedBy = requester.ID for all room types; Room.Name = "" for DM/botDM

**Files:**
- Modify: `room-worker/handler.go` — `resolveRoomName`, `createdByForType` (delete), `processCreateRoom` Room build.
- Modify: `room-worker/handler_test.go` — DM/botDM Room assertions.
- Modify: `room-worker/integration_test.go` — DM/botDM Room assertions.

- [ ] **Step 1: Locate the helpers**

Run: `grep -n "func resolveRoomName\|func createdByForType\|CreatedBy: createdByForType" room-worker/handler.go`

- [ ] **Step 2: Delete `createdByForType`**

Edit `room-worker/handler.go`. Delete the helper at ~line 843 entirely.

- [ ] **Step 3: Update the Room build site**

In `processCreateRoom`, change the `CreatedBy` line:

```go
// Before:
CreatedBy: createdByForType(requester.ID, roomType),
// After:
CreatedBy: requester.ID,
```

- [ ] **Step 4: Update `resolveRoomName`**

Replace the helper:

```go
// resolveRoomName: DM/botDM use empty string (per-subscriber name lives on
// Subscription.Name); channels use req.Name verbatim (room-service has
// already validated non-empty and ≤ 100 runes).
func resolveRoomName(req *model.CreateRoomRequest, roomType model.RoomType) string {
    if roomType == model.RoomTypeChannel {
        return req.Name
    }
    return ""
}
```

- [ ] **Step 5: Update DM happy-path test**

Find `TestProcessCreateRoom_DM_*` and update the Room assertions:

```go
assert.Equal(t, "", room.Name)                   // was: room.ID
assert.Equal(t, requester.ID, room.CreatedBy)    // was: ""
```

- [ ] **Step 6: Update botDM happy-path test**

Same as above for `TestProcessCreateRoom_BotDM_*`.

- [ ] **Step 7: Update collision-on-redelivery test**

If a test asserts the existing-room match check uses `Name` and `CreatedBy`, the values must reflect the new shape. Search for tests that mention "room ID collision":

```bash
grep -n "RoomIDCollision\|room ID collision" room-worker/handler_test.go
```

For each, ensure the seeded existing room has `Name=""` and `CreatedBy=requester.ID` for DM/botDM cases.

- [ ] **Step 8: Update integration tests**

Same field updates in `room-worker/integration_test.go` for `TestRoomWorker_CreateDM_Integration` etc.

- [ ] **Step 9: Run room-worker tests**

Run: `make test SERVICE=room-worker`

Expected: PASS.

- [ ] **Step 10: Stage**

Run: `git add room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go`

---

## Task 7: Update room-worker outbox publish to drop dropped fields

**Files:**
- Modify: `room-worker/handler.go` — outbox publish step (around line 1136).

- [ ] **Step 1: Locate the outbox publish**

Run: `grep -n "RoomCreatedOutbox{" room-worker/handler.go`

Expected: one site in `processCreateRoom` building the outbox payload.

- [ ] **Step 2: Update the struct literal**

The literal must become exactly:

```go
out := model.RoomCreatedOutbox{
    RoomID:           room.ID,
    RoomType:         room.Type,
    RoomName:         room.Name,
    HomeSiteID:       room.SiteID,
    Accounts:         accounts,
    RequesterAccount: requester.Account,
    Timestamp:        req.Timestamp,
}
```

(No `RequesterEngName`, `RequesterChineseName`, `AppName`.)

- [ ] **Step 3: Build**

Run: `go build ./room-worker/... 2>&1 | head -5`

Expected: success.

- [ ] **Step 4: Run room-worker tests**

Run: `make test SERVICE=room-worker`

Expected: PASS. Any test that asserted outbox payload field values must already have been updated in Task 4 — if not, fix here.

- [ ] **Step 5: Stage**

Run: `git add room-worker/handler.go room-worker/handler_test.go`

---

## Task 8: Drop inbox-worker subscriptionSidebarName, composeName, and SidebarName field write

**Files:**
- Modify: `inbox-worker/handler.go`

- [ ] **Step 1: Locate the helpers and the field write**

Run: `grep -n "subscriptionSidebarName\|func composeName\|SidebarName:" inbox-worker/handler.go`

Expected:
- `composeName` definition around line 197.
- `subscriptionSidebarName` around line 231.
- One field write at line 299 inside the sub-construction loop.

- [ ] **Step 2: Delete `composeName` and `subscriptionSidebarName`**

Remove both functions entirely.

- [ ] **Step 3: Remove the `SidebarName` field write**

In the sub-construction loop (around line 299), delete the line:

```go
SidebarName:  subscriptionSidebarName(&data, &u),
```

- [ ] **Step 4: Build**

Run: `go build ./inbox-worker/... 2>&1 | head -5`

Expected: success.

- [ ] **Step 5: Verify `subscriptionName` for botDM still uses `RequesterAccount` for both sides**

Run: `grep -n "subscriptionName\|case model.RoomTypeBotDM" inbox-worker/handler.go`

The current `subscriptionName` already uses `d.RequesterAccount` for both branches of botDM (the bot side and the defensive human-side fallback). No change needed there.

- [ ] **Step 6: Stage**

Run: `git add inbox-worker/handler.go`

---

## Task 9: Update inbox-worker tests

**Files:**
- Modify: `inbox-worker/handler_test.go`
- Modify: `inbox-worker/integration_test.go`

- [ ] **Step 1: Find every SidebarName / RequesterEngName assertion**

Run: `grep -n "SidebarName\|RequesterEngName\|RequesterChineseName\|AppName" inbox-worker/handler_test.go inbox-worker/integration_test.go`

- [ ] **Step 2: Drop assertions and outbox-payload field setters**

For each match:
  - Remove the assertion line.
  - Remove the field assignment in any `model.RoomCreatedOutbox{...}` literal that sets the dropped fields.
  - For setup lines that exist only to set those fields, remove them.

Example — in `TestHandleRoomCreatedDMBuildsRemoteSub`:

```go
// Before:
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
...
assert.Equal(t, "alice", subs[0].Name)
assert.Equal(t, "Alice 爱丽丝", subs[0].SidebarName)

// After:
payload, _ := json.Marshal(model.RoomCreatedOutbox{
    RoomID:           "u_aliceu_bob",
    RoomType:         model.RoomTypeDM,
    RoomName:         "",                 // DM/botDM Room.Name is empty
    HomeSiteID:       "site-A",
    Accounts:         []string{"bob"},
    RequesterAccount: "alice",
    Timestamp:        1740000000000,
})
...
assert.Equal(t, "alice", subs[0].Name)
// SidebarName assertion removed
```

- [ ] **Step 3: Run inbox-worker tests**

Run: `make test SERVICE=inbox-worker`

Expected: PASS.

- [ ] **Step 4: Stage**

Run: `git add inbox-worker/handler.go inbox-worker/handler_test.go inbox-worker/integration_test.go`

---

## Task 10: Skip EngName/ChineseName check on botDM counterpart in room-service

**Files:**
- Modify: `room-service/handler.go:237`
- Modify: `room-service/handler_test.go` (add new test)

- [ ] **Step 1: Read the existing check**

Run: `sed -n '225,245p' room-service/handler.go`

Expected: lines 237–239 reject when `other.EngName == ""` or `other.ChineseName == ""`.

- [ ] **Step 2: Gate the check on `roomType == RoomTypeDM`**

Edit `room-service/handler.go`. Find `handleCreateRoomDMOrBotDM`. Replace the validation block:

```go
// Before:
if other.EngName == "" || other.ChineseName == "" {
    return nil, errInvalidUserData
}

// After:
if roomType == model.RoomTypeDM && (other.EngName == "" || other.ChineseName == "") {
    // botDMs counterpart is an app/bot whose users-collection record
    // typically has empty name fields; the GetApp + Assistant.Enabled
    // check below is the right validation for that case.
    return nil, errInvalidUserData
}
```

- [ ] **Step 3: Write the failing test**

Add to `room-service/handler_test.go`:

```go
func TestHandleCreateRoom_BotDM_AppCounterpartNoNameFields(t *testing.T) {
    ctrl := gomock.NewController(t)
    store := NewMockRoomStore(ctrl)
    store.EXPECT().GetUser(gomock.Any(), "alice").Return(aliceUser(), nil)
    // Bot user record exists but has empty EngName/ChineseName — common
    // for app/bot accounts in the users collection.
    store.EXPECT().GetUser(gomock.Any(), "weather.bot").Return(&model.User{
        ID: "u_wbot", Account: "weather.bot", SiteID: "site-a",
    }, nil)
    store.EXPECT().FindDMSubscription(gomock.Any(), "alice", "weather.bot").
        Return(nil, model.ErrSubscriptionNotFound)
    store.EXPECT().GetApp(gomock.Any(), "weather.bot").Return(&model.App{
        ID:        "a_wbot",
        Name:      "Weather Bot",
        Assistant: &model.AppAssistant{Enabled: true, Name: "weather.bot"},
    }, nil)

    // Capture the publish to verify it runs (no errInvalidUserData rejection).
    var published bool
    h := &Handler{
        store: store, siteID: "site-a", maxRoomSize: 1000,
        publishCanonical: func(_ context.Context, _ string, _ []byte) error {
            published = true
            return nil
        },
    }

    body, _ := json.Marshal(model.CreateRoomRequest{Users: []string{"weather.bot"}})
    resp, err := h.handleCreateRoom(ctxWithReqID(), createRoomSubj("alice", "site-a"), body)
    require.NoError(t, err)
    assert.True(t, published, "canonical event must publish for botDM with bot lacking name fields")
    assert.Contains(t, string(resp), `"roomType":"botDM"`)
}
```

> Adapt the publishCanonical injection to whatever shape the existing tests use — search `handler_test.go` for an existing botDM happy-path test and copy the wiring exactly.

- [ ] **Step 4: Run the test, confirm it fails before the source edit was applied**

(If you applied Step 2 first the test will pass immediately; that's fine — the red is verified by the contradicting prior shape of the validation. Keep Step 2's edit.)

Run: `go test ./room-service/... -run TestHandleCreateRoom_BotDM_AppCounterpartNoNameFields -v -count=1`

Expected: PASS.

- [ ] **Step 5: Stage**

Run: `git add room-service/handler.go room-service/handler_test.go`

---

## Task 11: Drop req.AppName population in room-service

**Files:**
- Modify: `room-service/handler.go:267`

- [ ] **Step 1: Locate the assignment**

Run: `grep -n "req.AppName" room-service/handler.go`

Expected: one site at ~line 267 (`req.AppName = app.Name`).

- [ ] **Step 2: Delete that assignment**

Edit the file and remove the single line. The `app` variable is still used for the `Assistant.Enabled` guard — keep that.

- [ ] **Step 3: Build room-service**

Run: `go build ./room-service/... 2>&1 | head -5`

Expected: success (the field was already dropped from the model in Task 1).

- [ ] **Step 4: Run room-service tests**

Run: `make test SERVICE=room-service`

Expected: PASS. If a test asserted `req.AppName == "Weather Bot"` after `handleCreateRoom`, drop that assertion now.

- [ ] **Step 5: Stage**

Run: `git add room-service/handler.go room-service/handler_test.go`

---

## Task 12: Consolidate self-DM check into post-strip logic

**Files:**
- Modify: `room-service/handler.go:192-216` (`classifyAndValidate`)

- [ ] **Step 1: Read the current classifyAndValidate**

Run: `sed -n '190,225p' room-service/handler.go`

Expected: a pre-strip self-DM block (lines 197–203) followed by `req.Users = stripAccount(...)` at line 205.

- [ ] **Step 2: Replace with a consolidated single-pass version**

Edit `room-service/handler.go`. Replace the body (lines 193–225 area) with this exact shape:

```go
func classifyAndValidate(req *model.CreateRoomRequest, requesterAccount string) (model.RoomType, error) {
    if req.Name == "" && len(req.Users) == 0 && len(req.Orgs) == 0 && len(req.Channels) == 0 {
        return "", errEmptyCreateRequest
    }

    // Single dedup + strip pass; capture the pre-strip dedup'd length so we
    // can detect self-DM (originalUsers == [requesterAccount]) without a
    // second pass.
    deduped := dedup(req.Users)
    req.Users = stripAccount(deduped, requesterAccount)

    if req.Name == "" && len(req.Orgs) == 0 && len(req.Channels) == 0 {
        if len(deduped) == 1 && len(req.Users) == 0 {
            // Pre-strip set was [requester] and post-strip is empty →
            // self-DM.
            return "", errSelfDM
        }
    }

    roomType := determineRoomType(req)

    if roomType == model.RoomTypeChannel {
        if strings.TrimSpace(req.Name) == "" {
            return "", errChannelNameRequired
        }
        if utf8.RuneCountInString(req.Name) > maxChannelNameRunes {
            return "", errChannelNameTooLong
        }
        for _, a := range req.Users {
            if isBot(a) {
                return "", errBotInChannel
            }
        }
    }

    return roomType, nil
}
```

- [ ] **Step 3: Run room-service tests**

Run: `make test SERVICE=room-service`

Expected: PASS — the existing self-DM tests (`TestHandleCreateRoom_SelfDM_*`) must still hit `errSelfDM`. If any fail, the consolidated logic missed an edge case; restore the previous logic and re-do.

- [ ] **Step 4: Stage**

Run: `git add room-service/handler.go`

---

## Task 13: Consolidate sanitizeError pass-through cases

**Files:**
- Modify: `room-service/helper.go:172-194`

- [ ] **Step 1: Read the current cases**

Run: `sed -n '160,200p' room-service/helper.go`

Expected: three branches — `errors.Is(err, errNotRoomMember)` (returns bare), then two separate pass-through groups returning `err.Error()`.

- [ ] **Step 2: Merge the two pass-through groups into one branch**

Replace lines 172–194 (the two pass-through case blocks) with a single consolidated branch:

```go
case errors.Is(err, errInvalidRole),
    errors.Is(err, errOnlyOwners),
    errors.Is(err, errAlreadyOwner),
    errors.Is(err, errNotOwner),
    errors.Is(err, errCannotDemoteLast),
    errors.Is(err, errRoomTypeGuard),
    errors.Is(err, errTargetNotMember),
    errors.Is(err, errInvalidOrg),
    errors.Is(err, errPromoteRequiresIndividual),
    errors.Is(err, errEmptyCreateRequest),
    errors.Is(err, errSelfDM),
    errors.Is(err, errBotInChannel),
    errors.Is(err, errBotNotAvailable),
    errors.Is(err, errInvalidUserData),
    errors.Is(err, errMissingRequestID),
    errors.Is(err, errInvalidRequestID),
    errors.Is(err, errChannelNameRequired),
    errors.Is(err, errChannelNameTooLong),
    errors.Is(err, errUserNotFound),
    errors.Is(err, &dmExistsError{}),
    errors.Is(err, &channelExpandTimeoutError{}):
    return err.Error()
```

The `errNotRoomMember` branch above stays separate (returns the bare message).

- [ ] **Step 3: Run room-service tests**

Run: `make test SERVICE=room-service`

Expected: PASS — `helper_test.go` exercises every sentinel; behavior is unchanged, only structure.

- [ ] **Step 4: Stage**

Run: `git add room-service/helper.go`

---

## Task 14: Replace MongoStore.db with explicit apps field

**Files:**
- Modify: `room-service/store_mongo.go`

- [ ] **Step 1: Read the struct and constructor**

Run: `sed -n '15,35p' room-service/store_mongo.go`

Expected:
```go
type MongoStore struct {
    db            *mongo.Database
    rooms         *mongo.Collection
    subscriptions *mongo.Collection
    roomMembers   *mongo.Collection
    users         *mongo.Collection
}
```

- [ ] **Step 2: Find every `s.db` use**

Run: `grep -n "s\.db" room-service/store_mongo.go`

Expected: `s.db.Collection("apps")` at lines 78 and 616.

- [ ] **Step 3: Refactor the struct and constructor**

Edit `room-service/store_mongo.go`. Replace the struct + constructor:

```go
type MongoStore struct {
    rooms         *mongo.Collection
    subscriptions *mongo.Collection
    roomMembers   *mongo.Collection
    users         *mongo.Collection
    apps          *mongo.Collection
}

func NewMongoStore(db *mongo.Database) *MongoStore {
    return &MongoStore{
        rooms:         db.Collection("rooms"),
        subscriptions: db.Collection("subscriptions"),
        roomMembers:   db.Collection("room_members"),
        users:         db.Collection("users"),
        apps:          db.Collection("apps"),
    }
}
```

- [ ] **Step 4: Update both call sites**

Replace `s.db.Collection("apps")` → `s.apps` at lines 78 and 616.

After this, `s.db` is unreferenced — confirm with:

```bash
grep -n "s\.db" room-service/store_mongo.go
```

Expected: no matches.

- [ ] **Step 5: Build room-service**

Run: `go build ./room-service/... 2>&1 | head -5`

Expected: success.

- [ ] **Step 6: Run room-service tests (unit + integration)**

Run: `make test SERVICE=room-service`

Expected: PASS.

If integration tests start the container too — guard with `make test-integration SERVICE=room-service`. Skip the integration step if Docker is unavailable; the integration test container will be exercised in CI later.

- [ ] **Step 7: Stage**

Run: `git add room-service/store_mongo.go`

---

## Task 15: Final build + commit + push

**Files:** all staged from Tasks 1–14.

- [ ] **Step 1: Verify staging**

Run: `git status --short`

Expected: every file from the File Structure Map listed as modified, no untracked files (the plan touches only existing files).

- [ ] **Step 2: Build the entire repo**

Run: `make build SERVICE=room-service && make build SERVICE=room-worker && make build SERVICE=inbox-worker`

Expected: all three succeed.

- [ ] **Step 3: Run lint**

Run: `make lint`

Expected: `0 issues.`

- [ ] **Step 4: Run all unit tests**

Run: `make test`

Expected: PASS for every package.

- [ ] **Step 5: Run integration tests for the affected services**

Run:
```bash
make test-integration SERVICE=room-service
make test-integration SERVICE=room-worker
make test-integration SERVICE=inbox-worker
```

Expected: PASS.

If Docker is not available locally, defer integration runs to CI — but mark this step as skipped in the commit message so the reviewer knows.

- [ ] **Step 6: Commit**

```bash
git commit -m "$(cat <<'EOF'
fix(create-room): apply round-3 review cleanups (mliu)

- Drop Subscription.SidebarName; Subscription.Name now carries the
  counterpart account for DM/botDM (Room.Name empty). Frontend
  resolves display names from locally-replicated users/apps records.
- Drop RoomCreatedOutbox.RequesterEngName / RequesterChineseName /
  AppName; drop CreateRoomRequest.AppName.
- Room.CreatedBy is the requester for every room type (was empty
  for DM/botDM).
- room-service: skip EngName/ChineseName check on botDM counterpart
  (apps don't populate those fields). Self-DM check folded into the
  post-strip length comparison; one dedup/strip pass.
- room-service.helper.sanitizeError: two pass-through case groups
  merged into one branch.
- room-service.MongoStore: explicit apps *mongo.Collection field;
  drop the unused *mongo.Database field.
- room-worker: single acceptedAt derivation (already in place);
  channel sub building consolidated into one loop with the owner;
  drop redundant length guards; drop composeName /
  composeNameOrAccount helpers.
- inbox-worker: drop subscriptionSidebarName / composeName helpers
  and the SidebarName field write.
- Tests updated across pkg/model, room-service, room-worker, and
  inbox-worker to match the new field shapes.

Spec: docs/superpowers/specs/2026-04-28-create-room-design.md
(v2 update marker dated 2026-05-05).
EOF
)"
```

- [ ] **Step 7: Push**

Run: `git push origin claude/implement-create-room-ePJUK`

Expected: push succeeds. PR #142 remains in draft so no CI runs.

---

## Task 16: Add bot rejection parity to add-member

**Files:**
- Modify: `room-service/handler.go` — `handleAddMembers` (around lines 599–674).
- Modify: `room-service/handler_test.go` — add bot-rejection tests for add-member.

**Goal:** Match create-channel behavior — direct bots in `req.Users` produce a hard error; bots from channel-ref expansion are silently filtered out.

- [ ] **Step 1: Locate the inbound-users handling in `handleAddMembers`**

Run: `sed -n '620,645p' room-service/handler.go`

Expected: lines around 625–641 unmarshal `req`, expand channels, dedup. No bot check today.

- [ ] **Step 2: Write the failing test for direct-bot rejection**

Add to `room-service/handler_test.go`:

```go
func TestHandleAddMembers_RejectsDirectBot(t *testing.T) {
    ctrl := gomock.NewController(t)
    store := NewMockRoomStore(ctrl)
    // The handler is expected to short-circuit before any store call,
    // but the room/sub lookup happens earlier — wire what's needed to
    // reach the bot check. Mirror the existing add-member happy-path
    // test setup for these calls.
    setupAddMemberRoomAndSub(t, store, "r1", "alice", model.RoomTypeChannel)

    h := &Handler{store: store, siteID: "site-a", maxRoomSize: 1000}
    body, _ := json.Marshal(model.AddMembersRequest{
        Users: []string{"weather.bot"},
    })
    _, err := h.handleAddMembers(ctxWithReqID(), addMemberSubj("alice", "site-a", "r1"), body)
    require.Error(t, err)
    assert.True(t, errors.Is(err, errBotInChannel))
}
```

> `setupAddMemberRoomAndSub` and `addMemberSubj` are existing helpers in the file — search for an existing add-member test (e.g. `TestHandleAddMembers_HappyPath`) and copy the setup pattern exactly.

- [ ] **Step 3: Run the test, confirm it FAILS**

Run: `go test ./room-service/... -run TestHandleAddMembers_RejectsDirectBot -v -count=1`

Expected: FAIL — current handler accepts bots.

- [ ] **Step 4: Add the direct-bot check in `handleAddMembers`**

Edit `room-service/handler.go`. Right after the `json.Unmarshal(data, &req)` block and the `roomID` consistency check (around line 631), insert:

```go
for _, a := range req.Users {
    if isBot(a) {
        return nil, errBotInChannel
    }
}
```

The block belongs **before** `expandChannelRefs` so a malformed request fails fast.

- [ ] **Step 5: Run the test, confirm it PASSES**

Run: `go test ./room-service/... -run TestHandleAddMembers_RejectsDirectBot -v -count=1`

Expected: PASS.

- [ ] **Step 6: Write the failing test for silent channel-ref bot filtering**

Add to the same file:

```go
func TestHandleAddMembers_SilentlyFiltersBotsFromChannelRefs(t *testing.T) {
    ctrl := gomock.NewController(t)
    store := NewMockRoomStore(ctrl)
    setupAddMemberRoomAndSub(t, store, "r1", "alice", model.RoomTypeChannel)

    // expandChannelRefs returns a mix of human and bot accounts. The
    // mock simulates a same-site source channel resolution.
    setupChannelRefExpand(t, store, []model.ChannelRef{{RoomID: "r_src", SiteID: "site-a"}},
        nil, []string{"bob", "weather.bot"})

    // CountNewMembers expects bob only — the bot is filtered before counting.
    store.EXPECT().CountNewMembers(gomock.Any(), gomock.Any(),
        []string{"bob"}, "r1", gomock.Any()).Return(1, nil)

    var publishedPayload []byte
    h := &Handler{
        store: store, siteID: "site-a", maxRoomSize: 1000,
        publishToStream: func(_ context.Context, _ string, data []byte) error {
            publishedPayload = data
            return nil
        },
    }

    body, _ := json.Marshal(model.AddMembersRequest{
        Users:    []string{},
        Channels: []model.ChannelRef{{RoomID: "r_src", SiteID: "site-a"}},
    })
    _, err := h.handleAddMembers(ctxWithReqID(), addMemberSubj("alice", "site-a", "r1"), body)
    require.NoError(t, err)

    var published model.AddMembersRequest
    require.NoError(t, json.Unmarshal(publishedPayload, &published))
    assert.NotContains(t, published.Users, "weather.bot",
        "bot from channel-ref must be silently filtered before publishing")
    assert.Contains(t, published.Users, "bob")
}
```

> `setupChannelRefExpand` is also an existing helper — copy from any existing channel-ref add-member test.

- [ ] **Step 7: Run the test, confirm it FAILS**

Run: `go test ./room-service/... -run TestHandleAddMembers_SilentlyFiltersBotsFromChannelRefs -v -count=1`

Expected: FAIL — `weather.bot` currently passes through.

- [ ] **Step 8: Add `filterBots` after `expandChannelRefs`**

Edit `room-service/handler.go`. Find the line `channelOrgIDs, channelAccounts, err := h.expandChannelRefs(...)` (around line 634) and immediately after the error check, add:

```go
// Strip bots from channel-ref expansion so a source channel can never
// silently inject a bot into this channel. Mirrors create-channel.
channelAccounts = filterBots(channelAccounts)
```

- [ ] **Step 9: Run both new tests, confirm PASS**

Run: `go test ./room-service/... -run 'TestHandleAddMembers_RejectsDirectBot|TestHandleAddMembers_SilentlyFiltersBotsFromChannelRefs' -v -count=1`

Expected: both PASS.

- [ ] **Step 10: Run the full add-member test suite to confirm no regressions**

Run: `go test ./room-service/... -run TestHandleAddMembers -count=1`

Expected: PASS for every existing add-member test.

- [ ] **Step 11: Verify `sanitizeError` already passes through `errBotInChannel`**

Run: `grep -n "errBotInChannel" room-service/helper.go`

Expected: a match on a `case errors.Is(err, ..., errBotInChannel, ...)` line. (Task 13 already includes it in the consolidated allowlist.) If for any reason it's missing, add it.

- [ ] **Step 12: Stage**

Run: `git add room-service/handler.go room-service/handler_test.go`

> Task 15 (final commit + push) is updated implicitly to include these files; no separate commit here.

---

## Self-Review Checklist (already run while writing this plan)

- ✅ Spec coverage: every mliu comment (1–17) maps to a task, except the deferred sync-DM endpoint (#8 → spec follow-ups section) and the compliment (#14).
- ✅ No placeholders ("TBD", "TODO", "implement later", etc.) in any step.
- ✅ Type consistency: `newSub` signature in Task 3 matches usage in Tasks 3–6. `RoomCreatedOutbox` literal in Task 7 matches the model definition in Task 1. `Room.CreatedBy = requester.ID` is consistent across Tasks 6 and the test updates in Tasks 4, 6, 9.
- ✅ Build sequence: model changes (Task 1) intentionally break compilation; the gap closes after Tasks 2–9 (room-worker + inbox-worker). Task 15 verifies green end-to-end before committing.

---

*End of v2 cleanups plan.*
