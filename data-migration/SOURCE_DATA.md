# Source data shape (legacy RocketChat)

## 1. Source document (`rocketchat_message`)

Example document (values rotated/sanitized):

```js
{
    _id: 'aB3kD9fH2jL5mN7pQ',
    t: 'au',
    u: {
        _id: 'xY8wV2tR6sU4qP0nM',
        username: 'p_demoadmin'
    },
    ts: ISODate('2024-01-15T09:00:00.000Z'),
    msg: 'p_newmember',
    _updatedAt: ISODate('2024-01-15T09:05:00.000Z'),
    unread: true,
    rid: 'rM4nB7vC2xZ9kJ5hG',
    groupable: false,
    federation: {
        origin: 'site-a.example.internal'
    }
}
```

### `t` — message type

| `t` | Meaning |
|---|---|
| `null` (absent) | Normal message |
| `room_changed_avatar` | Avatar changed |
| `room_changed_description` | Description changed |
| `room_changed_name` | Name changed |
| `room_changed_privacy` | Privacy changed |
| `user_added` | User added |
| `user_removed` | User removed |
| `user_left` | User left |
| `msg_encrypted` | Encrypted (requires decryption) |
| `subscription_role` | Role changed |

### Key fields

| Field | Notes |
|---|---|
| `_id` | PK; used for deduplication (always present) |
| `msg` | Meaningful when `t` is null; localized system text when `t` is set |
| `ts` | Insert timestamp — ordering, immutable |
| `_updatedAt` | Modification timestamp — edit detection |
| `federation.origin` | Remote server origin (if federated) |
| `unread` | Legacy flag — do not rely on (use receipts / group mentions) |

## 2. Change event structure

### 2A. Common fields

| Field | Type | Notes |
|---|---|---|
| `_id` | ObjectId | Event id (not the doc id). Used for `resumeAfter`. Not the msg id. |
| `operationType` | str | `insert` \| `update` \| `delete` \| `replace` \| `drop` \| `rename` \| `dropDatabase` \| `invalidate` |
| `clusterTime` | Timestamp | Monotonic; different values = distinct ops |
| `ns.db` / `ns.coll` | str | DB / collection names |
| `wallTime` | Date | Applied wall-clock time |
| `documentKey` | `{_id:<val>}` | Affected doc's `_id` (RC msg id) |
| `txnNumber` (Long) / `txnId` (UUID) | — | Present when inside a transaction |
| `userName` | str | Authenticated user that executed the op |

### 2B. Insert event
- **Insert-only:** `fullDocument` — the complete, newly-inserted doc.
- **Never:** `updateDescription`, `previousDocument`, `fullDocumentBeforeChange`.

### 2C. Update event
- `updateDescription` — **always**. The diff: `updatedFields` (set/modified), `removedFields` (deleted paths), `truncatedArrays` (`$pop`/`$slice`).
- `fullDocument` — only with `updateLookup`/`whenAvailable`. The post-update doc (re-read; may reflect concurrent writes).
- `previousDocument` — only with `showExpandedEvents:true` (5.0+). The pre-update doc.

### 2D. Delete event
- `fullDocument` — **never** (absent; cannot re-read).
- `previousDocument` — only with `showExpandedEvents:true` (5.0+). The pre-deletion doc — **critical for deleted content**. Without it, only `documentKey._id` is available; no way to reconstruct `msg`.

### 2E. Replace event
- `updateDescription` — absent (no field-level diff).
- `fullDocument` — the new version (with the `fullDocument` option).
- `previousDocument` — the old version (with `showExpandedEvents:true`).

---

# Collections migration — source schema (assumptions, for source-engineer cross-check)

> This section is the migration team's **current understanding** of the operational
> source collections read by the collections path (`oplog-collections-transformer`,
> design: `docs/superpowers/specs/2026-06-16-oplog-transformer-collections-design.md`).
> **Every "Assumed" row drives a write into the new system — please correct anything wrong.**
> Legend: ✅ confirmed by source team · ❓ assumption awaiting confirmation · ⛔ deliberately ignored.

## Conventions assumed across these collections
- ✅ **`federation.origin`** is authoritative. **Absent** ⇒ record is **local**. **Present** ⇒ a federated peer domain whose **first dotted label is the site code** (`0030204.tchat-test.test.company.com` ⇒ `0030204`).
- ❓ `federation.origin` is never the literal `"local"` (we treat absent ⇒ local).
- ✅ Each site's source DB already holds its **federated copies**; we migrate the full local source with **no** drop-filter.

## 3. `rocketchat_rooms`

| Source field | Type | Interpretation | Status |
|---|---|---|---|
| `_id` | string | Room id | ✅ |
| `t` | string | Room type — **only** `c`,`p`,`d`,`l`,`v` exist | ✅ |
| `prid` | string (opt) | Parent room id — **present ⇒ discussion** (`t` is `p`) | ✅ |
| `teamId` | string (opt) | Room belongs to a Team | ✅ |
| `teamMain` | bool (opt) | True only on a team's **primary** room | ✅ |
| `name` | string | Machine/handle name | ❓ |
| `fname` | string | Friendly display name | ❓ |
| `uids` / `usernames` | array | Members; for `t:d` length **can exceed 2** (group DM) | ✅ |
| `u` | object | Creator (`u._id`, `u.username`) | ❓ |
| `ts` / `_updatedAt` | date | Created / last-updated | ❓ |
| `restricted` | bool (opt) | **Authoritative restriction flag** (TSMC custom; absent ⇒ false). Confirmed on TKMS. RC's `ro` (read-only/announcement) is a **different concept** — deliberately ignored | ✅ |
| **external/federation access** | ? | **Which field is authoritative for "external access allowed"?** | ❓ |
| `federation.origin` | string (opt) | Origin site | ✅ |
| `federation.domains[]` | array | Member domains, service-synced, may be stale | ✅ ⛔ |

Type mapping logic to sanity-check: `c`/`p` (no `prid`) → one channel type (no public/private split); `p`+`prid` → discussion; `d` (2 participants) → dm (botDM if a participant is a bot); `d` (>2) → **skip** (no group DM); `l`/`v` → **skip**; team rooms → plain channel (`teamId`/`teamMain` dropped).

## 4. `rocketchat_subscriptions`

One row per (user, room). ✅ Unique index `{ rid:1, 'u._id':1 }`.

| Source field | Type | Interpretation | Status |
|---|---|---|---|
| `u._id`, `u.username` | string | Member identity | ✅ |
| `rid` | string | Room id | ✅ |
| `open` | bool | **Membership active.** Leave ⇒ `open:false` (no delete); re-join ⇒ true | ✅ |
| `ts` | date | Join time (set once, stable across re-joins) | ✅ |
| `roles[]` | string[] | `owner`/`moderator`/`leader`/`user` (role-based ownership) | ✅ |
| `ls` | date | Last **seen** (scrolled cursor) | ✅ |
| `lr` | date | Last **read** (explicit mark) | ✅ |
| `alert` | bool | True if **any** unread content (not just mentions) | ✅ |
| `userMentions` / `groupMentions` | int | Unread `@user` / `@all`,`@here` counts | ✅ |
| `tunread[]` | string[] | Parent-message ids (`tmid`) of threads with any unread | ✅ |
| `tunreadGroup[]` / `tunreadUser[]` | string[] | …group-mention / direct-mention variants | ✅ |
| `disableNotifications` | bool | **TSMC custom — authoritative mute (all-off)** | ✅ |
| `muteGroupMentions` | bool | `@all`/`@here` only (**not** our mute flag) | ✅ |
| `f` | bool (opt) | Favorited (absent ⇒ false) | ✅ |
| `favoritedAt` | date (opt) | Last favorite time. Exists at source (TKMS) but **unused by CDC** — per the agreed guard mapping below, all guards derive from `_updatedAt` | ✅ ⛔ |
| `name` / `fname` | string | Machine name / friendly display name | ✅ |
| `federation.origin` | string (opt) | Origin site (assumed consistent with room) | ✅ ❓ |

Derived: "has mention" = `userMentions>0 || groupMentions>0`; "muted" = `disableNotifications`; **read timestamp (`lastSeenAt`) = `max(ls, lr)`** (resolved per design D1 — the furthest point consumed by either the scrolled cursor or the explicit mark-read).

**Guard timestamps (agreed with source team, supersedes the earlier conditional/null mapping):** every
destination high-water guard derives uniformly from the source **`_updatedAt`** — `rolesUpdatedAt`,
`muteUpdatedAt`, `favoriteUpdatedAt` (subscriptions) and `nameUpdatedAt`, restricted-guard (rooms).
No null-when-false conditional, no `favoritedAt` source. Note: the canonical restricted guard field
(`restrictedUpdatedAt`) is not in the destination codebase yet; inbox-worker currently applies
`room_restricted` via `visibilityUpdatedAt` — accepted until the rename lands in main.

## 5. `tsmc_thread_subscriptions`

One row per (user, thread). ✅ Unique index `{ 'u._id':1, 'parentMessage._id':1 }`.

| Source field | Type | Interpretation | Status |
|---|---|---|---|
| `_id` | string | Row id | ✅ |
| `u._id`, `u.username` | string | Follower identity | ✅ |
| `rid` | string | Room id (matches parent room) | ✅ |
| `parentMessage._id` | string | Thread root message id (`tmid`) — the thread key | ✅ |
| `lastMessage._id` / `._updatedAt` | string/date | Last message in thread | ✅ |
| `createdAt` | date | Row creation (lazy — on follow/first reply) | ✅ |
| `lastSeenAt` | date | Last-read timestamp for the thread | ✅ |
| `unreadMention` | int | Thread mention/unread count | ✅ |

Lifecycle: created lazily; **unfollow deletes the row** (no soft-delete); no `federation.origin` (site inherited from room/user). **Open:** please share a redacted sample doc to confirm nothing is missed.

## 6. `users`

| Source field | Type | Interpretation | Status |
|---|---|---|---|
| `_id` | string (17-char base62) | Stable user id | ✅ |
| `username` | string | **Account id — unique but mutable** | ✅ |
| `type` | string | `user` or `bot` (bot has `appId`); no other non-human types | ✅ |
| `appId` | string (opt) | Present on bot/app accounts | ✅ |
| `name` | string | Display name | ✅ |
| `customFields.engName` / `tsmcName` | string | English / Chinese name | ✅ |
| `customFields.deptId` / `deptName` | string | Department id / name | ✅ |
| `customFields.sectId` / `sectName` | string | Section id / name | ✅ |
| `customFields.appId` / `appName` | string | App id / name | ✅ |
| `hrInfo` | `ITsmcUser[]` | HR directory records | ❓ (not consumed yet) |
| `statusText` / `status` | string | Status message / presence | ✅ |
| `roles[]` | string[] | Global roles (`admin` marker) | ✅ |
| `active` | bool | Deactivation ⇒ `active:false` (no deletion) | ✅ |
| `isRemote` | bool | True on local docs of **federated** users | ✅ |
| `federation.origin` | string (opt) | Origin site (absent ⇒ local) | ✅ |
| **employee id** | ? | **Where does an employee id live — is it `username`?** | ❓ |
| **Traditional-Chinese dept/sect names** | ? | Is there a TC variant of `deptName`/`sectName`? | ❓ |

Seeded (insert-if-absent, keyed by account): `username`, `engName`, `tsmcName`, dept/sect ids+names, `roles`, `statusText`, site, bot flag. Everything else is owned by the company-wide user sync.

Post-seed **updates**: HR fields are **not** re-propagated (the company-wide sync keeps them current). The **one exception is `statusText`** — it is chat-originated (not in the HR dataset), so a live `statusText` change fans a `user_status_updated` event to all sites (design §4.1a); without it, legacy status changes during the migration window would be lost.

## Explicitly **not** migrated
`federation.domains[]`; livechat (`l`) / voip (`v`) rooms; group DMs (`d`>2); team grouping (`teamId`/`teamMain`); user deactivation/deletion; thread-sub unfollows during cutover. Flag any of these you'd expect to matter.
