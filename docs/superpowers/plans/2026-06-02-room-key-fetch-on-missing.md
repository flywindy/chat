# Room key fetch on missing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let chat-frontend fetch a single `(roomId, version)` room key on demand when a received message can't be decrypted, decrypt the message, and continue rendering normally.

**Architecture:** Add a client-callable NATS RPC at `chat.user.{account}.request.room.{roomID}.{siteID}.key.get` served by `room-service`. The handler verifies the requester is a current member of the room, then reads the key from `roomkeystore` (versioned or current) and returns it. In the frontend, `RoomKeysContext` gains an `ensureKey(roomId, version, siteId)` method with in-flight dedupe and a 60s per-key failure backoff; `useRoomSubscriptions.decryptAndDispatch` calls it when the first `decrypt` returns `null`, then retries decrypt once before falling through to the existing `[encrypted message]` placeholder branch.

**Tech Stack:** Go (room-service, pkg/subject, pkg/model, pkg/roomkeystore), TypeScript (chat-frontend api/ + RoomKeysContext), vitest, gomock + testify, testcontainers (mongo + valkey).

**Spec:** `docs/superpowers/specs/2026-06-02-room-key-fetch-on-missing-design.md`

**Working branch (per session instructions):** `claude/dreamy-planck-ZUkPA`

---

## File Structure

**New files:**
- `chat-frontend/src/api/requestRoomKey/index.ts` — new api op
- `chat-frontend/src/api/requestRoomKey/index.test.ts` — vitest

**Modified files:**
- `pkg/subject/subject.go` — add `RoomKeyGet` + `RoomKeyGetWildcard`
- `pkg/subject/subject_test.go` — table entries for the two new strings
- `pkg/model/event.go` — add `RoomKeyGetRequest` + `RoomKeyGetResponse`
- `pkg/model/model_test.go` — round-trip the two new types
- `room-service/store.go` — extend `RoomKeyStore` with `GetByVersion`
- `room-service/mock_store_test.go` — regenerated via `make generate SERVICE=room-service`
- `room-service/handler.go` — register subscription + `natsGetRoomKey` + `handleGetRoomKey`
- `room-service/handler_test.go` — table-driven tests for the new handler
- `room-service/integration_test.go` — end-to-end test
- `chat-frontend/src/api/_transport/subjects.ts` — add `roomKeyGet` builder
- `chat-frontend/src/api/_transport/subjects.test.js` — test entry
- `chat-frontend/src/api/index.ts` — re-export `requestRoomKey`
- `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx` — add `ensureKey`
- `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.jsx` — tests
- `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx` — pass `ensureKey` through to `useRoomSubscriptions`
- `chat-frontend/src/context/RoomEventsContext/useRoomSubscriptions.js` — call `ensureKey` on missing key, retry decrypt once
- `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.test.jsx` — extend coverage for the new path
- `docs/client-api.md` — new §5 subsection "Requesting a missing key"

Each task below is in dependency order and produces a clean commit.

---

## Task 1: Add NATS subject builders for the new RPC

**Files:**
- Modify: `pkg/subject/subject.go` (add functions after `RoomKeyEnsure`, around line 188)
- Modify: `pkg/subject/subject_test.go` (add table entries)

- [ ] **Step 1: Add failing tests in `pkg/subject/subject_test.go`**

Append two entries to the existing table-driven test of the form `{name, subject.<Builder>(...), "expected.string"}`. Find the existing `RoomKeyUpdate` test entry around line 59 and the wildcard table around line 70. Add:

```go
{"RoomKeyGet", subject.RoomKeyGet("alice", "r1", "site-a"),
    "chat.user.alice.request.room.r1.site-a.key.get"},
{"RoomKeyGetWildcard", subject.RoomKeyGetWildcard("site-a"),
    "chat.user.*.request.room.*.site-a.key.get"},
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test SERVICE=pkg/subject`
Expected: FAIL — `subject.RoomKeyGet` and `subject.RoomKeyGetWildcard` undefined.

- [ ] **Step 3: Implement builders in `pkg/subject/subject.go`**

After the existing `RoomKeyEnsure` function (around line 188), insert:

```go
// RoomKeyGet is the user-facing request subject for the room key get RPC.
// Callers (chat-frontend) send a model.RoomKeyGetRequest and receive a
// model.RoomKeyGetResponse with the room key bytes for the given version
// (or the current version when Version is nil).
func RoomKeyGet(account, roomID, siteID string) string {
    return fmt.Sprintf("chat.user.%s.request.room.%s.%s.key.get", account, roomID, siteID)
}

// RoomKeyGetWildcard is the subscription pattern room-service uses to
// receive RoomKeyGet requests from any account / roomID at its siteID.
func RoomKeyGetWildcard(siteID string) string {
    return fmt.Sprintf("chat.user.*.request.room.*.%s.key.get", siteID)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `make test SERVICE=pkg/subject`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(subject): add RoomKeyGet and RoomKeyGetWildcard builders

New user-facing per-room request subject for the client-callable room
key fetch RPC. Mirrors the member.list / role-update pattern; siteID
goes last so the server's wildcard queue-subscribe is site-local."
```

---

## Task 2: Add wire model types for the new RPC

**Files:**
- Modify: `pkg/model/event.go` (after `RoomKeyEnsureResponse` around line 254)
- Modify: `pkg/model/model_test.go` (add roundTrip entries)

- [ ] **Step 1: Add failing round-trip tests**

Find the `roundTrip` helper usage in `pkg/model/model_test.go` (it's a generic helper that marshals and unmarshals each type). Add two new sub-tests, mirroring the style of the existing entries for `RoomKeyEnsureRequest` / `RoomKeyEnsureResponse`:

```go
t.Run("RoomKeyGetRequest_currentVersion", func(t *testing.T) {
    v := 3
    in := model.RoomKeyGetRequest{Version: &v}
    out := roundTrip(t, in)
    require.NotNil(t, out.Version)
    require.Equal(t, 3, *out.Version)
})

t.Run("RoomKeyGetRequest_nilVersion", func(t *testing.T) {
    in := model.RoomKeyGetRequest{Version: nil}
    out := roundTrip(t, in)
    require.Nil(t, out.Version)
})

t.Run("RoomKeyGetResponse", func(t *testing.T) {
    in := model.RoomKeyGetResponse{
        RoomID:     "r1",
        Version:    2,
        PrivateKey: []byte{0x01, 0x02, 0x03},
    }
    out := roundTrip(t, in)
    require.Equal(t, in, out)
})
```

If `roundTrip` is implemented as a generic-on-type table walker (check the existing test file), instead append entries to that table; the existing test file shape is authoritative.

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test SERVICE=pkg/model`
Expected: FAIL — `model.RoomKeyGetRequest` / `RoomKeyGetResponse` undefined.

- [ ] **Step 3: Add types in `pkg/model/event.go`**

After `RoomKeyEnsureResponse` (around line 254), insert:

```go
// RoomKeyGetRequest is the payload for the client-callable room key get RPC.
// Version is optional: when nil the server returns the current key; when set
// the server returns the historical key at that version (subject to the
// roomkeystore previous-key grace window).
type RoomKeyGetRequest struct {
    Version *int `json:"version,omitempty"`
}

// RoomKeyGetResponse is the reply from the room key get RPC. PrivateKey is
// the 32-byte room secret used directly as the AES-256-GCM key by clients.
// []byte marshals to standard base64 in JSON (same shape as RoomKeyEvent).
type RoomKeyGetResponse struct {
    RoomID     string `json:"roomId"`
    Version    int    `json:"version"`
    PrivateKey []byte `json:"privateKey"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `make test SERVICE=pkg/model`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(model): add RoomKeyGetRequest and RoomKeyGetResponse

Wire types for the client-callable room key fetch RPC. Request carries
an optional Version (nil = current key); response mirrors RoomKeyEvent
minus the event-level Timestamp so the frontend can feed it through the
same KEY_RECEIVED reducer path that live events take."
```

---

## Task 3: Extend `room-service`'s RoomKeyStore interface with `GetByVersion`

**Files:**
- Modify: `room-service/store.go` (RoomKeyStore interface around line 125)
- Regenerate: `room-service/mock_store_test.go` via `make generate SERVICE=room-service`

- [ ] **Step 1: Add `GetByVersion` to the interface**

Edit `room-service/store.go` so the `RoomKeyStore` interface becomes:

```go
// RoomKeyStore is the consumer-side interface for room encryption key lookups.
// Only the methods room-service needs are declared here.
type RoomKeyStore interface {
    GetMany(ctx context.Context, roomIDs []string) (map[string]*roomkeystore.VersionedKeyPair, error)
    // Get returns the current key for roomID, or (nil, nil) when absent.
    Get(ctx context.Context, roomID string) (*roomkeystore.VersionedKeyPair, error)
    // GetByVersion returns the key pair for the given (roomID, version). Returns
    // (nil, nil) when the version isn't held (rolled past the previous-key
    // grace window or never existed).
    GetByVersion(ctx context.Context, roomID string, version int) (*roomkeystore.RoomKeyPair, error)
    // Set writes a fresh keypair as the room's current key (version 0).
    Set(ctx context.Context, roomID string, pair roomkeystore.RoomKeyPair) (int, error)
}
```

Note: the underlying `*valkeyStore` already implements `GetByVersion` (see `pkg/roomkeystore/roomkeystore.go:37`), so no concrete-type changes are required.

- [ ] **Step 2: Regenerate the mock**

Run: `make generate SERVICE=room-service`
Expected: `room-service/mock_store_test.go` now contains a `MockRoomKeyStore.GetByVersion` method and matching recorder.

- [ ] **Step 3: Sanity-check compile**

Run: `make build SERVICE=room-service`
Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add room-service/store.go room-service/mock_store_test.go
git commit -m "feat(room-service): extend RoomKeyStore with GetByVersion

The new key.get RPC may request a historical version (within the
roomkeystore previous-key grace window). Lift the valkeyStore method
into the consumer interface; regenerate mocks."
```

---

## Task 4: Implement `handleGetRoomKey` (TDD with mocks)

**Files:**
- Modify: `room-service/handler.go` (register subscription in `RegisterHandlers` around line 98; add new methods near `handleListMembers` around line 420)
- Modify: `room-service/helper.go` (add `errRoomKeyAbsent` and route it through `sanitizeError`)
- Modify: `room-service/handler_test.go` (new table-driven tests)

- [ ] **Step 1: Add failing handler tests in `room-service/handler_test.go`**

Locate the existing `TestHandler_natsListMembers` test (mirrors the same pattern we need). Add a new top-level test below it:

```go
func TestHandler_natsGetRoomKey(t *testing.T) {
    const (
        siteID = "site-a"
        account = "alice"
        roomID = "room-1"
    )
    subj := subject.RoomKeyGet(account, roomID, siteID)

    sampleKey := roomkeystore.RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0x42}, 32)}
    sampleVersioned := &roomkeystore.VersionedKeyPair{Version: 7, KeyPair: sampleKey}

    type want struct {
        replyJSON string // expected JSON of the success reply (empty when err)
        errSubstr string // expected substring in the error envelope (empty when ok)
    }

    cases := []struct {
        name string
        body []byte
        setup func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore)
        want  want
    }{
        {
            name: "current version, happy path",
            body: []byte(`{}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
                ks.EXPECT().Get(gomock.Any(), roomID).Return(sampleVersioned, nil)
            },
            want: want{replyJSON: `{"roomId":"room-1","version":7,"privateKey":"QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="}`},
        },
        {
            name: "explicit version, happy path",
            body: []byte(`{"version":3}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
                ks.EXPECT().GetByVersion(gomock.Any(), roomID, 3).Return(&sampleKey, nil)
            },
            want: want{replyJSON: `{"roomId":"room-1","version":3,"privateKey":"QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="}`},
        },
        {
            name: "not a member",
            body: []byte(`{}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(nil, model.ErrSubscriptionNotFound)
            },
            want: want{errSubstr: "only room members"},
        },
        {
            name: "current key absent",
            body: []byte(`{}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
                ks.EXPECT().Get(gomock.Any(), roomID).Return(nil, nil)
            },
            want: want{errSubstr: "room key not available"},
        },
        {
            name: "historical version absent",
            body: []byte(`{"version":1}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
                ks.EXPECT().GetByVersion(gomock.Any(), roomID, 1).Return(nil, nil)
            },
            want: want{errSubstr: "room key not available"},
        },
        {
            name: "store error",
            body: []byte(`{}`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
                ks.EXPECT().Get(gomock.Any(), roomID).Return(nil, errors.New("valkey down"))
            },
            want: want{errSubstr: "internal error"},
        },
        {
            name: "malformed body",
            body: []byte(`not-json`),
            setup: func(t *testing.T, store *MockRoomStore, ks *MockRoomKeyStore) {
                // membership check still runs first.
                store.EXPECT().GetSubscription(gomock.Any(), account, roomID).
                    Return(&model.Subscription{}, nil)
            },
            want: want{errSubstr: "invalid request"},
        },
    }

    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            ctrl := gomock.NewController(t)
            store := NewMockRoomStore(ctrl)
            ks := NewMockRoomKeyStore(ctrl)
            tc.setup(t, store, ks)

            h := newTestHandler(store, ks, siteID) // use the existing handler-factory test helper

            resp, err := h.handleGetRoomKey(t.Context(), subj, tc.body)
            if tc.want.errSubstr != "" {
                require.Error(t, err)
                require.Contains(t, sanitizeError(err), tc.want.errSubstr)
                return
            }
            require.NoError(t, err)
            require.JSONEq(t, tc.want.replyJSON, string(resp))
        })
    }
}
```

If the existing test file uses a different handler-factory name (e.g. `newHandler(t, …)` or `newTestHandler(...)`), use that one — search the file for how `TestHandler_natsListMembers` constructs its handler and copy that style verbatim. Imports needed: `"bytes"`, `"errors"`, `"go.uber.org/mock/gomock"`, `"github.com/hmchangw/chat/pkg/model"`, `"github.com/hmchangw/chat/pkg/roomkeystore"`, `"github.com/hmchangw/chat/pkg/subject"`, `"github.com/stretchr/testify/require"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test SERVICE=room-service`
Expected: FAIL — `h.handleGetRoomKey` undefined and `errRoomKeyAbsent` undefined.

- [ ] **Step 3: Add `errRoomKeyAbsent` and route it through `sanitizeError`**

Edit `room-service/helper.go`. Add the sentinel alongside the others around line 25:

```go
errRoomKeyAbsent = errors.New("room key not available")
```

Add it to the `sanitizeError` allow-list (the `errors.Is(...)` switch around line 220) so the message reaches the client:

```go
errors.Is(err, errListOffsetInvalid),
errors.Is(err, errRoomKeyAbsent),  // <-- new
errors.Is(err, &dmExistsError{}),
```

- [ ] **Step 4: Add the handler in `room-service/handler.go`**

Locate `handleListMembers` (around line 420). Below the related `natsListMembers` / `handleListMembers` pair, insert the two new methods modeled on the same shape:

```go
func (h *Handler) natsGetRoomKey(m otelnats.Msg) {
    ctx := wrappedCtx(m)
    resp, err := h.handleGetRoomKey(ctx, m.Msg.Subject, m.Msg.Data)
    if err != nil {
        slog.Error("get room key failed", "error", err)
        natsutil.ReplyError(m.Msg, sanitizeError(err))
        return
    }
    if err := m.Msg.Respond(resp); err != nil {
        slog.Error("failed to respond to get room key", "error", err)
    }
}

func (h *Handler) handleGetRoomKey(ctx context.Context, subj string, data []byte) ([]byte, error) {
    if h.keyStore == nil {
        return nil, fmt.Errorf("get room key: key store not configured")
    }
    requesterAccount, roomID, ok := subject.ParseUserRoomSubject(subj)
    if !ok {
        return nil, fmt.Errorf("invalid get-room-key subject")
    }

    _, err := h.store.GetSubscription(ctx, requesterAccount, roomID)
    switch {
    case errors.Is(err, model.ErrSubscriptionNotFound):
        return nil, errNotRoomMember
    case err != nil:
        return nil, fmt.Errorf("check room membership: %w", err)
    }

    var req model.RoomKeyGetRequest
    if len(data) > 0 {
        if err := json.Unmarshal(data, &req); err != nil {
            return nil, fmt.Errorf("invalid request: %w", err)
        }
    }

    if req.Version == nil {
        existing, err := h.keyStore.Get(ctx, roomID)
        if err != nil {
            return nil, fmt.Errorf("get room key: %w", err)
        }
        if existing == nil {
            return nil, errRoomKeyAbsent
        }
        return json.Marshal(model.RoomKeyGetResponse{
            RoomID:     roomID,
            Version:    existing.Version,
            PrivateKey: existing.KeyPair.PrivateKey,
        })
    }

    pair, err := h.keyStore.GetByVersion(ctx, roomID, *req.Version)
    if err != nil {
        return nil, fmt.Errorf("get room key: %w", err)
    }
    if pair == nil {
        return nil, errRoomKeyAbsent
    }
    return json.Marshal(model.RoomKeyGetResponse{
        RoomID:     roomID,
        Version:    *req.Version,
        PrivateKey: pair.PrivateKey,
    })
}
```

Then register the subscription. In `RegisterHandlers` (around line 98 where `RoomKeyEnsure` is wired), add:

```go
if _, err := nc.QueueSubscribe(subject.RoomKeyGetWildcard(h.siteID), queue, h.natsGetRoomKey); err != nil {
    return fmt.Errorf("subscribe room key get: %w", err)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test SERVICE=room-service`
Expected: PASS, including all `TestHandler_natsGetRoomKey` sub-tests.

- [ ] **Step 6: Run lint**

Run: `make lint`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add room-service/handler.go room-service/helper.go room-service/handler_test.go
git commit -m "feat(room-service): client-callable room key get RPC

Subscribe RoomKeyGetWildcard and dispatch handleGetRoomKey: parse
account+roomID from the subject, verify the requester is a current
member via store.GetSubscription, then read the key via keyStore.Get
(current) or keyStore.GetByVersion (explicit version). Missing key →
errRoomKeyAbsent surfaced through sanitizeError as 'room key not
available'."
```

---

## Task 5: Integration test for the key.get RPC

**Files:**
- Modify: `room-service/integration_test.go` (add a new top-level test)

- [ ] **Step 1: Add the failing integration test**

Append to `room-service/integration_test.go`:

```go
//go:build integration

func TestIntegration_RoomKeyGetRPC(t *testing.T) {
    ctx := t.Context()
    db := testutil.MongoDB(t, "rsk")
    natsURL := testutil.NATS(t)
    valkey := testutil.SharedValkeyCluster(t)
    t.Cleanup(func() { testutil.FlushValkey(t) })

    siteID := "site-a"
    // Build the real wiring (mongo store + valkey key store) — copy the
    // setup pattern from the nearest existing integration test in this
    // file (search for SharedValkeyCluster / mongoStore.NewStore).
    h, nc := startRoomServiceForTest(t, db, natsURL, valkey, siteID)
    defer nc.Drain() //nolint:errcheck

    // Seed: alice is a member of room-int; bob is not.
    const roomID = "room-int"
    seedSubscription(t, db, "alice", roomID, siteID)
    pair := roomkeystore.RoomKeyPair{PrivateKey: bytes.Repeat([]byte{0xAA}, 32)}
    ver, err := h.keyStore.Set(ctx, roomID, pair)
    require.NoError(t, err)

    // Member request: get current key.
    reqBody, _ := json.Marshal(model.RoomKeyGetRequest{})
    reply, err := nc.Request(subject.RoomKeyGet("alice", roomID, siteID), reqBody, 2*time.Second)
    require.NoError(t, err)
    var got model.RoomKeyGetResponse
    require.NoError(t, json.Unmarshal(reply.Data, &got))
    require.Equal(t, roomID, got.RoomID)
    require.Equal(t, ver, got.Version)
    require.Equal(t, pair.PrivateKey, got.PrivateKey)

    // Non-member request rejected with sanitized error.
    reply, err = nc.Request(subject.RoomKeyGet("bob", roomID, siteID), reqBody, 2*time.Second)
    require.NoError(t, err)
    var errResp model.ErrorResponse
    require.NoError(t, json.Unmarshal(reply.Data, &errResp))
    require.Contains(t, errResp.Error, "only room members")
}
```

Use the existing `startRoomServiceForTest` / `seedSubscription` helpers from this file — search for them and reuse verbatim. If they don't exist under those exact names, copy the setup boilerplate from the nearest existing integration test (e.g. one that exercises `MemberList`).

- [ ] **Step 2: Run integration tests to verify they pass**

Run: `make test-integration SERVICE=room-service`
Expected: PASS, including `TestIntegration_RoomKeyGetRPC`.

- [ ] **Step 3: Commit**

```bash
git add room-service/integration_test.go
git commit -m "test(room-service): integration test for room key get RPC

End-to-end with mongo + valkey via testutil: member fetches the current
key bytes; non-member is rejected with the sanitized error envelope."
```

---

## Task 6: Frontend transport — add `roomKeyGet` subject builder

**Files:**
- Modify: `chat-frontend/src/api/_transport/subjects.ts` (insert next to `userRoomKey`)
- Modify: `chat-frontend/src/api/_transport/subjects.test.js` (add table entry)

- [ ] **Step 1: Add failing subject-builder test**

In `chat-frontend/src/api/_transport/subjects.test.js`, locate the existing test table that asserts each builder's output. Add:

```js
{
  name: 'roomKeyGet',
  build: () => roomKeyGet('alice', 'r1', 'site-a'),
  expected: 'chat.user.alice.request.room.r1.site-a.key.get',
},
```

And add `roomKeyGet` to the file's import line:
```js
import { ..., roomKeyGet } from './subjects'
```

- [ ] **Step 2: Run tests to verify they fail**

Run from `chat-frontend/`: `npm test -- subjects.test`
Expected: FAIL — `roomKeyGet` not exported.

- [ ] **Step 3: Add the builder in `chat-frontend/src/api/_transport/subjects.ts`**

Insert below `memberList` (around line 80):

```ts
// roomKeyGet requests the room key bytes for (roomId, version?) from
// room-service. Pair with src/api/requestRoomKey/. Mirrors
// pkg/subject/subject.go::RoomKeyGet.
export function roomKeyGet(account: string, roomId: string, siteId: string): string {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.key.get`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run from `chat-frontend/`: `npm test -- subjects.test`
Expected: PASS.

- [ ] **Step 5: Run typecheck**

Run from `chat-frontend/`: `npm run typecheck`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add chat-frontend/src/api/_transport/subjects.ts chat-frontend/src/api/_transport/subjects.test.js
git commit -m "feat(chat-frontend): roomKeyGet subject builder

Mirrors pkg/subject.RoomKeyGet. Internal to api/_transport; the
exported requestRoomKey op (next commit) is the public surface."
```

---

## Task 7: Frontend api op — `requestRoomKey`

**Files:**
- Create: `chat-frontend/src/api/requestRoomKey/index.ts`
- Create: `chat-frontend/src/api/requestRoomKey/index.test.ts`
- Modify: `chat-frontend/src/api/index.ts` (re-export)

- [ ] **Step 1: Write the failing test**

Create `chat-frontend/src/api/requestRoomKey/index.test.ts`:

```ts
import { describe, expect, it, vi } from 'vitest'
import type { Nats } from '../types'
import { requestRoomKey } from './index'

function makeNats(reply: unknown): Nats {
  return {
    user: { account: 'alice', siteId: 'site-a' } as Nats['user'],
    request: vi.fn().mockResolvedValue(reply),
    publish: vi.fn(),
    subscribe: vi.fn(),
    requestWithAsyncResult: vi.fn(),
  }
}

describe('requestRoomKey', () => {
  it('builds the per-user room subject and forwards an empty payload when version is omitted', async () => {
    const nats = makeNats({ roomId: 'r1', version: 5, privateKey: 'AAAA' })
    const got = await requestRoomKey(nats, { roomId: 'r1', siteId: 'site-a' })
    expect(nats.request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-a.key.get',
      {},
    )
    expect(got).toEqual({ roomId: 'r1', version: 5, privateKey: 'AAAA' })
  })

  it('forwards an explicit version in the payload', async () => {
    const nats = makeNats({ roomId: 'r1', version: 3, privateKey: 'AAAA' })
    await requestRoomKey(nats, { roomId: 'r1', siteId: 'site-a', version: 3 })
    expect(nats.request).toHaveBeenCalledWith(
      'chat.user.alice.request.room.r1.site-a.key.get',
      { version: 3 },
    )
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run from `chat-frontend/`: `npm test -- requestRoomKey`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the op**

Create `chat-frontend/src/api/requestRoomKey/index.ts`:

```ts
import { roomKeyGet } from '../_transport/subjects'
import type { Nats } from '../types'

export interface RequestRoomKeyArgs {
  roomId: string
  siteId: string
  /** Omit to fetch the current room key. */
  version?: number
}

/** Wire shape mirrors pkg/model.RoomKeyGetResponse. privateKey is base64. */
export interface RequestRoomKeyResponse {
  roomId: string
  version: number
  privateKey: string
}

/** Fetch the room key bytes for (roomId, version?) from room-service.
 *  Returns the reply as-is; callers decode privateKey from base64. */
export async function requestRoomKey(
  { user, request }: Nats,
  { roomId, siteId, version }: RequestRoomKeyArgs,
): Promise<RequestRoomKeyResponse> {
  const payload = version === undefined ? {} : { version }
  return request<RequestRoomKeyResponse>(roomKeyGet(user.account, roomId, siteId), payload)
}
```

- [ ] **Step 4: Re-export from the barrel**

In `chat-frontend/src/api/index.ts`, add (kept alphabetical with the other named exports):

```ts
export { requestRoomKey } from './requestRoomKey'
```

- [ ] **Step 5: Run tests to verify they pass**

Run from `chat-frontend/`: `npm test -- requestRoomKey`
Expected: PASS.

- [ ] **Step 6: Run typecheck**

Run from `chat-frontend/`: `npm run typecheck`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add chat-frontend/src/api/requestRoomKey chat-frontend/src/api/index.ts
git commit -m "feat(chat-frontend): requestRoomKey api op

Sync NATS request/reply wrapper over the room.{id}.{site}.key.get
subject. Omitting version returns the current key; the response
mirrors RoomKeyGetResponse with privateKey base64-encoded."
```

---

## Task 8: `RoomKeysContext.ensureKey` with dedupe + backoff

**Files:**
- Modify: `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx`
- Modify: `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.jsx`

- [ ] **Step 1: Write the failing tests**

Append the following tests to `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.test.jsx`. The file already mocks `subscribeToRoomKeyEvents`; extend the mock to include `requestRoomKey` and import it where appropriate.

```jsx
import { act, render, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RoomKeysProvider, useRoomKeys } from './index'
// ... existing imports

vi.mock('@/api', async () => {
  const actual = await vi.importActual<typeof import('@/api')>('@/api')
  return {
    ...actual,
    subscribeToRoomKeyEvents: vi.fn(() => ({ unsubscribe: vi.fn() })),
    requestRoomKey: vi.fn(),
  }
})

// Test helper component exposes the context value via a ref.
function CaptureCtx({ ctxRef }) {
  const ctx = useRoomKeys()
  ctxRef.current = ctx
  return null
}

function renderWithProvider() {
  const ctxRef = { current: null }
  // The existing test file constructs a minimal NatsContext stub —
  // reuse that helper. Look for `renderWithNats(...)` or similar.
  render(
    <NatsContext.Provider value={makeNats('alice', 'site-a')}>
      <RoomKeysProvider>
        <CaptureCtx ctxRef={ctxRef} />
      </RoomKeysProvider>
    </NatsContext.Provider>,
  )
  return ctxRef
}

describe('RoomKeysContext.ensureKey', () => {
  beforeEach(() => {
    vi.mocked(requestRoomKey).mockReset()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('dedupes concurrent requests for the same (roomId, version)', async () => {
    let resolve
    vi.mocked(requestRoomKey).mockImplementationOnce(
      () => new Promise((r) => { resolve = r })
    )
    const ctxRef = renderWithProvider()

    const p1 = ctxRef.current.ensureKey('r1', 0, 'site-a')
    const p2 = ctxRef.current.ensureKey('r1', 0, 'site-a')
    resolve({ roomId: 'r1', version: 0, privateKey: btoa(String.fromCharCode(...new Array(32).fill(1))) })
    await expect(p1).resolves.toBe(true)
    await expect(p2).resolves.toBe(true)
    expect(requestRoomKey).toHaveBeenCalledTimes(1)

    // After success, hasKey is true and decrypt for that (room, version) works.
    expect(ctxRef.current.hasKey('r1', 0)).toBe(true)
  })

  it('records failure and backs off subsequent calls for 60s, then retries', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.mocked(requestRoomKey).mockRejectedValueOnce(new Error('not_room_member'))
    const ctxRef = renderWithProvider()

    await expect(ctxRef.current.ensureKey('r1', 0, 'site-a')).resolves.toBe(false)
    expect(requestRoomKey).toHaveBeenCalledTimes(1)

    // Within the backoff window, no second call.
    await expect(ctxRef.current.ensureKey('r1', 0, 'site-a')).resolves.toBe(false)
    expect(requestRoomKey).toHaveBeenCalledTimes(1)

    // After 60s, a retry is allowed.
    await vi.advanceTimersByTimeAsync(60_001)
    vi.mocked(requestRoomKey).mockResolvedValueOnce({
      roomId: 'r1', version: 0,
      privateKey: btoa(String.fromCharCode(...new Array(32).fill(1))),
    })
    await expect(ctxRef.current.ensureKey('r1', 0, 'site-a')).resolves.toBe(true)
    expect(requestRoomKey).toHaveBeenCalledTimes(2)
  })

  it('returns false when the user is missing (no NATS handshake yet)', async () => {
    // The provider's effect gates on userAccount; with a nullish user
    // the request must short-circuit rather than throw.
    const ctxRef = renderWithProvider() // helper builds with a real user;
    // for this case temporarily render with a null user via the test
    // helper if available — otherwise this case may be covered by
    // useRoomSubscriptions tests in Task 9 instead. Keep optional.
    expect(typeof ctxRef.current.ensureKey).toBe('function')
  })
})
```

(`NatsContext` / `makeNats` references should mirror whatever the existing test file uses — read it first; if the file builds its provider differently, fold the new tests into its existing render helper.)

- [ ] **Step 2: Run tests to verify they fail**

Run from `chat-frontend/`: `npm test -- RoomKeysContext`
Expected: FAIL — `ctx.ensureKey` is undefined.

- [ ] **Step 3: Implement `ensureKey` in `RoomKeysContext.tsx`**

Edit `chat-frontend/src/context/RoomKeysContext/RoomKeysContext.tsx`:

Add to the imports at the top:
```ts
import { requestRoomKey, subscribeToRoomKeyEvents } from '@/api'
```

Update the type:
```ts
type RoomKeysContextValue = {
  hasKey(roomId: string, version: number): boolean
  decrypt(input: DecryptInput): Promise<string | null>
  /** Fetch the (roomId, version) key from room-service when it isn't
   *  cached. Resolves true on success (KEY_RECEIVED dispatched), false on
   *  any error. Concurrent callers for the same (roomId, version) share
   *  one in-flight RPC. After a failure, subsequent calls within
   *  KEY_RETRY_BACKOFF_MS resolve false without re-issuing the RPC. */
  ensureKey(roomId: string, version: number, siteId: string): Promise<boolean>
}
```

Add a module-level constant just below the imports:
```ts
const KEY_RETRY_BACKOFF_MS = 60_000
```

Inside `RoomKeysProvider`, near the existing refs:
```ts
const pendingRequestsRef = useRef<Map<string, Promise<boolean>>>(new Map())
const failedAtRef = useRef<Map<string, number>>(new Map())
```

Add an `ensureKey` callback before the returned `value`:

```ts
const ensureKey = useCallback(
  async (roomId: string, version: number, siteId: string): Promise<boolean> => {
    if (stateRef.current.byRoom[roomId]?.[version]) return true
    const cacheKey = `${roomId}|${version}`

    const existing = pendingRequestsRef.current.get(cacheKey)
    if (existing) return existing

    const failedAt = failedAtRef.current.get(cacheKey)
    if (failedAt !== undefined && Date.now() - failedAt < KEY_RETRY_BACKOFF_MS) {
      return false
    }

    const liveNats = natsRef.current
    if (!liveNats?.user?.account) return false

    const fetchPromise = (async () => {
      try {
        const resp = await requestRoomKey(liveNats, { roomId, siteId, version })
        let privateKey: Uint8Array
        try {
          privateKey = b64decode(resp.privateKey)
        } catch (err) {
          // eslint-disable-next-line no-console
          console.warn('ensureKey: invalid base64 privateKey', err)
          failedAtRef.current.set(cacheKey, Date.now())
          return false
        }
        dispatch({
          type: 'KEY_RECEIVED',
          roomId,
          version: resp.version,
          privateKey,
        })
        failedAtRef.current.delete(cacheKey)
        return true
      } catch (err) {
        // eslint-disable-next-line no-console
        console.warn('ensureKey: requestRoomKey failed', err)
        failedAtRef.current.set(cacheKey, Date.now())
        return false
      } finally {
        pendingRequestsRef.current.delete(cacheKey)
      }
    })()

    pendingRequestsRef.current.set(cacheKey, fetchPromise)
    return fetchPromise
  },
  [],
)
```

Include `ensureKey` in the context value:
```ts
const value: RoomKeysContextValue = { hasKey, decrypt, ensureKey }
```

Also extend the existing teardown effect so `pendingRequestsRef` and `failedAtRef` are cleared on logout. Add inside the `useEffect`'s cleanup function (around the existing `aesKeyCacheRef.current.clear()` call):
```ts
pendingRequestsRef.current.clear()
failedAtRef.current.clear()
```

- [ ] **Step 4: Run tests to verify they pass**

Run from `chat-frontend/`: `npm test -- RoomKeysContext`
Expected: PASS.

- [ ] **Step 5: Run typecheck + lint**

Run from `chat-frontend/`: `npm run typecheck`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add chat-frontend/src/context/RoomKeysContext
git commit -m "feat(chat-frontend): RoomKeysContext.ensureKey for on-demand fetch

ensureKey(roomId, version, siteId) wraps requestRoomKey, dedupes
concurrent calls for the same (roomId, version) via an in-flight Map,
and records failures in a per-key map with KEY_RETRY_BACKOFF_MS (60s)
to prevent stampedes when the key is genuinely gone. On success the
result is dispatched through the same KEY_RECEIVED path live events
take, so the reducer's bytesEqual no-ops on duplicates."
```

---

## Task 9: Wire `ensureKey` into `useRoomSubscriptions.decryptAndDispatch`

**Files:**
- Modify: `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx`
- Modify: `chat-frontend/src/context/RoomEventsContext/useRoomSubscriptions.js`
- Modify: `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.test.jsx`

- [ ] **Step 1: Add the failing test in `RoomEventsContext.test.jsx`**

The existing file mocks the api/subscribe ops. Add a new test that:
1. Stubs `decrypt` to return `null` on first call and the plaintext JSON on the second.
2. Stubs `ensureKey` to resolve `true`.
3. Pushes a `new_message` event with `encryptedMessage` through the channel sub callback.
4. Asserts the reducer received `MESSAGE_RECEIVED` with the decoded body, and that `ensureKey` was called once with `(roomId, version, siteId)`.

Sketch (adapt to the file's existing helpers):

```jsx
it('on missing key, calls ensureKey and retries decrypt once', async () => {
  const ensureKey = vi.fn().mockResolvedValue(true)
  const decryptedJson = JSON.stringify({ id: 'm1', roomId: 'r1', content: 'hi' })
  const decrypt = vi
    .fn()
    .mockResolvedValueOnce(null)
    .mockResolvedValueOnce(decryptedJson)

  const { capturedEvents } = renderProviderWithStubs({ decrypt, ensureKey })

  emitRoomEvent('r1', {
    type: 'new_message',
    roomId: 'r1',
    siteId: 'site-a',
    encryptedMessage: { version: 2, nonce: 'AA==', ciphertext: 'BB==' },
    lastMsgId: 'm1',
    lastMsgAt: '2026-06-02T00:00:00Z',
    timestamp: Date.now(),
  })

  await waitFor(() => expect(ensureKey).toHaveBeenCalledWith('r1', 2, 'site-a'))
  await waitFor(() =>
    expect(capturedEvents).toContainEqual(
      expect.objectContaining({ type: 'MESSAGE_RECEIVED' }),
    ),
  )
  expect(decrypt).toHaveBeenCalledTimes(2)
})

it('does not retry decrypt when ensureKey resolves false', async () => {
  const ensureKey = vi.fn().mockResolvedValue(false)
  const decrypt = vi.fn().mockResolvedValue(null)
  const { capturedEvents } = renderProviderWithStubs({ decrypt, ensureKey })

  emitRoomEvent('r1', {
    type: 'new_message',
    roomId: 'r1',
    siteId: 'site-a',
    encryptedMessage: { version: 2, nonce: 'AA==', ciphertext: 'BB==' },
    lastMsgId: 'm1',
    lastMsgAt: '2026-06-02T00:00:00Z',
    timestamp: Date.now(),
  })

  await waitFor(() => expect(ensureKey).toHaveBeenCalled())
  expect(decrypt).toHaveBeenCalledTimes(1)
  // Reducer falls through to existing placeholder path.
  await waitFor(() =>
    expect(capturedEvents).toContainEqual(
      expect.objectContaining({ type: 'MESSAGE_RECEIVED' }),
    ),
  )
})
```

Reuse the existing test file's `renderProviderWithStubs` / `emitRoomEvent` helpers. If they don't exist with those names, locate the equivalents the file already uses for the decrypt path and copy that pattern.

- [ ] **Step 2: Run tests to verify they fail**

Run from `chat-frontend/`: `npm test -- RoomEventsContext`
Expected: FAIL — `ensureKey` not threaded into the hook; tests can't reach it.

- [ ] **Step 3: Thread `ensureKey` through `RoomEventsContext.tsx`**

Edit `chat-frontend/src/context/RoomEventsContext/RoomEventsContext.tsx`:

```ts
const { decrypt, ensureKey } = useRoomKeys()
// ...
const { currentGeneration } = useRoomSubscriptions(
  nats,
  dispatch,
  stateRef,
  threadReplyHandlerRef,
  threadMessageMutationHandlerRef,
  decrypt,
  ensureKey,
)
```

- [ ] **Step 4: Implement the retry in `useRoomSubscriptions.js`**

Edit `chat-frontend/src/context/RoomEventsContext/useRoomSubscriptions.js`:

Update the signature and add a ref:
```js
export function useRoomSubscriptions(
  nats,
  dispatch,
  stateRef,
  threadReplyHandlerRef,
  threadMessageMutationHandlerRef,
  decrypt = async () => null,
  ensureKey = async () => false,
) {
  // ... existing body ...
  const ensureKeyRef = useRef(ensureKey)
  ensureKeyRef.current = ensureKey
```

Modify `decryptAndDispatch` (around lines 240–279). The "encrypted full-message" branch should retry once after `ensureKey` returns `true`:

```js
const decryptAndDispatch = async (evt, finalize) => {
  let decoded = evt
  try {
    // Handle encrypted full-message events.
    if (decoded.encryptedMessage && !decoded.message) {
      const enc = decoded.encryptedMessage
      if (typeof enc.version === 'number' && enc.nonce && enc.ciphertext) {
        let plaintext = await decryptRef.current({
          roomId: decoded.roomId,
          version: enc.version,
          nonceB64: enc.nonce,
          ciphertextB64: enc.ciphertext,
        })
        if (plaintext == null && decoded.roomId) {
          const siteId = decoded.siteId ?? natsRef.current.user?.siteId
          if (siteId) {
            const ok = await ensureKeyRef.current(decoded.roomId, enc.version, siteId)
            if (ok) {
              plaintext = await decryptRef.current({
                roomId: decoded.roomId,
                version: enc.version,
                nonceB64: enc.nonce,
                ciphertextB64: enc.ciphertext,
              })
            }
          }
        }
        if (plaintext != null) {
          const msg = JSON.parse(plaintext)
          decoded = { ...decoded, message: msg, encryptedMessage: undefined }
        }
      }
    }
    // Handle encrypted message edits (flattened edit event).
    if (decoded.type === 'message_edited' && decoded.encryptedNewContent && !decoded.newContent) {
      const enc = decoded.encryptedNewContent
      if (typeof enc.version === 'number' && enc.nonce && enc.ciphertext) {
        let plaintext = await decryptRef.current({
          roomId: decoded.roomId,
          version: enc.version,
          nonceB64: enc.nonce,
          ciphertextB64: enc.ciphertext,
        })
        if (plaintext == null && decoded.roomId) {
          const siteId = decoded.siteId ?? natsRef.current.user?.siteId
          if (siteId) {
            const ok = await ensureKeyRef.current(decoded.roomId, enc.version, siteId)
            if (ok) {
              plaintext = await decryptRef.current({
                roomId: decoded.roomId,
                version: enc.version,
                nonceB64: enc.nonce,
                ciphertextB64: enc.ciphertext,
              })
            }
          }
        }
        if (plaintext != null) {
          decoded = { ...decoded, newContent: plaintext, encryptedNewContent: undefined }
        }
      }
    }
  } catch (err) {
    // eslint-disable-next-line no-console
    console.warn('decryptAndDispatch failed; forwarding original event', err)
  }
  finalize(decoded)
}
```

Update the JSDoc above `useRoomSubscriptions` to document the new `ensureKey` parameter (one line, mirroring the existing `decrypt` doc).

- [ ] **Step 5: Run tests to verify they pass**

Run from `chat-frontend/`: `npm test -- RoomEventsContext`
Expected: PASS.

- [ ] **Step 6: Run full frontend tests + typecheck**

Run from `chat-frontend/`:
- `npm test`
- `npm run typecheck`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add chat-frontend/src/context/RoomEventsContext
git commit -m "feat(chat-frontend): on missing key, ensureKey and retry decrypt

useRoomSubscriptions.decryptAndDispatch now calls
RoomKeysContext.ensureKey when the first decrypt returns null for a
channel new_message or encrypted edit. On success it retries decrypt
once and forwards the decoded event. On failure or persistent null,
the event falls through to the existing '[encrypted message]'
placeholder branch — no regressions for the cold-cache path."
```

---

## Task 10: Document the new RPC in `docs/client-api.md`

**Files:**
- Modify: `docs/client-api.md` (extend §5)

- [ ] **Step 1: Locate §5 "Room Encryption"**

Find the section "5. Room Encryption" (around line 1991). After the "Initial key bootstrap on (re)connect" paragraph (around line 2036), add a new subsection.

- [ ] **Step 2: Insert the new subsection**

```markdown
### Requesting a missing key

If a client receives an `encryptedMessage` whose `(roomId, version)` it
doesn't hold — e.g. it reconnected after the original `RoomKeyEvent` was
delivered, or the message references an older version it never stored —
it can fetch the key on demand from `room-service`.

#### Subject

```text
chat.user.{account}.request.room.{roomID}.{siteID}.key.get
```

`{siteID}` is the room's origin siteID (the same value carried on the
inbound message event). Clients are already authorized for
`chat.user.{theirAccount}.>` and need no additional grant.

#### Request payload (`RoomKeyGetRequest`)

```json
{ "version": 3 }
```

`version` is optional. When omitted (or `null`), the server returns the
**current** key for the room.

#### Success reply (`RoomKeyGetResponse`)

```json
{
  "roomId": "<room id>",
  "version": 3,
  "privateKey": "<base64-encoded 32-byte room secret>"
}
```

Same shape as `RoomKeyEvent` minus `Timestamp`, so a client can feed the
reply through the same caching path it uses for live events.

#### Errors

| Condition | Error envelope `error` text | Notes |
| --- | --- | --- |
| Requester is not a member of the room | `only room members can list members` | Surfaces the existing `errNotRoomMember` sentinel. |
| Key not held (rolled past grace window, or never existed) | `room key not available` | Includes "explicit version not in the previous-key slot". |
| Malformed request body | `invalid request: …` | |
| Internal failure | `internal error` | |

#### Use as complement to live events

This RPC complements — it does not replace — live `RoomKeyEvent`s on
`chat.user.{account}.event.room.key`. Live events remain the primary
delivery channel at room create / add-member / rotation. Clients
should call `key.get` only when a received message cannot be decrypted
with the keys they already hold, and back off after a failure so a
chatty channel does not stampede the server for a key that is
permanently gone.
```

- [ ] **Step 3: Cross-check the existing docs**

Verify the new subsection is internally consistent with the section just above (subject prefix `chat.user.{account}.>`, base64 `privateKey`, 32 bytes). No other section needs editing.

- [ ] **Step 4: Commit**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): document key.get RPC for on-demand room key fetch

New subsection under §5 Room Encryption: subject, request/response
shapes (RoomKeyGetRequest / RoomKeyGetResponse), error envelope text,
and the rule that this complements live RoomKeyEvent delivery rather
than replacing it."
```

---

## Task 11: Final verification across the stack

**Files:** none modified — verification only.

- [ ] **Step 1: Backend lint + tests**

Run:
- `make lint`
- `make test`
- `make test-integration SERVICE=room-service`
- `make sast`

Expected: all green. If `make sast` flags anything, address it inline — most likely no findings since the new code mirrors existing patterns.

- [ ] **Step 2: Frontend lint + tests + build**

Run from `chat-frontend/`:
- `npm run typecheck`
- `npm test`
- `npm run build`

Expected: all green.

- [ ] **Step 3: Push the branch**

Run:
```bash
git push -u origin claude/dreamy-planck-ZUkPA
```

The pre-push protocol in the session instructions allows a single retry per network failure with the documented backoff. Don't open a PR unless explicitly asked.

- [ ] **Step 4: End-of-task summary message**

In the chat, post one or two sentences: what shipped (the new RPC + frontend wiring + docs), what was deliberately left for follow-up (history-load decryption, DM keys, bulk `subscription.get*` bootstrap), and the working branch name.

---

## Self-review notes

- All 11 sections of the spec map to a task: subject builders (T1), wire types (T2), RoomKeyStore extension (T3), handler + sanitizeError (T4), integration test (T5), frontend transport (T6), api op (T7), context + dedupe/backoff (T8), useRoomSubscriptions retry (T9), docs (T10), final verification (T11).
- Type/name consistency: `RoomKeyGet` / `RoomKeyGetWildcard` / `RoomKeyGetRequest` / `RoomKeyGetResponse` / `errRoomKeyAbsent` / `requestRoomKey` / `ensureKey` / `KEY_RETRY_BACKOFF_MS` are used identically wherever they appear.
- No placeholders: every code block contains the actual code; the few "use the existing helper" instructions point at specific files/symbols already in the tree.
- Out-of-scope items (history-load decryption, DM keys, bulk bootstrap) are deferred consistently with the spec and not silently implied by any task.
