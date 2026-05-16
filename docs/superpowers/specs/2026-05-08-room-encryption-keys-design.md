# Room Encryption Keys — Design Spec

**Date:** 2026-05-08
**Status:** Shipped (Sprint 0 + Sprint 1)
**Branch:** `claude/room-encryption-keys-5vlQ2`

> **Post-review amendment (2026-05-14):** Cross-site key replication has been
> removed. A room only ever exists on its origin site, so the broadcast
> pipeline runs there and reads the key from the origin's local Valkey only.
> `inbox-worker` no longer calls `chat.server.request.roomkey.{siteID}.get`,
> no longer holds a Valkey/`RoomKeyStore` dependency, and only replicates
> subscription/room metadata. Cross-site clients still receive `RoomKeyEvent`
> directly from origin `room-worker`'s user-subject fan-out, routed by the
> NATS supercluster. Sections below that describe the old replication flow
> are retained for historical context; the actual code matches this
> amendment, not the original spec.

> **Post-review amendment (2026-05-15):** Key rotation on member-remove has
> moved from `room-service` to `room-worker`, with the order changed to
> **delete → fan-out new key → rotate Valkey → publish system message**. This
> closes the previous survivor-decrypt window (broadcast-worker no longer
> starts encrypting under v+1 before survivors have v+1). `room-service`
> stamps the current Valkey version as `RemoveMemberRequest.BaseKeyVersion`;
> `room-worker` uses it as a skip-rotation guard against JetStream
> redeliveries of an already-rotated event. New flow diagram and residual
> risks in the **Remove-member rotation flow (post-review)** section below.
> The pre-amendment "rotate before publish in room-service" description in
> the Scope and Architecture sections is retained for historical context.

## Remove-member rotation flow (post-review)

### Sequence

```
Client ──┐
         │ RemoveMember
         ▼
   room-service
         │  validate (room type, owner, last-member, dual-membership reject)
         │  keyStore.Get(roomID) → current version v
         │  publish canonical{ accounts, baseKeyVersion=v }
         │
         ▼
  ROOMS stream
         │  (chat.room.canonical.{site}.member.remove)
         └─────────► room-worker (member_removed handler)
                        1. keyStore.Get(roomID) → currentPair
                        2. shouldRotate := currentPair.Version <= req.BaseKeyVersion
                        3. DeleteSubscription(s) + DeleteRoomMember + ReconcileMemberCounts
                        4. if shouldRotate && something_actually_deleted:
                              a. survivors := ListByRoom(roomID)        # post-deletion
                              b. newPair := roomkeystore.GenerateKeyPair()
                              c. fanOutRoomKeyToSurvivors(newPair, version=v+1)  # ── chat.user.*.event.room.key
                              d. keyStore.Rotate(roomID, newPair)        # broadcast-worker now uses v+1
                        5. publish system message → MESSAGES_CANONICAL
                                                  ── broadcast-worker fans out, encrypted with v+1

  broadcast-worker (concurrent, processes message-create events):
         1. ListByRoom(Mongo) → addressing
         2. keyStore.Get(Valkey) → current key (v or v+1, monotonic)
         3. encrypt + fan out to current subs
```

Rationale for the order:
- **Delete before rotate:** once the subscription is gone, broadcast-worker won't address the removed user — even with the old key still active.
- **Fan-out before rotate:** survivors hold v+1 *before* broadcast-worker switches; eliminates the survivor decrypt-failure window of the pre-amendment design.
- **System message last:** encrypted under v+1, decryptable by every survivor since they've all received v+1.

What `actually_deleted` means in step 4:
- **Individual remove:** skipped when the target has dual membership (`user.HasOrgMembership` is true) — the user remains in the room via their org sub, so no real subscription was deleted, so no rotation.
- **Org remove:** skipped when every targeted org member is also individually subscribed — the org-membership row is deleted but `DeleteSubscriptionsByAccounts` runs with an empty `accounts` slice (`len(accounts) > 0` gate).

### Residual risks

1. **Removed-user-read window (~10–100ms):** between room-service publishing the canonical event and room-worker reaching step 3 (Mongo delete), the removed user is still in `subscriptions` AND Valkey still holds v. Concurrent messages from other room members in that window are addressed to the removed user and encrypted under v, which they can decrypt with the key they hold. Accepted as a documented limitation; closing it would require synchronous subscription delete in room-service (rejected by ownership boundaries) or a fence flag broadcast-worker checks (rejected for hot-path cost).

2. **Key-gen non-idempotence on JetStream redelivery:** step 4b regenerates fresh ECDSA bytes on every redelivery. The skip-rotation guard (step 2) catches the common case (crash after step 4d, ack lost); does not catch crash between 4b and 4d (re-gen with different bytes, partial caches diverge). Recoverable through a future client-side refetch-on-decrypt-failure RPC; not blocking on this PR. Noise scope is bounded: stale cached keys are dead weight, not security regressions.

## Summary

Wires the existing `pkg/roomkeystore` (Valkey-backed key storage) and `pkg/roomkeysender` (NATS key delivery) libraries into the room lifecycle. After this spec ships, every room has a P-256 key pair generated at create time and pushed to every member's NATS subject so clients can decrypt messages encrypted by `broadcast-worker`. Removing a channel member rotates the key so the removed user can no longer decrypt messages sent after their removal.

The current state of the codebase has the libraries built and tested, but no service writes keys yet — `broadcast-worker` reads keys that nothing produces. This spec closes that loop.

## Implementation status

**Shipped — Sprint 0:** `pkg/roomkeystore`, `pkg/roomkeysender`, `pkg/roomcrypto` and their unit + integration test suites; `broadcast-worker` Valkey wiring.

**Shipped — Sprint 1:** All items in the Scope section below. Additionally:
- `pkg/roomkeymetrics` OTel instruments (see Operational addendum).
- `pkg/roomkeystore/doc.go` package documentation covering versioning, concurrency, and single-master topology.
- Operational addendum in this spec (ops guide folded in from a now-deleted separate doc).
- `otelutil.InitMeter` wired in `room-worker` and `inbox-worker` with shutdown hooks.
- `ROOM_KEY_RPC_TIMEOUT` configurable via env var (default 5 s), exposed in `inbox-worker/deploy/docker-compose.yml`.
- Sentinel errors `ErrRoomKeyNotFound` and `ErrRoomKeyStoreInternal` exported from `room-worker/handler.go`.

**Deferred (known follow-up items):**
- Item 4: fatal/best-effort policy reconciliation across all fan-out call sites.
- Item 7: version-guard on `inbox-worker` redelivery path.
- Item 24: circuit breaker around inter-site RPC.
- Items 23, 25, 26, 27 from the review backlog.

## Scope

In scope:

- **Create-room** (all room types: `dm`, `botDM`, `channel`): `room-service` generates a P-256 key pair, writes it to local Valkey via `keyStore.Set`, then publishes the canonical create event. `room-worker` reads the key back from Valkey and gates its Mongo writes on the key being present, then fans out `RoomKeyEvent` to every initial member via `roomkeysender`.
- **Add-member** (channel only — DM/botDM blocked at `room-service`): worker reads the current key from local Valkey and fans out `RoomKeyEvent` to each newly-added account. No rotation; no version bump. Add-member does NOT create a key for un-keyed rooms — backfill behavior deferred to a follow-up.
- **Remove-member** (channel only — DM/botDM blocked at `room-service`): `room-service` rotates the room key via `keyStore.Rotate` after validation passes, **unless** the target has both individual and org membership (dual-membership), in which case rotation is skipped because the user remains in the room via their org membership. `room-worker` performs Mongo deletes, then fans out the new `RoomKeyEvent` to every surviving subscriber via `fanOutRoomKeyToSurvivors`. A single rotation per `RemoveMemberRequest` for non-dual-membership cases, regardless of org-vs-individual or removed-count. **Superseded by 2026-05-15 amendment:** rotation has moved from `room-service` to `room-worker`; `room-service` only stamps `BaseKeyVersion` from a `Get`. See "Remove-member rotation flow (post-review)" at the top of this doc.
- **Cross-site replication** (channels only — DM/botDM never spans sites except via the existing federated DM creation path which falls under create-room above): origin's `room-worker` publishes the existing outbox events (`room_created`, `member_added`, `member_removed`) without keypair bytes — and *also* publishes `RoomKeyEvent` to **every** room member's user subject (`chat.user.{account}.event.room.key`) so the NATS supercluster delivers the key to clients across sites. Remote `inbox-worker` instances replicate only subscription and room metadata; they do not hold a copy of the room key. The broadcast pipeline for any given room runs on the origin site (where the room lives), so only the origin's Valkey is consulted at encrypt time. Pre-amendment versions of this spec described a remote-Valkey replication path via `chat.server.request.roomkey.{originSiteID}.get`; that path has been removed.
- **Defensive room-type guards** in `room-worker` for the add/remove paths. `RemoveMemberRequest` now carries a `RoomType` field (`pkg/model/member.go`). The worker reads it from the canonical event directly and asserts `room.Type == model.RoomTypeChannel`. As a backward-compatibility gate, an empty `RoomType` value is tolerated (federation redeliveries from pre-Batch-3 senders). A non-empty, non-channel `RoomType` fails as a permanent error (treated as a malformed canonical event since `room-service` is responsible for blocking these). For `processAddMembers`, `GetRoom` is still called for other reasons; the type guard on the add path continues to use that result.

Out of scope:

- A pull-on-demand RPC for clients reconnecting or resyncing key state. Clients rely on push for now; missed pushes will be addressed in a follow-up.
- Key regeneration after Valkey data loss on the origin site. If a flush occurs, all rooms hosted on that site enter a degraded state — the worker logs structured errors and emits `AsyncJobResult` errors back to clients on subsequent operations. Operational recovery (e.g., bulk regenerate) is a separate spec.
- Encryption-toggle interaction. Key generation, rotation, and distribution are **gated on `VALKEY_ADDR` being configured**, not on the per-service `ENCRYPTION_ENABLED` flags in `broadcast-worker` and `history-service`. This mirrors the pattern at `broadcast-worker/main.go:80-97`: the Valkey wiring is conditional on configuration presence, but once Valkey is wired, key operations run regardless of consumer-side encryption toggles. Concretely: deployments that set `VALKEY_ADDR` get full key management even if `broadcast-worker.ENCRYPTION_ENABLED=false` (so flipping the consumer toggle on later does not require a key backfill); deployments that leave `VALKEY_ADDR` empty (e.g. early-stage dev) skip key handling entirely — `room-service` does not Set/Rotate, worker handlers skip the `Get` gate and fan-out branches, and `inbox-worker` does not RPC origin. The cost of always-on key management on enabled-but-unencrypted deployments is one Valkey HSET per create / per remove — negligible.
- Frontend changes for displaying or selecting key versions. Clients are expected to maintain a `{version → privateKey}` map internally; spec'd in `docs/client-api.md` updates only.
- Backfill of keys for rooms that already exist when this spec ships. Pre-existing rooms have no key; a separate migration tool will generate keys for them.
- Backfill of pre-existing rooms after Valkey loss (out-of-band recovery tooling is a separate spec).
- Vector-clock or multi-rotator versioning. The scalar int version gate works because `room-service` is the sole origin of all rotations — see "Single-rotator invariant" in the Error Handling section.
- Multi-rotator support. The Lua rotate script and scalar version comparison assume a single Valkey master per site; distributing rotation authority across multiple writers would require a different versioning scheme.
- Per-room NATS subject fan-out optimization (currently each `Send` publishes to `chat.user.{account}.event.room.key` individually).

## Architecture & Data Flow

### Create-room (all room types)

```text
Client
  │ chat.user.{account}.request.room.{siteID}.create
  ▼
room-service
  1. Validate (existing flow: capacity, dedup, etc.)
  2. roomID = idgen.GenerateID() | BuildDMRoomID(...)
  3. pair = generateRoomKeyPair()                       ← new
  4. keyStore.Set(ctx, roomID, pair)                    ← new
  5. publishToStream(chat.room.canonical.{site}.create, req)
  6. Reply CreateRoomReply{accepted, roomID}

room-worker (origin site)
  7. keyStore.Get(roomID) → must return pair            ← new gate
       nil → AsyncJobResult{error:"room key missing"}, ack
  8. Mongo writes: room, subscriptions, room_members
  9. For EVERY initial member account (local + remote):
        roomkeysender.Send(account, RoomKeyEvent)       ← new
        NATS supercluster routes user-subjects to home sites
 10. For each remote site with members:
        publish outbox.{site}.to.{dest}.room_created
        (existing model.RoomCreatedOutbox payload at pkg/model/event.go:228 —
         no key bytes added)
 11. Sys-message + per-user events (existing)

inbox-worker (each remote site)
 12. handleRoomCreated: write replicated subs (existing)
 13. fetchAndStoreKey: RPC chat.server.request.roomkey.{originSite}.get {roomID}  ← new
        ↓ reply: model.RoomKeyEvent (RoomID, Version, PublicKey, PrivateKey)
 14. keyStore.SetWithVersion(roomID, pair, fetched.Version) on local Valkey  ← new
     (no Send — origin room-worker already published to every member via supercluster)
```

### Add-member (channel only)

```text
room-service
  1. Validate (existing add-member checks; rejects DM/botDM)
  2. publishToStream(chat.room.canonical.{site}.member.add, req)

room-worker (origin)
  3. Defensive check: req implies channel context; reject permanently if not.
  4. Mongo writes (existing)
  5. keyStore.Get(roomID) → versionedPair               ← new
       nil → permanent error + AsyncJobResult error
  6. For EVERY newly-added account (local + remote):
        roomkeysender.Send(account, RoomKeyEvent)       ← new
        NATS supercluster routes user-subjects to home sites
  7. Outbox member_added to remote sites (existing payload)

inbox-worker (each remote site receiving new members)
  8. Replicate subs (existing)
  9. replicateLocalKey: local keyStore.Get(roomID) hit → no-op (already present);
     miss → fetchAndStoreKey: RPC origin + keyStore.SetWithVersion at fetched version  ← new
     (no Send — origin room-worker already published via supercluster)
```

A remote site that already has members of this room will already have the key locally from the create-time replication; a cache hit is a no-op. A remote site receiving its **first** member of a room takes the RPC + SetWithVersion path.

### Remove-member (channel only)

> **Stale — superseded by 2026-05-15 amendment.** The flow below describes the
> original rotate-in-`room-service` design. The shipped flow has rotation in
> `room-worker` with order delete → fan-out → rotate → publish, and `inbox-worker`
> no longer fetches keys from origin. Authoritative version: "Remove-member
> rotation flow (post-review)" at the top of this doc.

```text
room-service
  1. Validate (existing: authz, last-owner guard, last-member guard, org-only guard,
              roomType=channel guard)
  2. newPair = generateRoomKeyPair()
  3. newVer = keyStore.Rotate(roomID, newPair) → returns int  ← new
  4. publishToStream(chat.room.canonical.{site}.member.remove,
                     req with NewKeyVersion=newVer)
  5. Reply (accepted)

room-worker (origin)
  6. Defensive roomType=channel guard (reads req.RoomType; empty tolerated for federation).
  7. keyStore.Get(roomID): assert version >= req.NewKeyVersion ← new
       nil or stale → transient error (NAK + retry); NOT permanent
  8. Mongo deletes (dual-membership logic; see "Dual-membership skip-rotation" note)
  9. For EVERY surviving subscriber (all sites, via ListByRoom(roomID, "")):
        roomkeysender.Send(account, RoomKeyEvent with new pair)   ← new
        NATS supercluster routes user-subjects to home sites
 10. Outbox member_removed to remote sites
        (existing payload + NewKeyVersion)
 11. Sys-message + per-user events (existing)

inbox-worker (each remote site with surviving members)
 12. Delete listed subscriptions (existing)
 13. fetchAndStoreKey: RPC chat.server.request.roomkey.{originSite}.get {roomID} ← new
       ↓ reply: model.RoomKeyEvent (carries the new pair + version)
       failure → NAK (fatal on this path, not best-effort)
 14. keyStore.SetWithVersion(roomID, fetchedPair, fetched.Version) on local Valkey  ← new
       (origin's version is adopted exactly; no local rotate — broadcast-worker
        on this site will encrypt envelopes with the version every client holds)
     (no Send — origin room-worker already published to all survivors via supercluster)
```

### Why rotate-first (in `room-service`) rather than rotate-after (in worker post-Mongo-delete)

> **Stale — superseded by 2026-05-15 amendment.** Rotation now happens in
> `room-worker` after the Mongo delete, with fan-out before Valkey rotate so
> survivors hold v+1 before broadcast-worker switches. The rationale below is
> retained for historical context only.

Rotating before Mongo deletes guarantees that from the moment of rotation, `broadcast-worker` encrypts under the new public key, and the about-to-be-removed user — who only holds the old private key — cannot decrypt any message published after the rotation. That's the security property rotation exists for. Rotate-after (worker-side) would leave a window where the removed user could still decrypt new messages until the worker finished. Worse posture.

The downside of rotate-first is that if the worker fails permanently (rare), the room is briefly unusable for everyone (encrypted under a key whose distribution to surviving members never completed). JetStream redelivery makes the window short; on a true permanent failure the `AsyncJobResult` error tells the requester to retry, and a retry generates a fresh rotation that completes cleanly.

## New & Changed Code

### New: cross-site key RPC handler in `room-worker`

> **Stale — removed by 2026-05-14 amendment.** The cross-site key RPC has been
> deleted: `NatsHandleGetRoomKey`, `subject.ServerRoomKeyGet`, and the
> `RoomKeyGetRequest` payload no longer exist. Rooms only live on their origin
> site, so broadcast reads from the origin's local Valkey and no remote site
> needs to fetch keys. This entire subsection is retained for historical context.

Subject: `chat.server.request.roomkey.{siteID}.get` — server-to-server, NKey-authed via the existing inter-site server connection.

Request payload:

```go
type RoomKeyGetRequest struct {
    RoomID string `json:"roomId"`
}
```

Reply payload (success): `model.RoomKeyEvent` already defined in `pkg/model/event.go` is reused — `RoomID`, `Version`, `PublicKey`, `PrivateKey`, and the existing `Timestamp` field are exactly the data needed. Note this struct serves a dual role: as a fan-out event payload (`Timestamp` set by `roomkeysender.Send` at publish time, see `pkg/roomkeysender/roomkeysender.go`) and as the RPC reply payload here (`Timestamp` set by the RPC handler at reply time). Both producers stamp the field; consumers ignore it for any logic.

Reply payload (error): `model.ErrorResponse` via `natsutil.ReplyError`.

The handler exposes two sentinel errors for callers to branch on via `errors.Is`:

```go
var (
    ErrRoomKeyNotFound      = errors.New("room key not found")
    ErrRoomKeyStoreInternal = errors.New("room key store internal error")
)
```

The public `NatsHandleGetRoomKey` method delegates to an internal `handleGetRoomKey(ctx, roomID)` that returns `*model.RoomKeyEvent` or one of the sentinels above. `NatsHandleGetRoomKey` extracts the request ID and tracing context from NATS headers via `natsutil.ContextWithRequestIDFromHeaders` before dispatching.

The handler is registered in `room-worker/main.go` alongside the existing canonical consumer subscription.

### Unchanged: `pkg/model/event.go` `RoomKeyEvent`

`RoomKeyEvent` is already correctly shaped — `RoomID`, `Version`, `PublicKey`, `PrivateKey`, and `Timestamp` are all in place today (`pkg/model/event.go:162-168`). `roomkeysender.Send` already stamps `Timestamp` at publish time. **No change required here.**

The new RPC reply handler (`NatsHandleGetRoomKey`) reuses this struct verbatim and explicitly sets `Timestamp` at reply time so consumers see a non-zero value on every wire form.

### Changed: `pkg/model/room.go`

```go
type RemoveMemberRequest struct {
    // ... existing fields ...
    NewKeyVersion int `json:"newKeyVersion" bson:"newKeyVersion"`  // NEW
}

type MemberRemoveEvent struct {
    // ... existing fields ...
    NewKeyVersion int `json:"newKeyVersion" bson:"newKeyVersion"`  // NEW
}
```

`AddMembersRequest` and `MemberAddEvent` are unchanged — the worker reads the current version directly from Valkey at fan-out time.

### Changed: `pkg/subject/subject.go`

```go
func ServerRoomKeyGet(siteID string) string {
    return fmt.Sprintf("chat.server.request.roomkey.%s.get", siteID)
}
```

### Changed: `room-service`

- `Config` keeps existing `VALKEY_ADDR`, `VALKEY_PASSWORD`, `VALKEY_KEY_GRACE_PERIOD` (already wired).
- Extend the consumer-side `RoomKeyStore` interface in `room-service/store.go`:

  ```go
  type RoomKeyStore interface {
      GetMany(ctx context.Context, roomIDs []string) (map[string]*roomkeystore.VersionedKeyPair, error)
      Set(ctx context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error)        // NEW
      Rotate(ctx context.Context, roomID string, newPair roomkeystore.RoomKeyPair) (int, error)  // NEW
  }
  ```

- New helper `generateRoomKeyPair() (roomkeystore.RoomKeyPair, error)` in `room-service/keygen.go` using `crypto/ecdh.P256().GenerateKey(rand.Reader)`. Lives only in `room-service` since neither `room-worker` nor `inbox-worker` generate keys.
- `handleCreateRoom` calls `generateRoomKeyPair` + `keyStore.Set` between roomID assignment (`req.RoomID = idgen.GenerateID()` at `handler.go:323`) and `publishToStream`.
- `handleRemoveMember` calls `generateRoomKeyPair` then `keyStore.Rotate` after validation passes, sets `req.NewKeyVersion` from the returned int, and publishes.
  - **Pre-existing-room compatibility:** `Rotate` returns `roomkeystore.ErrNoCurrentKey` (`pkg/roomkeystore/roomkeystore.go:14`) when no current key exists in Valkey — the case for any channel created before this spec ships (per the "no backfill" out-of-scope statement). On `errors.Is(err, roomkeystore.ErrNoCurrentKey)`, the service falls back to `keyStore.Set(ctx, roomID, newPair)`, which writes the pair as version `0`. `req.NewKeyVersion` is set to `0` in that branch. The worker's version assertion (`>= req.NewKeyVersion`) still holds. Surviving members receive the new key as if the room had just been freshly keyed. This makes the remove-member flow safe to deploy without a separate backfill step.
- All errors from `Set` / `Rotate` abort the request and are surfaced to the client via the existing `ReplyError` path. No canonical event is published on failure.

### Changed: `room-worker`

- New deps: `pkg/roomkeystore` (read-only `Get`), `pkg/roomkeysender` (`Send`), `pkg/roomkeymetrics`.
- `Config` adds:

  ```go
  ValkeyAddr           string        `env:"VALKEY_ADDR,required"`
  ValkeyPassword       string        `env:"VALKEY_PASSWORD" envDefault:""`
  ValkeyKeyGracePeriod time.Duration `env:"VALKEY_KEY_GRACE_PERIOD" envDefault:"24h"`
  ```

  `VALKEY_ADDR` is required: `room-worker` refuses to start without it.

- New consumer-side interface in `room-worker/store.go`:

  ```go
  type RoomKeyStore interface {
      Get(ctx context.Context, roomID string) (*roomkeystore.VersionedKeyPair, error)
  }
  ```

  `SubscriptionStore.ListByRoom` now takes a `siteID` parameter: `ListByRoom(ctx context.Context, roomID, siteID string) ([]model.Subscription, error)`.

- `Handler` gains `keyStore RoomKeyStore`, `keySender *roomkeysender.Sender`. Constructor signature updated; tests pass mocks. Existing mock store regenerated via `make generate` (pre-existing tests in `room-worker/handler_test.go` need their `NewHandler` calls updated to pass the new dependencies).
- Branches added to `processCreateRoom`, `processAddMembers`, `processRemoveMember` exactly as in the flow diagrams.
- `buildAndFanOutRoomKey` (previously sketched as `fanOutRoomKey`) fetches the current key from Valkey, builds the `RoomKeyEvent`, and fans it out to every local-site account in the provided `[]*model.User` slice. Used by create-room and add-member paths.
- `fanOutRoomKeyToSurvivors` fans out a pre-fetched `*roomkeystore.VersionedKeyPair` to a pre-computed `[]model.Subscription` survivors slice. Used by remove-member paths. Callers do the `ListByRoom(roomID, siteID)` call themselves before invoking it.
- `pkg/roomkeysender.Sender.Send` accepts `model.RoomKeyEvent` by **value** (not pointer). The caller's struct is never mutated.
- New RPC handler `NatsHandleGetRoomKey` registered in `main.go` via `nc.QueueSubscribe(subject.ServerRoomKeyGet(cfg.SiteID), "room-worker", handler)` — same queue-group convention as the existing inter-site handlers.
- Defensive `roomType == channel` guard on the remove-member path reads `req.RoomType` directly from the canonical event (empty value tolerated for federation backward-compat). On a non-empty, non-channel value, return a permanent error. Create path accepts all room types.
- `otelutil.InitMeter("room-worker")` wired in `main.go`; shutdown hook registered alongside the tracer shutdown.
- Stale-version gate in `processRemoveMember` (`pair == nil || pair.Version < req.NewKeyVersion`) returns a **transient** error (NAK + JetStream retry), not a permanent one. This is the correct posture: stale-key means Valkey hasn't yet propagated the write, not that the event is malformed.

### Changed: `room-service` `RoomKeyStore` interface — mock regen required

The extended interface (`Set`, `Rotate`) is a breaking change to `room-service/store.go:93-95`. Existing tests in `room-service/handler_test.go` instantiate the generated `MockRoomKeyStore`; after `make generate` they will compile but tests that invoke create-room or remove-member without setting `EXPECT().Set(...)` / `EXPECT().Rotate(...)` will fail at the mock's strict-call expectations. Each affected test gets new expectations as part of the corresponding TDD step.

### Changed: `inbox-worker`

- New deps: `pkg/roomkeystore`, `pkg/roomkeysender`, `pkg/roomkeymetrics`, plus the inter-site key client (a thin wrapper around `nc.RequestMsgWithContext`).
- `Config` adds the same Valkey block as `room-worker`, plus:

  ```go
  ValkeyAddr           string        `env:"VALKEY_ADDR,required"`
  RoomKeyRPCTimeout    time.Duration `env:"ROOM_KEY_RPC_TIMEOUT" envDefault:"5s"`
  RoomKeyMaxRedeliver  int           `env:"ROOM_KEY_MAX_REDELIVER" envDefault:"10"`
  ```

  `VALKEY_ADDR` is required: `inbox-worker` refuses to start without it.

- New consumer-defined interface in `inbox-worker/store.go` (per `CLAUDE.md` Section 3, "Define interfaces in the consumer, not the implementer"):

  ```go
  type InterSiteKeyClient interface {
      GetRoomKey(ctx context.Context, originSiteID, roomID string) (*model.RoomKeyEvent, error)
  }
  ```

  Production implementation: `natsInterSiteKeyClient` in `inbox-worker/intersite_key.go`. It builds the request via `natsutil.NewMsg` (which propagates the `X-Request-ID` header into NATS message headers) and calls `nc.RequestMsgWithContext`. Note: `natsutil.NewMsg` propagates `X-Request-ID` only — it does not propagate W3C tracing headers. If `room-worker` ever needs the same client (it currently does not), extract to `pkg/intersitekey/` then. YAGNI until then.

- `InboxStore.ListByRoom` takes a `siteID` parameter pushed down to Mongo: `ListByRoom(ctx context.Context, roomID, siteID string) ([]model.Subscription, error)`.

- `handleRoomCreated` extended: after sub writes succeed, calls `fetchAndStoreKey` which RPCs the origin and replicates the key into local Valkey via `SetWithVersion` at origin's version. No user-event fan-out from inbox-worker — origin `room-worker` already published `RoomKeyEvent` to every member via the NATS supercluster.
- `handleMemberAdded` extended: `replicateLocalKey` path (local hit → no-op; miss → RPC + SetWithVersion). Local Valkey Get failure returns error (caller NAKs). No fan-out from inbox-worker.
- `handleMemberRemoved` extended: after sub deletes, calls `fetchAndStoreKey` to pull the rotated key from origin and write it into local Valkey at origin's version. RPC or write failure returns error (caller NAKs — this path is fatal, not best-effort). No fan-out from inbox-worker — origin already published to survivors.

- `replicateLocalKey` returns an error on Valkey Get failure (NAK + retry), and now also surfaces `errKeyDepsMissing` when the handler was constructed without Valkey wiring so a miswired worker fails loudly instead of silently Acking key-bearing outbox events.
- `fetchAndStoreKey` compares the fetched origin version to the local stored version and writes via `SetWithVersion` only when origin is strictly newer. Redelivered events never re-rotate or bump the local version independently of origin.

**Sequential consumer caveat.** `inbox-worker` uses `cons.Consume` for sequential processing. Per `CLAUDE.md` Section 6 ("Match the pattern already used by the service being modified"), this spec preserves sequential processing. Each new cross-site RPC adds a synchronous round-trip per inbox event, serialized behind the single Consume callback. Acceptable at the project's current event rate; if rate-limit issues surface, a follow-up spec can introduce bounded concurrency inside the handler. Documented here so the implementer doesn't silently switch to `cons.Messages`.

### File layout (additions only)

```text
room-service/
  keygen.go                    — generateRoomKeyPair helper
  keygen_test.go               — TDD tests

room-worker/
  (handler.go modifications, main.go RPC registration — no new files;
   the worker is a server, not a client of the cross-site RPC)

inbox-worker/
  intersite_key.go             — InterSiteKeyClient interface + nats-backed impl
  intersite_key_test.go
```

## Error Handling & Failure Modes

| Failure | Where | Handling |
|---|---|---|
| `keyStore.Set` fails on create | `room-service` | Return error to client (no canonical published) |
| `keyStore.Rotate` fails on remove | `room-service` | Return error to client (no canonical published) |
| `keyStore.Get` returns `nil` in worker | `room-worker` | Permanent error, `AsyncJobResult{error:"room key missing"}`, ack message |
| `keyStore.Get` version stale on remove | `room-worker` | **Transient** error (NAK + retry). Stale means Valkey propagation hasn't caught up yet, not a malformed event. The single-rotator invariant guarantees the version will eventually be present. |
| `keyStore.Get` returns transient error | `room-worker` | NAK, JetStream redelivers |
| RPC to origin times out (create / add) | `inbox-worker` | NAK, JetStream redelivers with backoff. Subs already replicated; key fan-out deferred. |
| RPC fails on member-remove path | `inbox-worker` | NAK (fatal). The rotate-and-fan-out step is not best-effort on the remove path; survivors must receive the new key. |
| RPC returns 404 (origin Valkey lost key) | `inbox-worker` | Log structured error (roomID, originSiteID), ack. Room exists on remote without key — operational alarm. |
| `roomkeysender.Send` fails for a single account | `room-worker` / `inbox-worker` | Log structured error per-account, continue iterating (best-effort). The current key remains in Valkey; clients can pull on next reconnect once the future pull RPC ships. |
| Valkey Get fails in `replicateOrSendLocalKey` | `inbox-worker` | Returns error (caller NAKs). Previously this was logged and silently fell through to RPC — now correctly surfaces transient Valkey errors for retry. |
| Removed user reconnects before grace expiry | — | See "Removed user semantics" below. |
| Orphan key in Valkey (room-service writes, then crashes pre-publish) | — | Tolerated. No Mongo state exists. The `roomID` is unique enough that reuse is astronomically unlikely; for DM IDs the next legitimate create simply overwrites. |
| Valkey total flush | All services | Subsequent worker `Get` calls return `nil` → permanent errors propagate to clients. Out-of-band recovery via the deferred regeneration tool. |
| Remove-member retry after partial failure | `room-service` | Each retry of an interrupted remove generates a fresh `Rotate` (and a new key version). The previous (interrupted) rotation's key sits in Valkey's `:prev` slot until grace expiry — `broadcast-worker` may still serve `GetByVersion` for messages encrypted under it. This is fine: the retry's new version becomes current and is fanned out to survivors; the abandoned intermediate version naturally ages out. No deduplication on `RequestID` is required for correctness. |

## Removed User Semantics

After a user is removed from a channel:

- Their auth-service JWT continues to allow subscription to `chat.user.{theirAccount}.>` (no auth change is part of this spec). They simply receive no further `RoomKeyEvent`s for the affected room because the worker iterations exclude them.
- Any old key versions they previously received remain valid for decrypting messages encrypted under those versions (i.e., messages sent before their removal). Clients are expected to retain old `(roomID, version) → privateKey` entries to support history scrolling.
- Messages encrypted by `broadcast-worker` after the rotation use the new public key; the removed user has no access to the corresponding new private key and cannot decrypt them.

## Single-rotator invariant

The version-gate in `room-worker.processRemoveMember` compares the scalar `pair.Version` against `req.NewKeyVersion` using a plain integer comparison (`pair.Version < req.NewKeyVersion`). This works because **only `room-service` originates key rotations**. No other service calls `keyStore.Rotate` on the origin Valkey. Therefore version numbers form a strictly monotone sequence, and a scalar `>=` check is sufficient to determine freshness. If multiple services could rotate keys (multi-rotator topology), a vector clock or external sequence would be required instead — see "Out of scope" above.

## Operational addendum

For package-level documentation covering versioning, concurrency guarantees, and topology requirements, see `pkg/roomkeystore/doc.go`.

### Fan-out ownership summary

| Service | Role |
|---|---|
| `room-service` | Generates keys on room create (`Set`) and rotates on member-remove (`Rotate`, with `Set` fallback on `ErrNoCurrentKey`). The single rotator in the system; downstream version comparisons depend on this. |
| `room-worker` (origin) | Reads the current key from origin Valkey on create / member-add / member-remove and fans out `RoomKeyEvent` to **every room member** (local + remote) via `roomkeysender.Send`. NATS supercluster routes `chat.user.{account}.event.*` subjects to home sites. Also serves the inter-site `chat.server.request.roomkey.{siteID}.get` RPC. |
| `inbox-worker` (remote site) | Pulls the current key from origin via the RPC and replicates it into local Valkey at the origin's exact version (`SetWithVersion`). Does **not** fan out user events — origin `room-worker` already did that. |
| `broadcast-worker` | Reads the current key from local Valkey to encrypt outgoing messages. Requires `VALKEY_ADDR` and `ENCRYPTION_ENABLED=true`. |

### Service interplay

| Service | VALKEY_ADDR | Behavior |
|---|---|---|
| `room-service` | required | Always wires key generation/rotation on create / remove |
| `room-worker` | required | Key gate + fan-out to all members; refuses to start without `VALKEY_ADDR` |
| `inbox-worker` | required | Cross-site key replication into local Valkey; refuses to start without `VALKEY_ADDR` |
| `broadcast-worker` | required when `ENCRYPTION_ENABLED=true` | Encrypts outgoing room messages using current key |
| `history-service` | required when its encryption toggle is true | Encrypts message history on edit |

`VALKEY_ADDR` graduated from "optional with `slog.Warn` at startup" to a hard requirement on the workers. The earlier "key handling disabled" mode produced a silent split-brain — rooms were created and members removed in Mongo while no key flowed to clients — and (once cross-site versions had to match origin's exactly) made any cross-site message undecryptable. Failing loudly at startup matches what the system actually requires.

`ENCRYPTION_ENABLED` is a consumer-side toggle in `broadcast-worker` and `history-service`.
It does NOT control whether keys are generated — keys are always generated when the
producer side (`room-service` + workers) is wired to Valkey. This lets operators
flip on encryption later without a key backfill.

### Partial deployments

`room-worker` and `inbox-worker` refuse to start without `VALKEY_ADDR`, so a
worker process either has Valkey wiring or it isn't running. There is no
"silent skip" mode anymore — earlier drafts let workers degrade gracefully,
which masked misconfigurations and (after origin-version replication landed)
produced cross-site messages no client could decrypt. Operators see the
startup failure immediately.

### Valkey data loss

If Valkey is wiped, the next operation on a previously-keyed room will return
`ErrRoomKeyNotFound` (room-worker) or fail the rotate-with-Set-fallback (inbox-worker).
Recovery requires regenerating keys. There is no recovery tool yet — see the day-2
ops backlog.

### Single-master Valkey

This system requires a single-master Valkey deployment per site. The atomic rotate
operation uses a single Lua script and does not function across Redis Cluster slots.
See `pkg/roomkeystore/doc.go` for details.

### Metrics exported by `pkg/roomkeymetrics`

| Instrument | Go name | Type | Description |
|---|---|---|---|
| `room_key_fanout_errors_total` | `FanoutErrors` | `Int64Counter` | Incremented on every `roomkeysender.Send` failure (room-worker fan-out) |
| `room_key_rpc_duration_seconds` | `RPCDuration` | `Float64Histogram` | Wraps `natsInterSiteKeyClient.GetRoomKey` round-trip latency (inbox-worker) |
| `room_key_generated_total` | `KeyGenerated` | `Int64Counter` | `room-service` `Set` success (new key generated at create / first remove) |
| `room_key_rotated_total` | `KeyRotated` | `Int64Counter` | `room-service` `Rotate`/Set-fallback success (remove-member path) |
| `room_key_valkey_errors_total` | `ValkeyErrors` | `Int64Counter` | Valkey operation failures; tagged by `op` attribute (`Get`/`Set`/`Rotate`/`GetMany`) |

All instruments are initialised in `pkg/roomkeymetrics/metrics.go` and fall back to no-op counters/histograms if the global meter provider is not yet set at init time. `otelutil.InitMeter("<service>")` is wired in `room-worker/main.go` and `inbox-worker/main.go` with shutdown hooks registered before the `shutdown.Wait` call.

Available on the OpenTelemetry meter once a meter provider is registered.

## Operational Requirements

- **Valkey persistence must be enabled** (AOF or RDB). A non-persistent Valkey loses every room's key on restart and forces every active room into the "key missing" permanent-error path.
- **Each site's services point at the same site-local Valkey master.** Async-replicated replicas would break read-after-write consistency for the worker's `Get` gate. The `Config.Addr` is a single endpoint by design.
- **NKey-authed inter-site server connection** must allow `chat.server.request.roomkey.>` between sites. Existing inter-site server requests already use this connection class.

## Configuration

| Service | New env vars | Existing |
|---|---|---|
| `room-service` | (none) | `VALKEY_ADDR`, `VALKEY_PASSWORD`, `VALKEY_KEY_GRACE_PERIOD` |
| `room-worker` | `VALKEY_ADDR`, `VALKEY_PASSWORD`, `VALKEY_KEY_GRACE_PERIOD` | — |
| `inbox-worker` | `VALKEY_ADDR`, `VALKEY_PASSWORD`, `VALKEY_KEY_GRACE_PERIOD`, `ROOM_KEY_RPC_TIMEOUT` (default `5s`), `ROOM_KEY_MAX_REDELIVER` (default `10`) | — |

`docker-local/docker-compose.yml` and each affected service's `deploy/docker-compose.yml` get updated to provide these vars. The local Valkey container is already present (used by `room-service` and `broadcast-worker`).

## Testing

### Unit Tests (TDD: red → green → refactor → commit)

`room-service/handler_test.go` (new test cases):

- create-room generates and `Set`s key before publishing; verify call order with mock `RoomKeyStore`
- create-room: `Set` failure aborts; no canonical event published; client receives error
- remove-member calls `Rotate`; `req.NewKeyVersion` populated from returned int
- remove-member: `Rotate` failure aborts; no canonical event published
- DM/botDM remove path remains blocked at validation (existing behavior preserved)

`room-service/keygen_test.go` (new file):

- `generateRoomKeyPair` returns 65-byte public + 32-byte private
- Two calls produce distinct keys
- Round-trip: encode + decrypt with `pkg/roomcrypto` succeeds

`room-worker/handler_test.go` (new test cases):

- create-room: `keyStore.Get` returning `nil` → permanent error + AsyncJobResult; no Mongo writes attempted
- create-room: `Get` succeeds → Mongo writes proceed → `Send` called once per expanded member account
- create-room: `Send` failure on one account logged but doesn't abort the loop
- add-member (channel): `Get` succeeds → `Send` called for each newly-added account, not for existing members
- add-member: defensive guard rejects non-channel `roomType` as permanent error
- remove-member: `Get` returning version `< NewKeyVersion` → **transient** error (NAK + retry). Stale-version means Valkey propagation hasn't yet caught up to the room-service write; JetStream redelivery resolves it. Not permanent.
- remove-member: `Send` called for survivors, never for removed accounts
- remove-member: defensive guard rejects non-channel
- `NatsHandleGetRoomKey`: returns `RoomKeyEvent` on hit, 404 on miss, 500 on Valkey error

`inbox-worker/handler_test.go` (new test cases):

- `handleRoomCreated`: replicates subs → calls `interSiteClient.GetRoomKey` → `SetWithVersion` on local Valkey at origin's version (no user-side `Send` — origin `room-worker` already published).
- `handleMemberAdded`: local key present → no-op. Local key absent → RPC + `SetWithVersion`.
- `handleMemberRemoved`: deletes subs → RPC origin → `SetWithVersion` on local Valkey at origin's new version.
- Duplicate JetStream delivery → version comparison short-circuits; no re-write or version bump on the local Valkey.
- RPC failure → NAK path.
- RPC 404 → wrapped with `errRoomKeyAbsent` sentinel so callers can distinguish from transient RPC errors.

Mocks generated via `mockgen` for: `RoomKeyStore`, `roomkeysender.Publisher`, `InterSiteKeyClient`. Stored in `mock_*_test.go` files per project convention; `make generate` updated accordingly.

### Integration Tests (`//go:build integration`)

- `room-service` + `room-worker` + Valkey container: full create flow exercises real Valkey `Set` and `Get` ; verify `RoomKeyEvent` published to expected `chat.user.{account}.event.room.key` subjects (capture via test NATS subscription).
- Two-site test: spin up two `room-worker` instances each with their own Valkey, simulate `inbox-worker` calling the cross-site RPC, verify replication populates the second Valkey and surviving members receive their key after a rotation.
- Round-trip: `roomcrypto.Encode` with the published `PublicKey` decrypts cleanly with the published `PrivateKey` — exercises the full produce-key + send-event + decrypt loop.

### Coverage Targets

≥ 80% for changed packages, ≥ 90% for new code per project rules. Specifically:

- `room-service/keygen.go` — 100% (small, deterministic)
- New worker branches in `processCreateRoom` / `processAddMembers` / `processRemoveMember` — each error path covered
- `NatsHandleGetRoomKey` — all three reply paths covered
- `inbox-worker` extended handlers — all three RPC outcomes (success, transient error, 404) covered

## Client API Documentation

Per project rule, `docs/client-api.md` must be updated in this PR. New subsection: **Room Encryption Keys**.

Required content:

- Clients subscribe to `chat.user.{account}.event.room.key` (already covered by `chat.user.{theirUsername}.>` permissions).
- Payload: `RoomKeyEvent{roomId, version, publicKey, privateKey, timestamp}`. Keys are base64-encoded JSON byte arrays.
- Required client behavior: maintain a `{(roomId, version) → privateKey}` map. Decrypt incoming `EncryptedMessage` (which carries its `version`) by looking up the matching private key.
- Behavior on rotation: a new `RoomKeyEvent` with an incremented `version` arrives; clients add it to the map and retain the previous version for at least the configured grace period (Valkey-side TTL on `:prev` is the upper bound for what the server can decrypt; clients can keep older keys longer if they want history access).
- Removed members: stop receiving `RoomKeyEvent`s for the affected room. Their existing keys can still decrypt old messages but cannot decrypt anything published after their removal.

## Workflow & Commit Plan

Per the project's TDD rule, work is broken into small red-green-refactor commits. Suggested sequence:

1. Add `NewKeyVersion` to `RemoveMemberRequest` + `MemberRemoveEvent` + model tests. Note these are two separate fields with separate roles: `RemoveMemberRequest.NewKeyVersion` is the canonical-event payload (drives the worker's local version assertion); `MemberRemoveEvent.NewKeyVersion` is the federation/outbox payload (drives the remote inbox-worker's local rotate). Both are populated by `room-service` from the same `Rotate` return value, then propagated through their respective channels.
2. Add `subject.ServerRoomKeyGet`.
3. `room-service`: extend `RoomKeyStore` interface; regenerate mocks; update existing affected handler tests with the new `EXPECT().Set(...)` / `EXPECT().Rotate(...)` calls so the test suite stays green at this commit.
4. `room-service`: `generateRoomKeyPair` helper + tests.
5. `room-service`: wire `Set` into `handleCreateRoom`; tests for happy + abort paths.
6. `room-service`: wire `Rotate` into `handleRemoveMember` (with `ErrNoCurrentKey` → `Set` fallback for pre-existing rooms); tests.
7. `room-worker`: add Valkey wiring (Config, Connect, store interface) gated on `cfg.ValkeyAddr != ""`.
8. `room-worker`: gate `processCreateRoom` on `Get`; integrate `roomkeysender`; tests.
9. `room-worker`: extend `processAddMembers` with key fan-out + roomType guard (reusing the existing `GetRoom` call); tests.
10. `room-worker`: extend `processRemoveMember` with `GetRoom` + roomType guard + version assertion + fan-out; tests.
11. `room-worker`: implement `NatsHandleGetRoomKey` + register in `main.go` with `"room-worker"` queue group; tests.
12. `inbox-worker`: add Valkey + sender + inter-site client wiring (gated on `cfg.ValkeyAddr != ""`).
13. `inbox-worker`: extend `handleRoomCreated` with RPC + `Set` + fan-out; tests.
14. `inbox-worker`: extend `handleMemberAdded` and `handleMemberRemoved`; tests.
15. Integration tests: two-site cross-site replication + round-trip decrypt.
16. Update `docs/client-api.md`, each affected `deploy/docker-compose.yml`, and `docker-local/docker-compose.yml`.

Each commit is gated by `make lint` + `make test` per the existing pre-commit hook.
