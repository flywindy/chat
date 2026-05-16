# Room Encryption Keys Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Post-review amendment (2026-05-14):** Cross-site key replication has
> been removed from the shipped implementation. A room only ever exists on
> its origin site, so the broadcast pipeline runs there and reads the key
> from the origin's local Valkey only. `inbox-worker` no longer holds a
> Valkey/`RoomKeyStore` dependency and only replicates subscription/room
> metadata; the `chat.server.request.roomkey.{siteID}.get` RPC is no
> longer called. Tasks below that describe inter-site key fetching are
> obsolete — the corresponding code has been deleted. The
> `RoomKeyMaxRedeliver` cap is also gone (it existed solely to bound the
> NAK-loop on that RPC).

> **Post-review amendment (2026-05-15):** Key rotation on member-remove has
> moved from `room-service` to `room-worker`. Room creation key generation
> stays in `room-service` (sync failure surface). On remove, `room-service`
> validates and stamps the current Valkey version as
> `RemoveMemberRequest.BaseKeyVersion`; `room-worker` does
> **delete → fan-out new key → rotate Valkey → publish system message**.
> The new order eliminates the survivor decrypt-failure window present in
> the pre-amendment design. Authoritative flow + residual risks live in
> the spec's *Remove-member rotation flow (post-review)* section.

### Remove-member flow (post-review, authoritative)

```
Client ──► room-service ──► ROOMS stream       ──► room-worker
              │ validate            (member_removed)        │ Get(roomID) → currentPair
              │ Get(roomID) → v                             │ shouldRotate := currentPair.Version <= req.BaseKeyVersion
              │ publish{ baseKeyVersion=v }                 │ Delete sub + reconcile counts
              ▼                                             │ if shouldRotate && actually_deleted:
                                                            │     survivors := ListByRoom (post-delete)
                                                            │     newPair := roomkeystore.GenerateKeyPair()
                                                            │     fanOutRoomKeyToSurvivors(newPair, v+1)
                                                            │     keyStore.Rotate(newPair)
                                                            │ publish system message
                                                            ▼
                                                       broadcast-worker fans out, encrypted with v+1
```

Residual risks (accepted, documented in spec):
1. Removed-user-read window (~10–100ms) between canonical publish and room-worker's Mongo delete — concurrent messages encrypted under v reach the still-listed removed user.
2. Key-gen non-idempotence on JetStream redelivery between fan-out and Rotate — partial caches diverge. Recoverable through a future client-side refetch-on-decrypt-failure RPC.

`actually_deleted` (rotation skip cases): individual remove with `user.HasOrgMembership` (user stays via org sub); org remove where every targeted account is also individually subscribed (`accounts` slice ends up empty, `len(accounts) > 0` gate fails).

**Goal:** Wire room encryption keys end-to-end across `room-service`, `room-worker`, and `inbox-worker`. After this plan ships, every newly-created room has a P-256 keypair stored in Valkey, channel `member.remove` rotates the key, and channel `member.add` distributes the current key to new members. Cross-site clients receive `RoomKeyEvent` directly from the origin `room-worker`'s user-subject fan-out, routed by the NATS supercluster — there is no server-side key replication.

**Architecture:** `room-service` generates the room key at create and stamps the pre-rotation Valkey version on remove. `room-worker` (origin) owns rotation: it deletes the subscription, fans out the new `RoomKeyEvent` to post-deletion survivors via `roomkeysender.Send`, then commits via `keyStore.Rotate`. `inbox-worker` on remote sites replicates subscription and room metadata only; it does not hold or replicate the room key.

> **Implementation drift — read before following any task literally.** The
> sections below were written in TDD-style as the design evolved. The
> shipped implementation diverges in a few places that matter; the
> authoritative descriptions live in the spec and the code. Notable drifts:
>
> 1. **`VALKEY_ADDR` is a hard startup requirement on every worker** that
>    touches keys (`room-service`, `room-worker`, `inbox-worker`) —
>    `env:"VALKEY_ADDR,required"`. Snippets that show
>    `if cfg.ValkeyAddr != ""` optional wiring (Task 8 originally,
>    Task 15) are obsolete. `VALKEY_KEY_GRACE_PERIOD`,
>    `ROOM_KEY_RPC_TIMEOUT`, and `ROOM_KEY_MAX_REDELIVER` are validated
>    `> 0` immediately after config parse.
> 2. **`room-worker` and `inbox-worker` handler code has NO `if h.keyStore != nil`
>    nil-guards** — the dependencies are always non-nil in production
>    (the startup gate above guarantees it). The plan's snippets that
>    wrap key operations in those guards are pre-fix; the production
>    code calls `h.keyStore.Get` / `h.keySender.Send` unconditionally.
> 3. **`room-worker` fans out `RoomKeyEvent` to EVERY room member** —
>    local and remote — via `roomkeysender.Send`. NATS supercluster
>    routes `chat.user.{account}.event.*` to home sites. The plan's
>    fan-out snippets that skip remote-site users (e.g. `if u.SiteID
>    != h.siteID { continue }`) are pre-fix. The shipped helper is
>    `buildAndFanOutRoomKey`; for remove-member it's
>    `fanOutRoomKeyToSurvivors` using a `ListByRoom(roomID, "")` survivor
>    snapshot.
> 4. **`inbox-worker` mirrors keys via `SetWithVersion`, not
>    `Set`/`Rotate`.** It pulls the key from origin via
>    `chat.server.request.roomkey.{siteID}.get` RPC and writes the
>    pair into local Valkey at the origin's exact version so this
>    site's `broadcast-worker` emits envelopes whose version every
>    client (across every site) already holds. `inbox-worker` does
>    NOT instantiate or call a `roomkeysender.Sender`. `NewHandler`
>    takes `(store, siteID, keyStore, interSiteClient)`.
> 5. **Stale local key version on remove (`pair.Version <
>    req.NewKeyVersion`) is a transient error** (`NAK + retry`), not
>    permanent — it's a Valkey propagation race, not a malformed
>    event. Some Task 12 snippets show `newPermanent`/`errPermanent`;
>    that was changed.
> 6. **Subscription writes are upserts**, not "InsertMany then ignore
>    duplicate-key errors". `mongoInboxStore.BulkCreateSubscriptions`
>    uses `bulkWrite` with `UpdateOne + $setOnInsert` keyed on
>    `(roomId, u.account)` so redeliveries preserve `LastSeenAt`,
>    `Alert`, and roles on the existing local sub.
> 7. **Shutdown hook order**: OTel `tracerShutdown` / `meterShutdown`
>    run AFTER `nc.Drain` / mongo disconnect / `keyStore.Close` so
>    telemetry emitted during drain is captured.
> 8. **Integration tests use the testcontainers-go NATS module**, not
>    an embedded in-process server.
>
> Each task body is left intact as an implementation log. When the
> code and a snippet disagree, the code wins. See
> [`docs/superpowers/specs/2026-05-08-room-encryption-keys-design.md`](../specs/2026-05-08-room-encryption-keys-design.md)
> for the design-level reference.

**Tech Stack:** Go 1.25, `pkg/roomkeystore` (Valkey via `go-redis/v9`), `pkg/roomkeysender` (NATS), `crypto/ecdh.P256`, `caarlos0/env`, `go.uber.org/mock`, `stretchr/testify`, `testcontainers-go`.

**Spec reference:** `docs/superpowers/specs/2026-05-08-room-encryption-keys-design.md`

**Branch:** `claude/room-encryption-keys-5vlQ2`

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `pkg/subject/subject.go` | Modify | `ServerRoomKeyGet(siteID)` builder |
| `pkg/subject/subject_test.go` | Modify | Test new builder |
| `pkg/model/member.go` | Modify | `NewKeyVersion` on `RemoveMemberRequest` |
| `pkg/model/event.go` | Modify | `NewKeyVersion` on `MemberRemoveEvent` |
| `pkg/model/model_test.go` | Modify | Round-trip tests |
| `room-service/store.go` | Modify | Extend `RoomKeyStore` with `Set`, `Rotate` |
| `room-service/mock_store_test.go` | Regenerate | Via `make generate SERVICE=room-service` |
| `room-service/keygen.go` | Create | `generateRoomKeyPair` helper |
| `room-service/keygen_test.go` | Create | Helper tests |
| `room-service/handler.go` | Modify | Channel guard + Set + Rotate |
| `room-service/handler_test.go` | Modify | New + adjusted tests |
| `room-worker/main.go` | Modify | Valkey + sender wiring; RPC registration |
| `room-worker/store.go` | Modify | New `RoomKeyStore` consumer interface |
| `room-worker/mock_store_test.go` | Regenerate | Mocks for new interface |
| `room-worker/handler.go` | Modify | Get gate, fan-out, version assertion, RPC handler |
| `room-worker/handler_test.go` | Modify | New + adjusted tests |
| `inbox-worker/store.go` | Modify | `InterSiteKeyClient` + `RoomKeyStore` consumer interfaces |
| `inbox-worker/mock_store_test.go` | Regenerate | Mocks |
| `inbox-worker/intersite_key.go` | Create | NATS-backed `InterSiteKeyClient` impl |
| `inbox-worker/intersite_key_test.go` | Create | Client tests |
| `inbox-worker/handler.go` | Modify | RPC + Set/Rotate + fan-out in three handlers |
| `inbox-worker/handler_test.go` | Modify | New + adjusted tests |
| `inbox-worker/main.go` | Modify | Wire keystore + sender + client |
| `room-service/integration_test.go` | Modify | Confirm key persisted on create |
| `room-worker/integration_test.go` | Modify | End-to-end create flow with Valkey |
| `inbox-worker/integration_test.go` | Modify | Two-site cross-site replication |
| `docs/client-api.md` | Modify | Document `RoomKeyEvent` subject + client behavior |
| `docker-local/docker-compose.yml` | Modify | Pass `VALKEY_*` to room-worker, inbox-worker |
| `room-worker/deploy/docker-compose.yml` | Modify | Add Valkey service + env |
| `inbox-worker/deploy/docker-compose.yml` | Modify | Add Valkey service + env |

---

# PART 1 — Foundation (room-service)

## Task 1: Add `subject.ServerRoomKeyGet` builder

**Files:**
- Modify: `pkg/subject/subject.go`
- Test: `pkg/subject/subject_test.go`

- [ ] **Step 1: Failing test**

Append to `pkg/subject/subject_test.go`:

```go
func TestServerRoomKeyGet(t *testing.T) {
	got := subject.ServerRoomKeyGet("site-a")
	want := "chat.server.request.roomkey.site-a.get"
	if got != want {
		t.Fatalf("ServerRoomKeyGet = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=pkg/subject
```

- [ ] **Step 3: Implement**

Append to `pkg/subject/subject.go` after the existing `RoomKeyUpdate` builder:

```go
// Inter-site server-to-server RPC subject for fetching a room's keypair.
func ServerRoomKeyGet(siteID string) string {
	return fmt.Sprintf("chat.server.request.roomkey.%s.get", siteID)
}
```

- [ ] **Step 4: Run — expect PASS**

```bash
make test SERVICE=pkg/subject
make lint
```

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(pkg/subject): add ServerRoomKeyGet builder"
```

---

## Task 2: Add `NewKeyVersion` model fields

**Files:**
- Modify: `pkg/model/member.go`, `pkg/model/event.go`, `pkg/model/model_test.go`

- [ ] **Step 1: Failing tests**

Append round-trip subtests in `pkg/model/model_test.go` (next to existing `RemoveMemberRequest` and `MemberRemoveEvent` tests):

```go
t.Run("RemoveMemberRequest with NewKeyVersion", func(t *testing.T) {
	r := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "bob",
		Timestamp: 1700000000000, NewKeyVersion: 3}
	roundTrip(t, &r, &model.RemoveMemberRequest{})
})

t.Run("MemberRemoveEvent with NewKeyVersion", func(t *testing.T) {
	e := model.MemberRemoveEvent{Type: "member_removed", RoomID: "r1",
		Accounts: []string{"bob"}, SiteID: "site-a",
		Timestamp: 1700000000000, NewKeyVersion: 3}
	roundTrip(t, &e, &model.MemberRemoveEvent{})
})
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=pkg/model
```

- [ ] **Step 3: Add field to `RemoveMemberRequest`**

In `pkg/model/member.go`, append `NewKeyVersion int \`json:"newKeyVersion" bson:"newKeyVersion"\`` as the last field of `RemoveMemberRequest`. Single-line doc comment:

```go
	// New room-key version after room-service rotates on remove.
	NewKeyVersion int `json:"newKeyVersion" bson:"newKeyVersion"`
```

- [ ] **Step 4: Add field to `MemberRemoveEvent`**

In `pkg/model/event.go` (around line 170, in `MemberRemoveEvent`), append the same field with a one-line comment:

```go
	// Federated key version for inbox-worker's local rotation.
	NewKeyVersion int `json:"newKeyVersion" bson:"newKeyVersion"`
```

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=pkg/model
make lint
```

- [ ] **Step 6: Commit**

```bash
git add pkg/model/member.go pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(pkg/model): add NewKeyVersion to remove-member request and event"
```

---

## Task 3: Extend `room-service` `RoomKeyStore` interface

**Files:**
- Modify: `room-service/store.go`
- Regenerate: `room-service/mock_store_test.go`

- [ ] **Step 1: Edit interface**

In `room-service/store.go`, replace:

```go
type RoomKeyStore interface {
	GetMany(ctx context.Context, roomIDs []string) (map[string]*roomkeystore.VersionedKeyPair, error)
}
```

with:

```go
type RoomKeyStore interface {
	GetMany(ctx context.Context, roomIDs []string) (map[string]*roomkeystore.VersionedKeyPair, error)
	// Set writes a fresh keypair as the room's current key (version 0).
	Set(ctx context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error)
	// Rotate increments version and demotes current key to :prev with grace TTL.
	Rotate(ctx context.Context, roomID string, newPair roomkeystore.RoomKeyPair) (int, error)
}
```

- [ ] **Step 2: Regenerate mocks**

```bash
make generate SERVICE=room-service
```

- [ ] **Step 3: Verify package compiles**

```bash
make test SERVICE=room-service
```

Expected: package compiles. Test failures from missing `EXPECT()` are fine; later tasks fix them.

- [ ] **Step 4: Commit**

```bash
git add room-service/store.go room-service/mock_store_test.go
git commit -m "feat(room-service): extend RoomKeyStore with Set and Rotate"
```

---

## Task 4: Channel-only guard in `handleRemoveMember`

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

- [ ] **Step 1: Failing test**

Append to `room-service/handler_test.go`:

```go
func TestHandler_RemoveMember_RejectsNonChannelRoom(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Type: model.RoomTypeDM,
	}, nil)
	h := &Handler{store: store, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: func(_ context.Context, _ string, _ []byte) error {
			t.Fatal("publishToStream must not be called")
			return nil
		},
	}
	req := model.RemoveMemberRequest{Account: "bob"}
	data, _ := json.Marshal(req)
	_, err := h.handleRemoveMember(t.Context(),
		"chat.user.alice.request.room.r1.site-a.member.remove", data)
	if err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("expected channel-type error, got %v", err)
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-service
```

- [ ] **Step 3: Add the guard**

In `room-service/handler.go` `handleRemoveMember`, insert immediately after `req.Requester = requesterAccount`:

```go
	// Channel-only: DM/botDM removals are not supported.
	room, err := h.store.GetRoom(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("get room: %w", err)
	}
	if room.Type != model.RoomTypeChannel {
		return nil, fmt.Errorf("remove-member only supported on channel rooms, got %s", room.Type)
	}
```

If a later `var err error` in the same function conflicts, change it to `err =`.

- [ ] **Step 4: Update existing happy-path tests**

Find each test in `room-service/handler_test.go` that calls `handleRemoveMember` and exercises the success path. Add a `store.EXPECT().GetRoom(gomock.Any(), "<roomID>").Return(&model.Room{ID: "<roomID>", Type: model.RoomTypeChannel}, nil)` ahead of existing expectations. Use `make test SERVICE=room-service` to enumerate failures.

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-service
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "feat(room-service): block remove-member on non-channel rooms"
```

---

## Task 5: `generateRoomKeyPair` helper

**Files:**
- Create: `room-service/keygen.go`, `room-service/keygen_test.go`

- [ ] **Step 1: Failing tests**

Create `room-service/keygen_test.go`:

```go
package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/roomcrypto"
)

func TestGenerateRoomKeyPair_Shape(t *testing.T) {
	pair, err := generateRoomKeyPair()
	require.NoError(t, err)
	assert.Len(t, pair.PublicKey, 65)
	assert.Len(t, pair.PrivateKey, 32)
}

func TestGenerateRoomKeyPair_Distinct(t *testing.T) {
	a, err := generateRoomKeyPair()
	require.NoError(t, err)
	b, err := generateRoomKeyPair()
	require.NoError(t, err)
	assert.False(t, bytes.Equal(a.PublicKey, b.PublicKey))
	assert.False(t, bytes.Equal(a.PrivateKey, b.PrivateKey))
}

func TestGenerateRoomKeyPair_RoundTripWithRoomcrypto(t *testing.T) {
	pair, err := generateRoomKeyPair()
	require.NoError(t, err)
	encrypted, err := roomcrypto.Encode("hello", pair.PublicKey, 0)
	require.NoError(t, err)
	assert.Len(t, encrypted.EphemeralPublicKey, 65)
	assert.Len(t, encrypted.Nonce, 12)
	assert.NotEmpty(t, encrypted.Ciphertext)
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-service
```

- [ ] **Step 3: Implement**

Create `room-service/keygen.go`:

```go
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"

	"github.com/hmchangw/chat/pkg/roomkeystore"
)

// Generate a fresh P-256 keypair for a new room.
func generateRoomKeyPair() (roomkeystore.RoomKeyPair, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return roomkeystore.RoomKeyPair{}, fmt.Errorf("generate P-256 key: %w", err)
	}
	return roomkeystore.RoomKeyPair{
		PublicKey:  priv.PublicKey().Bytes(),
		PrivateKey: priv.Bytes(),
	}, nil
}
```

- [ ] **Step 4: Run — expect PASS**

```bash
make test SERVICE=room-service
make lint
```

- [ ] **Step 5: Commit**

```bash
git add room-service/keygen.go room-service/keygen_test.go
git commit -m "feat(room-service): add generateRoomKeyPair helper"
```

---

## Task 6: Wire `Set` into `publishCreateRoom`

**Files:**
- Modify: `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Failing tests**

Append to `room-service/handler_test.go` (uses the same fixture shape as existing happy-path create-room tests; copy that surrounding fixture if these stubs are insufficient):

```go
func TestHandler_CreateRoom_WritesKeyBeforePublish(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)

	store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u-alice", Account: "alice", SiteID: "site-a",
	}, nil)
	store.EXPECT().CountNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "", "alice").
		Return(1, nil)

	var publishCalls int
	keyStore.EXPECT().Set(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error) {
			assert.NotEmpty(t, roomID)
			assert.Len(t, pair.PublicKey, 65)
			assert.Len(t, pair.PrivateKey, 32)
			return 0, nil
		})

	publish := func(_ context.Context, subj string, _ []byte) error {
		publishCalls++
		assert.Equal(t, "chat.room.canonical.site-a.create", subj)
		return nil
	}

	h := &Handler{store: store, keyStore: keyStore, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: publish}

	req := model.CreateRoomRequest{Type: model.RoomTypeChannel, Name: "general", Users: []string{"bob"}}
	data, _ := json.Marshal(req)
	_, err := h.handleCreateRoom(t.Context(),
		"chat.user.alice.request.room.site-a.create", data)
	require.NoError(t, err)
	assert.Equal(t, 1, publishCalls)
}

func TestHandler_CreateRoom_AbortsOnKeyStoreSetError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)

	store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{
		ID: "u-alice", Account: "alice", SiteID: "site-a",
	}, nil)
	store.EXPECT().CountNewMembers(gomock.Any(), gomock.Any(), gomock.Any(), "", "alice").
		Return(1, nil)
	keyStore.EXPECT().Set(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(0, fmt.Errorf("valkey down"))

	h := &Handler{store: store, keyStore: keyStore, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: func(_ context.Context, _ string, _ []byte) error {
			t.Fatal("publishToStream must not be called when Set fails")
			return nil
		},
	}

	req := model.CreateRoomRequest{Type: model.RoomTypeChannel, Name: "general", Users: []string{"bob"}}
	data, _ := json.Marshal(req)
	_, err := h.handleCreateRoom(t.Context(),
		"chat.user.alice.request.room.site-a.create", data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store room key")
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-service
```

- [ ] **Step 3: Wire `Set` into `publishCreateRoom`**

In `room-service/handler.go`, locate `publishCreateRoom`. After `req.Timestamp = time.Now().UTC().UnixMilli()` and any span-attribute block, before `payload, err := json.Marshal(req)`, insert:

```go
	// Generate and store room key BEFORE canonical event so worker's Get gate succeeds.
	if h.keyStore != nil {
		pair, err := generateRoomKeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate room key: %w", err)
		}
		if _, err := h.keyStore.Set(ctx, req.RoomID, pair); err != nil {
			return nil, fmt.Errorf("store room key: %w", err)
		}
	}
```

The `nil` guard implements "gated on `VALKEY_ADDR` configured" — deployments without Valkey skip key handling.

- [ ] **Step 4: Update pre-existing create-room tests**

Tests with non-nil `keyStore` need `keyStore.EXPECT().Set(gomock.Any(), gomock.Any(), gomock.Any()).Return(0, nil)`. Tests that don't care about keys can pass `keyStore: nil`. Use `make test SERVICE=room-service` to enumerate.

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-service
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "feat(room-service): generate and store room key on create"
```

---

## Task 7: Wire `Rotate` (with Set fallback) into `handleRemoveMember`

**Files:**
- Modify: `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Failing tests**

Append to `room-service/handler_test.go`:

```go
func TestHandler_RemoveMember_RotatesKeyAndStampsVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Type: model.RoomTypeChannel,
	}, nil)
	store.EXPECT().GetSubscriptionWithMembership(gomock.Any(), "r1", "bob").Return(
		&SubscriptionWithMembership{
			Subscription:            &model.Subscription{User: model.SubscriptionUser{Account: "bob"}, RoomID: "r1", Roles: []model.Role{model.RoleMember}},
			HasIndividualMembership: true,
		}, nil)
	store.EXPECT().GetSubscription(gomock.Any(), "alice", "r1").Return(
		&model.Subscription{User: model.SubscriptionUser{Account: "alice"}, RoomID: "r1",
			Roles: []model.Role{model.RoleOwner, model.RoleMember}}, nil)
	store.EXPECT().CountMembersAndOwners(gomock.Any(), "r1").Return(
		&MembersAndOwnersCount{MemberCount: 5, OwnerCount: 2}, nil)

	keyStore.EXPECT().Rotate(gomock.Any(), "r1", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, pair roomkeystore.RoomKeyPair) (int, error) {
			assert.Len(t, pair.PublicKey, 65)
			return 7, nil
		})

	var captured model.RemoveMemberRequest
	publish := func(_ context.Context, _ string, data []byte) error {
		require.NoError(t, json.Unmarshal(data, &captured))
		return nil
	}

	h := &Handler{store: store, keyStore: keyStore, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: publish}

	req := model.RemoveMemberRequest{Account: "bob"}
	data, _ := json.Marshal(req)
	_, err := h.handleRemoveMember(t.Context(),
		"chat.user.alice.request.room.r1.site-a.member.remove", data)
	require.NoError(t, err)
	assert.Equal(t, 7, captured.NewKeyVersion)
}

func TestHandler_RemoveMember_FallsBackToSetOnNoCurrentKey(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Type: model.RoomTypeChannel,
	}, nil)
	store.EXPECT().GetSubscriptionWithMembership(gomock.Any(), "r1", "bob").Return(
		&SubscriptionWithMembership{
			Subscription:            &model.Subscription{User: model.SubscriptionUser{Account: "bob"}, RoomID: "r1", Roles: []model.Role{model.RoleMember}},
			HasIndividualMembership: true,
		}, nil)
	store.EXPECT().GetSubscription(gomock.Any(), "alice", "r1").Return(
		&model.Subscription{User: model.SubscriptionUser{Account: "alice"}, RoomID: "r1",
			Roles: []model.Role{model.RoleOwner, model.RoleMember}}, nil)
	store.EXPECT().CountMembersAndOwners(gomock.Any(), "r1").Return(
		&MembersAndOwnersCount{MemberCount: 5, OwnerCount: 2}, nil)

	gomock.InOrder(
		keyStore.EXPECT().Rotate(gomock.Any(), "r1", gomock.Any()).
			Return(0, roomkeystore.ErrNoCurrentKey),
		keyStore.EXPECT().Set(gomock.Any(), "r1", gomock.Any()).Return(0, nil),
	)

	var captured model.RemoveMemberRequest
	publish := func(_ context.Context, _ string, data []byte) error {
		require.NoError(t, json.Unmarshal(data, &captured))
		return nil
	}

	h := &Handler{store: store, keyStore: keyStore, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: publish}

	req := model.RemoveMemberRequest{Account: "bob"}
	data, _ := json.Marshal(req)
	_, err := h.handleRemoveMember(t.Context(),
		"chat.user.alice.request.room.r1.site-a.member.remove", data)
	require.NoError(t, err)
	assert.Equal(t, 0, captured.NewKeyVersion)
}

func TestHandler_RemoveMember_AbortsOnRotateError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockRoomStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{
		ID: "r1", Type: model.RoomTypeChannel,
	}, nil)
	store.EXPECT().GetSubscriptionWithMembership(gomock.Any(), "r1", "bob").Return(
		&SubscriptionWithMembership{
			Subscription:            &model.Subscription{User: model.SubscriptionUser{Account: "bob"}, RoomID: "r1", Roles: []model.Role{model.RoleMember}},
			HasIndividualMembership: true,
		}, nil)
	store.EXPECT().GetSubscription(gomock.Any(), "alice", "r1").Return(
		&model.Subscription{User: model.SubscriptionUser{Account: "alice"}, RoomID: "r1",
			Roles: []model.Role{model.RoleOwner, model.RoleMember}}, nil)
	store.EXPECT().CountMembersAndOwners(gomock.Any(), "r1").Return(
		&MembersAndOwnersCount{MemberCount: 5, OwnerCount: 2}, nil)
	keyStore.EXPECT().Rotate(gomock.Any(), "r1", gomock.Any()).
		Return(0, fmt.Errorf("valkey down"))

	h := &Handler{store: store, keyStore: keyStore, siteID: "site-a", maxRoomSize: 1000,
		publishToStream: func(_ context.Context, _ string, _ []byte) error {
			t.Fatal("publishToStream must not be called when Rotate fails")
			return nil
		},
	}

	req := model.RemoveMemberRequest{Account: "bob"}
	data, _ := json.Marshal(req)
	_, err := h.handleRemoveMember(t.Context(),
		"chat.user.alice.request.room.r1.site-a.member.remove", data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rotate room key")
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-service
```

- [ ] **Step 3: Wire `Rotate`**

In `room-service/handler.go` `handleRemoveMember`, insert immediately after `req.Timestamp = time.Now().UTC().UnixMilli()` and BEFORE `data, err = json.Marshal(req)`:

```go
	// Rotate before publish so broadcast-worker encrypts under the new key immediately.
	if h.keyStore != nil {
		pair, err := generateRoomKeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate new room key: %w", err)
		}
		newVer, err := h.keyStore.Rotate(ctx, req.RoomID, pair)
		if err != nil {
			if errors.Is(err, roomkeystore.ErrNoCurrentKey) {
				// Pre-existing un-keyed room: fall back to Set (version 0).
				if _, setErr := h.keyStore.Set(ctx, req.RoomID, pair); setErr != nil {
					return nil, fmt.Errorf("store room key (fallback): %w", setErr)
				}
				newVer = 0
			} else {
				return nil, fmt.Errorf("rotate room key: %w", err)
			}
		}
		req.NewKeyVersion = newVer
	}
```

If `roomkeystore` isn't yet imported in `handler.go`, add: `"github.com/hmchangw/chat/pkg/roomkeystore"`.

- [ ] **Step 4: Update pre-existing remove-member tests**

Tests with non-nil `keyStore` need `keyStore.EXPECT().Rotate(...)`. Tests with `keyStore: nil` are unaffected.

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-service
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-service/handler.go room-service/handler_test.go
git commit -m "feat(room-service): rotate room key on channel member removal"
```

---

# PART 2 — `room-worker`

> **Drift notice:** in addition to the top-level drift summary, the
> per-task snippets in Tasks 10–14 still show the original
> `if h.keyStore != nil` / `if h.keyStore == nil || h.keySender == nil`
> nil-guards. Production code has none of those — the
> `VALKEY_ADDR=required` startup gate guarantees non-nil deps. The
> handler calls `h.keyStore.Get` / `h.keySender.Send` unconditionally.
> Fan-out helpers are named `buildAndFanOutRoomKey` (create / add) and
> `fanOutRoomKeyToSurvivors` (remove). The remove path's survivor list
> comes from `h.store.ListByRoom(req.RoomID, "")` — empty `siteID` so
> both local and remote subscribers receive the rotated key. ListByRoom
> failure surfaces as an error (no Ack).

## Task 8: Add Valkey + sender wiring to `room-worker/main.go`

**Files:**
- Modify: `room-worker/main.go`

- [ ] **Step 1: Extend config**

In `room-worker/main.go`, add to the `config` struct. `VALKEY_ADDR` is a hard
startup requirement — there is no "silent skip" mode:

```go
	// Valkey wiring; required. room-worker needs the key on every create /
	// add / remove path and the inter-site
	// `chat.server.request.roomkey.{siteID}.get` RPC handler depends on
	// the keystore.
	ValkeyAddr           string        `env:"VALKEY_ADDR,required"`
	ValkeyPassword       string        `env:"VALKEY_PASSWORD"           envDefault:""`
	ValkeyKeyGracePeriod time.Duration `env:"VALKEY_KEY_GRACE_PERIOD"   envDefault:"24h"`
```

Validate `VALKEY_KEY_GRACE_PERIOD > 0` immediately after `env.ParseAs[config]()`:

```go
	if cfg.ValkeyKeyGracePeriod <= 0 {
		slog.Error("VALKEY_KEY_GRACE_PERIOD must be a positive duration",
			"valkey_key_grace_period", cfg.ValkeyKeyGracePeriod)
		os.Exit(1)
	}
```

- [ ] **Step 2: Wire keystore + sender after `nc` connect (fail-fast)**

After the `nc, err := natsutil.Connect(...)` block and before the existing handler construction, add:

```go
	keyStore, err := roomkeystore.NewValkeyStore(roomkeystore.Config{
		Addr:        cfg.ValkeyAddr,
		Password:    cfg.ValkeyPassword,
		GracePeriod: cfg.ValkeyKeyGracePeriod,
	})
	if err != nil {
		slog.Error("valkey connect failed", "error", err)
		os.Exit(1)
	}
	keySender := roomkeysender.NewSender(nc.NatsConn())
```

`nc` here is the OpenTelemetry-wrapped connection returned by `natsutil.Connect`
(`*otelnats.Conn`); `nc.NatsConn()` returns the underlying `*nats.Conn`, which
satisfies `roomkeysender.Publisher` directly. No bespoke adapter type is needed.

Add imports: `"github.com/hmchangw/chat/pkg/roomkeystore"`, `"github.com/hmchangw/chat/pkg/roomkeysender"`.

- [ ] **Step 3: Plumb `keyStore` and `keySender` through `NewHandler`**

Update the `NewHandler` call site to pass the new dependencies (signature change in next task).

- [ ] **Step 4: Append `keyStore.Close()` to the shutdown hook chain**

Insert `keyStore.Close()` BEFORE the OTel `tracerShutdown` / `meterShutdown`
hooks — telemetry must flush after client connections close so spans emitted
during drain are captured. The full shipped ordering is `iter.Stop` → wait for
in-flight workers → `nc.Drain` → mongo disconnect → `keyStore.Close` →
`tracerShutdown` → `meterShutdown`.

```go
	hooks := []func(ctx context.Context) error{
		// ... iter.Stop, wg.Wait, nc.Drain, mongoutil.Disconnect ...
		func(ctx context.Context) error { return keyStore.Close() },
		func(ctx context.Context) error { return tracerShutdown(ctx) },
		func(ctx context.Context) error { return meterShutdown(ctx) },
	}
```

- [ ] **Step 5: Compile**

```bash
go build ./room-worker/...
```

(Won't link until Task 9 lands the new `Handler` constructor.)

- [ ] **Step 6: Commit**

```bash
git add room-worker/main.go
git commit -m "feat(room-worker): add Valkey and roomkeysender wiring"
```

---

## Task 9: Extend `room-worker` Handler + store interface

**Files:**
- Modify: `room-worker/store.go`, `room-worker/handler.go`
- Regenerate: `room-worker/mock_store_test.go`

- [ ] **Step 1: Add `RoomKeyStore` interface to `store.go`**

Append to `room-worker/store.go`:

```go
// Read-only key store used by room-worker.
type RoomKeyStore interface {
	Get(ctx context.Context, roomID string) (*roomkeystore.VersionedKeyPair, error)
}
```

Add import: `"github.com/hmchangw/chat/pkg/roomkeystore"`.

- [ ] **Step 2: Extend `Handler` struct + constructor**

In `room-worker/handler.go`, change:

```go
type Handler struct {
	store   SubscriptionStore
	siteID  string
	publish PublishFunc
}

func NewHandler(store SubscriptionStore, siteID string, publish PublishFunc) *Handler {
	return &Handler{store: store, siteID: siteID, publish: publish}
}
```

to:

```go
type Handler struct {
	store     SubscriptionStore
	siteID    string
	publish   PublishFunc
	keyStore  RoomKeyStore
	keySender *roomkeysender.Sender
}

func NewHandler(store SubscriptionStore, siteID string, publish PublishFunc, keyStore RoomKeyStore, keySender *roomkeysender.Sender) *Handler {
	return &Handler{store: store, siteID: siteID, publish: publish, keyStore: keyStore, keySender: keySender}
}
```

Add import: `"github.com/hmchangw/chat/pkg/roomkeysender"`.

- [ ] **Step 3: Regenerate mocks**

```bash
make generate SERVICE=room-worker
```

- [ ] **Step 4: Update test fixtures**

In `room-worker/handler_test.go`, every `NewHandler(store, siteID, publish)` call becomes `NewHandler(store, siteID, publish, nil, nil)`. Pass `nil` for tests that aren't exercising key behavior.

- [ ] **Step 5: Update `main.go` constructor call**

```go
handler := NewHandler(store, cfg.SiteID, publishFunc, keyStore, keySender)
```

- [ ] **Step 6: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 7: Commit**

```bash
git add room-worker/store.go room-worker/handler.go room-worker/handler_test.go room-worker/main.go room-worker/mock_store_test.go
git commit -m "feat(room-worker): add RoomKeyStore + sender to Handler"
```

---

## Task 10: Gate `processCreateRoom` on key presence + fan-out

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`

- [ ] **Step 1: Failing tests**

Append to `room-worker/handler_test.go`:

```go
func TestProcessCreateRoom_PermanentErrorWhenKeyMissing(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	store.EXPECT().GetUser(gomock.Any(), "alice").Return(&model.User{ID: "u-a", Account: "alice", SiteID: "site-a"}, nil)
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(nil, nil) // no key

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, nil)

	req := model.CreateRoomRequest{RoomID: "r1", RequesterAccount: "alice", Type: model.RoomTypeChannel, Name: "g", Users: []string{"bob"}, Timestamp: time.Now().UnixMilli()}
	data, _ := json.Marshal(req)
	ctx := natsutil.ContextWithRequestID(context.Background(), "req-1")

	err := h.processCreateRoom(ctx, data)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent), "missing key must be permanent")
}

func TestProcessCreateRoom_FansOutKeyAfterMongoWrites(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	pair := &roomkeystore.VersionedKeyPair{Version: 0, KeyPair: roomkeystore.RoomKeyPair{
		PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x01}, 32),
	}}
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(pair, nil)

	// Stub remaining Mongo operations (use existing happy-path test as template).
	// ... [fixture matching existing happy-path test for processCreateRoomChannel] ...

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, keySender)

	req := model.CreateRoomRequest{RoomID: "r1", RequesterAccount: "alice", Type: model.RoomTypeChannel, Name: "g", Users: []string{"bob"}, Timestamp: time.Now().UnixMilli()}
	data, _ := json.Marshal(req)
	ctx := natsutil.ContextWithRequestID(context.Background(), "req-1")
	require.NoError(t, h.processCreateRoom(ctx, data))

	// One Send per local-site member account.
	assert.Equal(t, 2, pubMock.publishCount(), "send to alice + bob")
}
```

`mockPublisher` is a small in-test helper — add to a `_test.go` file:

```go
type mockPublisher struct {
	mu       sync.Mutex
	subjects []string
	payloads [][]byte
}

func newMockPublisher(_ *testing.T) *mockPublisher { return &mockPublisher{} }

func (p *mockPublisher) Publish(subj string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subj)
	p.payloads = append(p.payloads, append([]byte(nil), data...))
	return nil
}
func (p *mockPublisher) publishCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subjects)
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-worker
```

- [ ] **Step 3: Add Get gate to `processCreateRoom`**

In `room-worker/handler.go` `processCreateRoom`, after `req` is unmarshaled and `req.RoomID` is set, but BEFORE `h.store.GetUser` (the existing first Mongo call), insert:

```go
	// Gate: key MUST exist before any Mongo write.
	if h.keyStore != nil {
		pair, err := h.keyStore.Get(ctx, req.RoomID)
		if err != nil {
			return fmt.Errorf("get room key: %w", err)
		}
		if pair == nil {
			return newPermanent("room key missing for %s", req.RoomID)
		}
	}
```

- [ ] **Step 4: Add fan-out at the end of `finishCreateRoom`**

In `finishCreateRoom`, append before `return nil`:

```go
	// Fan out current key to every local-site member.
	if err := h.fanOutRoomKey(ctx, room.ID, allUsers); err != nil {
		slog.Error("room key fan-out failed", "error", err, "roomId", room.ID)
	}
```

Add the helper at the bottom of `handler.go`:

```go
func (h *Handler) fanOutRoomKey(ctx context.Context, roomID string, users []*model.User) error {
	if h.keyStore == nil || h.keySender == nil {
		return nil
	}
	pair, err := h.keyStore.Get(ctx, roomID)
	if err != nil {
		return fmt.Errorf("get room key for fan-out: %w", err)
	}
	if pair == nil {
		return fmt.Errorf("room key missing at fan-out time for %s", roomID)
	}
	evt := &model.RoomKeyEvent{
		RoomID: roomID, Version: pair.Version,
		PublicKey: pair.KeyPair.PublicKey, PrivateKey: pair.KeyPair.PrivateKey,
	}
	for _, u := range users {
		if u.SiteID != h.siteID && u.SiteID != "" {
			continue // remote-site users get keys via inbox-worker on their site
		}
		if err := h.keySender.Send(u.Account, evt); err != nil {
			slog.Error("send room key", "error", err, "account", u.Account, "roomId", roomID)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): gate processCreateRoom on key + fan out RoomKeyEvent"
```

---

## Task 11: Fan-out new key on `processAddMembers` (channel)

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`

- [ ] **Step 1: Failing test**

Append to `room-worker/handler_test.go`:

```go
func TestProcessAddMembers_FansOutKeyToNewAccountsOnly(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a"}, nil)
	// Use existing happy-path test as fixture template for ListNewMembers + FindUsersByAccounts + BulkCreateSubscriptions.
	// ...
	pair := &roomkeystore.VersionedKeyPair{Version: 1, KeyPair: roomkeystore.RoomKeyPair{
		PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x02}, 32),
	}}
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(pair, nil)

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, keySender)

	req := model.AddMembersRequest{RoomID: "r1", RequesterAccount: "alice", Users: []string{"charlie"}}
	data, _ := json.Marshal(req)
	ctx := natsutil.ContextWithRequestID(context.Background(), "req-1")
	require.NoError(t, h.processAddMembers(ctx, data))

	// Expect exactly one Send for charlie. Existing members (alice, bob) are NOT re-keyed.
	assert.Equal(t, 1, pubMock.publishCount())
	assert.Contains(t, pubMock.subjects[0], "chat.user.charlie.event.room.key")
}

func TestProcessAddMembers_PermanentErrorWhenKeyMissing(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a"}, nil)
	// ... add minimal upstream stubs to reach the key-Get call ...
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(nil, nil)

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, nil)
	req := model.AddMembersRequest{RoomID: "r1", RequesterAccount: "alice", Users: []string{"charlie"}}
	data, _ := json.Marshal(req)
	ctx := natsutil.ContextWithRequestID(context.Background(), "req-1")
	err := h.processAddMembers(ctx, data)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent))
}

func TestProcessAddMembers_RejectsNonChannel(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeDM, SiteID: "site-a"}, nil)

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, nil, nil)
	req := model.AddMembersRequest{RoomID: "r1", RequesterAccount: "alice", Users: []string{"x"}}
	data, _ := json.Marshal(req)
	err := h.processAddMembers(natsutil.ContextWithRequestID(context.Background(), "req-1"), data)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errPermanent))
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-worker
```

- [ ] **Step 3: Implement**

In `room-worker/handler.go` `processAddMembers`:

(a) Right after `room, err := h.store.GetRoom(ctx, req.RoomID)` and the err check, add:

```go
	// Defensive channel-only guard.
	if room.Type != model.RoomTypeChannel {
		return newPermanent("add-member only valid on channel rooms, got %s", room.Type)
	}
```

(b) After the existing `BulkCreateSubscriptions` succeeds and before any other event publishing, add:

```go
	// Fan out current key to newly-added local-site accounts only.
	if h.keyStore != nil && h.keySender != nil {
		pair, err := h.keyStore.Get(ctx, req.RoomID)
		if err != nil {
			return fmt.Errorf("get room key: %w", err)
		}
		if pair == nil {
			return newPermanent("room key missing for %s", req.RoomID)
		}
		evt := &model.RoomKeyEvent{
			RoomID: req.RoomID, Version: pair.Version,
			PublicKey: pair.KeyPair.PublicKey, PrivateKey: pair.KeyPair.PrivateKey,
		}
		for _, u := range users { // 'users' is the *model.User slice from FindUsersByAccounts
			if u.SiteID != h.siteID && u.SiteID != "" {
				continue
			}
			if err := h.keySender.Send(u.Account, evt); err != nil {
				slog.Error("send room key", "error", err, "account", u.Account, "roomId", req.RoomID)
			}
		}
	}
```

The exact variable name (`users`) is whatever the existing handler uses — match the surrounding code. If the function uses `accounts []string` instead of `[]*model.User`, look up users via `h.store.FindUsersByAccounts(ctx, accounts)` first.

- [ ] **Step 4: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): fan out current key to new channel members"
```

---

## Task 12: Version assertion + fan-out on `processRemoveMember`

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`

- [ ] **Step 1: Failing tests**

Append:

```go
func TestProcessRemoveMember_TransientErrorWhenVersionStale(t *testing.T) {
	// Stale version means room-service rotated but the worker's local Valkey
	// read hasn't caught up yet — this is a propagation race, not a malformed
	// event. The handler must return a non-permanent error so JetStream NAKs
	// and retries until the rotated version becomes visible.
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a"}, nil)
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(&roomkeystore.VersionedKeyPair{Version: 2}, nil)

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, nil)
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "bob", NewKeyVersion: 5}
	data, _ := json.Marshal(req)
	err := h.processRemoveMember(natsutil.ContextWithRequestID(context.Background(), "req-1"), data)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errPermanent), "stale-version must be transient (NAK + retry), not permanent")
}

func TestProcessRemoveMember_FansOutNewKeyToSurvivors(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", Type: model.RoomTypeChannel, SiteID: "site-a"}, nil)
	pair := &roomkeystore.VersionedKeyPair{Version: 5, KeyPair: roomkeystore.RoomKeyPair{
		PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x03}, 32),
	}}
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(pair, nil)
	// ... use existing happy-path remove-member test as fixture template for the rest ...

	h := NewHandler(store, "site-a", func(_ context.Context, _ string, _ []byte, _ string) error { return nil }, keyStore, keySender)
	req := model.RemoveMemberRequest{RoomID: "r1", Requester: "alice", Account: "bob", NewKeyVersion: 5}
	data, _ := json.Marshal(req)
	require.NoError(t, h.processRemoveMember(natsutil.ContextWithRequestID(context.Background(), "req-1"), data))

	// Survivors get the new key. 'bob' (removed) does not.
	for _, subj := range pubMock.subjects {
		assert.NotContains(t, subj, ".bob.")
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-worker
```

- [ ] **Step 3: Add channel guard + version gate in `processRemoveMember`**

In `room-worker/handler.go` `processRemoveMember`, before the existing `Org`/`Individual` branch, add:

```go
	room, err := h.store.GetRoom(ctx, req.RoomID)
	if err != nil {
		return fmt.Errorf("get room: %w", err)
	}
	if room.Type != model.RoomTypeChannel {
		return newPermanent("remove-member only valid on channel rooms, got %s", room.Type)
	}
	// Version assertion: room-service rotated; worker must see the new version.
	// Stale-version is a Valkey propagation race, not a malformed event — return
	// a plain (transient) error so JetStream NAKs and retries.
	if h.keyStore != nil {
		pair, err := h.keyStore.Get(ctx, req.RoomID)
		if err != nil {
			return fmt.Errorf("get room key: %w", err)
		}
		if pair == nil || pair.Version < req.NewKeyVersion {
			return fmt.Errorf("stale key version: have=%v want>=%d", pair, req.NewKeyVersion)
		}
	}
```

- [ ] **Step 4: Add fan-out helper for survivors**

After the existing Mongo-deletes complete in both `processRemoveIndividual` and `processRemoveOrg`, before any outbox or sys-message publishing, call:

```go
	if err := h.fanOutRoomKeyToSurvivors(ctx, req.RoomID); err != nil {
		slog.Error("survivor key fan-out failed", "error", err, "roomId", req.RoomID)
	}
```

Add the helper at the bottom of `handler.go`:

```go
// Fan out current key to every local-site subscriber (post-removal survivors).
func (h *Handler) fanOutRoomKeyToSurvivors(ctx context.Context, roomID string) error {
	if h.keyStore == nil || h.keySender == nil {
		return nil
	}
	pair, err := h.keyStore.Get(ctx, roomID)
	if err != nil {
		return fmt.Errorf("get room key: %w", err)
	}
	if pair == nil {
		return fmt.Errorf("room key missing for %s", roomID)
	}
	subs, err := h.store.ListSubscriptions(ctx, roomID)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	evt := &model.RoomKeyEvent{
		RoomID: roomID, Version: pair.Version,
		PublicKey: pair.KeyPair.PublicKey, PrivateKey: pair.KeyPair.PrivateKey,
	}
	for _, sub := range subs {
		if sub.SiteID != h.siteID && sub.SiteID != "" {
			continue
		}
		if err := h.keySender.Send(sub.User.Account, evt); err != nil {
			slog.Error("send room key", "error", err, "account", sub.User.Account, "roomId", roomID)
		}
	}
	return nil
}
```

If `ListSubscriptions(ctx, roomID)` doesn't already exist on `SubscriptionStore`, add it (the broadcast-worker has a similar method — mirror that signature).

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go room-worker/store.go room-worker/store_mongo.go room-worker/mock_store_test.go
git commit -m "feat(room-worker): version-gate + fan out new key on member removal"
```

---

## Task 13: Outbox `MemberRemoveEvent` carries `NewKeyVersion`

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`

- [ ] **Step 1: Failing test**

Append a test that captures the outbox publish payload and asserts `NewKeyVersion` is set:

```go
func TestProcessRemoveMember_OutboxCarriesNewKeyVersion(t *testing.T) {
	// Use the existing remove-member outbox test as a template; add:
	//   var captured model.MemberRemoveEvent
	//   <publish func captures envelope.Payload then unmarshals into captured>
	//   assert.Equal(t, 5, captured.NewKeyVersion)
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-worker
```

- [ ] **Step 3: Implement**

Find the `MemberRemoveEvent{...}` struct construction in `processRemoveIndividual` and `processRemoveOrg`. Add `NewKeyVersion: req.NewKeyVersion` as the last field.

- [ ] **Step 4: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 5: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go
git commit -m "feat(room-worker): propagate NewKeyVersion through MemberRemoveEvent outbox"
```

---

## Task 14: `NatsHandleGetRoomKey` RPC handler

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`, `room-worker/main.go`

- [ ] **Step 1: Failing tests**

Append:

```go
func TestNatsHandleGetRoomKey_NotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	keyStore := NewMockRoomKeyStore(ctrl)
	keyStore.EXPECT().Get(gomock.Any(), "r-missing").Return(nil, nil)

	h := NewHandler(nil, "site-a", nil, keyStore, nil)
	reqBody, _ := json.Marshal(map[string]string{"roomId": "r-missing"})
	respondedErr := captureNatsReplyError(t, func(replyTo string) {
		h.NatsHandleGetRoomKey(otelnats.Msg{Msg: &nats.Msg{Data: reqBody, Reply: replyTo}})
	})
	assert.Equal(t, http.StatusNotFound, respondedErr.Code)
}

func TestNatsHandleGetRoomKey_Returns(t *testing.T) {
	ctrl := gomock.NewController(t)
	keyStore := NewMockRoomKeyStore(ctrl)
	pair := &roomkeystore.VersionedKeyPair{Version: 3, KeyPair: roomkeystore.RoomKeyPair{
		PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x05}, 32),
	}}
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(pair, nil)

	h := NewHandler(nil, "site-a", nil, keyStore, nil)
	reqBody, _ := json.Marshal(map[string]string{"roomId": "r1"})
	resp := captureNatsReplyJSON(t, func(replyTo string) {
		h.NatsHandleGetRoomKey(otelnats.Msg{Msg: &nats.Msg{Data: reqBody, Reply: replyTo}})
	})
	var evt model.RoomKeyEvent
	require.NoError(t, json.Unmarshal(resp, &evt))
	assert.Equal(t, "r1", evt.RoomID)
	assert.Equal(t, 3, evt.Version)
	assert.Len(t, evt.PublicKey, 65)
}
```

`captureNatsReplyError` and `captureNatsReplyJSON` are small helpers — add to a `_test.go` file using an `nats.Conn` against an embedded test server, or use `natsutil.ReplyError`/`ReplyJSON` interception via a fake `Msg.RespondMsg`. If the project already has a NATS test harness (search for it), reuse it.

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=room-worker
```

- [ ] **Step 3: Implement handler**

Add to `room-worker/handler.go`:

```go
type roomKeyGetRequest struct {
	RoomID string `json:"roomId"`
}

// NatsHandleGetRoomKey serves the inter-site key-fetch RPC.
func (h *Handler) NatsHandleGetRoomKey(m otelnats.Msg) {
	ctx := wrappedCtx(m)
	var req roomKeyGetRequest
	if err := json.Unmarshal(m.Msg.Data, &req); err != nil {
		natsutil.ReplyError(m.Msg, model.ErrorResponse{Code: http.StatusBadRequest, Message: "invalid request"})
		return
	}
	if h.keyStore == nil {
		natsutil.ReplyError(m.Msg, model.ErrorResponse{Code: http.StatusServiceUnavailable, Message: "key store not configured"})
		return
	}
	pair, err := h.keyStore.Get(ctx, req.RoomID)
	if err != nil {
		slog.Error("get room key", "error", err, "roomId", req.RoomID)
		natsutil.ReplyError(m.Msg, model.ErrorResponse{Code: http.StatusInternalServerError, Message: "get room key"})
		return
	}
	if pair == nil {
		natsutil.ReplyError(m.Msg, model.ErrorResponse{Code: http.StatusNotFound, Message: "room key not found"})
		return
	}
	natsutil.ReplyJSON(m.Msg, model.RoomKeyEvent{
		RoomID:    req.RoomID,
		Version:   pair.Version,
		PublicKey: pair.KeyPair.PublicKey,
		PrivateKey: pair.KeyPair.PrivateKey,
		Timestamp: time.Now().UTC().UnixMilli(),
	})
}
```

- [ ] **Step 4: Register in `main.go`**

After existing subscription registration:

```go
if keyStore != nil {
	if _, err := nc.QueueSubscribe(subject.ServerRoomKeyGet(cfg.SiteID), "room-worker", func(m *nats.Msg) {
		handler.NatsHandleGetRoomKey(otelnats.Msg{Msg: m})
	}); err != nil {
		slog.Error("subscribe roomkey get", "error", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=room-worker
make lint
```

- [ ] **Step 6: Commit**

```bash
git add room-worker/handler.go room-worker/handler_test.go room-worker/main.go
git commit -m "feat(room-worker): add NatsHandleGetRoomKey RPC for cross-site replication"
```

---

# PART 3 — `inbox-worker` + integration + docs

> **Drift notice — read first.** Part 3 below was drafted around an
> earlier design in which `inbox-worker` ran its own `Set`/`Rotate` on
> local Valkey, owned a `roomkeysender.Sender`, and fanned `RoomKeyEvent`s
> out to local users. The shipped implementation is materially different:
>
> - The sole user-event publisher is the **origin** `room-worker`. The
>   NATS supercluster routes `chat.user.{account}.event.room.key` to
>   home sites, so a remote `inbox-worker` does **not** need (and does
>   not have) a `Sender`. `inbox-worker.NewHandler` takes
>   `(store, siteID, keyStore, interSiteClient)` — no sender argument.
> - The cross-site replication primitive is `SetWithVersion(roomID, pair,
>   originVersion)`, not `Set`/`Rotate`. `inbox-worker` mirrors origin's
>   exact version into local Valkey so the local `broadcast-worker`'s
>   on-wire envelopes carry the version every client across every site
>   already holds. Calling `Rotate` locally would diverge versions and
>   break cross-site decryption.
> - `VALKEY_ADDR` is required at startup (`env:"VALKEY_ADDR,required"`);
>   `ROOM_KEY_RPC_TIMEOUT`, `ROOM_KEY_MAX_REDELIVER`, and
>   `VALKEY_KEY_GRACE_PERIOD` are validated for `> 0` at startup too. There
>   is no "optional Valkey, silent skip" mode.
> - On `member_removed` with empty `Accounts`, `inbox-worker` still calls
>   `fetchAndStoreKey` — the key rotated on origin even when no local sub
>   was deleted.
>
> Treat the per-task body that follows as historical implementation notes
> rather than literal guidance. The authoritative descriptions live in
> [`docs/superpowers/specs/2026-05-08-room-encryption-keys-design.md`](../specs/2026-05-08-room-encryption-keys-design.md)
> ("Architecture & Data Flow", "Fan-out ownership summary") and the
> shipped code under `inbox-worker/`.

## Task 15: `inbox-worker` Valkey + sender + inter-site client wiring

**Files:**
- Modify: `inbox-worker/main.go`, `inbox-worker/store.go`, `inbox-worker/handler.go`
- Create: `inbox-worker/intersite_key.go`, `inbox-worker/intersite_key_test.go`
- Regenerate: `inbox-worker/mock_store_test.go`

- [ ] **Step 1: Add config + interfaces**

Add to `inbox-worker/main.go` `config`:

```go
	ValkeyAddr           string        `env:"VALKEY_ADDR"`
	ValkeyPassword       string        `env:"VALKEY_PASSWORD"           envDefault:""`
	ValkeyKeyGracePeriod time.Duration `env:"VALKEY_KEY_GRACE_PERIOD"   envDefault:"24h"`
```

Append to `inbox-worker/store.go`:

```go
// Local Valkey-backed keystore used by inbox-worker.
type RoomKeyStore interface {
	Get(ctx context.Context, roomID string) (*roomkeystore.VersionedKeyPair, error)
	Set(ctx context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error)
	Rotate(ctx context.Context, roomID string, newPair roomkeystore.RoomKeyPair) (int, error)
}

// Cross-site RPC for fetching the keypair from origin.
type InterSiteKeyClient interface {
	GetRoomKey(ctx context.Context, originSiteID, roomID string) (*model.RoomKeyEvent, error)
}
```

Add import: `"github.com/hmchangw/chat/pkg/roomkeystore"`.

- [ ] **Step 2: Failing test for the NATS client**

Create `inbox-worker/intersite_key_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

func TestNatsInterSiteKeyClient_GetRoomKey(t *testing.T) {
	srv := startTestNatsServer(t) // existing helper or testcontainers-go nats module
	nc, err := nats.Connect(srv.URL)
	require.NoError(t, err)
	defer nc.Close()

	_, err = nc.Subscribe(subject.ServerRoomKeyGet("site-a"), func(m *nats.Msg) {
		evt := model.RoomKeyEvent{RoomID: "r1", Version: 2, PublicKey: []byte("pk"), PrivateKey: []byte("sk")}
		data, _ := json.Marshal(evt)
		_ = m.Respond(data)
	})
	require.NoError(t, err)

	c := newNatsInterSiteKeyClient(nc, 2*time.Second)
	got, err := c.GetRoomKey(context.Background(), "site-a", "r1")
	require.NoError(t, err)
	assert.Equal(t, 2, got.Version)
}
```

- [ ] **Step 3: Run — expect FAIL**

```bash
make test SERVICE=inbox-worker
```

- [ ] **Step 4: Implement client**

Create `inbox-worker/intersite_key.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/subject"
)

type natsInterSiteKeyClient struct {
	nc      *nats.Conn
	timeout time.Duration
}

func newNatsInterSiteKeyClient(nc *nats.Conn, timeout time.Duration) *natsInterSiteKeyClient {
	return &natsInterSiteKeyClient{nc: nc, timeout: timeout}
}

func (c *natsInterSiteKeyClient) GetRoomKey(ctx context.Context, originSiteID, roomID string) (*model.RoomKeyEvent, error) {
	body, err := json.Marshal(map[string]string{"roomId": roomID})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.nc.RequestWithContext(rctx, subject.ServerRoomKeyGet(originSiteID), body)
	if err != nil {
		return nil, fmt.Errorf("rpc roomkey get: %w", err)
	}
	if errResp, ok := natsutil.ParseError(resp.Data); ok {
		return nil, fmt.Errorf("origin error %d: %s", errResp.Code, errResp.Message)
	}
	var evt model.RoomKeyEvent
	if err := json.Unmarshal(resp.Data, &evt); err != nil {
		return nil, fmt.Errorf("unmarshal reply: %w", err)
	}
	return &evt, nil
}
```

(If `natsutil.ParseError` doesn't exist, write a minimal local helper that detects `{"code":...,"message":...}` shape.)

- [ ] **Step 5: Wire in `main.go`**

After Mongo + NATS connect, before `NewHandler`:

```go
var keyStore RoomKeyStore
var keySender *roomkeysender.Sender
var interSiteClient InterSiteKeyClient
if cfg.ValkeyAddr != "" {
	ks, err := roomkeystore.NewValkeyStore(roomkeystore.Config{
		Addr: cfg.ValkeyAddr, Password: cfg.ValkeyPassword, GracePeriod: cfg.ValkeyKeyGracePeriod,
	})
	if err != nil {
		slog.Error("valkey connect failed", "error", err)
		os.Exit(1)
	}
	keyStore = ks
	keySender = roomkeysender.NewSender(natsPublisherAdapter{nc: nc})
	interSiteClient = newNatsInterSiteKeyClient(nc, 5*time.Second)
}
```

Update `NewHandler` signature accordingly (next task).

- [ ] **Step 6: Run — expect PASS**

```bash
make test SERVICE=inbox-worker
make lint
```

- [ ] **Step 7: Commit**

```bash
git add inbox-worker/main.go inbox-worker/store.go inbox-worker/intersite_key.go inbox-worker/intersite_key_test.go inbox-worker/mock_store_test.go
git commit -m "feat(inbox-worker): wire Valkey keystore + sender + inter-site key client"
```

---

## Task 16: Extend `inbox-worker.handleRoomCreated`

**Files:**
- Modify: `inbox-worker/handler.go`, `inbox-worker/handler_test.go`

- [ ] **Step 1: Failing test**

Append:

```go
func TestHandleRoomCreated_RPCsOriginAndFansOut(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockInboxStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	client := NewMockInterSiteKeyClient(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	// Existing happy-path setup for replicating subs:
	// store.EXPECT().FindUsersByAccounts(...).Return(...)
	// store.EXPECT().BulkCreateSubscriptions(...).Return(nil)
	// ...

	client.EXPECT().GetRoomKey(gomock.Any(), "site-origin", "r1").Return(&model.RoomKeyEvent{
		RoomID: "r1", Version: 1, PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x06}, 32),
	}, nil)
	keyStore.EXPECT().Set(gomock.Any(), "r1", gomock.Any()).Return(0, nil)

	h := NewHandler(store, "site-b", keyStore, keySender, client)

	outbox := model.RoomCreatedOutbox{RoomID: "r1", HomeSiteID: "site-origin", Accounts: []string{"bob"}, Timestamp: time.Now().UnixMilli()}
	pData, _ := json.Marshal(outbox)
	envelope := model.OutboxEvent{Type: model.OutboxTypeRoomCreated, SiteID: "site-origin", DestSiteID: "site-b", Payload: pData}
	require.NoError(t, h.handleRoomCreated(context.Background(), &envelope))
	assert.GreaterOrEqual(t, pubMock.publishCount(), 1)
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=inbox-worker
```

- [ ] **Step 3: Extend `Handler` and `handleRoomCreated`**

Update `Handler` struct + constructor in `inbox-worker/handler.go`:

```go
type Handler struct {
	store           InboxStore
	siteID          string
	keyStore        RoomKeyStore
	keySender       *roomkeysender.Sender
	interSiteClient InterSiteKeyClient
}

func NewHandler(store InboxStore, siteID string, keyStore RoomKeyStore, keySender *roomkeysender.Sender, client InterSiteKeyClient) *Handler {
	return &Handler{store: store, siteID: siteID, keyStore: keyStore, keySender: keySender, interSiteClient: client}
}
```

(If existing `NewHandler` doesn't take `siteID`, look up where the worker uses its site ID and adapt.)

At the end of `handleRoomCreated`, after subscription replication succeeds, append:

```go
	if err := h.replicateRoomKey(ctx, evt.SiteID, payload.RoomID, payload.Accounts); err != nil {
		slog.Error("replicate room key", "error", err, "roomId", payload.RoomID)
	}
	return nil
```

Add helper near the bottom:

```go
// Pull keypair from origin, write to local Valkey, fan out to listed accounts.
func (h *Handler) replicateRoomKey(ctx context.Context, originSiteID, roomID string, accounts []string) error {
	if h.keyStore == nil || h.keySender == nil || h.interSiteClient == nil {
		return nil
	}
	evt, err := h.interSiteClient.GetRoomKey(ctx, originSiteID, roomID)
	if err != nil {
		return fmt.Errorf("rpc origin: %w", err)
	}
	pair := roomkeystore.RoomKeyPair{PublicKey: evt.PublicKey, PrivateKey: evt.PrivateKey}
	if _, err := h.keyStore.Set(ctx, roomID, pair); err != nil {
		return fmt.Errorf("set local: %w", err)
	}
	for _, acct := range accounts {
		if err := h.keySender.Send(acct, evt); err != nil {
			slog.Error("send room key", "error", err, "account", acct, "roomId", roomID)
		}
	}
	return nil
}
```

- [ ] **Step 4: Update `main.go` constructor call**

```go
handler := NewHandler(store, cfg.SiteID, keyStore, keySender, interSiteClient)
```

- [ ] **Step 5: Update existing fixture tests**

Tests calling `NewHandler(store)` need updating to `NewHandler(store, "site-x", nil, nil, nil)`.

- [ ] **Step 6: Run — expect PASS**

```bash
make test SERVICE=inbox-worker
make lint
```

- [ ] **Step 7: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go inbox-worker/main.go
git commit -m "feat(inbox-worker): replicate room key on handleRoomCreated"
```

---

## Task 17: Extend `inbox-worker.handleMemberAdded` and `handleMemberRemoved`

**Files:**
- Modify: `inbox-worker/handler.go`, `inbox-worker/handler_test.go`

- [ ] **Step 1: Failing tests**

Append:

```go
func TestHandleMemberAdded_FetchesKeyOnLocalMiss(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockInboxStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	client := NewMockInterSiteKeyClient(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	// existing add-member happy-path replicating subs ...
	keyStore.EXPECT().Get(gomock.Any(), "r1").Return(nil, nil)
	client.EXPECT().GetRoomKey(gomock.Any(), "site-origin", "r1").Return(&model.RoomKeyEvent{
		RoomID: "r1", Version: 2, PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x07}, 32),
	}, nil)
	keyStore.EXPECT().Set(gomock.Any(), "r1", gomock.Any()).Return(0, nil)

	h := NewHandler(store, "site-b", keyStore, keySender, client)

	memberAdded := model.MemberAddEvent{RoomID: "r1", Accounts: []string{"charlie"}, SiteID: "site-origin"}
	pData, _ := json.Marshal(memberAdded)
	envelope := model.OutboxEvent{Type: model.OutboxTypeMemberAdded, SiteID: "site-origin", DestSiteID: "site-b", Payload: pData}
	require.NoError(t, h.handleMemberAdded(context.Background(), &envelope))
	assert.GreaterOrEqual(t, pubMock.publishCount(), 1)
}

func TestHandleMemberRemoved_RotatesLocalAndFansOutSurvivors(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockInboxStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	client := NewMockInterSiteKeyClient(ctrl)
	pubMock := newMockPublisher(t)
	keySender := roomkeysender.NewSender(pubMock)

	// existing remove-member subs-delete fixture ...
	client.EXPECT().GetRoomKey(gomock.Any(), "site-origin", "r1").Return(&model.RoomKeyEvent{
		RoomID: "r1", Version: 5, PublicKey: bytes.Repeat([]byte{0x04}, 65), PrivateKey: bytes.Repeat([]byte{0x08}, 32),
	}, nil)
	keyStore.EXPECT().Rotate(gomock.Any(), "r1", gomock.Any()).Return(5, nil)
	store.EXPECT().ListSubscriptions(gomock.Any(), "r1").Return([]*model.Subscription{
		{User: model.SubscriptionUser{Account: "alice"}, RoomID: "r1", SiteID: "site-b"},
	}, nil)

	h := NewHandler(store, "site-b", keyStore, keySender, client)

	rmv := model.MemberRemoveEvent{RoomID: "r1", Accounts: []string{"bob"}, SiteID: "site-origin", NewKeyVersion: 5}
	pData, _ := json.Marshal(rmv)
	envelope := model.OutboxEvent{Type: model.OutboxTypeMemberRemoved, SiteID: "site-origin", DestSiteID: "site-b", Payload: pData}
	require.NoError(t, h.handleMemberRemoved(context.Background(), &envelope))
	for _, subj := range pubMock.subjects {
		assert.NotContains(t, subj, ".bob.")
	}
}
```

- [ ] **Step 2: Run — expect FAIL**

```bash
make test SERVICE=inbox-worker
```

- [ ] **Step 3: Extend `handleMemberAdded`**

After existing sub-replication succeeds:

```go
	if h.keyStore != nil && h.keySender != nil && h.interSiteClient != nil {
		var pair *roomkeystore.VersionedKeyPair
		pair, err := h.keyStore.Get(ctx, payload.RoomID)
		if err != nil {
			slog.Error("get local key", "error", err, "roomId", payload.RoomID)
		}
		var evt *model.RoomKeyEvent
		if pair != nil {
			evt = &model.RoomKeyEvent{RoomID: payload.RoomID, Version: pair.Version,
				PublicKey: pair.KeyPair.PublicKey, PrivateKey: pair.KeyPair.PrivateKey}
		} else {
			fetched, err := h.interSiteClient.GetRoomKey(ctx, evt2.SiteID, payload.RoomID)
			if err != nil {
				slog.Error("rpc origin", "error", err, "roomId", payload.RoomID)
			} else {
				if _, err := h.keyStore.Set(ctx, payload.RoomID, roomkeystore.RoomKeyPair{
					PublicKey: fetched.PublicKey, PrivateKey: fetched.PrivateKey,
				}); err != nil {
					slog.Error("set local key", "error", err, "roomId", payload.RoomID)
				}
				evt = fetched
			}
		}
		if evt != nil {
			for _, acct := range payload.Accounts {
				if err := h.keySender.Send(acct, evt); err != nil {
					slog.Error("send room key", "error", err, "account", acct)
				}
			}
		}
	}
```

(`evt2` here is whatever the surrounding code names the unmarshaled `OutboxEvent`. Use the existing variable name.)

- [ ] **Step 4: Extend `handleMemberRemoved`**

After existing sub-deletes:

```go
	if h.keyStore != nil && h.keySender != nil && h.interSiteClient != nil {
		fetched, err := h.interSiteClient.GetRoomKey(ctx, evt.SiteID, payload.RoomID)
		if err != nil {
			slog.Error("rpc origin", "error", err, "roomId", payload.RoomID)
			return nil
		}
		pair := roomkeystore.RoomKeyPair{PublicKey: fetched.PublicKey, PrivateKey: fetched.PrivateKey}
		if _, err := h.keyStore.Rotate(ctx, payload.RoomID, pair); err != nil {
			if errors.Is(err, roomkeystore.ErrNoCurrentKey) {
				if _, err := h.keyStore.Set(ctx, payload.RoomID, pair); err != nil {
					slog.Error("set local key (fallback)", "error", err, "roomId", payload.RoomID)
				}
			} else {
				slog.Error("rotate local key", "error", err, "roomId", payload.RoomID)
			}
		}
		subs, err := h.store.ListSubscriptions(ctx, payload.RoomID)
		if err != nil {
			slog.Error("list subs", "error", err, "roomId", payload.RoomID)
			return nil
		}
		for _, sub := range subs {
			if sub.SiteID != h.siteID && sub.SiteID != "" {
				continue
			}
			if err := h.keySender.Send(sub.User.Account, fetched); err != nil {
				slog.Error("send room key", "error", err, "account", sub.User.Account)
			}
		}
	}
```

If `ListSubscriptions` isn't on `InboxStore`, add it to the interface and implement.

- [ ] **Step 5: Run — expect PASS**

```bash
make test SERVICE=inbox-worker
make lint
```

- [ ] **Step 6: Commit**

```bash
git add inbox-worker/handler.go inbox-worker/handler_test.go inbox-worker/store.go inbox-worker/main.go inbox-worker/mock_store_test.go
git commit -m "feat(inbox-worker): replicate key on add-member and rotate on remove-member"
```

---

## Task 18: Integration tests

**Files:**
- Modify: `room-service/integration_test.go`, `room-worker/integration_test.go`, `inbox-worker/integration_test.go`

- [ ] **Step 1: room-service integration**

Add a `//go:build integration`-tagged test:

```go
func TestIntegration_CreateRoom_PersistsKeyInValkey(t *testing.T) {
	ctx := context.Background()
	valkeyAddr := setupValkey(ctx, t)
	keyStore, err := roomkeystore.NewValkeyStore(roomkeystore.Config{Addr: valkeyAddr, GracePeriod: 24 * time.Hour})
	require.NoError(t, err)
	defer keyStore.Close()

	// ... boot a Mongo container, mount keyStore onto Handler ...
	// drive a real handleCreateRoom call, assert keyStore.Get returns a non-nil pair.
}
```

(`setupValkey` follows the project's testcontainers idiom — a generic `valkey/valkey:8` container.)

- [ ] **Step 2: room-worker integration**

Add a test that drives a full create-room canonical event through the worker against real Mongo + Valkey + NATS, asserting `RoomKeyEvent` is published on `chat.user.{account}.event.room.key`.

- [ ] **Step 3: inbox-worker two-site integration**

Spin up two `room-worker` instances each with their own Valkey + Mongo; one is "origin", the other publishes a `room_created` outbox to the inbox-worker on the second site; assert the second site's Valkey ends up with the same keypair after RPC + Set.

```go
func TestIntegration_CrossSiteKeyReplication(t *testing.T) {
	// origin site
	originValkey := setupValkey(ctx, t)
	originKS, _ := roomkeystore.NewValkeyStore(...)
	// register NatsHandleGetRoomKey on origin

	// destination site
	destValkey := setupValkey(ctx, t)
	destKS, _ := roomkeystore.NewValkeyStore(...)

	// seed origin with a keypair via originKS.Set("r1", pair)
	// drive inbox-worker.handleRoomCreated on dest with an outbox payload pointing at origin
	// assert destKS.Get("r1") == seeded pair
}
```

- [ ] **Step 4: Run integration suite**

```bash
make test-integration
```

- [ ] **Step 5: Commit**

```bash
git add room-service/integration_test.go room-worker/integration_test.go inbox-worker/integration_test.go
git commit -m "test: integration tests for key persistence and cross-site replication"
```

---

## Task 19: Docs and docker-compose updates

**Files:**
- Modify: `docs/client-api.md`, `docker-local/docker-compose.yml`, `room-worker/deploy/docker-compose.yml`, `inbox-worker/deploy/docker-compose.yml`

- [ ] **Step 1: Update `docs/client-api.md`**

Append a new section "Room Encryption Keys":

```markdown
## Room Encryption Keys

Clients receive a per-room P-256 key pair on the subject:

    chat.user.{account}.event.room.key

Payload:

    { "roomId": "...", "version": <int>, "publicKey": "<base64>", "privateKey": "<base64>", "timestamp": <unixMillis> }

Clients maintain a `(roomId, version) -> privateKey` map. Encrypted messages
arriving via the room event subject embed the `version` they were encrypted
under; clients select the matching private key.

When a member is removed from a channel, the server rotates the room key.
Surviving members receive a new RoomKeyEvent with an incremented `version`.
Clients should retain old versions to support history scrolling — the server
keeps the previous version for at least `VALKEY_KEY_GRACE_PERIOD`.

Removed members stop receiving RoomKeyEvents for that room. Their stored
private keys still decrypt history but cannot decrypt messages sent after
their removal.
```

- [ ] **Step 2: Update docker-compose files**

In `docker-local/docker-compose.yml`, ensure `valkey` service exists, then add to `room-worker` and `inbox-worker` env blocks:

```yaml
      VALKEY_ADDR: valkey:6379
      VALKEY_PASSWORD: ""
      VALKEY_KEY_GRACE_PERIOD: 24h
```

Mirror in each service's `deploy/docker-compose.yml`.

- [ ] **Step 3: Commit**

```bash
git add docs/client-api.md docker-local/docker-compose.yml room-worker/deploy/docker-compose.yml inbox-worker/deploy/docker-compose.yml
git commit -m "docs: room encryption keys client API + docker-compose Valkey wiring"
```

---

## Task 20: Final verification + push

- [ ] **Step 1: Full test sweep**

```bash
make test
make test-integration
make lint
```

- [ ] **Step 2: Push**

```bash
git push -u origin claude/room-encryption-keys-5vlQ2
```

---

## Self-Review Notes

**Spec coverage checklist:**

- Section 1 (Scope: create + add-member + remove-member + cross-site): Tasks 6, 7, 11, 12, 16, 17.
- Section 2 (Architecture: rotate-first, version assertion, fan-out): Tasks 7, 12.
- Section 3 (New code: subject builder, model fields, interfaces, RPC handler, helpers): Tasks 1, 2, 3, 5, 9, 14, 15.
- Section 4 (Failure modes): Tasks 6, 7, 10, 12 (each covers one row of the failure-modes table).
- Section 5 (Operational requirements: Valkey persistence, single master): Captured in `docs/client-api.md` and operational documentation outside this plan's scope.
- Section 6 (Configuration: env vars, gating on `VALKEY_ADDR`): Tasks 8, 15, 19.
- Section 7 (Removed user semantics): documented in `docs/client-api.md` (Task 19).
- Section 8 (Testing): Tasks 6, 7, 10, 11, 12, 14, 16, 17 (units) and Task 18 (integration).
- Section 9 (Workflow & commit plan): each task is one commit per the spec's TDD discipline.

**Add-member-without-existing-key behavior:** Add-member does NOT create a new key for un-keyed rooms — backfill behavior deferred to a follow-up. Task 11 returns a permanent error in this case.
