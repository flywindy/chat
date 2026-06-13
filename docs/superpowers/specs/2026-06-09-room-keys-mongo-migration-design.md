# Room Keys: Valkey → MongoDB rooms collection — Design Spec

**Date:** 2026-06-09
**Status:** Shipped
**Scope:** `pkg/roomkeystore`, `room-service`, `room-worker`, `broadcast-worker`, `tools/loadgen`, `tools/seed-sample-data`
**Supersedes (storage layer):** `2026-03-30-valkey-room-key-library-design.md`, `2026-03-30-room-key-rotation-design.md`, and the storage sections of `2026-05-08-room-encryption-keys-design.md`

## Overview

Move the room encryption key out of Valkey and into the room's own document in
the MongoDB `rooms` collection, under an `encKey` sub-document. MongoDB becomes
the single source of truth for room keys. The standalone Valkey-backed
`roomkeystore` implementation is removed; the `RoomKeyStore` interface is
unchanged so consumers swap only their constructor.

## Motivation

- **One source of truth, one datastore.** Room metadata already lives in the
  `rooms` collection in MongoDB. Keeping the key in a *separate* store (Valkey)
  meant a room's key and its metadata could drift, had independent lifecycles,
  and required every key-consuming service to also run a Valkey connection.
- **Drop a dependency for key storage.** room-service, room-worker, and
  broadcast-worker used Valkey *only* for room keys. Moving keys to MongoDB —
  which all three already connect to — removes Valkey from those services
  entirely. (Valkey remains in the system for its other uses: the search-service
  restricted-rooms cache, notification-worker's `roomsubcache`, etc.)
- **Lifecycle coupling is correct.** A room key has no meaning without its room.
  Embedding it makes "no room ⇒ no key" a structural invariant and lets the key
  appear/disappear atomically with the room document.

## Data Model

The key is a BSON sub-document at `encKey` inside the room document. It is
**never** part of `model.Room` (no `json`/`bson` tags on `Room`), so it can
never be serialized to clients and is never clobbered by full-document room
writes (`UpdateOne {$set: room}` only touches `model.Room` fields).

```text
rooms/{_id}:
  ...room metadata (model.Room)...
  encKey:
    priv:          <binary, 32-byte AES-256-GCM secret>   # current key
    ver:           <int>                                    # current version
    prevPriv:      <binary, 32-byte>   # previous key (set by Rotate)
    prevVer:       <int>
    prevExpiresAt: <date>              # grace-window expiry for the previous key
```

The 32-byte secret is the same material `pkg/roomcrypto` uses directly as the
AES-256-GCM key (no derivation). The legacy P-256 `pub` field is gone.

### Previous-key grace window

Valkey expired the previous-key slot with a per-key TTL. MongoDB has no
per-field TTL (a TTL index would delete the whole room document), so the grace
window is represented by an **explicit `prevExpiresAt` timestamp**. Reads
(`GetByVersion`) ignore the previous slot once `now >= prevExpiresAt`. This is
fully deterministic and testable via an injectable clock.

## Interface & Behaviour

The `roomkeystore.RoomKeyStore` interface is unchanged. Constructor:

```go
func NewMongoStore(col *mongo.Collection, gracePeriod time.Duration) RoomKeyStore
```

`col` is the `rooms` collection; the underlying mongo client is owned by the
caller, so `Close()` is a no-op.

| Method | MongoDB operation |
|---|---|
| `Set` | `$set encKey.priv,encKey.ver=0`. Returns `ErrRoomNotFound` if no room document matched. Does not touch the previous slot. |
| `SetWithVersion` | `$set encKey.priv,encKey.ver=<v>`. Returns `ErrRoomNotFound` if absent. Rotate-fallback only. |
| `Get` | Projected read of `encKey`; `(nil, nil)` when the room or its key is absent. |
| `GetMany` | `_id $in […]` projected read; rooms without a key are omitted. |
| `GetByVersion` | Current slot, or previous slot while `now < prevExpiresAt`; else `(nil, nil)`. |
| `Rotate` | Single atomic aggregation-pipeline `FindOneAndUpdate`: demote current→previous (stamp `prevExpiresAt = now + grace`), `ver = ver + 1`, install new current. `ErrNoCurrentKey` when no current key exists. |
| `Delete` | `$unset encKey`. No-op when absent. |

`Rotate` runs as one pipeline update against a single document, so the
demote-and-bump is atomic — no concurrent reader sees a partially-rotated key
(replacing the Valkey Lua script's atomicity).

### New error: `ErrRoomNotFound`

Because a key cannot exist without its room, `Set`/`SetWithVersion` now return
`roomkeystore.ErrRoomNotFound` when no room document matches, instead of
silently succeeding (a `$set` on a missing document is a no-op).

## Create-flow change (the key reordering)

Previously, **room-service** generated and `Set` the key *before* publishing the
canonical create event, so room-worker's "key must exist" gate would pass. With
the key now a field of the room document, that ordering is impossible — the room
document does not exist until room-worker inserts it.

New flow:

1. **room-service** no longer provisions a key on create. It only *reads* keys
   (batch room info, `getRoomKey`, `ensureRoomKey`).
2. **room-worker** inserts the room document, then calls `ensureRoomKey`:
   `Get`; if absent, generate a fresh v0 key and `Set` it; reuse the existing
   key on a JetStream redelivery (so clients holding it keep decrypting). The
   resulting key is then fanned out to members exactly as before.

The old hard gate (`permanent("room key absent")` before any Mongo write) is
removed — the worker now *provisions* the key instead of *requiring* it.

The synchronous DM path (`serverCreateDM`/`createSelfDM`) is unchanged: it never
provisioned a key and still relies on lazy provisioning via `ensureRoomKey`
(see `2026-06-02-room-key-fetch-on-missing-design.md`). `ErrRoomNotFound` cannot
fire on these because the room document exists before any key write.

## Affected services & config

| Service | Change |
|---|---|
| room-service | `NewMongoStore(db.Collection("rooms"), grace)`; removed create-time `Set`. |
| room-worker | `NewMongoStore(...)`; `ensureRoomKey` after `CreateRoom`; gate removed. |
| broadcast-worker | `NewMongoStore(...)`; PR #191's in-process TTL cache retained, wrapping the new store. |

Config: `VALKEY_ADDRS` / `VALKEY_PASSWORD` / `VALKEY_KEY_GRACE_PERIOD` are
removed from these three services and replaced by `ROOM_KEY_GRACE_PERIOD`
(default `24h`). `docker-compose.yml` env updated accordingly.

Observability: `roomkeymetrics.ValkeyErrors` (`room_key_valkey_errors_total`) is
renamed to `StoreErrors` (`room_key_store_errors_total`).

## Federation

Unchanged and reaffirmed: room keys are **site-local**. A room exists only on its
origin site, whose broadcast pipeline reads the key from that site's local
`rooms` collection. inbox-worker replicates room/subscription metadata across
sites but never room keys.

## Testing

- **Unit:** pure-function tests for the `encKey` decode and the
  current/previous-version selection (including grace expiry) via an injectable
  clock. New room-worker handler tests cover create-time key provisioning
  (absent ⇒ generate+`Set`; present ⇒ reuse, no `Set`) and the `Get`-error path.
- **Integration (`//go:build integration`, testcontainers MongoDB):** round-trip,
  `ErrRoomNotFound`, `SetWithVersion`, missing-key, rotate round-trip,
  grace-period expiry (advanced via injected clock, no sleep), rotate-with-no-key,
  delete, and `GetMany`. The Valkey `CLUSTER KEYSLOT` slot-consistency test is
  removed (no longer applicable).
