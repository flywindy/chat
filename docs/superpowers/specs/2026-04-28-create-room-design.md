# Create Room Design

**Date:** 2026-04-28
**Status:** Draft

> _Updated 2026-05-05: `Subscription.SidebarName` removed; `Subscription.Name` now carries the counterpart account for DM/botDM (`Room.Name` empty); `Room.CreatedBy` always set to requester for every room type; misc validation/refactor cleanups; sync server-to-server DM endpoint deferred to a follow-up._

## Summary

Adds the ability for a user to create a new room. Three room types are supported, all derived from a single request payload:

- **`dm`** — exactly two human users.
- **`botDM`** — one human user plus one bot user (account ends in `.bot`).
- **`channel`** — any combination of users / orgs / source-channel members. The client MUST supply a non-empty channel name (≤ 100 runes); the server preserves it verbatim and never auto-generates or truncates it.

The room is always created on the requester's site. Cross-site members get their subscriptions written on their home sites via a single new `room_created` outbox event. Room and `room_members` documents live exclusively on the home site; they are not replicated. Subscriptions are replicated: the home site holds subs for everyone in the room (so room-scoped queries answer locally), and each remote site additionally holds subs for its own users (so user-scoped queries answer locally).

The flow mirrors `add-member` end to end (`room-service` validates and publishes to the `ROOMS` stream → `room-worker` consumes, writes Mongo state, publishes events and outbox → remote sites' `inbox-worker` writes their slice of subscriptions). Reusable helpers from `add-member` (`expandChannelRefs`, `dedup`, `sanitizeError`, `BulkCreateSubscriptions`, `FindUsersByAccounts`) are shared without modification.

## Scope

In scope:

**New endpoints and events**
- New NATS request/reply endpoint `chat.user.{account}.request.room.{siteID}.create`.
- New canonical operation `chat.room.canonical.{siteID}.create` on the existing `ROOMS` stream.
- New outbox event type `room_created` and a matching `handleRoomCreated` handler in `inbox-worker`.
- Two new system messages on channel creation: `room_created` and `members_added`.

**Model additions**
- New `RoomTypeBotDM` value (`"botDM"`) on `RoomType`.
- New `Room.AppCount` field; `ReconcileUserCount` is replaced by `ReconcileMemberCounts` which writes both `UserCount` (non-bot subs) and `AppCount` (bot subs).
- Three new `Subscription` fields: `Name`, `RoomType`, `IsSubscribed`. `RoomType` is denormalized from `Room.Type` to enable single-collection DM-dedup queries. `Name` carries the counterpart account for DM/botDM and the room name for channels (the frontend renders display names from the user/app records, so `Subscription.Name` no longer needs a pre-composed `engName chineseName` form).
- New `App` domain type (read-only) for the `apps` collection lookup.
- New `CreateRoomRequest` model carrying client-supplied + server-populated fields.
- New `RoomCreatedOutbox` event payload.
- `ErrorResponse` gains an optional `RoomID` field (used for the `dm already exists` reply).
- `MemberAddEvent` gains a `RoomName` field so `inbox-worker` can populate `Subscription.Name` for cross-site add-member subs.

**Behaviour**
- DM/botDM idempotency: `subscriptions.findOne({u.account, name, roomType $in {dm,botDM}})` rejects duplicate creates with the existing `roomId` in the error reply.
- `apps` collection lookup for botDM creation, gated on `App.Assistant.Enabled == true`.
- Channels require a client-supplied `Name`. Empty/whitespace-only is rejected with `errChannelNameRequired`; > 100 runes is rejected with `errChannelNameTooLong`. The server never auto-generates or truncates a channel name. (DM/botDM rooms never carry a request `Name`; their `Room.Name` is `RoomID`.)
- `CreateRoomRequest.Users`/`Orgs` are the literal client request (used verbatim in sys-message payloads). `CreateRoomRequest.ResolvedUsers`/`ResolvedOrgs` carry the post-expansion (channel-ref-merged, requester-stripped, dedup'd) sets used by the worker for member materialization.
- `Subscription.SiteID` for cross-site participants is the **room's home site**, not the user's home site.
- Async-job result emission to `subject.UserResponse(requesterAccount)` with new `Operation` values `"room.create"` (this feature) and `"room.member.add"` (retrofit).

**Mongo indices (new — created via `EnsureIndexes` in `room-service`)**
- `apps`: `{"assistant.name": 1}` — used by botDM lookup.
- `subscriptions`: `{"u.account": 1, "name": 1, "roomType": 1}` — compound index for the DM-dedup query. Existing single-key indices (`u.account` and `roomId`) stay as-is; they cover the existing add-member queries.

**Add-member retrofit (in this same spec, behind PR #131's foundation)**
- Drop `RequestID` from `AddMembersRequest` payload — header-only via `X-Request-ID` (per PR #131 convention).
- Generate new subscription IDs via `idgen.GenerateUUIDv7()` (32-char hex, time-ordered for B-tree locality). Redelivery safety relies on the unique compound index on `(roomId, u.account)` — duplicate-key on bulk insert is treated as success-on-redelivery, not on a deterministic ID derived from `requestID`.
- Populate `Subscription.Name = Room.Name` and `Subscription.RoomType = Room.Type` (`"channel"`) on every new sub.
- Extend `MemberAddEvent` with `RoomName` and propagate it through the cross-site outbox so `inbox-worker.handleMemberAdded` can populate `Subscription.Name` on remote-side subs.
- Emit `AsyncJobResult{Operation:"room.member.add"}` on `subject.UserResponse(requesterAccount)` at completion.
- Update `ReconcileUserCount` call site to `ReconcileMemberCounts`.

Out of scope:

- Room deletion, room rename, room-type conversion (e.g., DM → channel).
- Bot lifecycle / app management — the `apps` collection is read-only here; provisioning, enabling/disabling, and bot-account creation in the `users` collection are upstream.
- "Self-DM" (a room containing only the requester) — explicitly rejected at validation; tracked as a separate feature with its own roomType later.
- Cross-site DM-creation race resolution — when alice@A and bob@B both initiate DMs to each other simultaneously, the losing site's room-worker hits a `mongo.ErrDuplicateKey` mismatch and surfaces an error `AsyncJobResult`. A future tiebreak ("smaller siteID wins") would close this gap.
- `Restricted` channels at creation time. New rooms are unrestricted; toggling `Restricted` is a separate feature.
- Migrating / backfilling existing subscription docs with the new `Name` and `RoomType` fields. Existing rows simply have these fields absent in Mongo; read paths tolerate missing values. Subscriptions newly created or modified after this feature ships will have the fields populated.
- Frontend changes (covered in a separate frontend spec).
- DM-dedup tightening for the rare case where a requester has both a channel and a DM with the same `name == account`. The `roomType $in {dm,botDM}` clause already filters out the channel, so the previously-discussed `findOne` ambiguity is resolved at the database layer; no further work is needed.

## NATS Subjects

### Request/Reply (Client → room-service)

| Operation | Subject | Wildcard | Queue Group |
|-----------|---------|----------|-------------|
| Create room | `chat.user.{account}.request.room.{siteID}.create` | `chat.user.*.request.room.{siteID}.create` | `room-service` |

The `{siteID}` in the subject is **the requester's site** (the room will be created there). NATS gateways route cross-site requests transparently. Each `room-service` instance subscribes with its own `siteID` baked into the wildcard.

`X-Request-ID` header is **mandatory** on the inbound request — `room-service` rejects with `"missing X-Request-ID"` if absent. The header is propagated through every subsequent NATS hop via `natsutil.NewMsg(ctx, ...)`.

### ROOMS Stream (room-service → room-worker)

Existing stream `ROOMS_{siteID}` (subjects: `chat.room.canonical.{siteID}.>`). New canonical subject:

- `chat.room.canonical.{siteID}.create` — built via `subject.RoomCanonical(siteID, "create")`.

`room-worker` already has a durable consumer on this stream; the existing dispatcher branches on the trailing operation token (`member.add` today; `create` is added).

### Events (room-worker → clients / outbox)

| Subject | Payload | Purpose |
|---------|---------|---------|
| `chat.user.{account}.event.subscription.update` | `SubscriptionUpdateEvent` | Per-user UI sidebar update — fired for every new sub, including remote-site users (NATS gateway routes to their home site). |
| `chat.msg.canonical.{siteID}.created` | `MessageEvent` (system message) | Channels only. Two messages: `room_created` then `members_added`. Persisted via `message-worker` to Cassandra, broadcast to all subscribers via `broadcast-worker`. |
| `outbox.{siteID}.to.{destSiteID}.room_created` | `OutboxEvent` wrapping `RoomCreatedOutbox` | One event per remote site that has at least one local member of the new room. |
| `chat.user.{account}.event.async-job-result` (or whatever PR #131 names it) | `AsyncJobResult` | Final notification to the requester — `{requestId, operation, status, roomId, error?}`. Fires once at end of `processCreateRoom`, both success and permanent-error paths. |

### Inbox (cross-site → inbox-worker)

`INBOX_{siteID}` already sources from `outbox.*.to.{siteID}.>`. The new `room_created` event-type token routes to a new `handleRoomCreated` handler; existing `handleMemberAdded` and `handleRoomSync` are untouched.

## Data Models

### Room

```go
type RoomType string

const (
    RoomTypeDM      RoomType = "dm"
    RoomTypeBotDM   RoomType = "botDM"  // NEW
    RoomTypeChannel RoomType = "channel"
)

type Room struct {
    ID               string     `json:"id"               bson:"_id"`
    Name             string     `json:"name"             bson:"name"`
    Type             RoomType   `json:"type"             bson:"type"`
    CreatedBy        string     `json:"createdBy"        bson:"createdBy"`
    SiteID           string     `json:"siteId"           bson:"siteId"`
    UserCount        int        `json:"userCount"        bson:"userCount"`
    AppCount         int        `json:"appCount"         bson:"appCount"`         // NEW
    LastMsgAt        *time.Time `json:"lastMsgAt,omitempty"        bson:"lastMsgAt,omitempty"`
    LastMsgID        string     `json:"lastMsgId"                  bson:"lastMsgId"`
    LastMentionAllAt *time.Time `json:"lastMentionAllAt,omitempty" bson:"lastMentionAllAt,omitempty"`
    CreatedAt        time.Time  `json:"createdAt"        bson:"createdAt"`
    UpdatedAt        time.Time  `json:"updatedAt"        bson:"updatedAt"`
    Restricted       bool       `json:"restricted,omitempty"       bson:"restricted,omitempty"`
}
```

Field semantics for create-room:

- **`ID`**: channels use `idgen.GenerateID()` (17-char base62) generated by `room-service`; DM/botDM use `idgen.BuildDMRoomID(requester.ID, other.ID)` (sorted concat of two 17-char base62 user IDs, no separator — 34 chars total, order-independent).
- **`Name`**: channels — request `Name` (required, non-empty, ≤ 100 runes; rejected otherwise); DM/botDM — **empty string** (the per-subscriber counterpart name lives on `Subscription.Name`; `Room.Name` is intentionally blank for DM/botDM).
- **`CreatedBy`**: always the requester's `User.ID`, regardless of room type. The room doc lives only on the requester's home site, so `CreatedBy` unambiguously identifies the originator (including for botDM, where the bot is the counterpart, not the creator).
- **`UserCount`**: count of non-bot subscriptions. Used by add-member's capacity check.
- **`AppCount`**: count of bot subscriptions. Stored for completeness; not used in capacity.
- **`Restricted`**: never written by create-room (relies on `omitempty` so the field is absent in Mongo).

### Subscription

```go
type Role string

const (
    RoleOwner  Role = "owner"
    RoleMember Role = "member"
)

type SubscriptionUser struct {
    ID      string `json:"id"      bson:"_id"`
    Account string `json:"account" bson:"account"`
}

type Subscription struct {
    ID                 string           `json:"id"                          bson:"_id"`
    User               SubscriptionUser `json:"u"                           bson:"u"`
    RoomID             string           `json:"roomId"                      bson:"roomId"`
    SiteID             string           `json:"siteId"                      bson:"siteId"`
    Roles              []Role           `json:"roles"                       bson:"roles"`
    Name               string           `json:"name"                        bson:"name"`                        // NEW
    RoomType           RoomType         `json:"roomType"                    bson:"roomType"`                    // NEW (denormalized)
    IsSubscribed       bool             `json:"isSubscribed,omitempty"      bson:"isSubscribed,omitempty"`      // NEW
    HistorySharedSince *time.Time       `json:"historySharedSince,omitempty" bson:"historySharedSince,omitempty"`
    JoinedAt           time.Time        `json:"joinedAt"                    bson:"joinedAt"`
    LastSeenAt         time.Time        `json:"lastSeenAt"                  bson:"lastSeenAt"`
    HasMention         bool             `json:"hasMention"                  bson:"hasMention"`
}
```

Population rules:

| Field | Channels | DM | botDM (human's sub) | botDM (bot's sub) |
|-------|----------|----|--------------------:|------------------:|
| `Name` | `Room.Name` | other user's `Account` | bot's `Account` | human's `Account` |
| `RoomType` | `"channel"` | `"dm"` | `"botDM"` | `"botDM"` |
| `IsSubscribed` | `false` (omitted) | `false` (omitted) | **`true`** | `false` (omitted) |
| `Roles` | requester: `[owner]`; others: `[member]` | `nil` | `nil` | `nil` |

`Subscription.RoomType` is denormalized from `Room.Type` so DM dedup queries can filter by it without a `$lookup`. Set on every subscription written by create-room AND by add-member's retrofit (always `"channel"` there). Inbox-worker also populates it from the outbox payload's `RoomType` field.

`Subscription.Name` is the only display field on the subscription. The frontend resolves human-readable names (engName, chineseName, app.name, etc.) from the `users` / `apps` collections by joining on `Name` (the counterpart account or the room name). There is no pre-composed `engName chineseName` form persisted on the subscription.

Channels require a client-supplied `Name`; room-service rejects an empty or > 100-rune Name in `classifyAndValidate` (`errChannelNameRequired`/`errChannelNameTooLong`). No server-side normalization — no truncation, no auto-name.

### App (new domain type, read-only here)

```go
type App struct {
    ID          string         `json:"id"          bson:"_id"`
    Name        string         `json:"name"        bson:"name"`
    Description string         `json:"description" bson:"description"`
    Assistant   *AppAssistant  `json:"assistant,omitempty"   bson:"assistant,omitempty"`
    Sponsors    []AppSponsor   `json:"sponsors,omitempty"    bson:"sponsors,omitempty"`
}

type AppAssistant struct {
    Enabled     bool   `json:"enabled"     bson:"enabled"`
    Name        string `json:"name"        bson:"name"`        // bot account, ends in ".bot"
    SettingsURL string `json:"settingsUrl" bson:"settingsUrl"`
}

type AppSponsor struct {
    Name  string `json:"name"  bson:"name"`
    Phone string `json:"phone" bson:"phone"`
}
```

`apps` is queried by `assistant.name == "<botAccount>"`. The collection is provisioned upstream; create-room only reads. botDM creation requires both that the queried document exists and `Assistant.Enabled == true`.

### CreateRoomRequest

```go
type CreateRoomRequest struct {
    // Client-supplied
    Name     string       `json:"name"     bson:"name"`
    Users    []string     `json:"users"    bson:"users"`     // accounts
    Orgs     []string     `json:"orgs"     bson:"orgs"`      // org IDs (User.SectID values)
    Channels []ChannelRef `json:"channels" bson:"channels"`

    // Server-populated by room-service before publishing to ROOMS stream
    RoomID           string `json:"roomId"           bson:"roomId"`           // channels: pre-generated; DM/botDM: deterministic
    RequesterID      string `json:"requesterId"      bson:"requesterId"`
    RequesterAccount string `json:"requesterAccount" bson:"requesterAccount"`
    Timestamp        int64  `json:"timestamp"        bson:"timestamp"`
}
```

`requestId` is **not** in the payload — it rides as the `X-Request-ID` NATS header per PR #131. Both `room-service` and `room-worker` read it from `ctx`.

`Timestamp` is set by room-service to `time.Now().UTC().UnixMilli()` immediately before publishing to the canonical stream. Room-worker uses it as `acceptedAt` (the `Room.CreatedAt` / `Subscription.JoinedAt` value). Defensive default: if `Timestamp == 0` reaches room-worker (e.g. from a malformed redelivery), the worker falls back to `time.Now().UTC()` rather than `1970-01-01`.

`AddMembersRequest` is updated in this spec to **drop its `RequestID` field** (if present after PR #131) and rely on the header. See "Add-member retrofit" later.

### RoomCreatedOutbox (new)

```go
// pkg/model/event.go (or pkg/model/room.go)
type RoomCreatedOutbox struct {
    RoomID           string   `json:"roomId"`
    RoomType         RoomType `json:"roomType"`
    RoomName         string   `json:"roomName"`         // empty for DM/botDM
    HomeSiteID       string   `json:"homeSiteId"`
    Accounts         []string `json:"accounts"`          // accounts on the destination site only; non-empty
    RequesterAccount string   `json:"requesterAccount"`
    Timestamp        int64    `json:"timestamp"`
}
```

The payload no longer carries `RequesterEngName` / `RequesterChineseName` / `AppName`. Inbox-worker writes `Subscription.Name` straight from `RequesterAccount` (DM) or the bot account in `Accounts` (botDM-bot-side); display-name composition is the frontend's job, identical to the home-site rendering path.

Wrapped in the existing `OutboxEvent` envelope (`{Type: "room_created", SiteID, DestSiteID, Payload: <marshalled RoomCreatedOutbox>, Timestamp}`).

### ErrorResponse extension

```go
type ErrorResponse struct {
    Error  string `json:"error"`
    RoomID string `json:"roomId,omitempty"`  // populated for "dm already exists"
}
```

`omitempty` keeps existing callers compatible.

### AsyncJobResult (migrate existing struct)

The branch currently carries `AsyncJobResult` with `Job string` and `Success bool` fields (added by a prior PR). This PR **replaces** that shape with the canonical form below. The frontend has no consumer of this event yet, so there is no wire-compatibility concern.

```go
type AsyncJobResult struct {
    RequestID string `json:"requestId"`
    Operation string `json:"operation"`         // see constants below
    Status    string `json:"status"`             // "ok" | "error"
    RoomID    string `json:"roomId,omitempty"`   // populated for room.create; omit for member ops
    Error     string `json:"error,omitempty"`    // sanitized
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

All three existing `publishAsyncJobResult` call sites in `room-worker/handler.go` must be updated to use `Operation`/`Status` instead of `Job`/`Success`, and to use the typed constants above.

### Sub-ID and Member-ID generation

All new subscriptions (create-room AND the add-member retrofit) use `idgen.GenerateUUIDv7()` (32-char hex, no hyphens), consistent with the established convention for `Subscription._id`, `RoomMember._id`, `ThreadRoom._id`, and `ThreadSubscription._id`.

Idempotency on JetStream redelivery is achieved via compound unique indices rather than deterministic IDs — see the `EnsureIndexes` section and the idempotency tables below.

## Reply Contract

**Success reply (NATS request/reply, room-service → client):**

```json
{
  "status":   "accepted",
  "roomId":   "<id>",
  "roomType": "dm" | "botDM" | "channel"
}
```

`roomId` is the final ID. For channels it was generated by room-service before publishing; for DM/botDM it is the deterministic `BuildDMRoomID` output. The client can immediately reflect optimistic UI based on this reply.

**Error reply (`model.ErrorResponse`):**

```json
{ "error": "<sanitized message>" }
```

Or, only for the DM-already-exists case:

```json
{ "error": "dm already exists", "roomId": "<existing room id>" }
```

The client uses the second form to navigate the user to the existing DM.

**Async completion (`subject.UserResponse(requesterAccount)`, fired once by room-worker):**

```json
{
  "requestId": "<UUID>",
  "operation": "room.create",
  "status":    "ok" | "error",
  "roomId":    "<id>",
  "error":     "<sanitized; only on error>",
  "timestamp": 1740000000000
}
```

The frontend correlates by `requestId` and clears any pending UI affordance.

## room-service Request Handling

`room-service` is the synchronous validation gateway. It reads the request, runs all admit-time checks against local Mongo, finalizes the canonical event payload, publishes to the `ROOMS` stream, and replies to the client. It performs no Mongo writes itself — all persistence is in `room-worker`.

### Subscription wiring

Registered in `room-service/main.go` next to the existing `MemberAddWildcard` subscription:

```go
nc.QueueSubscribe(
    subject.RoomCreateWildcard(cfg.SiteID),
    "room-service",
    natsrouter.HandlerWithMiddleware(handler.natsCreateRoom, requestIDMW, traceMW, ...),
)
```

### Handler outline (`room-service/handler.go`)

```go
func (h *Handler) natsCreateRoom(m otelnats.Msg) {
    resp, err := h.handleCreateRoom(m.Context(), m.Msg.Subject, m.Msg.Data)
    if err != nil {
        if errors.Is(err, errDMAlreadyExists) {
            replyDMExists(m.Msg, errDMRoomID(err))
            return
        }
        slog.Error("create-room failed", "error", err)
        natsutil.ReplyError(m.Msg, sanitizeError(err))
        return
    }
    if err := m.Msg.Respond(resp); err != nil {
        slog.Error("failed to respond to create-room", "error", err)
    }
}
```

`errDMAlreadyExists` is wrapped (with `fmt.Errorf("...: %w", errDMAlreadyExists)`) and the existing room ID is plumbed via a small typed wrapper so `replyDMExists` can pull it out. (Implementation detail: a `*dmExistsError` struct value implementing `Unwrap` and exposing `RoomID()`.)

### `handleCreateRoom` — validation pipeline

Steps below are the canonical sequence. Each numbered step has explicit reject conditions; passing all of them reaches step 9 (publish).

```text
1. Parse subject → requesterAccount, expectedSiteID.
   Defensive: expectedSiteID must equal h.siteID. Mismatch → reject (internal error).

2. Read X-Request-ID from ctx (set by natsrouter middleware).
   If empty → reject errMissingRequestID.

3. Unmarshal CreateRoomRequest from m.Msg.Data.
   Validate at least one of (Name, Users, Orgs, Channels) is non-empty.
   Empty all four → reject errEmptyCreateRequest.

4. Look up requester:
   requester, err := h.store.GetUser(ctx, requesterAccount)
   Not found → reject "requester not found" (sanitized to "internal error" since
   this is unreachable in normal operation — the auth flow already validated
   the requester).
   EngName=="" or ChineseName=="" on requester → reject errInvalidUserData.

5. Dedup and strip the requester from req.Users in one pass:
   req.Users = stripAccount(dedup(req.Users), requesterAccount)

6. Determine roomType from the post-strip request:
   - len(Users)==1 && len(Orgs)==0 && len(Channels)==0 && Name=="":
     - Users[0] ends with ".bot" → roomType = botDM
     - else                       → roomType = dm
   - Else                         → roomType = channel

   Self-DM is detected after the strip: if the deduped pre-strip set was
   exactly `[requesterAccount]` (i.e. pre-strip length 1 and stripped down
   to length 0) AND `Name`, `Orgs`, `Channels` are all empty → reject
   errSelfDM. This is a single comparison after the dedup/strip — no
   separate pre-strip pass.

7. If roomType == channel:
   7a. If any account in Users (post-strip) ends with ".bot"  → reject errBotInChannel.
   7b. Expand channelRefs via the existing helper:
         channelOrgIDs, channelAccounts, err := h.expandChannelRefs(ctx, requesterAccount, req.Channels)
       Reuses local-channel-subscribed check + cross-site member.list call.
       Errors propagate as-is (errNotRoomMember etc.).

       Per-ref deadline: each channel reference is bounded by
       `MEMBER_LIST_TIMEOUT` (default 5s). If a same-site Mongo lookup or
       cross-site `member.list` call exceeds the deadline, the helper
       returns `*channelExpandTimeoutError{SiteID, RoomID}` rendered as
       `"timeout listing members of channel {roomId}@{siteId}"`. The
       sync reply surfaces it verbatim via `sanitizeError` so the client
       sees exactly which channel source stalled.

       Bot filter on expansion: the accounts returned by the helper are
       passed through `filterBots` before the merge below. Any account
       matching the bot pattern (suffix `.bot` or prefix `p_`) is dropped
       so a source channel can never silently inject a bot into a new
       channel via channelRefs. This is silent (no error) — it mirrors
       the post-strip individual-bot check in 7a but operates on the
       expanded set; without it, a `.bot` member in the source channel
       would re-introduce the same bot the explicit users-list rejects.
   7c. Merge:
         allOrgs  := dedup(append(req.Orgs,  channelOrgIDs...))
         allUsers := dedup(append(req.Users, filterBots(channelAccounts)...))
         allUsers = stripAccount(allUsers, requesterAccount)  // channels can re-introduce
   7d. Re-check non-emptiness post-merge: if allUsers, allOrgs, and Name are
       ALL empty → reject errEmptyCreateRequest.
       (We allow Name + at least one of users/orgs/channels — the latter
       is guaranteed by reaching this branch in step 6.)
   7e. Capacity check via the empty-roomID variant of CountNewMembers:
         newCount, err := h.store.CountNewMembers(ctx, allOrgs, allUsers, "", requesterAccount)
       (Empty roomID skips the "already-subscribed" filter — the room doesn't
       exist yet — and just dedups + filters bots, returning the unique count.)
       newCount == 0 → reject errEmptyCreateRequest.
       // Channel must have at least one member besides the creator.
       (newCount + 1) > h.maxRoomSize → reject "exceeds maximum capacity".
       // +1 accounts for the creator/owner subscription.
   7f. Generate channel roomID:
         req.RoomID = idgen.GenerateID()
       (Pre-generated here so the synchronous reply carries it.)
   7g. Update req.Users / req.Orgs to the merged-and-stripped arrays — the
       canonical event payload carries the resolved set, mirroring add-member.
       req.Channels stays as-is for record-keeping (the sys-message replay uses
       it).

8. If roomType == dm or botDM:
   8a. otherUser, err := h.store.GetUser(ctx, req.Users[0])
       Not found → reject "user not found".
       For DM only: EngName=="" or ChineseName=="" on otherUser → reject
       errInvalidUserData. For botDM: skip — apps populate the `apps`
       collection but typically have empty EngName/ChineseName in the
       `users` collection (validated separately via GetApp + Assistant.Enabled).
   8b. Compute deterministic roomID:
         req.RoomID = idgen.BuildDMRoomID(requester.ID, otherUser.ID)
   8c. Dedup check (runs BEFORE GetApp so an existing botDM is returned
       even if the bot was disabled later):
         existing, err := h.store.FindDMSubscription(
           ctx, requesterAccount, otherUser.Account)
       This is a new store method that performs
         subscriptions.findOne({
           "u.account": requesterAccount,
           "name":      otherUser.Account,
           "roomType":  {"$in": ["dm", "botDM"]},
         })
       and returns the matching Subscription (or ErrSubscriptionNotFound).
       The `roomType` clause filters out channel subscriptions whose Name
       happens to equal an account string (e.g., a channel literally named
       "bob") — the denormalized `Subscription.RoomType` field makes this
       a single-collection query with no $lookup.

       If found → reject errDMAlreadyExists with roomId = existing.RoomID.
       If ErrSubscriptionNotFound → proceed.
   8d. If roomType == botDM (only reached when no existing sub was found):
         app, err := h.store.GetApp(ctx, otherUser.Account)
         Not found → reject errBotNotAvailable.
         app.Assistant == nil || !app.Assistant.Enabled → reject errBotNotAvailable.
         (No app metadata is copied onto the request — the `apps` lookup
         is purely a guard. `Subscription.Name` and `Room.Name` derive
         from accounts/IDs already in scope, and the frontend resolves
         the human-readable app name from the `apps` collection at
         render time.)

   For DM/botDM, room-service skips the `EngName`/`ChineseName` check on
   the counterpart user. App ("bot") records do not populate those fields
   in the `users` collection; the requester's own name fields are still
   validated (the frontend uses them to render the requester everywhere).

9. Populate server-side fields:
   req.RequesterID      = requester.ID
   req.RequesterAccount = requester.Account
   req.Timestamp        = time.Now().UTC().UnixMilli()
   (req.RoomID was set in steps 7f/8b.)

10. Publish to ROOMS stream:
    payload, _ := json.Marshal(req)
    msg := natsutil.NewMsg(ctx, subject.RoomCanonical(h.siteID, "create"), payload)
    _, err := h.js.PublishMsg(ctx, msg)
    Failure → reject (internal error).

11. Reply to client:
    json.Marshal(map[string]string{
      "status":   "accepted",
      "roomId":   req.RoomID,
      "roomType": string(roomType),
    })
```

### Helpers added to room-service

```go
// helper.go
func stripAccount(slice []string, account string) []string {
    out := make([]string, 0, len(slice))
    for _, s := range slice {
        if s != account {
            out = append(out, s)
        }
    }
    return out
}

// Sentinel errors (added to existing block)
var (
    errEmptyCreateRequest   = errors.New("request must include at least one of users, orgs, channels, or name")
    errSelfDM               = errors.New("cannot create a DM with yourself")
    errBotInChannel         = errors.New("bots cannot be added to a channel during creation")
    errBotNotAvailable      = errors.New("bot not available")
    errInvalidUserData      = errors.New("user is missing required name fields")
    errMissingRequestID     = errors.New("missing X-Request-ID header")
    errInvalidRequestID     = errors.New("invalid X-Request-ID format")
    errChannelNameRequired  = errors.New("channel name is required")
    errChannelNameTooLong   = errors.New("channel name must be at most 100 characters")
    // errDMAlreadyExists handled via typed wrapper, see below
)

// dmExistsError carries the existing room ID through the error chain so
// natsCreateRoom can unwrap it for the special reply shape.
type dmExistsError struct{ existingRoomID string }
func (e *dmExistsError) Error() string         { return "dm already exists" }
func (e *dmExistsError) Is(target error) bool  { _, ok := target.(*dmExistsError); return ok }
func (e *dmExistsError) RoomID() string        { return e.existingRoomID }

var errDMAlreadyExists = &dmExistsError{}  // Is() target

// sanitizeError extension — pass-through allowlist gains the new sentinels.
```

### Store interface additions (`room-service/store.go`)

```go
type RoomStore interface {
    // existing methods unchanged...

    GetUser(ctx context.Context, account string) (*model.User, error)        // NEW
    GetApp(ctx context.Context, botAccount string) (*model.App, error)       // NEW

    // NEW: returns the requester's existing DM/botDM subscription where
    // Subscription.Name == targetName, or ErrSubscriptionNotFound. Filters
    // on Subscription.RoomType to ignore channel subs that happen to have
    // Name == an account string. Used for DM dedup.
    FindDMSubscription(ctx context.Context, account, targetName string) (*model.Subscription, error)

    // CHANGED: signature unchanged but the empty-roomID branch is documented.
    // When roomID == "", skips the "not already subscribed" lookup and returns
    // the count of unique non-bot users (orgs + directs) that would result.
    CountNewMembers(ctx context.Context, orgIDs, directAccounts []string, roomID, excludeAccount string) (int, error)
}
```

### Mongo implementations (`room-service/store_mongo.go`)

- `GetUser`: `users.findOne({account})`. Returns `ErrUserNotFound` (new sentinel) on no document.
- `GetApp`: `apps.findOne({"assistant.name": botAccount})`.
- `FindDMSubscription`: `subscriptions.findOne({"u.account": account, "name": targetName, "roomType": {"$in": ["dm", "botDM"]}})`. Returns `ErrSubscriptionNotFound` on no document. No `$lookup` needed — the denormalized `Subscription.RoomType` carries the type discriminator.
- `CountNewMembers` (modified): wrap the existing aggregation. When `roomID == ""`, drop the `$lookup` + `$match` stages that filter "already-subscribed" users; the rest of the pipeline (`$or` on `sectId`/`account`, regex bot filter, `$count`) runs as-is.

`MongoStore` carries an explicit `*mongo.Collection` for every collection it touches (`rooms`, `subscriptions`, `roomMembers`, `users`, `apps`). The struct does not retain a `*mongo.Database` field — collections are bound once in `NewMongoStore` and the database handle is no longer needed at call time. This keeps each method's dependencies explicit and matches the pattern used by the other collection fields.

### Mongo indices (`room-service/store_mongo.go.EnsureIndexes`)

Per CLAUDE.md, indices are created in the store constructor or a dedicated `EnsureIndexes` method at startup. Both new collections that this feature queries get explicit index ensures alongside the existing add-member ones.

```go
// inside EnsureIndexes
appsIndex := mongo.IndexModel{
    Keys:    bson.D{{Key: "assistant.name", Value: 1}},
    Options: options.Index().SetName("assistant_name_idx"),
}
_, err := s.apps.Indexes().CreateOne(ctx, appsIndex)

dmDedupIndex := mongo.IndexModel{
    Keys: bson.D{
        {Key: "u.account", Value: 1},
        {Key: "name",      Value: 1},
        {Key: "roomType",  Value: 1},
    },
    Options: options.Index().
        SetName("u_account_name_roomtype_idx").
        SetUnique(true).
        SetPartialFilterExpression(bson.M{
            "roomType": bson.M{"$in": bson.A{"dm", "botDM"}},
        }),
}
_, err = s.subscriptions.Indexes().CreateOne(ctx, dmDedupIndex)
```

Two additional unique compound indices are created by `room-service` to support idempotency on JetStream redelivery (since subscription and member IDs are random UUIDv7, not deterministic):

```go
// Prevents duplicate subscriptions for the same user in the same room on redelivery.
subUniqueIndex := mongo.IndexModel{
    Keys: bson.D{
        {Key: "roomId",    Value: 1},
        {Key: "u.account", Value: 1},
    },
    Options: options.Index().
        SetName("roomid_u_account_unique_idx").
        SetUnique(true),
}
_, err = s.subscriptions.Indexes().CreateOne(ctx, subUniqueIndex)

// Prevents duplicate room_members entries for the same member in the same room.
memberUniqueIndex := mongo.IndexModel{
    Keys: bson.D{
        {Key: "roomId",    Value: 1},
        {Key: "member.id", Value: 1},
    },
    Options: options.Index().
        SetName("roomid_member_id_unique_idx").
        SetUnique(true),
}
_, err = s.roomMembers.Indexes().CreateOne(ctx, memberUniqueIndex)
```

Notes:

- The `apps` index is **not** marked unique. While `assistant.name` is expected to be unique in practice, uniqueness is a property of the `apps` collection's source-of-truth provisioning, not something this feature should enforce.
- The subscription DM-dedup partial index and the new `(roomId, u.account)` unique index coexist — they serve different query patterns and neither replaces the other.
- Existing subscription indices (`u.account`, `roomId`, etc.) stay as they are.
- `EnsureIndexes` calls are idempotent — re-running them is a no-op when the index already exists with the same definition.
- `inbox-worker` and `room-worker` don't ensure these indices: only `room-service` queries them. (Convention check: existing code in this repo does the same — only the service that queries a collection ensures its indices, even though indices are global to the Mongo collection.)

### Tracing / logging

Every log line in `handleCreateRoom` carries:

- `requestId` (slog `slog.Attr`)
- `requesterAccount`
- `roomType` (once determined)
- `roomId` (once known)

slog values use the existing JSON formatter at the service level. OTel spans wrap the handler via `otelnats.Msg`'s context.

## room-worker Handler

`room-worker` consumes the `chat.room.canonical.{siteID}.create` event from the existing `ROOMS_{siteID}` JetStream consumer. The dispatcher in the consume loop branches on the trailing operation token:

```go
switch subject.RoomCanonicalOperation(msg.Subject) {
case "member.add":
    err = h.processAddMembers(ctx, msg.Data())
case "create":
    err = h.processCreateRoom(ctx, msg.Data())
default:
    slog.Warn("unknown room canonical operation", "subject", msg.Subject)
}
```

`subject.RoomCanonicalOperation` is a small helper added to `pkg/subject` that extracts the trailing token after `chat.room.canonical.{siteID}.`.

### `processCreateRoom` outline (`room-worker/handler.go`)

```text
1. Unmarshal CreateRoomRequest from data.
   requestID, _ := natsutil.RequestIDFromContext(ctx)
   if requestID == "" → return errPermanent (room-service is supposed to enforce
   this; treat as misconfiguration).
   now := time.Now().UTC()
   acceptedAt := now
   if req.Timestamp > 0 {
       acceptedAt = time.UnixMilli(req.Timestamp).UTC()
   }
   // Single derivation at the top of the function. `acceptedAt` is reused
   // for Room.CreatedAt / Room.UpdatedAt / Subscription.JoinedAt /
   // RoomMember.Ts everywhere below — no per-branch recomputation. The
   // `req.Timestamp > 0` guard turns a missing-or-zero timestamp into
   // "now" rather than the unix epoch (room-service always sets it, but
   // this protects against a malformed redelivery).

2. Re-derive roomType from the payload using the same rules as room-service.
   This is defense-in-depth and saves carrying a roomType field on the request.

3. Look up requester:
     requester, err := h.store.GetUser(ctx, req.RequesterAccount)
   Not found → wrap with errPermanent (the requester must exist; this is a
   room-service invariant).

4. Build the Room doc:
     room := &model.Room{
       ID:        req.RoomID,
       Name:      resolveRoomName(req, roomType),
       Type:      roomType,
       CreatedBy: requester.ID,                // always the requester, every room type
       SiteID:    h.siteID,
       UserCount: 0,
       AppCount:  0,
       CreatedAt: acceptedAt,
       UpdatedAt: acceptedAt,
     }
     err := h.store.CreateRoom(ctx, room)
   Duplicate-key handling:
     - If errors.Is(err, mongo.ErrDuplicateKey):
         existing, _ := h.store.GetRoom(ctx, req.RoomID)
         If existing matches what we'd write (same Type, same SiteID, same
         Name, same CreatedBy), treat as redelivery → continue with the
         rest of the handler. NATS-Msg-Id dedup at downstream consumers
         handles re-published events.
         Else → wrap with errPermanent ("room ID collision").

   resolveRoomName:
     - dm/botDM: return "" (Room.Name is intentionally blank — the
       per-subscriber counterpart name lives on Subscription.Name).
     - channel: return req.Name verbatim. room-service has already validated
       that req.Name is non-empty (errChannelNameRequired) and ≤ 100 runes
       (errChannelNameTooLong) before publishing the canonical event; the
       worker neither auto-generates nor truncates names.

5. Build subscription list — branches on roomType.

   `newSub(id, user, room, roles, name, isSubscribed, joinedAt)` — the
   helper has no `sidebarName` parameter. `Subscription.Name` is the
   only display field; the frontend resolves human-readable names from
   the `users`/`apps` collections.

   --- dm branch ---
   otherUser, err := h.store.GetUser(ctx, req.Users[0])
   subs := []*model.Subscription{
     newSub(idgen.GenerateUUIDv7(), requester, room, nil,
            otherUser.Account, false /* IsSubscribed */, acceptedAt),
     newSub(idgen.GenerateUUIDv7(), otherUser, room, nil,
            requester.Account, false, acceptedAt),
   }

   --- botDM branch ---
   bot, err := h.store.GetUser(ctx, req.Users[0])
   subs := []*model.Subscription{
     // Human's sub: Name = bot account; IsSubscribed = true.
     newSub(idgen.GenerateUUIDv7(), requester, room, nil,
            bot.Account, true, acceptedAt),
     // Bot's sub: Name = requester's account; IsSubscribed = false.
     newSub(idgen.GenerateUUIDv7(), bot, room, nil,
            requester.Account, false, acceptedAt),
   }

   --- channel branch ---
   accounts, err := h.store.ListNewMembersForNewRoom(ctx, req.Orgs, req.Users, req.RequesterAccount)
   // Empty-roomID variant (next section). Returns dedup'd, non-bot accounts.
   users, err := h.store.FindUsersByAccounts(ctx, accounts)
   // Validate that every user has EngName and ChineseName populated. Any
   // missing field → wrap errInvalidUserData with errPermanent. (This
   // check runs only on channels, not DM/botDM, because app records do
   // not populate those fields.)

   // Single loop — owner is included alongside the others, no separate
   // append at the end.
   allUsers := append([]*model.User{requester}, users...)
   subs := make([]*model.Subscription, 0, len(allUsers))
   for _, u := range allUsers {
       roles := []model.Role{model.RoleMember}
       if u.ID == requester.ID {
           roles = []model.Role{model.RoleOwner}
       }
       subs = append(subs, newSub(
           idgen.GenerateUUIDv7(), u, room, roles,
           room.Name,                         // Subscription.Name = Room.Name
           false, acceptedAt))
   }

6. Persist subscriptions:
     err := h.store.BulkCreateSubscriptions(ctx, subs)
   Duplicate-key on bulk insert → treat as success-on-redelivery (deterministic
   IDs mean we wrote these on a previous run; downstream events are NATS-Msg-Id
   dedup'd).

7. Channel only — write room_members. The subscription list now
   contains the owner as its first entry (see step 5), so the loop
   walks `subs` directly and appends one individual row per sub plus
   one org row per `req.Orgs` entry. No trailing owner-append.

   members := make([]*model.RoomMember, 0, len(subs)+len(req.Orgs))
   for _, sub := range subs {
       members = append(members, &model.RoomMember{
         ID: idgen.GenerateUUIDv7(),
         RoomID: room.ID,
         Ts: acceptedAt,
         Member: model.RoomMemberEntry{
           ID: sub.User.ID, Type: model.RoomMemberIndividual, Account: sub.User.Account,
         },
       })
   }
   for _, org := range req.Orgs {
       members = append(members, &model.RoomMember{
         ID: idgen.GenerateUUIDv7(),
         RoomID: room.ID, Ts: acceptedAt,
         Member: model.RoomMemberEntry{ID: org, Type: model.RoomMemberOrg},
       })
   }
   err := h.store.BulkCreateRoomMembers(ctx, members)

   For dm/botDM: skip — no room_members.

8. Reconcile counts:
     err := h.store.ReconcileMemberCounts(ctx, room.ID)
   Splits the existing single-aggregation count into two: non-bot subs →
   UserCount, bot subs (account regex \.bot$) → AppCount. Updates room doc
   atomically. Replaces ReconcileUserCount everywhere; add-member's call site
   is updated to use ReconcileMemberCounts in this same spec.

9. Publish per-user subscription.update events for ALL subs:
   for _, sub := range subs {
       evt := model.SubscriptionUpdateEvent{
         UserID: sub.User.ID,
         Subscription: *sub,
         Action: "added",
         Timestamp: now.UnixMilli(),
       }
       data, _ := json.Marshal(evt)
       msg := natsutil.NewMsg(ctx, subject.SubscriptionUpdate(sub.User.Account), data)
       if err := h.nc.PublishMsg(msg); err != nil {
         slog.Error("subscription update publish failed", "error", err, "account", sub.User.Account)
       }
       // Errors are logged, not returned — subs are durable; UI updates can
       // catch up via a refresh on reconnect.
   }

10. Channel only — publish two system messages to MESSAGES_CANONICAL.
    No system messages for dm/botDM.

    sysData1, _ := json.Marshal(model.RoomCreated{
      Name: room.Name, Users: req.Users, Orgs: req.Orgs, Channels: req.Channels,
      AddedUsersCount: len(subs) - 1,  // exclude owner from count
    })
    msg1 := model.Message{
      ID: idgen.MessageIDFromRequestID(requestID, "room_created"),
      RoomID: room.ID,
      UserID: requester.ID, UserAccount: requester.Account,
      Type: "room_created",
      Content: "a new room has been created",
      SysMsgData: sysData1,
      CreatedAt: acceptedAt,
    }
    publishCanonical(ctx, msg1, room.SiteID, now)

    sysData2, _ := json.Marshal(model.MembersAdded{
      Individuals:     req.Users,
      Orgs:            req.Orgs,
      Channels:        req.Channels,
      AddedUsersCount: len(subs) - 1,
    })
    msg2 := model.Message{
      ID: idgen.MessageIDFromRequestID(requestID, "members_added"),
      RoomID: room.ID,
      UserID: requester.ID, UserAccount: requester.Account,
      Type: "members_added",
      Content: "",                    // rendered client-side from SysMsgData
      SysMsgData: sysData2,
      CreatedAt: acceptedAt.Add(time.Millisecond),  // strict ordering after msg1
    }
    publishCanonical(ctx, msg2, room.SiteID, now)

    publishCanonical helper:
      evt := model.MessageEvent{Event: model.EventCreated, Message: msg, SiteID: siteID, Timestamp: now.UnixMilli()}
      data, _ := json.Marshal(evt)
      m := natsutil.NewMsg(ctx, subject.MsgCanonicalCreated(siteID), data)
      m.Header.Set("Nats-Msg-Id", msg.ID)
      _, err := h.js.PublishMsg(ctx, m)

    Both messages conform to the Cassandra schema in
    docs/cassandra_message_model.md (Type, SysMsgData, no Content for
    members_added — UI renders from SysMsgData).

11. Cross-site outbox — group remote-site users by their User.SiteID:
    remoteSiteAccounts := map[string][]string{}
    for _, sub := range subs {
        u := userByAccount(sub.User.Account, allUsersIncludingRequester)
        if u.SiteID == h.siteID { continue }
        remoteSiteAccounts[u.SiteID] = append(remoteSiteAccounts[u.SiteID], u.Account)
    }
    for destSiteID, accounts := range remoteSiteAccounts {
        out := model.RoomCreatedOutbox{
          RoomID: room.ID, RoomType: room.Type, RoomName: room.Name,
          HomeSiteID:       room.SiteID,
          Accounts:         accounts,
          RequesterAccount: requester.Account,
          Timestamp:        req.Timestamp,
        }
        outData, _ := json.Marshal(out)
        envelope := model.OutboxEvent{
          Type: "room_created", SiteID: room.SiteID, DestSiteID: destSiteID,
          Payload: outData, Timestamp: now.UnixMilli(),
        }
        envData, _ := json.Marshal(envelope)
        m := natsutil.NewMsg(ctx, subject.Outbox(room.SiteID, destSiteID, "room_created"), envData)
        m.Header.Set("Nats-Msg-Id", requestID + ":" + destSiteID)
        _, err := h.js.PublishMsg(ctx, m)
    }

12. Async-job notification:
    publishAsyncJobResult(ctx, model.AsyncJobResult{
      RequestID: requestID,
      Operation: "room.create",
      Status:    "ok",
      RoomID:    room.ID,
      Timestamp: now.UnixMilli(),
    })

13. Return nil → the consume loop acks.
```

### Helpers (`room-worker/handler.go`)

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
        RoomType:     room.Type,            // denormalized; powers DM-dedup query
        IsSubscribed: isSubscribed,
        JoinedAt:     joinedAt,
    }
}
```

### Store interface additions (`room-worker/store.go`)

```go
type Store interface {
    // existing methods unchanged...

    CreateRoom(ctx context.Context, room *model.Room) error                          // NEW (mirrors room-service)
    GetUser(ctx context.Context, account string) (*model.User, error)                // NEW (room-worker also needs this)
    ListNewMembersForNewRoom(ctx context.Context, orgIDs, accounts []string, excludeAccount string) ([]string, error)  // NEW: empty-roomID variant
    ReconcileMemberCounts(ctx context.Context, roomID string) error                  // REPLACES ReconcileUserCount
}
```

### Mongo implementations (`room-worker/store_mongo.go`)

- `CreateRoom`: `rooms.insertOne(room)`. Returns `mongo.ErrDuplicateKey` on `_id` collision.
- `GetUser`: `users.findOne({account})`.
- `ListNewMembersForNewRoom`: same pipeline as `ListNewMembers` minus the `$lookup`/`$match` for already-subscribed users; `$group` on `account` returns the dedup'd list.
- `ReconcileMemberCounts`: two `$count` aggregations on the `subscriptions` collection (with regex `\.bot$` filter), then a single `rooms.updateOne` with both fields. Atomic from the Room doc's perspective (single update statement).

### Idempotency summary for `processCreateRoom`

| Step | Idempotency mechanism |
|------|----------------------|
| 4 — CreateRoom | Duplicate-key + matching-existing → continue. Mismatch → permanent error. |
| 6 — BulkCreateSubscriptions | Unique compound index `(roomId, u.account)`; duplicate-key → success. |
| 7 — BulkCreateRoomMembers | Unique compound index `(roomId, member.id)`; duplicate-key → success. |
| 8 — ReconcileMemberCounts | Idempotent by construction (counts the current state). |
| 9 — subscription.update | At-most-once delivery without dedup; redelivery republishes. Frontend is idempotent. |
| 10 — sys-messages | NATS-Msg-Id = msg.ID (deterministic via `MessageIDFromRequestID`). JetStream dedup catches replays. |
| 11 — outbox | NATS-Msg-Id = `requestID:destSiteID`. JetStream dedup catches replays. |
| 12 — async-job result | At-most-once; the frontend tolerates duplicates. |

## inbox-worker Handler

`inbox-worker` already consumes from `INBOX_{siteID}` and dispatches by event-type token. We add a third handler `handleRoomCreated` alongside the existing `handleMemberAdded` and `handleRoomSync`. The latter two are untouched.

### Dispatcher

In `inbox-worker/handler.go`, the existing event-type switch gains a new case:

```go
switch evt.Type {
case "member_added":
    err = h.handleMemberAdded(ctx, evt)
case "room_sync":
    err = h.handleRoomSync(ctx, evt)
case "room_created":
    err = h.handleRoomCreated(ctx, evt)        // NEW
default:
    slog.Warn("unknown inbox event type", "type", evt.Type)
}
```

### `handleRoomCreated` outline

```text
1. Unmarshal payload:
     var data model.RoomCreatedOutbox
     if err := json.Unmarshal(evt.Payload, &data); err != nil → return errPermanent.

   Read requestID from ctx (the home site set X-Request-ID on the outbox publish;
   the natsrouter middleware lifts it from the inbound headers).
   if requestID == "" → return errPermanent (mandatory; the home site is
   supposed to set it).

   Defensive: if len(data.Accounts) == 0 → log warn + return nil (ack).
   The home site only publishes outbox to a remote site if at least one account
   on that site is a member, so empty Accounts indicates a producer bug.

2. Look up local users:
     users, err := h.store.FindUsersByAccounts(ctx, data.Accounts)
   For each account in data.Accounts that has no matching User on this site →
   log warn (the User collection is globally replicated, so this should not
   happen; treat as transient and continue with the users we did find).

3. Build subscriptions — one per local user found:
     acceptedAt := time.UnixMilli(data.Timestamp).UTC()
     subs := make([]*model.Subscription, 0, len(users))
     for _, u := range users {
         sub := &model.Subscription{
           ID: idgen.GenerateUUIDv7(),
           User: model.SubscriptionUser{ID: u.ID, Account: u.Account},
           RoomID:       data.RoomID,
           SiteID:       data.HomeSiteID,         // NOT this site — room's home site
           Roles:        rolesForType(data.RoomType),
           Name:         subscriptionName(data, u),
           RoomType:     data.RoomType,           // denormalized
           IsSubscribed: subscriptionIsSubscribed(data, u),
           JoinedAt:     acceptedAt,
         }
         subs = append(subs, sub)
     }

4. Persist:
     err := h.store.BulkCreateSubscriptions(ctx, subs)
   Subscription IDs are random `idgen.GenerateUUIDv7()`. Redelivery safety
   comes from the unique compound index on `(roomId, u.account)` — a
   duplicate-key error on bulk insert means the sub already exists for
   this user/room pair, so treat it as success-on-redelivery.

5. Return nil → ack.
   No event publishing. The home site already published per-user
   subscription.update events targeting these users' subjects, and NATS
   gateways routed those events to this site for delivery to the user's
   frontend.
```

### Subscription field rules in inbox-worker

The inbox-worker mirrors the home-site logic exactly. The home site has already populated `Subscription.Name` for users it wrote locally; the remote site replicates that logic for its own users using the data the outbox carried.

```go
func rolesForType(t model.RoomType) []model.Role {
    if t == model.RoomTypeChannel {
        return []model.Role{model.RoleMember}
    }
    return nil  // dm, botDM
}

func subscriptionName(d model.RoomCreatedOutbox, u model.User) string {
    switch d.RoomType {
    case model.RoomTypeChannel:
        return d.RoomName
    case model.RoomTypeDM:
        // The DM has exactly two users. The user we're processing is on the
        // destination site; the requester is on the home site. Therefore the
        // "other party" from u's perspective is always the requester.
        return d.RequesterAccount
    case model.RoomTypeBotDM:
        // For botDM, two cases:
        //   - u is the bot (account ends in ".bot")  → other = requester (human)
        //   - u is the human (rare cross-site case)  → other = bot
        //     (but if the human is on a remote site, the bot is on the home
        //     site and we wouldn't be processing the human here. The home
        //     site's room-worker writes the human's sub locally on the home
        //     site even when the human is the requester. Cross-site for
        //     botDM only happens when the bot lives on a remote site.)
        if isBot(u.Account) {
            return d.RequesterAccount
        }
        // Defensive — should not happen given how home site segregates
        // accounts by SiteID before publishing outbox.
        slog.Warn("unexpected human account on remote botDM inbox event",
            "account", u.Account)
        return d.RequesterAccount
    }
    return ""
}

// isBot mirrors the home-site predicate (room-service/helper.go) so that
// remote-side classification matches: both ".bot" suffix and "p_" prefix
// (webhook-style bots) are treated as bots.
func isBot(account string) bool {
    return strings.HasSuffix(account, ".bot") || strings.HasPrefix(account, "p_")
}

func subscriptionIsSubscribed(d model.RoomCreatedOutbox, u model.User) bool {
    // Only the human's sub in a botDM has IsSubscribed = true.
    if d.RoomType != model.RoomTypeBotDM {
        return false
    }
    return !isBot(u.Account)
}
```

`subscriptionSidebarName` is removed entirely — the spec previously composed `RequesterEngName + RequesterChineseName` (or `AppName`) on this remote side; with `Subscription.SidebarName` gone, that helper has no consumer. The frontend resolves human-readable display names from the locally-replicated `users` / `apps` collections.

### Store

`inbox-worker` already has `FindUsersByAccounts` and `BulkCreateSubscriptions` from add-member. No new store methods are required for `handleRoomCreated`.

### What inbox-worker does NOT do

- It does **not** call `UpsertRoom`. Room docs live exclusively on the home site (the user explicitly clarified this — `room_sync` was originally designed for room mirroring but is not used by create-room).
- It does **not** write `room_members` documents. Those also live home-only.
- It does **not** publish events. The home site is solely responsible for `subscription.update` and `MESSAGES_CANONICAL` publishes.

### Idempotency

| Step | Mechanism |
|------|-----------|
| 4 — BulkCreateSubscriptions | Unique compound index `(roomId, u.account)`; duplicate-key → success. |
| Outbox-event-level | NATS-Msg-Id = `requestID:destSiteID`; JetStream dedups at consumer side. |

A redelivery from JetStream → already-deduped at the consumer level (NATS-Msg-Id). If the dedup window has expired and the handler runs again, the unique compound index on `(roomId, u.account)` makes the bulk insert a no-op.

## Cross-Site Scenarios

Four end-to-end traces. All assume `X-Request-ID` is present at the entry point and propagated correctly through every hop.

### Scenario A — Single-site channel

`alice@site-A` creates a channel with users `bob, carol` and org `org-fx`. All members live on site-A. No cross-site traffic.

```text
Client →  chat.user.alice.request.room.site-A.create
          payload: {name:"deal team", users:["bob","carol"], orgs:["org-fx"], channels:[]}

room-service (site-A)
  - validates, expands org-fx via CountNewMembers (count-only)
  - generates roomId = idgen.GenerateID() = "r_xyz"
  - publishes chat.room.canonical.site-A.create with the resolved payload
  - replies {status:"accepted", roomId:"r_xyz", roomType:"channel"}

room-worker (site-A)
  step 4   Room{id:r_xyz, type:channel, name:"deal team", siteID:site-A, ...}
  step 5   ListNewMembersForNewRoom(["org-fx"], ["bob","carol"]) → ["bob","carol","dave","emma"]
           strip alice (in case alice is in org-fx)
           Build 4 member subs + 1 owner sub for alice
           Each sub.Name = "deal team"
  step 6   BulkCreateSubscriptions (5 docs)
  step 7   RoomMembers: 4 individuals + 1 org row + 1 individual for alice
  step 8   ReconcileMemberCounts → UserCount=5, AppCount=0
  step 9   5 subscription.update events (one per sub)
  step 10  Two sys-messages on chat.msg.canonical.site-A.created
  step 11  No outbox (no remote-site users)
  step 12  AsyncJobResult{ok}

inbox-worker (site-A)  no inbox traffic — none of the outbox events apply
```

### Scenario B — Cross-site DM

`alice@site-A` creates a DM with `bob@site-B`.

```text
Client → chat.user.alice.request.room.site-A.create
         payload: {name:"", users:["bob"], orgs:[], channels:[]}

room-service (site-A)
  - strip alice from users → ["bob"], roomType=dm
  - GetUser("bob") → User{ID:"u_bob", SiteID:"site-B", EngName:"Bob", ChineseName:"鲍勃"}
    (User docs are globally replicated — site-A has bob's record.)
  - deterministicRoomID = idgen.BuildDMRoomID("u_alice", "u_bob") = "u_aliceu_bob"
  - dedup: FindDMSubscription(alice, "bob") → ErrSubscriptionNotFound
  - publish canonical event
  - reply {status:"accepted", roomId:"u_aliceu_bob", roomType:"dm"}

room-worker (site-A)
  step 4   Room{id:u_aliceu_bob, type:dm, name:"", siteID:site-A, createdBy:"u_alice"}
  step 5   dm branch — 2 subs:
           alice's: name="bob",   roles=nil
           bob's:   name="alice", roles=nil, siteID=site-A
           (Note: bob's sub is written on site-A's subscriptions collection.
           The home site holds subs for everyone in the room.)
  step 6   BulkCreateSubscriptions (2 docs on site-A)
  step 7   No room_members for dm
  step 8   ReconcileMemberCounts → UserCount=2, AppCount=0
  step 9   2 subscription.update events:
             subject.SubscriptionUpdate("alice") — delivered locally on site-A
             subject.SubscriptionUpdate("bob")   — NATS gateway routes to site-B
  step 10  No sys-messages for dm
  step 11  Outbox to site-B (single event):
             outbox.site-A.to.site-B.room_created
             payload: {RoomID:"u_aliceu_bob", RoomType:"dm",
                       RoomName:"", HomeSiteID:"site-A",
                       Accounts:["bob"], RequesterAccount:"alice",
                       Timestamp}
             Nats-Msg-Id = requestID + ":site-B"
  step 12  AsyncJobResult{ok} on subject.UserResponse(alice)

inbox-worker (site-B)
  Consumes from INBOX_site-B (sourced from outbox.site-A.to.site-B.>)
  handleRoomCreated:
    - FindUsersByAccounts(["bob"]) → [User{ID:"u_bob", Account:"bob", ...}]
    - Build sub for bob:
        ID:           idgen.GenerateUUIDv7()
        RoomID:       "u_aliceu_bob"
        SiteID:       "site-A"      (room's home, not bob's)
        Name:         "alice"
        Roles:        nil
        IsSubscribed: false (omitted)
    - BulkCreateSubscriptions on site-B's subscriptions collection
    - No event publishing
```

End state:
- Site-A: Room{id:u_aliceu_bob}, alice's sub, bob's sub (site-A copy).
- Site-B: bob's sub (site-B copy). No Room doc.
- bob's frontend on site-B sees the new room via the `subscription.update` event the home site already pushed (NATS gateway routed it to site-B).

### Scenario C — Cross-site channel

`alice@site-A` creates a channel with `name="deal team", users=["bob"], orgs=["org-fx"]`. `org-fx` resolves to local users carol, dave, frank, grace plus remote user ian on site-B. So site-B has bob and ian. (The client supplies the channel name — there is no auto-generation.)

```text
room-service (site-A)
  - users post-strip = ["bob"], orgs = ["org-fx"], roomType = channel
  - name = "deal team" (client-supplied, validated non-empty ≤ 100 runes)
  - capacity check via CountNewMembers(orgs, users, "", excludeAccount=alice)
    + 1 (the owner) — passes
  - generates roomId = "r_chan1"
  - publishes canonical event with name="deal team", users=["bob"], orgs=["org-fx"]

room-worker (site-A)
  step 4   Room{id:r_chan1, type:channel, name:"deal team"} (verbatim from request)
  step 5   ListNewMembersForNewRoom(["org-fx"], ["bob"], excludeAccount="alice") →
            ["bob","carol","dave","frank","grace","ian"]
           FindUsersByAccounts → bob:site-B, ian:site-B, others:site-A
           Build 7 subs in one loop: 1 owner sub for alice + 6 member subs.
           All subs.Name = "deal team"
  step 6   BulkCreateSubscriptions (7 docs on site-A)
  step 7   RoomMembers (channel + orgs non-empty):
             6 individual entries + 1 org entry "org-fx" + 1 individual for alice
  step 8   ReconcileMemberCounts → UserCount=7, AppCount=0
  step 9   7 subscription.update events (NATS routes each to that user's site)
  step 10  Two sys-messages on MsgCanonicalCreated(site-A)
  step 11  Outbox to site-B (only):
             Accounts on site-B: ["bob","ian"]
             outbox.site-A.to.site-B.room_created
             payload: {RoomID:"r_chan1", RoomType:"channel",
                       RoomName:"deal team", HomeSiteID:"site-A",
                       Accounts:["bob","ian"], RequesterAccount:"alice",
                       Timestamp}
             Nats-Msg-Id = requestID + ":site-B"
  step 12  AsyncJobResult{ok}

inbox-worker (site-B)
  - FindUsersByAccounts(["bob","ian"]) → 2 users
  - Build 2 subs:
      bob's: name="deal team", roles=[member], siteID=site-A
      ian's: same shape
  - BulkCreateSubscriptions on site-B
```

End state:
- Site-A: Room, all 7 subs (incl. cross-site users), all 8 room_members docs.
- Site-B: bob's and ian's subs (site-B copies, pointing at the site-A room).
- Sys-messages persist via message-worker on site-A → Cassandra. broadcast-worker delivers them to all subscribers on the appropriate sites.

### Scenario D — botDM (single-site)

`alice@site-A` creates a botDM with `weather.bot` (bot user lives on site-A).

```text
Client → payload: {name:"", users:["weather.bot"], orgs:[], channels:[]}

room-service (site-A)
  - strip alice → users=["weather.bot"], roomType=botDM
  - GetUser("weather.bot") → User{ID:"u_wbot", SiteID:"site-A", ...}
    (No EngName/ChineseName check on the bot — apps don't populate
    those fields in the users collection.)
  - deterministicRoomID = BuildDMRoomID("u_alice", "u_wbot")
  - dedup: FindDMSubscription(alice, "weather.bot") → not found
  - GetApp("weather.bot") → App{Assistant:{Enabled:true, Name:"weather.bot"}}
    (Lookup is purely a guard; no metadata is copied onto req.)
  - publishes canonical event
  - reply {status:"accepted", roomId:<computed>, roomType:"botDM"}

room-worker (site-A)
  step 4   Room{type:botDM, name:"", createdBy:"u_alice", ...}
  step 5   botDM branch:
           alice's sub: name="weather.bot", isSubscribed=true,  roles=nil
           bot's sub:   name="alice",       isSubscribed=false, roles=nil
  step 6   BulkCreateSubscriptions (2 docs)
  step 7   No room_members for botDM
  step 8   ReconcileMemberCounts → UserCount=1 (alice), AppCount=1 (weather.bot)
  step 9   2 subscription.update events
  step 10  No sys-messages
  step 11  No outbox (bot is on the same site)
  step 12  AsyncJobResult{ok}

inbox-worker  no traffic
```

If the bot lived on site-B instead, the outbox flow would mirror Scenario B with `RoomType:"botDM"` in the payload; inbox-worker on site-B would build the bot's sub with `Name="alice"` and `IsSubscribed=false`. The frontend renders the human-readable app name from the locally-replicated `apps` collection at display time, so no app metadata travels through the outbox.

### Race notes (recorded; not solved here)

1. **Concurrent DM creation** — alice@A and bob@B initiate DMs to each other simultaneously. Both `BuildDMRoomID` calls return the same `_id` (sorted concat is order-independent), but the rooms get created on **different home sites**. The losing site's room-worker hits `mongo.ErrDuplicateKey` on `CreateRoom`; the matching-existing check fails (different `SiteID`); the request fails with a permanent error and the client receives `AsyncJobResult{error}`. Acceptable corner case. A future tiebreak ("smaller siteID wins") could resolve this; left for follow-up.

2. **Outbox arrives before subscription.update** at the remote frontend. Both events are eventually consistent and the remote subscription is written before any messages can flow through the room. Frontend tolerates either order.

## Add-Member Retrofit

The add-member feature needs three small changes alongside this work so both features expose the same client contract and benefit from the same infrastructure improvements.

### 1. Drop `RequestID` from `AddMembersRequest` (header-only)

`X-Request-ID` is propagated through the canonical event header per PR #131. Carrying it in the payload as well is redundant and out of step with the new convention. Verification: if PR #131 already added `RequestID` to `AddMembersRequest`, remove it; if not, no diff is needed.

`room-service` makes the header **mandatory** for add-member requests too — no fallback.

### 2. Subscription IDs use `GenerateUUIDv7()`

Switch `processAddMembers` from `idgen.GenerateID()` to `idgen.GenerateUUIDv7()`, consistent with the established convention for `Subscription._id`. The same applies to `inbox-worker.handleMemberAdded` for cross-site sub IDs.

Idempotency on JetStream redelivery is provided by the unique compound index `(roomId, u.account)` on the `subscriptions` collection — duplicate-key on that index is treated as success-on-redelivery.

### 3. Populate `Subscription.Name` and `Subscription.RoomType`

In `processAddMembers`, when constructing each new subscription, set:

```go
sub.Name     = room.Name
sub.RoomType = room.Type   // always model.RoomTypeChannel for add-member
```

(`room` is already fetched at the top of the handler.) This populates the new fields for channels and aligns with the create-room rules: channel subs have `Name == Room.Name` and `RoomType == "channel"`.

`IsSubscribed` stays at its zero value (channels don't use it).

The same population is applied in `inbox-worker.handleMemberAdded`. The cross-site `member_added` event currently does not carry `RoomName`; it must be **extended** to include it:

```go
type MemberAddEvent struct {
    Type               string   `json:"type"`
    RoomID             string   `json:"roomId"`
    RoomName           string   `json:"roomName"`            // NEW — populated from room.Name on the home site
    Accounts           []string `json:"accounts"`
    SiteID             string   `json:"siteId"`
    JoinedAt           int64    `json:"joinedAt"`
    HistorySharedSince *int64   `json:"historySharedSince,omitempty"`
    Timestamp          int64    `json:"timestamp"`
}
```

This is a backward-additive change. Old events without `RoomName` would produce empty `Subscription.Name` on the remote side; given that PR #131 has not yet shipped and add-member is the only consumer, we can safely roll this out as a single deployment with create-room.

### 4. History timestamp fallback for `"none"` history config

When the add-members request specifies a history config of `"none"` (new members should not see history before joining), the config carries an optional `since` timestamp. If that timestamp is nil or absent, fall back to `time.Now().UTC()` so `Subscription.HistorySharedSince` is always populated when the intent is to restrict history:

```go
// inside processAddMembers, when building each sub:
if req.HistoryConfig == "none" {
    since := acceptedAt  // acceptedAt = time.UnixMilli(req.Timestamp).UTC()
    if req.HistorySharedSince != nil {
        since = time.UnixMilli(*req.HistorySharedSince).UTC()
    }
    sub.HistorySharedSince = &since
}
// if HistoryConfig != "none", leave HistorySharedSince nil (full history access)
```

The same fallback applies when constructing `MemberAddEvent.HistorySharedSince` for the outbox: if the config is `"none"` and no timestamp was in the request, the outbox event carries `time.Now().UnixMilli()` so `inbox-worker.handleMemberAdded` on remote sites also sets a non-nil `HistorySharedSince`.

On the inbox-worker side, if `evt.HistorySharedSince` is non-nil, use it as-is (the home site already applied the fallback).

### 5. Async-job notification on completion

At the end of `processAddMembers` (success and permanent-error paths), publish an `AsyncJobResult` to `subject.UserResponse(req.RequesterAccount)`:

```go
publishAsyncJobResult(ctx, model.AsyncJobResult{
    RequestID: requestID,
    Operation: "room.member.add",
    Status:    "ok" | "error",
    RoomID:    req.RoomID,
    Error:     sanitizedMsg,    // empty on success
    Timestamp: now.UnixMilli(),
})
```

PR #131 introduced this pattern as a bonus PR-#118 follow-up. If the helper already exists, reuse it. The new constant `room.member.add` for `Operation` is added to the model file alongside `room.create`.

### Backwards compatibility

Item 1 (drop `RequestID` field) is the only potentially breaking change. Mitigations:

- The `requestId` field in the JSON payload is simply ignored by both old and new servers — JSON unmarshal doesn't fail on unknown fields with the existing decoder configuration. Old clients can keep sending it; the value isn't used.
- New servers require the header; if old clients don't send the header, requests fail at the validation step. This is the intended behavior — clients must update to PR #131-compatible header generation.

Item 3 (extend `MemberAddEvent` with `RoomName`) is purely additive. Existing consumers ignore the field.

### 6. Bot rejection parity with create-channel

Add-member must enforce the same bot policy as create-channel:

- A `.bot` / `p_` account in `req.Users` (the inbound payload) → reject with `errBotInChannel` at `room-service.handleAddMembers`. Hard error returned to the caller.
- A bot account that comes back from `expandChannelRefs` (a source channel happens to contain a bot) → silently dropped via `filterBots(channelAccounts)` before merging into `allUsers`.

This mirrors `room-service.classifyAndValidate` (direct check) and `room-service.handleCreateRoomChannel` (`filterBots` after expansion). Today, the `pkg/pipelines.GetNewMembersPipeline` regex (`\.bot$|^p_`) already drops bots at the DB layer when room-worker materializes membership, so bots never become subscribed. The retrofit makes the rejection **explicit** at the room-service boundary instead of silent at the worker, so a client that explicitly lists a bot gets a useful error rather than a silent acceptance.

## Error Handling

### Sentinel errors (room-service)

Defined in `room-service/helper.go` next to the existing add-member sentinels:

```go
var (
    errEmptyCreateRequest   = errors.New("request must include at least one of users, orgs, channels, or name")
    errSelfDM               = errors.New("cannot create a DM with yourself")
    errBotInChannel         = errors.New("bots cannot be added to a channel during creation")
    errBotNotAvailable      = errors.New("bot not available")
    errInvalidUserData      = errors.New("user is missing required name fields")
    errMissingRequestID     = errors.New("missing X-Request-ID header")
    errInvalidRequestID     = errors.New("invalid X-Request-ID format")
    errChannelNameRequired  = errors.New("channel name is required")
    errChannelNameTooLong   = errors.New("channel name must be at most 100 characters")
    errUserNotFound         = errors.New("user not found")
    // existing reused: errNotRoomMember
)
```

Plus the typed wrapper `dmExistsError` (Part 2) for carrying the existing room ID through the error chain.

### `sanitizeError` extension

The existing pass-through allowlist (matched by `strings.Contains`) is extended with these substrings — all are user-displayable:

- `"request must include at least one of"`
- `"cannot create a DM with yourself"`
- `"bots cannot be added"`
- `"bot not available"`
- `"user is missing required"`
- `"dm already exists"` (special-cased in handler — uses `replyDMExists` directly)
- `"missing X-Request-ID"`
- `"user not found"`
- `"exceeds maximum capacity"` (already handled today)

Anything else collapses to `"internal error"`.

The pre-existing `sanitizeError` had three case branches: one for `errNotRoomMember` (returning the bare message instead of a wrapped one), and two separate `errors.Is` groups that both returned `err.Error()`. The two pass-through groups are merged into a single `case` — they do the same thing — leaving exactly two branches: the `errNotRoomMember` bare-message branch and the consolidated allowlist branch.

### Permanent vs. retryable errors in `processCreateRoom`

```go
var errPermanent = errors.New("permanent")
```

Permanent errors are wrapped with `fmt.Errorf("%w: %w", errPermanent, cause)` so `errors.Is(err, errPermanent)` returns true.

| Error source | Classification | Behavior |
|--------------|---------------|----------|
| Mongo timeout / connection | retryable | NAK; JetStream redelivers. |
| `mongo.ErrDuplicateKey` on `CreateRoom`, matching existing | success-on-redelivery | continue handler. |
| `mongo.ErrDuplicateKey` on `CreateRoom`, mismatched | permanent | ack + AsyncJobResult{error}. |
| `mongo.ErrDuplicateKey` on `BulkCreateSubscriptions` | success-on-redelivery | continue handler. |
| `mongo.ErrDuplicateKey` on `BulkCreateRoomMembers` | success-on-redelivery | continue handler. |
| User not found in step 3 | permanent | ack + AsyncJobResult{error}. |
| User not found in step 5 (channel branch) | permanent | ack + AsyncJobResult{error}. |
| `App` not found / disabled (botDM) — caught late | permanent | ack + AsyncJobResult{error}. |
| `EngName` or `ChineseName` missing on a user we're naming | permanent | ack + AsyncJobResult{error}. |
| NATS publish failure | retryable | NAK; redelivery re-runs publishes (NATS-Msg-Id dedups). |
| Marshalling errors | permanent | ack + AsyncJobResult{error}. |

The consumer dispatcher implements this:

```go
err := h.processCreateRoom(ctx, msg.Data())
switch {
case err == nil:
    publishAsyncJobResult(ctx, AsyncJobResult{Operation: "room.create", Status: "ok", RoomID: roomID, ...})
    msg.Ack()
case errors.Is(err, errPermanent):
    publishAsyncJobResult(ctx, AsyncJobResult{Operation: "room.create", Status: "error", Error: sanitizeError(err), ...})
    msg.Ack()  // do not retry
default:
    msg.Nak()  // transient; redelivery
}
```

The same dispatcher pattern applies to `processAddMembers` (the retrofit).

### inbox-worker error handling

`handleRoomCreated` follows the same retryable / permanent split:

| Error source | Classification |
|--------------|---------------|
| Empty `Accounts` | log warn + ack (no AsyncJobResult — requester is on a different site). |
| `FindUsersByAccounts` partial match | log warn for each missing account; continue with the users found. |
| `BulkCreateSubscriptions` duplicate-key | success-on-redelivery. |
| Mongo timeout | retryable; NAK. |
| Marshalling errors | permanent; log + ack. |

No async-job result is fired by inbox-worker — the home site already published one, and the requester is on the home site (not the inbox-worker's site).

### Logging

Every log line in the create-room paths carries:

- `requestId` (`slog.Attr` from ctx)
- `requesterAccount`
- `roomType` (once determined)
- `roomId` (once known)
- `siteId` (the service's site for inbox/room-worker; the destination site for outbox publishes)

Per CLAUDE.md: structured `log/slog` JSON. No PII (no message bodies, no full user docs). Account names are identifiers and acceptable to log.

### Frontend visibility of failures

| Where | What the client sees |
|-------|----------------------|
| Synchronous reply to NATS request/reply | Validation errors, `dm already exists` (with existing roomId), capacity errors. |
| `subject.UserResponse(requesterAccount)` | Permanent async errors (`AsyncJobResult{Status:"error", Error:"..."}`). |
| (No notification) | Retryable errors that succeed on redelivery. |
| `chat.user.{account}.event.subscription.update` | Successful sub creation (delivered to all members). |

## Testing Strategy

Per CLAUDE.md: TDD red→green→refactor; minimum 80% coverage / target 90% on core logic; race detector always on; table-driven tests for handlers; `go.uber.org/mock` for store mocking; `testcontainers-go` for integration tests under the `//go:build integration` tag.

### `pkg/model` round-trip tests

Add to `pkg/model/model_test.go`, leveraging the existing generic `roundTrip` helper:

- `TestRoom_BotDMRoundtrip` — `RoomType="botDM"` JSON/BSON round-trips.
- `TestRoom_AppCount` — `Room.AppCount` round-trips.
- `TestSubscription_NewFields` — all three new fields (`Name`, `RoomType`, `IsSubscribed`) round-trip; assert `omitempty` on `IsSubscribed`; assert `RoomType` is required (not omitted) and round-trips for all three values (`dm`, `botDM`, `channel`); assert that the previously-defined `SidebarName` field is no longer present on the struct.
- `TestCreateRoomRequest_JSON` — full struct round-trips with both client-supplied and server-populated fields.
- `TestRoomCreatedOutbox_JSON` — round-trips for `dm`, `botDM`, and `channel` payload shapes; asserts the legacy `RequesterEngName` / `RequesterChineseName` / `AppName` fields are absent from the struct.
- `TestErrorResponse_RoomIDOmitempty` — confirms `RoomID` is absent when empty.
- `TestApp_AssistantRoundtrip` — `App` and nested `AppAssistant` / `AppSponsor` round-trip; `Assistant.Enabled=false` round-trips.
- `TestMemberAddEvent_RoomNameField` — confirms the new field round-trips and is omitted when empty.

### `room-service/handler_test.go` — table-driven

Mock `RoomStore` (extending the existing `mock_store_test.go` via `make generate`). Tests for `handleCreateRoom`:

| # | Scenario | Setup | Expected |
|---|----------|-------|----------|
| 1 | Empty payload | `name=""`, `users=[]`, `orgs=[]`, `channels=[]` | reject `errEmptyCreateRequest` |
| 2 | Self-DM only | `users=[requester]` post-strip empty | reject `errSelfDM` |
| 3 | DM, counterpart not found | `GetUser(other)` returns `ErrNoDocuments` | reject `errUserNotFound` |
| 4 | DM, counterpart missing EngName | `EngName=""` | reject `errInvalidUserData` |
| 5 | DM, dedup hit | `FindDMSubscription` returns existing | reply `dmExistsError` with RoomID |
| 6 | DM, channel-name collision (alice has channel named "bob", no DM with bob) | `FindDMSubscription` returns ErrSubscriptionNotFound (channel sub filtered out by `roomType $in {dm,botDM}` clause) | proceed; publish |
| 7 | botDM, app not found | `GetApp` returns `ErrNoDocuments` | reject `errBotNotAvailable` |
| 8 | botDM, app disabled | `App.Assistant.Enabled=false` | reject `errBotNotAvailable` |
| 9 | botDM happy path | All present, enabled | publish; reply with deterministic roomId |
| 10 | Channel with bot | `users=["alice","weather.bot"]` | reject `errBotInChannel` |
| 11 | Channel name too long | client `Name` longer than 100 runes | reject `errChannelNameTooLong` |
| 11a | Channel missing name | `roomType=channel`, `Name=""` (with users/orgs/channels) | reject `errChannelNameRequired` |
| 12 | Channel exceeds capacity | `CountNewMembers` > `maxRoomSize` | reject "exceeds maximum capacity" |
| 13 | channelRef not subscribed | `expandChannelRefs` returns `errNotRoomMember` | reject `errNotRoomMember` |
| 14 | Missing X-Request-ID | header absent | reject `errMissingRequestID` |
| 15 | Stream publish fails | `js.Publish` returns error | reject (internal) |
| 16 | Channel happy path with orgs | All valid | publish; reply `{accepted, roomId, channel}` |
| 17 | DM happy path | All valid | publish; deterministic roomId via BuildDMRoomID |
| 18 | Channel with channels | non-empty `Name`, request has `Channels=[ref]` | published `Name` is the client-supplied value verbatim (already validated ≤ 100 runes by room-service); channelRef accounts/orgs merged via `expandChannelRefs` |

Each row asserts:

- The published canonical payload (captured via injected `publishToStream` func — same pattern as existing add-member tests).
- The reply body shape and content.
- That **no Mongo writes** happen in the handler — room-service is read-only at admit time.

### `room-worker/handler_test.go` — table-driven

Mock `room-worker.Store`. Tests for `processCreateRoom`:

- DM happy path → asserts 2 subs created with correct `Name` (other's account) / `Roles` (nil); Room doc with `Type=dm`, `Name=""`, `CreatedBy=requester.ID`, `UserCount=2`, `AppCount=0`; 2 `subscription.update` events; **no** sys-messages; AsyncJobResult{ok}.
- botDM happy path → human's sub has `Name=bot account`, `IsSubscribed=true`; bot's sub has `Name=human account`, `IsSubscribed=false`; Room has `CreatedBy=requester.ID`, `UserCount=1`, `AppCount=1`.
- Channel happy path with mixed local + remote users → expected sub count, requester gets `roles=[owner]`, 2 sys-messages with deterministic IDs (assert via `MessageIDFromRequestID(requestID, "room_created")` and `..., "members_added"`), outbox event per remote site with correct `Accounts` slice.
- Channel with org expansion → `ListNewMembersForNewRoom` called with empty `roomID` and the resolved orgs/users; resulting subs count matches.
- Channel `Name` longer than 100 runes → request rejected with `errChannelNameTooLong` (no truncation).
- `Subscription.Name == Room.Name` for all channel subs.
- Idempotency — duplicate `CreateRoom` returns existing matching room → handler proceeds; events re-publish (consumer-side dedup acceptable in mocks).
- Idempotency — duplicate-key on `BulkCreateSubscriptions` → handler treats as success.
- Permanent error path — missing user → AsyncJobResult{error}, message acked, no NAK.
- Retryable error — Mongo timeout simulated → NAK, no AsyncJobResult.
- `ReconcileMemberCounts` correctness — assert on the two-count-and-update call sequence.
- `subscription.update` fires for **all** subs (including remote-site users).
- Outbox `Accounts` only contains accounts whose `User.SiteID != room.SiteID`, grouped by destination site.

### `room-worker/integration_test.go` (`//go:build integration`)

Real Mongo via `testcontainers-go/modules/mongodb`, real NATS via `testcontainers-go/modules/nats` with JetStream. In-memory subject capture for assertions.

- End-to-end create-channel → assert Room/subs/room_members in Mongo, sys-messages on `chat.msg.canonical.{siteID}.created`.
- End-to-end create-DM → assert deterministic `roomID == BuildDMRoomID(...)`, two subs (both on home site's collection), no room_members.
- End-to-end create-botDM → assert `IsSubscribed=true` and `Name==bot account` on human's sub.
- `ReconcileMemberCounts` integration — bot-in-channel scenario gives correct `UserCount`/`AppCount`.
- Idempotent redelivery — publish same canonical event twice with same `X-Request-ID` → end state identical, no duplicate subs.

### `inbox-worker/handler_test.go`

Mock store. Add test cases for `handleRoomCreated`:

- DM remote receipt → builds bob's sub with `Name=alice's account`, `Roles=nil`, `SiteID=home site`.
- botDM remote receipt → bot's sub built with `Name=human account`, `IsSubscribed=false`.
- Channel remote receipt with multiple accounts → bulk insert all with `Name=Room.Name`.
- Empty `Accounts` → no-op + warn log; no Mongo write.
- Missing `X-Request-ID` → permanent error path.
- Idempotent redelivery — sub IDs are random UUIDv7; redelivery safety comes from the unique compound index on `(roomId, u.account)`. A duplicate-key error on bulk insert means the sub already exists for this user/room pair → treat as success.

### `inbox-worker/integration_test.go`

- Real Mongo, real NATS. Publish a fabricated outbox event to the INBOX stream → assert subs are created on the local subscriptions collection with all expected fields.
- Channel-with-multiple-accounts payload → end state has the right number of subs.
- Redelivery → no duplicates.

### `room-service/integration_test.go`

- End-to-end NATS request/reply: publish to `chat.user.alice.request.room.{siteID}.create`, get `accepted` reply with `roomId`, observe canonical event on the ROOMS stream.
- DM dedup: pre-seed alice's Subscription with `name="bob"` plus a Room of `type=dm` matching that subscription's roomId; observe `dm already exists` reply with the existing `roomId` populated.

### Add-member retrofit tests

Existing `add-member` unit tests gain new assertions:

- Subscription IDs are `idgen.GenerateUUIDv7()` (32-char hex, no hyphens).
- New subs have `Name == room.Name`.
- AsyncJobResult{ok} fires on success; AsyncJobResult{error} fires on permanent error.
- Missing `X-Request-ID` header now rejects.
- `MemberAddEvent` cross-site outbox carries `RoomName`.
- History config `"none"`, timestamp provided → `sub.HistorySharedSince == provided value`.
- History config `"none"`, timestamp absent → `sub.HistorySharedSince` is set to approximately `now` (non-nil, within a few seconds of the test start).
- History config absent (not `"none"`) → `sub.HistorySharedSince` is nil regardless.

`inbox-worker.handleMemberAdded` tests:

- New subs created from a remote `member_added` event have `Name == event.RoomName`.
- When `evt.HistorySharedSince` is non-nil, sub's `HistorySharedSince` equals that value.

### Coverage targets

- `room-service/handler.go` create-room paths: ≥ 90%.
- `room-worker/handler.go` create-room paths: ≥ 90%.
- `inbox-worker/handler.go` `handleRoomCreated`: ≥ 90%.
- `pkg/model` new field round-trips: 100%.
- New helpers (`stripAccount`, `subscriptionName`, `subscriptionIsSubscribed`, `rolesForType`, `isBot`): ≥ 95% (pure functions). `composeAutoName` and `truncateRunes` were removed when channels became required-name (the server preserves the client-supplied name verbatim). `composeName` and `subscriptionSidebarName` were removed when `Subscription.SidebarName` was dropped (frontend resolves display names from the locally-replicated `users`/`apps` collections).

Use `go test -coverprofile=coverage.out` and `go tool cover -func=coverage.out` to verify per-package thresholds before merge.

### Test fixtures

- `room-service/testdata/`: canonical request payloads — channel-with-orgs, channel-with-channelrefs, dm, botDM, edge cases (empty payload, self-DM, channel with bot, capacity overflow).
- `room-worker/testdata/`: matching canonical events and golden expected outputs (Room doc, subs, room_members, sys-message bodies).
- `inbox-worker/testdata/`: outbox event payloads for each room type.

### Out-of-band verification (manual checklist)

After implementation, before merge:

- `make lint` green.
- `make test` green (full repo, race detector enabled).
- `make test-integration SERVICE=room-service`, `SERVICE=room-worker`, `SERVICE=inbox-worker` all green.
- `grep -n "Subscription.Name" pkg/model/subscription.go` shows the new field.
- Manual NATS request to local docker-compose stack creates a channel end-to-end and emits the expected events on each subject (`chat.user.*.event.subscription.update`, `chat.msg.canonical.*.created`, `chat.user.{requester}.event.async-job-result`).

## Follow-ups (deferred to separate PRs)

### Sync server-to-server DM creation

The current async flow (request → JetStream → AsyncJobResult) is appropriate for end-user-initiated DMs. Internal server callers (e.g. `user-service`'s subscribing-app flow) need a synchronous DM-creation path so the call site can immediately use the returned room.

Planned shape:

- New subject `chat.server.request.room.{siteID}.create.dm` (queue group `room-service`), distinct from the user-facing `chat.user.{account}.request.room.{siteID}.create`. The `chat.server.*` namespace requires server credentials, not a user JWT.
- The handler runs the same validation as the async DM/botDM path (DM dedup, `errSelfDM`, `errBotNotAvailable` for botDM, etc.) and writes Room + Subscriptions inline (no `js.PublishMsg` to ROOMS), then publishes the same `subscription.update` events and outbox events that the async worker would.
- Reply payload contains the full `Room` doc (or the existing `roomId` on a dedup hit) so the caller doesn't need a follow-up read.
- No `AsyncJobResult` is published; the caller gets success/failure directly from the NATS reply.

This relies on the worker code structure remaining clean: `processCreateRoom` already dispatches to `processCreateRoomDM` / `processCreateRoomBotDM` / `processCreateRoomChannel`, each taking only `(req, room, requester, requestID, acceptedAt, now)`. The follow-up PR will:

1. Extract the room-create + duplicate-key collision-handling prelude from `processCreateRoom` into a shared helper (`createRoomDoc(ctx, req, requester, roomType) (*Room, error)`).
2. Have the new sync handler call that helper, then `processCreateRoomDM`/`BotDM`, and finally inline the outbox + per-user `subscription.update` publishes (skipping `publishAsyncJobResult`).
3. Keep the async path unchanged — `processCreateRoom` calls the same shared helper, then dispatches to the same per-type functions, then runs the async-job-result epilogue.

No changes to the create-room data model, outbox payload, or DM-dedup contract are required for this follow-up.

### DM-creation tiebreak across sites

If `alice@A` and `bob@B` initiate DMs to each other simultaneously, both home sites compute the same `BuildDMRoomID` but try to create the room locally. The losing site fails with a permanent error today (recorded in the "Race notes" section). A "smaller siteID wins" tiebreak — checking the deterministic ID's home-site preference before insertion — would resolve this without a roundtrip. Out of scope here; recorded as a known gap.

---

*End of design document.*

