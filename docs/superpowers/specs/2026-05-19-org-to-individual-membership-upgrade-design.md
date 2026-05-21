# Member-Add Improvements + DM-Already-Exists Frontend Fix

Three related changes, packaged together because Parts 1 and 2 share the same pipeline (`pkg/pipelines/member.go`) and the same candidate-selection path in `room-worker`; Part 3 sits alongside as a small frontend fix uncovered while investigating one of the user's reports.

1. **Part 1** — bug fix for the silent no-op when an already-org-subscribed user is added individually.
2. **Part 2** — new feature: accept `deptId` values inside the `orgs: [...]` field, alongside the existing `sectId` semantics.
3. **Part 3** — frontend fix: skip the wait-for-summaries loop in `CreateRoomDialog` when the backend reports the DM already exists, so the dialog navigates directly to the existing room instead of risking the 3-second "taking longer than expected" banner on a session-start race.

---

# Part 1 — Fix: Add-Member Silently No-ops When Target Already Subscribed Via Org

## Problem

When a user is already in a channel as part of an organization (their subscription was created by a prior `member.add` with `orgs: [...]`), a follow-up `member.add` that lists that user under `users: [...]` is silently accepted but does nothing meaningful — the user remains an org-only member.

Concretely, after the bug:
- `subscriptions` doc for the user exists (unchanged from the org add).
- `room_members` has a `member.type="org"` row for the org, but **no `member.type="individual"` row for the user**.
- The remove-member flow's `HasIndividualMembership` / `HasOrgMembership` distinction (`room-service/handler.go:500`) treats the user as org-only, blocking individual removal/leave operations with "org members cannot leave individually" — even though the operator explicitly added them as an individual.

## Root cause

`pkg/pipelines/member.go:50-65` — `GetNewMembersPipeline` filters candidates by the *existence* of a `subscriptions` document, with no awareness of `room_members.member.type`. Both `room-service.CountNewMembers` and `room-worker.ListNewMembers` use this pipeline.

When the target user already has a subscription (from the prior org add), the pipeline filters them out. `room-worker.processAddMembers` then hits the `len(accounts) == 0` early return at `room-worker/handler.go:714-716`, so the individual `room_members` row is never created.

The pipeline answers "does any subscription exist?" when, for individually-targeted accounts, the question should be "does an *individual* `room_members` row exist?"

## Design

Replace the filter-on-subscription pipeline (for `roomID != ""`) with a pipeline that returns each candidate plus two flags. The worker then decides what to write based on those flags.

### New pipelines

`pkg/pipelines/member.go` gains two sibling pipelines that share a private match-stage helper. Splitting capacity-counting from full-candidate-resolution avoids paying for the `room_members` lookup on the capacity path (room-service runs `CountNewMembers` on every add request).

```go
// GetNewMembersPipeline counts net-new subscriptions; caller appends $count
// (capacity check) or $group (worker candidate resolution). Existing API,
// extended with dept-aware $or as documented later in this section.
func GetNewMembersPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) bson.A

// GetAddMemberCandidatesPipeline returns per-candidate {account, hasSubscription, hasIndividualRoomMember} for the worker.
func GetAddMemberCandidatesPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) (bson.A, error)
```

Both pipelines:
- Reuse a private `matchCandidates(orgIDs, directAccounts, excludeAccount)` helper for the `$match` stage (org/account union, bot exclusion, optional excludeAccount).
- Use the existing `(roomId, u.account)` unique index on `subscriptions` for the subscriptions lookup.

`GetAddMemberCandidatesPipeline` additionally:
- Uses the `users` collection's `_id` (passed via `let: {uid: "$_id"}`) to drive the `room_members` lookup as `member.type=="individual" && member.id == $$uid`. **This pairs with the existing unique index `(rid, member.type, member.id)`**, so the lookup is index-covered and constant-time per candidate. Filtering on `member.account` instead would force a scan of every individual `room_members` row for the room — quadratic in (candidates × room size). The output `account` field is still projected from `$account` for the worker's bookkeeping.
- Projects to `{ account, hasSubscription, hasIndividualRoomMember }` where each flag is `{ "$gt": [{ "$size": "$<lookup>" }, 0] }`.

Both require `roomID != ""` (panic on empty — the existence checks are meaningless without a room). The create-room capacity-check path keeps using the existing `GetNewMembersPipeline` (which takes `roomID == ""` and skips the subscriptions lookup, since no subs can exist for a not-yet-created room).

### New worker store method

`room-worker/store.go` and `store_mongo.go`:

```go
type AddMemberCandidate struct {
    Account                 string
    HasSubscription         bool
    HasIndividualRoomMember bool
}

func (s *MongoStore) ListAddMemberCandidates(
    ctx context.Context,
    orgIDs, directAccounts []string,
    roomID string,
) ([]AddMemberCandidate, error)
```

Implementation: run the new pipeline against the `users` collection; decode straight into `[]AddMemberCandidate`.

The old `ListNewMembers` method is removed — the worker is its only caller.

### Worker handler changes

`room-worker/handler.go` `processAddMembers` is restructured:

1. Call `h.store.ListAddMemberCandidates(ctx, req.Orgs, req.Users, req.RoomID)`.
2. Compute the `writeIndividuals` gate as today: `writeIndividuals = len(req.Orgs) > 0 || hadOrgsBefore`. This preserves the existing convention that individual `room_members` rows are only tracked once orgs are involved (pre-orgs rooms keep using the subscription itself as the source of truth).
3. Compute three derived sets:
   - **`needSub`** = candidates where `!HasSubscription` — these go into `BulkCreateSubscriptions`.
   - **`needIndividualRoomMember`** = candidates where `Account ∈ req.Users` && `!HasIndividualRoomMember` **and `writeIndividuals == true`** — these get a `member.type="individual"` row.
   - **Org rows** = unchanged (one per `req.Orgs` entry).
4. Early-return only when **all three** sets are empty (truly nothing to do).
5. `FindUsersByAccounts` is called for the union of `needSub` and `needIndividualRoomMember` accounts (we need `user.ID` for the `room_members.member.id` field even on the upgrade path).
6. Subscriptions are built and inserted only for `needSub`. The `IsDuplicateKeyError` swallow in `BulkCreateSubscriptions` stays as a defense-in-depth against JetStream redelivery, but is no longer load-bearing for the org→individual upgrade path.
7. Individual `room_members` rows are built for `needIndividualRoomMember`, regardless of whether the sub was just inserted or already existed.
8. The backfill loop at `room-worker/handler.go:836-867` (first-org backfill of existing subscribers) is unaffected — it runs only when `len(req.Orgs) > 0 && !hadOrgsBefore`, a different trigger. The set we backfill is "existing subscribers minus accounts we already processed in this request"; under the new design that "already processed" set is `needSub ∪ needIndividualRoomMember` (anyone whose individual row we're writing now, whether they got a new sub or just an upgrade).

For the bug scenario specifically (alice already in via org, now added individually), `hadOrgsBefore == true` so `writeIndividuals == true` and the gate is naturally satisfied.

### room-service changes

`room-service/store.go` + `store_mongo.go`: `CountNewMembers` continues to use `GetNewMembersPipeline` (which natively handles both `roomID == ""` and `roomID != ""`) and appends `$count: "n"` as the terminal stage.

Public signature and return type are unchanged. Capacity semantics preserved: re-adding an org-only user as an individual produces a count of 0 (no new sub), so the user doesn't double-count against `maxRoomSize`. The lite pipeline skips the `room_members` lookup that the worker path needs, so capacity checks stay cheap.

The create-room path (`CountNewMembers` called with `roomID == ""`) keeps the old `GetNewMembersPipeline`.

### Behavior matrix

| Pre-state                                  | Request                  | Sub written? | Individual row written? | Org row written? |
|--------------------------------------------|--------------------------|--------------|-------------------------|------------------|
| alice not in room                          | add alice individually   | yes          | yes                     | —                |
| alice not in room                          | add org-1 (contains alice) | yes        | no                      | yes (for org-1)  |
| alice in room via org-1 only               | add alice individually   | no (skip)    | **yes (fix)**           | —                |
| alice in room via org-1 + individual       | add alice individually   | no           | no                      | —                |
| alice in room via individual only          | add org-1 (contains alice) | no         | no                      | yes (for org-1)  |
| alice in room via org-1                    | add org-2 (contains alice) | no         | no                      | yes (for org-2)  |

### Domain field semantics on upgrade

When alice is upgraded org → individual, her existing subscription's `JoinedAt` and `HistorySharedSince` are **not** touched. The individual `room_members` row gets `Ts = acceptedAt` (the new request's accepted time), which is the right value for "when did individual membership begin." This matches the system's existing model where the subscription represents membership-into-the-room and `room_members` represents membership-source rows.

## Testing

### Unit tests (`room-worker/handler_test.go`)

Table-driven cases against `processAddMembers` covering the matrix above. Key new cases:

- **Org→individual upgrade**: pre-state has subscription + org `room_members` row; request lists user under `Users`; assert `BulkCreateSubscriptions` is **not** called (or called with empty slice), `BulkCreateRoomMembers` is called with exactly one `member.type="individual"` row.
- **Mixed add**: request lists two users — one truly new, one already org-subscribed; assert sub is created only for the new one, individual rows are created for both.
- **No-op add**: request lists a user already both individually and org-subscribed; assert handler returns without writes.
- **Duplicate org add**: request lists same org again; assert org `room_members` insert is attempted (existing unique-index dedupe still applies; no change there).

### Unit tests (`room-service/handler_test.go`)

`CountNewMembers` is mocked at the store boundary, so room-service tests don't need pipeline-level changes. Add one regression test that confirms the capacity-check path still rejects when `newCount + room.UserCount > maxRoomSize`.

### Integration tests

`room-worker/integration_test.go`:
- Seed alice with a subscription and an org `room_members` row.
- Run `processAddMembers` with `Users: ["alice"]`.
- Assert: exactly one subscription doc for alice in the room (no duplicate), and exactly one individual `room_members` row for alice.

`room-service/integration_test.go`:
- Same pre-state.
- Call `CountNewMembers(orgIDs=nil, directAccounts=["alice"], roomID)`.
- Assert the result is 0 (capacity unchanged).

### Coverage gate

The CLAUDE.md 80% minimum applies; the new pipeline + store method should be at 90%+ given they're core business logic.

## Out of scope

- **Cross-site (OUTBOX) replay semantics.** The bug exists in the local `room-worker` path; cross-site `MemberAddEvent` handling uses a different code path that doesn't go through `ListNewMembers`. A spot-check of that path will be part of implementation, but a fix there (if needed) is a separate spec.
- **`docs/client-api.md`.** The wire request/response shapes for `member.add` are unchanged. The CLAUDE.md client-API gate doesn't apply.
- **Backfill of already-broken state.** Existing channels where alice is org-only but the operator intended individual membership won't auto-heal; a remediation job is out of scope. The operator can re-trigger `member.add` with `Users: ["alice"]` after this fix ships and it will create the missing row.

---

# Part 2 — Feature: Accept DeptId in `Orgs` Matching

## Motivation

Today, the `orgs: [...]` field in `member.add` and `room.create` is interpreted strictly as a list of `sectId`s — the pipeline at `pkg/pipelines/member.go:31` filters `users` on `sectId IN orgIDs`. Operators want to onboard whole departments without enumerating every constituent section. The fix accepts a `deptId` anywhere a `sectId` is accepted, with no API surface change.

## Domain assumption (confirmed)

In the operator's data, sectIds are unique across departments. When an ID exists as both a deptId (for some users) and a sectId (for others), the dept interpretation is a superset of the sect interpretation — every `sectId=X` user is also `deptId=X`. This lets us prefer the dept interpretation on overlap without losing members. This is a property of the source HR/directory data, not enforced by the chat system.

## User model changes

`pkg/model/user.go`:

```go
type User struct {
    ID          string `json:"id"           bson:"_id"`
    Account     string `json:"account"      bson:"account"`
    SiteID      string `json:"siteId"       bson:"siteId"`
    SectID      string `json:"sectId"       bson:"sectId"`
    SectName    string `json:"sectName"     bson:"sectName"`
    SectTCName  string `json:"sectTCName"   bson:"sectTCName"`  // NEW
    DeptID      string `json:"deptId"       bson:"deptId"`      // NEW
    DeptName    string `json:"deptName"     bson:"deptName"`    // NEW
    DeptTCName  string `json:"deptTCName"   bson:"deptTCName"`  // NEW
    EngName     string `json:"engName"      bson:"engName"`
    ChineseName string `json:"chineseName"  bson:"chineseName"`
    EmployeeID  string `json:"employeeId"   bson:"employeeId"`
}
```

The new fields are populated by the external HR / directory sync that already owns the `users` collection (no production code in this repo writes to `users`). Existing user docs that pre-date the sync extension won't have dept fields populated; dept-based adds for those users return 0 candidates — same outcome as adding an unknown `orgId` today.

## Pipeline changes

Both `GetNewMembersPipeline` (create-room) and the new `GetAddMemberCandidatesPipeline` introduced in Part 1 extend their `$match.$or`:

```go
if len(orgIDs) > 0 {
    orFilter = append(orFilter, bson.M{"sectId": bson.M{"$in": orgIDs}})
    orFilter = append(orFilter, bson.M{"deptId": bson.M{"$in": orgIDs}})  // NEW
}
```

Per-account dedup via the terminal `$addToSet $account` (or, for `GetAddMemberCandidatesPipeline`, grouping by `account`) handles the case where a single user matches both clauses (e.g., `user.sectId=X` and `user.deptId=Y`, both in `orgIDs`) — only one candidate row per account.

No tiebreak is needed at this stage. The tiebreak lives in the read paths below.

## Read-path tiebreak: prefer dept on overlap

The room-member display and remove-org sys-message resolve a single rendered string per `member.id` row. Two steps:

1. **Pick the winning interpretation.** Per the domain assumption, when `member.id` exists as both a deptId and a sectId, the dept's `(deptName, deptTCName)` pair wins.
2. **Format the rendered name.** The chosen pair is concatenated as `name + " " + tcName`, mirroring the existing `displayName(user) = engName + " " + chineseName` convention in `room-worker/sysmsg.go:10-25`. Empty-component fallbacks follow the same pattern (if either is empty, return the non-empty one; if both empty, the caller's "no name" branch fires).

The combined string is what appears in member-list display rows, in the system-message `Content` field, and in the `MemberRemoved.SectName` structured payload — same shape consumers see today, with broadened semantics.

The existing `displayName(user)` helper in `room-worker/sysmsg.go:10-25` already encodes the same "combine two names with a space, fall back to the non-empty one, then fall back to a third value" pattern (today the third value is `user.Account`). Extract the shared core into a new package `pkg/displayfmt` so both `room-worker` (sys-messages) and `room-service` (member-list enrichment) can use one canonical implementation:

```go
// pkg/displayfmt/combine.go
package displayfmt

import "strings"

// CombineWithFallback joins first and second with a space; returns the non-empty side or fallback when both are empty.
func CombineWithFallback(first, second, fallback string) string {
    first = strings.TrimSpace(first)
    second = strings.TrimSpace(second)
    switch {
    case first == "" && second == "":
        return fallback
    case first == "":
        return second
    case second == "":
        return first
    case first == second:
        return first
    default:
        return first + " " + second
    }
}
```

`room-worker/sysmsg.go` collapses its existing 16-line `displayName` to a one-line wrapper and adds `displayOrg`:

```go
func displayName(u *model.User) string  { return displayfmt.CombineWithFallback(u.EngName, u.ChineseName, u.Account) }
func displayOrg(name, tcName, orgID string) string { return displayfmt.CombineWithFallback(name, tcName, orgID) }

func formatRemovedOrg(name, tcName, orgID string) string {
    return quoted(displayOrg(name, tcName, orgID)) + " has been removed from the channel"
}
```

`room-service/store_mongo.go ListRoomMembers` uses `displayfmt.CombineWithFallback` directly in its decode loop (see the enrichment section below).

The MongoDB pipeline does NOT replicate the combine logic — it returns the raw `{name, tcName}` pair and the Go side combines once on read. This eliminates the BSON↔Go duplication and saves pipeline stages on the member-list hot path.

### Enrichment join (`room-service/store_mongo.go:451-472`)

The `_orgMatch` `$lookup`'s inner pipeline stays at 3 stages — match → addFields → group — matching today's stage count. Pipeline emits raw `{name, tcName, memberCount}` and the Go decode applies `combineWithFallback`. This avoids duplicating the helper logic in BSON and is a strict perf win on the member-list hot path:

```go
pipeline: bson.A{
    bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
        bson.M{"$eq": bson.A{"$$mtyp", "org"}},
        bson.M{"$or": bson.A{
            bson.M{"$eq": bson.A{"$deptId", "$$orgId"}},
            bson.M{"$eq": bson.A{"$sectId", "$$orgId"}},
        }},
    }}}},
    bson.M{"$addFields": bson.M{
        // Per-row name pair from whichever side matched; $max(_isDept) in the $group then picks dept-first.
        "_isDept": bson.M{"$eq": bson.A{"$deptId", "$$orgId"}},
        "_name":   bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$deptId", "$$orgId"}}, "$deptName", "$sectName"}},
        "_tcName": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$deptId", "$$orgId"}}, "$deptTCName", "$sectTCName"}},
    }},
    bson.M{"$group": bson.M{
        // $max on _isDept tells the Go decoder whether any dept row exists; pair with the matching name.
        "_id":         nil,
        "isDept":      bson.M{"$max": "$_isDept"},
        "deptName":    bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", "$_name", nil}}},
        "deptTCName":  bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", "$_tcName", nil}}},
        "sectName":    bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", nil, "$_name"}}},
        "sectTCName":  bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", nil, "$_tcName"}}},
        "memberCount": bson.M{"$sum": 1},
    }},
},
```

The outer `$set display` block (`store_mongo.go:475-489`) pulls **raw** name pairs into a temporary `display.orgRaw` sub-doc instead of constructing the combined string in the pipeline:

```go
"orgRaw": bson.M{"$arrayElemAt": bson.A{"$_orgMatch", 0}},
"memberCount": bson.M{"$arrayElemAt": bson.A{"$_orgMatch.memberCount", 0}},
```

`ListRoomMembers` Go decode loop then resolves the final string with the dept-first preference and orgId fallback, reusing the same `combineWithFallback` helper as the worker:

```go
for i := range members {
    if members[i].Member.Type != model.RoomMemberOrg {
        continue
    }
    raw := members[i].Display.OrgRaw
    name, tcName := raw.SectName, raw.SectTCName
    if raw.IsDept && raw.DeptName != "" {
        name, tcName = raw.DeptName, raw.DeptTCName
    }
    members[i].Display.SectName = combineWithFallback(name, tcName, members[i].Member.ID)
}
```

The field name `display.sectName` stays unchanged for wire stability; its semantics broaden to "the resolved org display name (dept-or-sect, `name + " " + tcName`, or the raw orgId when both names are empty)." Both paths (sys-message in worker, member-list in service) emit byte-identical strings for the same input because they share the same Go helper.

**Net stage count:** unchanged at 3 inner stages (vs. today's 3). All BSON-Go duplication is eliminated.

### List-org-members RPC (`room-service/store_mongo.go:707`)

The `chat.user.{account}.request.orgs.{orgId}.members` endpoint (the "expand org" RPC the client calls to enumerate an org row in the member roster) was sectId-only. Under the broadened semantics it must resolve a deptId-added org just as it resolves a sectId-added org — otherwise the roster shows the org row but expansion returns `errInvalidOrg`.

`ListOrgMembers` switches from `Find({sectId: orgId})` to:

```go
cursor, err := s.users.Find(ctx, bson.M{"$or": []bson.M{
    {"sectId": orgID},
    {"deptId": orgID},
}}, opts)
```

This is the symmetric companion to the `GetSubscriptionWithMembership` / `GetUserWithMembership` dept-aware lookups landed earlier in this PR — same `$or` shape, same indexes covering both branches. Under the operator's domain assumption (dept ⊇ sect for any overlapping id), a deptId-added org returns the dept's full membership; a sectId-only org returns just the sect's members. The wire response shape and `errInvalidOrg` semantics are unchanged.

Test coverage (integration, `room-service/integration_test.go::TestMongoStore_ListOrgMembers_Integration`):
- `matches_by_deptId` — users with distinct `sectId` and `deptId`; query by `deptId` returns dept rows the sect-only filter would have missed.
- `matches dept users when orgId equals deptId without parent sect match` — invariant case (`sectId == deptId` for dept users); query by that id returns only the dept user.

### Remove-org name harvest (`room-worker/handler.go:514-523`)

`OrgMemberStatus` (`room-worker/store.go:31`) drops `SectName` and gains a per-row resolved pair. The pipeline does the per-row name selection (sect-fields when the row matched sectId, dept-fields when it matched deptId); the handler does the org-level dept-first tiebreak:

```go
type OrgMemberStatus struct {
    Account                 string
    SiteID                  string
    Name                    string  // resolved per-row: deptName if IsDept else sectName
    TCName                  string  // resolved per-row: deptTCName if IsDept else sectTCName
    IsDept                  bool    // true if this row matched by deptId
    HasIndividualMembership bool
}
```

`GetOrgMembersWithIndividualStatus` (`room-worker/store_mongo.go:261-294`) extends the pipeline:
- `$match: {$or: [{sectId: orgID}, {deptId: orgID}]}` — index-covered on both branches by `(sectId, account)` and the new `(deptId, account)`.
- `$addFields` computes `isDept = (deptId == orgID)`, plus `name`/`tcName` via `$cond` on `isDept`.
- `$lookup` into `room_members` with `let: {uid: "$_id"}` and the inner match `member.type=="individual" && member.id == $$uid` — index-covered by the existing unique `(rid, member.type, member.id)`. The current code matches on `member.account == "$$acct"` which is *not* index-covered; switch to `member.id` for constant-time per-user lookup.
- `$project` keeps `account`, `siteId`, `name`, `tcName`, `isDept`, `hasIndividualMembership` — internal `$$` vars and the lookup arrays are explicitly dropped.

`processRemoveOrg`'s name-harvest loop is replaced with a two-pass resolution that picks a single `(name, tcName)`:

1. First pass — find any row with `IsDept == true`. If found, take `(Name, TCName)` from that row.
2. Second pass — if no dept match, find first row (any). Take its `(Name, TCName)`.
3. If `members` is empty (e.g., users were deleted between request and processing), the resolved pair is `("", "")`.

The resolved pair feeds `formatRemovedOrg(name, tcName, req.OrgID)` for the sys-msg `Content`, and `displayOrg(name, tcName, req.OrgID)` populates `MemberRemoved.SectName`. The `orgId` fallback inside `displayOrg` guarantees the rendered output is never empty:

- Both names present → `"name tcName"`
- Only one present → that one
- Both empty → `orgID` (the raw sectId or deptId from the request)

**The existing `"missing SectName on all members"` permanent error at `room-worker/handler.go:521-523` is removed.** With the orgId fallback, there's no longer a need to fail an org-remove on missing names — the message degrades gracefully to showing the ID instead. The check is replaced with a `slog.Warn` if you want to surface the data-quality issue without failing the operation; alternatively, drop the log too. (Spec choice: emit the warn, since silent data-quality degradation is harder to notice.)

## Sys-message rendering: `Message.Content` is the source of truth

**Frontend renders system messages by displaying `Message.Content` directly.** The structured `SysMsgData` payload (carrying `MemberRemoved`) is preserved for non-rendering consumers (analytics, audit, future structured features) but is **not** the rendering source. This matches today's behavior — the change here only broadens what `Content` says, not how it's consumed.

Concretely, for an org-remove where the winning interpretation is `dept` with `deptName="Engineering"` and `deptTCName="工程部"`:

- `Message.Content` = `"\"Engineering 工程部\" has been removed from the channel"` — what the user sees.
- `Message.SysMsgData` = `MemberRemoved{OrgID: "...", SectName: "Engineering 工程部", RemovedUsersCount: N}` — structured copy of the same combined string in the existing `SectName` field.

### `MemberRemoved` payload shape

`model.MemberRemoved` (`pkg/model/member.go`) — **no new fields**. The existing `SectName` field carries the combined `displayOrg(name, tcName)` string:

```go
type MemberRemoved struct {
    User              *SysMsgUser `json:"user,omitempty"`
    OrgID             string      `json:"orgId,omitempty"`
    SectName          string      `json:"sectName,omitempty"`  // existing — now carries displayOrg(name, tcName)
    RemovedUsersCount int         `json:"removedUsersCount"`
}
```

This keeps the wire schema stable. The field name's semantics drift (it's no longer guaranteed to be just the sectName — it's the resolved org display string), and a future cleanup PR can rename it; tracked in "Out of scope" below.

## Behavior matrix

| `req.Orgs` entry X is… | Candidates returned | Name resolved at read-time |
|---|---|---|
| A sectId only | All users with `sectId == X` | `sectName` |
| A deptId only | All users with `deptId == X` | `deptName` |
| Both (per domain assumption) | Union dedup'd by `account` — since dept ⊇ sect, this equals all users with `deptId == X` | `deptName` (prefer-dept wins) |
| Neither (unknown) | 0 candidates | — (no row written) |

## Callers / paths touched by Part 2

- `pkg/model/user.go` — new fields.
- `pkg/pipelines/member.go` — `$or` extended; applies to both `GetNewMembersPipeline` and the new `GetAddMemberCandidatesPipeline` from Part 1.
- `room-service/store_mongo.go:451-472, 475-489` — enrichment join + display fold (dept-preference, new TCName field).
- `room-service/store_mongo.go:707` — `ListOrgMembers` `$or` over `(sectId, deptId)` so the `orgs.{orgId}.members` RPC resolves deptId-added orgs.
- `room-worker/store.go` — `OrgMemberStatus` struct extended.
- `room-worker/store_mongo.go:261-294` — `GetOrgMembersWithIndividualStatus` pipeline extended.
- `room-worker/handler.go:508-523` — name-harvest loop with dept-first preference.
- `room-worker/handler.go:618-622` — `MemberRemoved.SectName` is populated with the combined `displayOrg(name, tcName, orgID)` string instead of a raw sectName.
- `room-service/store_mongo.go:63-71` (`EnsureIndexes`) — new composite index `(deptId, account)` on `users` to match the existing `(sectId, account)` index, so the extended `$or` predicate is index-covered on both branches.

No changes needed to:
- `room-service/handler.go` `classifyAndValidate` or `handleAddMembers` — `Orgs` field shape is unchanged.
- `room_members` collection schema — `member.type` stays `"org"`; `member.id` stays the raw orgId string.
- Cross-site `MemberAddEvent` (carries accounts only).
- OIDC claim parsing — `users` doc population is external to this repo.

## Testing (Part 2)

**Unit — pipeline (`pkg/pipelines/member_test.go`)**:
- New `GetNewMembersPipeline` test cases: orgId matching only a sectId, orgId matching only a deptId, orgId matching both (verify dedup).
- Same matrix for `GetAddMemberCandidatesPipeline`.

**Unit — room-service enrichment (`room-service/store_mongo_test.go` or via handler test)**:
- Room with `member.id=X` and one user `(sectId=X, sectName="Sec")` → enriched `sectName == "Sec"`, no TCName.
- Room with `member.id=X` and one user `(deptId=X, deptName="Dep", deptTCName="部门")` → enriched `sectName == "Dep"`, `sectTCName == "部门"`.
- Room with `member.id=X` and overlap (one user `sectId=X`, one user `deptId=X`) → dept wins, `memberCount == 2`.

**Unit — room-worker (`room-worker/handler_test.go`)**:
- `processRemoveOrg` with all users `sectId=orgID`: sys-msg `SectName` is sect's name, `SectTCName` is sect's TC name.
- `processRemoveOrg` with all users `deptId=orgID`: sys-msg `SectName` is dept's name, `SectTCName` is dept's TC name.
- `processRemoveOrg` with mixed (one dept-matched user, one sect-only): sys-msg uses dept's `(name, tcName)`.
- `processRemoveOrg` with every name empty: sys-msg `Content` and `SectName` fall back to `req.OrgID` (no error), a warning is logged.
- `processRemoveOrg` with only `tcName` populated (no `name`): sys-msg uses `tcName` alone (no leading space).
- Enrichment: room with an org member whose users have no `name`/`tcName` at all → `display.sectName` resolves to `member.id`.

**Integration**:
- Seed users where `users[0].sectId="X"` and `users[1].deptId="X"`.
- `member.add` with `Orgs: ["X"]` → both users get subscriptions.
- `member.remove` org-flow on X → sys-msg carries dept's name.

## Performance and indexes

Every new or modified query is verified against the existing indexes; nothing is left to a full-collection scan. The table below covers all read paths introduced or modified across Parts 1 and 2.

| Pipeline / query | Filter | Index used | Notes |
|---|---|---|---|
| `GetCapacityCheckPipeline` `$match` | `sectId IN orgs OR deptId IN orgs OR account IN directAccounts` | `(sectId, account)` + new `(deptId, account)` + `account` | $or with both branches index-covered; MongoDB executes an index union. |
| `GetCapacityCheckPipeline` `$lookup subscriptions` | `roomId == X && u.account == Y` | existing unique `(roomId, u.account)` | Index-covered. |
| `GetAddMemberCandidatesPipeline` `$match` | same as above | same as above | Same. |
| `GetAddMemberCandidatesPipeline` `$lookup subscriptions` | same as above | same as above | Same. |
| `GetAddMemberCandidatesPipeline` `$lookup room_members` | `rid == X && member.type == "individual" && member.id == $$uid` | existing unique `(rid, member.type, member.id)` | Index-covered. Critical: uses `member.id` (user's `_id`), not `member.account`. |
| `GetOrgMembersWithIndividualStatus` `$match` | `sectId == orgID OR deptId == orgID` | `(sectId, account)` + new `(deptId, account)` | Both branches index-covered; same index union as above. |
| `GetOrgMembersWithIndividualStatus` `$lookup room_members` | `rid == X && member.type == "individual" && member.id == $$uid` | existing unique `(rid, member.type, member.id)` | Index-covered. Switched from `member.account` to `member.id` as part of this work. |
| Enrichment `_orgMatch` `$lookup` (`room-service/store_mongo.go:451-472`) | `member.type == "org" && (sectId == $$orgId OR deptId == $$orgId)` | `(sectId, account)` + new `(deptId, account)` | $or both branches indexed; inner `$sort _isDept desc` + `$group $first` work on a small per-org candidate set (members of one org), not the whole users collection. |
| `ListOrgMembers.Find` (`room-service/store_mongo.go:707`) | `sectId == orgID OR deptId == orgID` | `(sectId, account)` + new `(deptId, account)` | $or both branches index-covered; sorted by `account` ASC matches the leading prefix of both compound indexes after the equality on the leading key. |

### Required new index

`room-service/store_mongo.go` `EnsureIndexes`:

```go
if _, err := s.users.Indexes().CreateOne(ctx, mongo.IndexModel{
    Keys: bson.D{{Key: "deptId", Value: 1}, {Key: "account", Value: 1}},
}); err != nil {
    return fmt.Errorf("ensure users (deptId,account) index: %w", err)
}
```

Mirrors the existing `(sectId, account)` index. Created in `EnsureIndexes` at service startup; idempotent.

### Hot-path cost analysis

- **`member.add` request** triggers two pipeline executions: `CountNewMembers` (room-service) and `ListAddMemberCandidates` (room-worker). The split design ensures the capacity-check path doesn't pay for the `room_members` lookup. Both are gated by the `users` `$match` filter so they only touch the candidate set (typically tens of users for a direct add, hundreds for an org add), not the whole `users` collection.
- **`member.list` enrichment** (`ListRoomMembers` with `enrich=true`) is the most frequent hot path. Added work per org row: an extra `$or` branch in the inner `$lookup` `$match` (negligible — still indexed), plus per-org `$addFields` / `$sort` / `$group` / `$switch` stages operating on a small in-memory result set. For a room with N org rows the marginal cost stays O(N) rather than O(N × users).
- **`member.remove` (org)** triggers one `GetOrgMembersWithIndividualStatus` pipeline. The room_members lookup switching from `member.account` to `member.id` removes a quadratic-in-room-size scan that was a latent slow-query risk in the current code — net performance improvement over today's behavior, not just a no-regression.

### Pipeline complexity audit

The new `GetAddMemberCandidatesPipeline` runs **4 stages per candidate** (`$match` → 2× `$lookup` → `$project`), the same operator count as the existing `GetNewMembersPipeline` for the non-empty-roomID case. No N+1 patterns; no per-row Go-side queries.

The enrichment `_orgMatch` inner pipeline stays at **3 stages** (`$match` → `$addFields` → `$group`) — the same count as before. The `$group` widens to carry `isDept` and parallel `deptName`/`deptTCName`/`sectName`/`sectTCName` accumulators, but no stages are added. Combining the chosen branch happens once in Go (`displayfmt.CombineWithFallback`) on read, not in BSON. Each pipeline runs on the small per-org candidate set, so absolute time stays in the low-millisecond range for typical rooms.

## Out of scope (Part 2)

- **Backfilling existing User docs** with dept fields. Handled by the external HR/directory sync.
- **OIDC claim extension** for `SectID`/`SectName`/`SectTCName`/`DeptTCName`. The current OIDC claim set only includes `DeptID`/`DeptName`, but `users` doc population doesn't go through OIDC anyway.
- **Wire-schema rename** of `MemberRemoved.SectName` → `OrgName` (and `display.sectName` → `display.orgName`). Tracked as a follow-up cleanup; the field name semantics drift but the wire stays stable for this PR.
- **Frontend rendering** of `SectTCName` / TCName variants in the member list and system messages. Backend exposes the fields; frontend uptake is separate.
- **Persistence of resolved name in `room_members`**. The room_members row keeps `member.id` only; the name is resolved on every read. This matches the existing pattern and the user's explicit "`RoomMember` will remain the same. No change there." constraint.

---

# Part 3 — Frontend: Navigate Directly On DM-Already-Exists Reply

## Problem

When a user starts a DM with someone they already have a DM with, `CreateRoomDialog` occasionally shows the "Room creation is taking longer than expected. If it succeeds, the room will appear in your sidebar shortly — you can dismiss this dialog." banner before navigating, instead of opening the existing room immediately.

The backend dedup is correct (`room-service/handler.go:272-278` returns `dmExistsError`; `replyDMExists` synchronously replies `{error: "dm already exists", roomId: "<existing>"}`). The frontend's `treatAsSuccess: isDMExistsReply` correctly short-circuits the async-result wait. The remaining gap is in the dialog's post-reply logic.

## Root cause

`CreateRoomDialog.jsx:87-105` treats both branches — genuine create and dedup — through the same flow:

1. Call `createRoom(...)`, get back `sync` (either `{status:"accepted",roomId,roomType}` or `{error:"dm already exists",roomId}`).
2. `setPendingRoom({id: roomId, ...})` — enters the wait-for-summaries state.
3. `useEffect` (`:41-47`) watches `summaries` for a row with id matching `pendingRoom.id`. On match → `onCreated(pendingRoom); onClose()`.
4. `useEffect` (`:61-70`) fires a 3-second timeout (`SUBSCRIPTION_WAIT_TIMEOUT_MS`); on expiry → sets the "taking longer than expected" error.

For a genuine create that's the correct shape — the room doesn't exist in `summaries` yet, and `subscription.update` will land shortly and dispatch `ROOM_ADDED`, populating `summaries`. The match effect fires and the dialog closes.

For the **dedup branch**, the room and subscription already exist. `summaries` should already contain the row from session start (`BUCKETS_LOADED`). In practice the match effect fires the same render tick and the dialog closes immediately — most users never see the banner. But the path is **brittle to any race** where `summaries` doesn't yet have that DM:

- User opens Create dialog before `BUCKETS_LOADED` resolves.
- Reconnect-rebuild of summaries races with the click.
- Future regressions to `useRoomSubscriptions` initial-load timing.

In those cases the 3-second timer wins and the user sees the banner for a room that already exists in their database.

## Design

Short-circuit the wait state on the dedup branch. The existing DM is guaranteed to exist server-side (the backend just confirmed it). The user's `subscriptions` and `summaries` will reflect it — and if they don't yet, they will momentarily; either way navigating directly to it is correct.

### Code change

`chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.jsx`, `handleSubmit`:

```jsx
const { sync } = await createRoom(
    nats,
    { name: trimmedName, users: finalUsers, orgs: finalOrgs, channels: finalChannels },
    { treatAsSuccess: isDMExistsReply }
)
const roomId = sync.roomId
const displayName = trimmedName || finalUsers[0] || ''

if (isDMExistsReply(sync)) {
    // Dedup branch: server already confirmed the DM; skip the summaries-wait that can trip the 3s banner on a BUCKETS_LOADED race.
    onCreated({ id: roomId, type: 'dm', siteId: user.siteId, name: displayName })
    onClose()
    return
}

const roomType = sync.roomType
setPendingRoom({ id: roomId, type: roomType, siteId: user.siteId, name: displayName })
```

The post-dedup branch:
- Drops the `pendingRoom` setter (no wait state needed).
- Drops the `roomType` fallback (`sync.roomType || (isDMExistsReply(sync) ? 'dm' : undefined)`) since the only path that hits this case is `isDMExistsReply==true` and we hardcode `'dm'`. Note: this collapses the dedup-only-knows-roomId-not-type concern into "we always say `'dm'` on dedup." If the existing room is actually a `botDM`, the receiver (`ChatPage` / sidebar) will correct the type from the canonical subscription record. The DM/botDM distinction in the dialog's local `pendingRoom` is only used as a hint for the wait-state UI, which we're skipping.

### Why not also fix this for channels?

The dedup path only fires for DMs/botDMs (deterministic room IDs). Channels always get a fresh random ID per request and `room-service` never short-circuits — they always go through the async create path. So Part 3's logic is correctly scoped to the `isDMExistsReply(sync)` branch.

### `onCreated` contract

`onCreated({id, type, siteId, name})` is consumed by `Sidebar` / `MainApp` to set the active room. The shape here matches the genuine-create call exactly; the only behavior difference is that we don't enter the `pendingRoom` wait first. The downstream consumer doesn't care that no `ROOM_ADDED` will fire — it already has the room in summaries.

## Testing

`CreateRoomDialog.test.jsx`:
- **Existing test** at `:139` passes today because `DEFAULT_SUMMARIES` (`:23-27`) already contains `{id: 'r-existing'}`, so the wait-for-summaries effect matches on the first render. Rewrite the test to render with `summaries: []` (override the default) so it would deadlock on `SUBSCRIPTION_WAIT_TIMEOUT_MS` under the pre-Part-3 code (failing the Red phase) and pass under Part 3's synchronous-navigate branch (Green).
- **New test** — same `summaries: []` setup, additionally assert the "taking longer than expected" banner is **never** rendered (`queryByText` returns `null`, no fake timers needed since the dedup branch skips the timeout entirely).

No backend tests change. Backend dedup behavior is unchanged.

## Out of scope (Part 3)

- **Channel/botDM dedup** — channels never dedup at room-service today; the backend would have to detect e.g. duplicate channel names per requester, which is a different feature.
- **Refactor of the `pendingRoom` wait machinery** — the wait state is still correct for the genuine-create branch. We're skipping it on the dedup branch, not removing it.
- **Frontend type-correction on dedup** — if a user creates what they think is a botDM but the dedup reply points at an existing regular DM (or vice versa), the dialog hardcodes `'dm'`. The canonical subscription record corrects this downstream. A more thoughtful "preserve sync.roomType when provided" tweak is plausible but the backend doesn't return `roomType` on the dedup reply today, so it's a backend-coupled improvement deferred to a follow-up.

---

# Part 4 — Remove `Room.CreatedBy` and rework the replay-equivalence check

## Problem

`Room.CreatedBy` is persisted (`bson:"createdBy"`), on the wire (`json:"createdBy"`), and surfaced in `chat-frontend/src/api/types.ts:110`. The frontend never reads it (the only write site is `fetchSidebarBuckets/index.ts:135`, which writes `""`). It IS used in `room-worker/handler.go` replay-equivalence checks at `:1141-1147` (async create) and `:1543-1551` (sync DM create) as one of four immutable identity fields compared after a duplicate-key on `CreateRoom`.

The check is over-determined and incidentally breaks DM concurrent-creation. When users A and B simultaneously hit "Create DM" with each other (race past room-service's `FindDMSubscription` dedup), both workers compute the same deterministic room ID via `BuildDMRoomID`. Whichever wins the insert sets `CreatedBy = winnerID`; the loser hits duplicate-key, fetches the existing room, and the equivalence check fails with "room ID collision" because `existing.CreatedBy != room.CreatedBy` — even though the room IS the intended DM, just created a millisecond earlier by the counterpart.

## Design

Drop the field everywhere it appears (model, wire, frontend type, frontend write site, docs). Replace the comparison with a requester-subscription check that's strictly more correct.

### Worker rewrite

Both `processCreateRoom` (`:1130-1153`) and the sync-DM path (`:1535-1558`) call a new private helper to avoid duplicating the recovery logic:

```go
// reconcileRoomOnDuplicateKey verifies the existing room is structurally compatible and the requester is a member; one source of truth for both create paths.
func (h *Handler) reconcileRoomOnDuplicateKey(ctx context.Context, want *model.Room, requesterAccount string) (*model.Room, error) {
	existing, err := h.store.GetRoom(ctx, want.ID)
	if err != nil {
		return nil, fmt.Errorf("fetch on duplicate-key: %w", err)
	}
	if existing.Type != want.Type || existing.SiteID != want.SiteID {
		return nil, newPermanent("room ID collision (existing type=%s site=%s; want %s/%s)",
			existing.Type, existing.SiteID, want.Type, want.SiteID)
	}
	if _, err := h.store.GetSubscription(ctx, requesterAccount, want.ID); err != nil {
		if errors.Is(err, model.ErrSubscriptionNotFound) {
			return nil, newPermanent("room ID collision (requester %s not a member of existing room %s)",
				requesterAccount, want.ID)
		}
		return nil, fmt.Errorf("check requester sub on duplicate-key: %w", err)
	}
	return existing, nil
}
```

Each call site collapses to:

```go
if err := h.store.CreateRoom(ctx, room); err != nil {
	if !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("create room: %w", err)
	}
	existing, err := h.reconcileRoomOnDuplicateKey(ctx, room, requester.Account)
	if err != nil {
		return err
	}
	room = existing
}
```

The sync-DM path additionally preserves `acceptedAt = existing.CreatedAt` after the assignment — that's the only divergence and stays at the caller.

The `Name` comparison drops — it was load-bearing only as another identity check; the new `(Type, SiteID, requester-sub-exists)` triple is strictly more correct and decouples the equivalence check from future name-mutation flows.

### Removal sites

- `pkg/model/room.go` — drop `CreatedBy` field.
- `room-worker/handler.go` — remove `CreatedBy: requester.ID` from both `room := &model.Room{…}` literals; rewrite the duplicate-key blocks as above.
- `chat-frontend/src/api/types.ts:110` — drop `createdBy: string` from the `Room` type.
- `chat-frontend/src/api/fetchSidebarBuckets/index.ts:135` — drop the `createdBy: ''` write.
- `docs/client-api.md` — remove the row + example occurrences.

### Testing

Add an integration test for the previously-broken DM concurrent-create case in `room-worker/integration_test.go`: pre-insert a room with two pre-existing subs (counterpart already raced to create), then run `processCreateRoom` for the requester's request. Assert: no error, no duplicate sub, no duplicate room.

# Part 5 — Remove `target_user` Cassandra column

## Problem

`target_user FROZEN<"Participant">` exists in four Cassandra tables (`messages_by_room`, `messages_by_id`, `thread_messages_by_room`, `pinned_messages_by_room`) and as `Message.TargetUser *Participant` in `pkg/model/cassandra/message.go:81`. It's read by history-service via `baseColumns` in `internal/cassrepo/{messages_by_room,thread_messages}.go`. It's documented in `docs/client-api.md:939` and `docs/cassandra_message_model.md`.

**It is never written.** Greppable: `grep -rn "TargetUser:" /home/user/chat/` outside the struct declaration yields zero hits. All reads return NULL.

## Design

Remove the column and all references:
- `pkg/model/cassandra/message.go` — drop `TargetUser` field.
- `docker-local/cassandra/init/10-table-messages_by_room.cql`, `11-table-thread_messages_by_room.cql`, `12-table-pinned_messages_by_room.cql`, `13-table-messages_by_id.cql` — drop the column declaration.
- `history-service/internal/cassrepo/messages_by_room.go` + `thread_messages.go` — remove `target_user` from `baseColumns`.
- `docs/cassandra_message_model.md` — the source-of-truth schema (4 sections).
- `docs/client-api.md:939` — drop the row.

Production schema migration (`ALTER TABLE … DROP target_user`) is owned by ops/IaC; the dev init scripts ship the new schema for fresh setups. Reads against existing production tables continue to work — gocql ignores columns not declared in the struct.

# Part 6 — Phantom Org / User Request-Time Validation

## Problem

Both `chat.user.{account}.request.room.{roomID}.{siteID}.member.add` and `chat.user.{account}.request.rooms.create` (channel branch) accepted requests carrying org IDs or account names with no backing user document. The candidates aggregation (`GetAddMemberCandidatesPipeline` / `GetNewMembersPipeline`) is a join on `users`, so phantom inputs silently produced an empty candidate set — `CountNewMembers` returned 0 for those entries, the request published to the canonical stream, and the room-worker's `req.Orgs` loop (`room-worker/handler.go:891-898` and `:1319-1325`) wrote a `room_members` row plus fired a `members_added` system message for an org with zero backing users. Phantom user accounts were silently dropped at the candidates pipeline; the async-job reply still reported `success: true`.

## Design

Reject at the room-service request boundary so the synchronous RPC reply carries the error and nothing reaches the canonical stream. Two new store methods on `RoomStore`:

- `FindExistingOrgIDs(ctx, orgIDs []string) ([]string, error)` — returns the subset of `orgIDs` that match at least one user via `sectId` or `deptId`. Two parallel `Distinct` calls — one on each indexed field — keep the result bounded by `len(orgIDs)` and ride the existing `(sectId, account)` / `(deptId, account)` compound indexes.
- `FindExistingAccounts(ctx, accounts []string) ([]string, error)` — returns the subset of `accounts` that have a matching user document. Single `Distinct` on the indexed `account` field.

Both methods no-op (return `nil, nil`) on empty input, so the round trip is skipped when the request carries only the other dimension.

Two handler helpers (`validateOrgIDs`, `validateAccountsExist` in `room-service/handler.go`) call into the store and return wrapped sentinels:

- `validateOrgIDs` → `errInvalidOrg` (already in `sanitizeError`'s allow-list).
- `validateAccountsExist` → `errUserNotFound` (already in the allow-list).

The wrap includes the offending id (`fmt.Errorf("org %q: %w", id, errInvalidOrg)`) so logs identify the missing entry; the wire envelope still sanitizes down to the sentinel's `Error()`.

Both helpers are called immediately after the dedup step in `handleAddMembers` (after channel-ref expansion) and `handleCreateRoomChannel`, before `CountNewMembers` and `publishToStream`. Capacity / restricted-channel / bot-rejection checks remain ahead of these (cheaper, no DB call).

## Why the gate lives at the room-service boundary

The room-worker can't return a synchronous error to the client — by the time it sees the event, the room-service has already replied `accepted`. Async-job results (`chat.user.{requesterAccount}.response.{requestID}`) deliver the error, but only when the client opted in by setting `X-Request-ID` and only after the worker reaches the validation point. Pushing the check upstream gives every client an immediate, in-line error envelope and prevents the worker from ever writing the bogus `room_members` row.

## Callers / paths touched

- `room-service/store.go` — two new interface methods, generated mock regenerated.
- `room-service/store_mongo.go` — `Distinct`-based implementations.
- `room-service/handler.go` — `validateOrgIDs` + `validateAccountsExist` helpers, called from `handleAddMembers:709` and `handleCreateRoomChannel:296` after `allOrgs`/`allUsers` dedup.
- `docs/client-api.md` — Add Members and Create Room error-response sections updated to document `"invalid org"` and `"user not found"` synchronous rejections.

## Testing

Unit (`room-service/handler_test.go`):
- `TestHandler_AddMembers_PhantomOrgRejected` — single phantom org, no publish, `errInvalidOrg`.
- `TestHandler_AddMembers_PartiallyInvalidOrgRejected` — mixed valid + phantom; the whole request rejects.
- `TestHandler_AddMembers_NoOrgsSkipsOrgValidation` — gomock fails if `FindExistingOrgIDs` is called for a users-only request.
- `TestHandler_AddMembers_PhantomUserRejected` — `errUserNotFound`.
- `TestHandler_AddMembers_NoUsersSkipsUserValidation` — symmetric guard against the unnecessary round trip.
- 12 existing happy-path tests gain an `expectAllAccountsExist(store)` helper expectation so the validation gate is satisfied.

Integration (`room-service/integration_test.go`):
- `TestMongoStore_FindExistingOrgIDs_Integration` — sectId + deptId set union, all-phantom, empty-input, dept-only invariant.
- `TestMongoStore_FindExistingAccounts_Integration` — matching subset, all-phantom, empty-input.

## Out of scope

- Worker-side defense-in-depth (a second validation pass in `room-worker` once the event arrives). The single request-time gate is sufficient; the worker trusts validated canonical events, as it does everywhere else.
- Org/user existence checks for the `Channels` ref (cross-site bulk-source). Channel expansion already pulls members from `room_members`, which can only carry rows for users that previously existed. The validation gate runs on the resulting `allUsers` set anyway, so a malformed remote source would still surface as `errUserNotFound`.
- Cross-site federation behavior: an account that exists on a remote site but not locally will reject with `errUserNotFound`. This is intentional under the current single-site `users` collection model; multi-site user replication is a separate problem.

# Part 7 — PR #171 (room-encryption-keys) Follow-up Findings

Two review threads left unresolved on PR #171 after merge. The PR is on `main` now (this branch rebased onto it), so the follow-ups land here.

## Finding 1 — `buildAndFanOutRoomKey` re-fetches the key at `room-worker/handler.go:1789`

**Reviewer comment (`@mliu33`):**

> One optimization is to pass room key down so that buildAndFanout does not need to make db call to fetch again.

**Decision: accept, under a caller-owns-the-fetch contract.**

**Reply to post on the thread:**

> Good catch on the create path — `processCreateRoom` already has the pair in scope from the gate-Get at `handler.go:1188` and discards it before `buildAndFanOutRoomKey` re-fetches at `handler.go:1793`. Refactored `buildAndFanOutRoomKey` to take `*VersionedKeyPair` as a parameter and threaded it through `finishCreateRoom`. Saves one Valkey round trip per channel/DM create.
>
> The `processAddMembers` path doesn't have the pair in scope; rather than two ways to invoke (with/without pair) I kept the contract uniform — caller always fetches, function always receives. Net: −1 Valkey Get on create, 0 change on add. The nil check stays inside `buildAndFanOutRoomKey` as a defensive guard so a future caller bug surfaces as a permanent error instead of a panic.

**Why the caller-owns-fetch pattern is safe against concurrent rotation:**

The theoretical race is: create's gate-Get (sees v=0) → concurrent `member.remove` on the same room rotates Valkey to v=1 → create's fan-out runs with the stashed v=0. `member.remove` can only be authorized once the room exists in Mongo, which only happens after `processCreateRoom` commits the room insert, well past the gate-Get. Within a single `processCreateRoom` invocation the pair is stable.

**Code change:**

1. Change `buildAndFanOutRoomKey` signature to take `pair *roomkeystore.VersionedKeyPair`. Keep the nil check and the permanent-absent error path inside as a defensive guard.
2. Thread `pair` through `finishCreateRoom` (new parameter) and pass through from both `processCreateRoom` call sites.
3. `processAddMembers`: add a `keyStore.Get` immediately before the fan-out call (unchanged Valkey round-trip count for this path).
4. Update `TestBuildAndFanOutRoomKey_SendsToAllMembersIncludingRemoteSite` to pass the pair directly and drop the `keyStore.Get` expectation.

## Finding 2 — success counters at `room-worker/handler.go:347, 356, 362`

**Reviewer comment (`@mliu33`):**

> In my opinion, I think there is not much benefit to track success rotate/generate key metrics. It's good to track error count, but not so much for success count as adding metric also costs some cpu time.

**Decision: drop them. Counter `Add` is cheap, but the team's stated convention is errors-only and we follow it.**

**Reply to post on the thread:**

> Agreed — dropped `KeyGenerated` and `KeyRotated` at every emit site (`room-worker/handler.go:347, 356, 362` and `room-service/handler.go:369`) and removed the counter declarations + `init()` registrations from `pkg/roomkeymetrics/metrics.go`. Denominator for error-rate alerting can come from JetStream consumer ack rate or upstream remove/create handler counts. Error counters (`FanoutErrors`, `ValkeyErrors`, `KeyAbsentErrors`) stay.

**Code change:** delete the four `KeyGenerated.Add` / `KeyRotated.Add` call sites and the two counter declarations + their `init()` registration blocks.
