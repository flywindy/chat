# Relax X-Request-ID Requirement for Server-to-Server RPCs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the `RoomsInfoBatch` (room-service) and `RoomCreateDMSync` (room-worker) server-to-server RPCs accept calls without an `X-Request-ID` header (minting one for logging), while every user-facing dedup-critical room-service handler keeps the strict requirement.

**Architecture:** Add a gin-style route **group** to `pkg/natsrouter` so a subset of routes can carry extra middleware on top of the base chain. room-service's base router mints (`RequestID()`); a strict group (`RequireRequestID()`) re-applies the requirement to every route except `RoomsInfoBatch`. room-worker (single route) swaps to a minting base globally, and its cross-site outbox dedup is made payload-derived so a minted/absent request ID can't break JetStream dedup.

**Tech Stack:** Go 1.25, NATS (`pkg/natsrouter`), `go.uber.org/mock`, `stretchr/testify`. Spec: `docs/superpowers/specs/2026-06-08-relax-request-id-server-rpc-design.md`.

---

## Environment note (read first)

Some verification steps use integration tests (`//go:build integration`, real NATS via testcontainers — needs Docker). The **mechanism** is proven by `pkg/natsrouter` and `room-worker` **unit** tests (no Docker). If Docker is unavailable during inline execution:
- Run the unit tests, `make lint`, and `make build` for each touched service (these gate the change).
- Write the integration tests anyway (they are correct and run in CI); note in the commit/summary that they were not run locally.

Run unit tests with the Makefile (it adds `-race`): `make test SERVICE=<name>` or, for `pkg/natsrouter`, `make test SERVICE=natsrouter` if supported, else `go test -race ./pkg/natsrouter/` is acceptable for local Red/Green (the Makefile is preferred where it accepts the target).

---

## Task 1: `pkg/natsrouter` — `registrar` interface + route `Group`

**Files:**
- Modify: `pkg/natsrouter/register.go` (change first param of 3 generic funcs from `*Router` to `registrar`)
- Create: `pkg/natsrouter/group.go` (the `registrar` interface, `Group`, `(*Router).Group`)
- Create: `pkg/natsrouter/group_test.go` (unit tests, no NATS)

- [ ] **Step 1: Write the failing tests** in `pkg/natsrouter/group_test.go`

```go
package natsrouter

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/natsutil"
)

// recordingRegistrar captures the (pattern, handlers) a Group forwards to its
// parent, so Group composition is unit-testable without a NATS subscription.
type recordingRegistrar struct {
	pattern  string
	handlers []HandlerFunc
}

func (r *recordingRegistrar) addRoute(pattern string, handlers []HandlerFunc) {
	r.pattern = pattern
	r.handlers = handlers
}

func TestGroup_PrependsMiddlewareInOrder(t *testing.T) {
	var order []string
	mkMW := func(tag string) HandlerFunc {
		return func(c *Context) { order = append(order, tag); c.Next() }
	}
	rec := &recordingRegistrar{}
	g := &Group{parent: rec, middleware: []HandlerFunc{mkMW("group1"), mkMW("group2")}}

	g.addRoute("subj.x", []HandlerFunc{func(c *Context) { order = append(order, "handler") }})

	require.Equal(t, "subj.x", rec.pattern)
	require.Len(t, rec.handlers, 3, "group middleware (2) + handler (1)")

	// Run the recorded chain to verify execution order.
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	c.chain.handlers = rec.handlers
	c.Next()
	assert.Equal(t, []string{"group1", "group2", "handler"}, order)
}

func TestRouterGroup_WiresParentAndMiddleware(t *testing.T) {
	r := New(nil, "q")
	g := r.Group(RequireRequestID())
	require.NotNil(t, g)
	p, ok := g.parent.(*Router)
	require.True(t, ok, "parent must be the originating *Router")
	assert.Same(t, r, p)
	assert.Len(t, g.middleware, 1)
}

// A base-router chain (RequestID only) mints a request ID for a header-less
// message and runs the handler — this is the relaxed RoomsInfoBatch posture.
func TestChain_BaseMintsAndRuns_NoHeader(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{}},
		chain: &chainState{index: -1},
	}
	var ran bool
	var seen string
	c.chain.handlers = []HandlerFunc{
		RequestID(),
		func(c *Context) { ran = true; seen = natsutil.RequestIDFromContext(c) },
	}
	c.Next()

	require.True(t, ran, "handler must run on the minting base chain")
	assert.NotEmpty(t, seen, "RequestID must mint an ID when the header is absent")
}

// A base + strict-group chain (RequestID then RequireRequestID) still rejects a
// header-less message — this is the strict posture for dedup-critical routes.
func TestChain_StrictGroupRejects_NoHeader(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequestID(),
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "strict group must reject a header-less message")
	assert.True(t, c.IsAborted())
}

func TestChain_StrictGroupPasses_ValidHeader(t *testing.T) {
	const id = "01970a4f-8c2d-7c9a-abcd-e0123456789f"
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{natsutil.RequestIDHeader: []string{id}}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequestID(),
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	require.True(t, ran, "strict group must pass a valid header through")
	assert.False(t, c.IsAborted())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./pkg/natsrouter/ -run 'TestGroup_|TestRouterGroup_|TestChain_'`
Expected: COMPILE FAILURE — `undefined: Group`, `g.parent undefined`, `r.Group undefined`. (The two `TestChain_*` tests would compile/pass once `Group` exists since they only use existing middleware, but the build fails until `Group` is defined, so the whole package fails to compile now.)

- [ ] **Step 3: Create `pkg/natsrouter/group.go`**

```go
package natsrouter

// registrar is the route-registration surface shared by *Router and *Group.
// The Register/RegisterNoBody/RegisterVoid helpers target it so routes can be
// registered on either the base router or a middleware group.
type registrar interface {
	addRoute(pattern string, handlers []HandlerFunc)
}

var (
	_ registrar = (*Router)(nil)
	_ registrar = (*Group)(nil)
)

// Group carries additional middleware that runs after the parent's middleware
// for every route registered on it. Create one with (*Router).Group. Routes on
// a group inherit the router's global middleware (prepended by Router.addRoute)
// followed by the group's middleware, then the route handler.
type Group struct {
	parent     registrar
	middleware []HandlerFunc
}

// Group returns a route group whose registered routes run mw after the router's
// global middleware (installed via Use) and before the route handler.
func (r *Router) Group(mw ...HandlerFunc) *Group {
	return &Group{parent: r, middleware: mw}
}

func (g *Group) addRoute(pattern string, handlers []HandlerFunc) {
	all := make([]HandlerFunc, 0, len(g.middleware)+len(handlers))
	all = append(all, g.middleware...)
	all = append(all, handlers...)
	g.parent.addRoute(pattern, all)
}
```

- [ ] **Step 4: Widen the `Register*` signatures in `pkg/natsrouter/register.go`**

Change the first parameter of all three generic functions from `r *Router` to `r registrar`. Exactly three edits:

`Register`:
```go
func Register[Req, Resp any](
	r registrar,
	pattern string,
	fn func(c *Context, req Req) (*Resp, error),
) {
```

`RegisterNoBody`:
```go
func RegisterNoBody[Resp any](
	r registrar,
	pattern string,
	fn func(c *Context) (*Resp, error),
) {
```

`RegisterVoid`:
```go
func RegisterVoid[Req any](
	r registrar,
	pattern string,
	fn func(c *Context, req Req) error,
) {
```

(The `r.addRoute(...)` calls inside each body are unchanged — both `*Router` and `*Group` satisfy `registrar`.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./pkg/natsrouter/ -run 'TestGroup_|TestRouterGroup_|TestChain_'`
Expected: PASS (5 tests).

- [ ] **Step 6: Run the full natsrouter unit suite + lint**

Run: `go test -race ./pkg/natsrouter/`
Expected: PASS (all existing tests still compile — every existing `Register(r, ...)` call passes a `*Router`, which satisfies `registrar`).
Run: `make lint`
Expected: no new findings.

- [ ] **Step 7: Commit**

```bash
git add pkg/natsrouter/group.go pkg/natsrouter/group_test.go pkg/natsrouter/register.go
git commit -m "feat(natsrouter): add route groups for per-route middleware"
```

---

## Task 2: `room-worker` — minting base + payload-derived outbox dedup

**Files:**
- Modify: `room-worker/handler_test.go:2900` (flip the dedup assertion to payload-derived — the Red step)
- Modify: `room-worker/handler.go:2867` test setup is in handler_test; implementation change is `room-worker/handler.go:1940-1945`
- Modify: `room-worker/main.go:154` (`RequireRequestID()` → `RequestID()`)
- Modify: `room-worker/integration_test.go:1210-1211` (comment only — payload-derived rationale)

- [ ] **Step 1: Update the failing assertion in `room-worker/handler_test.go`**

In `TestHandleSyncCreateDM_CrossSite_EmitsOutbox`, capture the created room and assert a payload-derived dedup ID. Replace the `CreateRoom` expectation and the final assertion.

Change the `CreateRoom` expectation (currently `room-worker/handler_test.go:2867`) from:
```go
	store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).Return(nil)
```
to:
```go
	var insertedRoom *model.Room
	store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, r *model.Room) error { insertedRoom = r; return nil })
```

Change the final assertion (currently `room-worker/handler_test.go:2900`) from:
```go
	assert.Equal(t, testRequestID+":site-b", outbox.msgID)
```
to:
```go
	// Dedup ID is payload-derived (room identity + createdAt + dest), NOT the
	// request ID — so a minted/absent X-Request-ID cannot break JetStream dedup.
	require.NotNil(t, insertedRoom)
	wantSeed := fmt.Sprintf("%s:%s:%d", insertedRoom.ID, "alice", insertedRoom.CreatedAt.UnixMilli())
	assert.Equal(t, wantSeed+":site-b", outbox.msgID)
	assert.NotContains(t, outbox.msgID, testRequestID, "dedup id must not embed the request ID")
```

Ensure `fmt` is imported in `room-worker/handler_test.go` (it almost certainly is; if `goimports`/lint flags it, add it).

- [ ] **Step 2: Add a header-less success + dedup test in `room-worker/handler_test.go`**

Add this test next to `TestHandleSyncCreateDM_CrossSite_EmitsOutbox`. It proves the handler works when the context carries **no** request ID (the mint/absent case) and that dedup is still payload-derived.

```go
// dmCtxNoID builds a sync-DM context with NO request ID — the relaxed,
// post-mint-boundary case (room-worker's router now mints instead of rejecting).
func dmCtxNoID() *natsrouter.Context {
	c := natsrouter.NewContext(map[string]string{})
	c.SetContext(context.Background())
	return c
}

func TestHandleSyncCreateDM_CrossSite_NoRequestID_PayloadDerivedDedup(t *testing.T) {
	h, store, capture := newSyncDMTestHandler(t)

	requester := &model.User{ID: "u-alice", Account: "alice", SiteID: "site-a"}
	other := &model.User{ID: "u-bob", Account: "bob", SiteID: "site-b"}
	store.EXPECT().FindUsersByAccounts(gomock.Any(), gomock.Any()).Return([]model.User{*requester, *other}, nil)
	var insertedRoom *model.Room
	store.EXPECT().CreateRoom(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, r *model.Room) error { insertedRoom = r; return nil })
	store.EXPECT().BulkCreateSubscriptions(gomock.Any(), gomock.Any()).Return(nil)
	store.EXPECT().FindDMSubscriptionPair(gomock.Any(), gomock.Any(), "alice").Return(
		&model.Subscription{User: model.SubscriptionUser{ID: "u-alice", Account: "alice"}},
		&model.Subscription{User: model.SubscriptionUser{ID: "u-bob", Account: "bob"}},
		nil)

	req := model.SyncCreateDMRequest{RoomType: model.RoomTypeDM, RequesterAccount: "alice", OtherAccount: "bob"}
	reply, err := h.serverCreateDM(dmCtxNoID(), req)
	require.NoError(t, err, "serverCreateDM must succeed without an X-Request-ID")
	require.NotNil(t, reply)
	assert.True(t, reply.Success)

	var outbox *dmCapturedPublish
	for i := range capture.captured {
		if capture.captured[i].subject == subject.Outbox("site-a", "site-b", model.OutboxMemberAdded) {
			outbox = &capture.captured[i]
			break
		}
	}
	require.NotNil(t, outbox, "expected a member_added outbox publish to site-b")

	require.NotNil(t, insertedRoom)
	wantSeed := fmt.Sprintf("%s:%s:%d", insertedRoom.ID, "alice", insertedRoom.CreatedAt.UnixMilli())
	assert.Equal(t, wantSeed+":site-b", outbox.msgID,
		"dedup id must be payload-derived even when no request ID was supplied")
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `make test SERVICE=room-worker`
Expected: FAIL —
- `TestHandleSyncCreateDM_CrossSite_EmitsOutbox`: got `0193abcd-...:site-b` (the current request-ID-derived value), want `<roomID>:alice:<ms>:site-b`.
- `TestHandleSyncCreateDM_CrossSite_NoRequestID_PayloadDerivedDedup`: with the current impl, `OutboxDedupID` falls back to the payload seed only when ctx has no ID — this case actually has no ID, so it may already match; the primary Red signal is the first test. (If both already pass, the impl change below is still required for the request-ID case and the first test is the gate.)

- [ ] **Step 4: Make the implementation change in `room-worker/handler.go`**

In `publishSyncDMOutbox`, replace the `OutboxDedupID` call (currently `room-worker/handler.go:1944`) so the dedup ID is derived directly from the payload seed, independent of the request ID. Change:
```go
	payloadSeed := fmt.Sprintf("%s:%s:%d", room.ID, requester.Account, room.CreatedAt.UnixMilli())
	return h.publish(ctx,
		subject.Outbox(room.SiteID, other.SiteID, model.OutboxMemberAdded),
		eData,
		natsutil.OutboxDedupID(ctx, other.SiteID, payloadSeed),
	)
```
to:
```go
	// Dedup keys on intrinsic room identity (stable across retries and
	// re-subscribes) plus the destination site, NOT the request ID — the router
	// now mints a fresh X-Request-ID when absent, which must not change the
	// dedup key. room.CreatedAt is the original creation time on a retry (the
	// duplicate-key reconcile path resolves room to the existing record).
	payloadSeed := fmt.Sprintf("%s:%s:%d", room.ID, requester.Account, room.CreatedAt.UnixMilli())
	return h.publish(ctx,
		subject.Outbox(room.SiteID, other.SiteID, model.OutboxMemberAdded),
		eData,
		payloadSeed+":"+other.SiteID,
	)
```

If `natsutil` becomes unused in `room-worker/handler.go` after this edit, remove the import. (It is used elsewhere in the file — `messageDedupSeed`, etc. — so it almost certainly stays; rely on `make lint`/`goimports` to confirm.)

- [ ] **Step 5: Swap the room-worker router base middleware in `room-worker/main.go`**

Change `room-worker/main.go:154` from:
```go
	router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
```
to:
```go
	router.Use(natsrouter.Recovery(), natsrouter.RequestID(), natsrouter.Logging())
```

- [ ] **Step 6: Update the stale comment in `room-worker/integration_test.go`**

At `room-worker/integration_test.go:1210-1211`, change:
```go
	// 3. Replay with the same X-Request-ID produces the same Nats-Msg-Id —
	//    on the wire, JetStream OUTBOX dedup would reject the second emit.
```
to:
```go
	// 3. Replay produces the same Nats-Msg-Id because the dedup key is derived
	//    from the (stable) payload seed — room identity + createdAt + dest — not
	//    the request ID. On the wire, JetStream OUTBOX dedup rejects the second emit.
```

Also update the function-header comment at `room-worker/integration_test.go:1169-1172` (the "Same X-Request-ID across replays produces the same Nats-Msg-Id" sentence) to: "The payload-derived dedup key (room id + createdAt + dest) is identical across replays, so JetStream dedup blocks duplicates." (No assertion change — line 1218 still holds.)

- [ ] **Step 7: Run the tests to verify they pass**

Run: `make test SERVICE=room-worker`
Expected: PASS (both updated/new unit tests green; the rest unchanged).
Run: `make build SERVICE=room-worker`
Expected: builds clean.
Run: `make lint`
Expected: no new findings.
If Docker is available: `make test-integration SERVICE=room-worker` — `TestSyncCreateDM_CrossSite_OutboxPayloadConverges` still passes (replay msgIDs match via the payload seed).

- [ ] **Step 8: Commit**

```bash
git add room-worker/main.go room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go
git commit -m "feat(room-worker): mint X-Request-ID for sync DM RPC; payload-derived outbox dedup"
```

---

## Task 3: `room-service` — minting base + strict group

**Files:**
- Modify: `room-service/main.go:184` (`RequireRequestID()` → `RequestID()`)
- Modify: `room-service/handler.go:78-99` (`Register`: strict group for all routes except `RoomsInfoBatch`)
- Create: integration test in `room-service/integration_test.go` (header-less `RoomsInfoBatch` succeeds; strict route still rejects)

- [ ] **Step 1: Write the failing integration test** in `room-service/integration_test.go`

Add this test (it mirrors `TestRoomsInfoBatchRPC`, but uses the production-shaped minting base and sends a header-less request). Place it directly after `TestRoomsInfoBatchRPC`.

```go
// TestRoomsInfoBatchRPC_NoRequestID proves the relaxed posture: with the
// production base middleware (RequestID mints, not RequireRequestID), the
// server-to-server RoomsInfoBatch RPC succeeds WITHOUT an X-Request-ID header,
// while a dedup-critical server route (RoomKeyEnsure, on the strict group) still
// rejects a header-less request.
func TestRoomsInfoBatchRPC_NoRequestID(t *testing.T) {
	db := setupMongo(t)
	keyStore := setupValkey(t)
	natsURL := setupNATS(t)

	store := NewMongoStore(db)

	mustInsertRoom(t, db, &model.Room{ID: "r1", Name: "room-1", Type: model.RoomTypeChannel, SiteID: "site-a"})

	otelNC, err := otelnats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = otelNC.Drain() })

	handler := NewHandler(store, keyStore, nil, nil, "site-a", 1000, 500, 5*time.Second, 5, func(context.Context, string, []byte, string) error { return nil }, func(context.Context, string, []byte) error { return nil }, nil, 0)
	router := natsrouter.New(otelNC, "room-service")
	// Production-shaped base: mint, do not require.
	router.Use(natsrouter.Recovery(), natsrouter.RequestID(), natsrouter.Logging())
	handler.Register(router)
	t.Cleanup(func() { _ = router.Shutdown(context.Background()) })
	require.NoError(t, otelNC.NatsConn().Flush())

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(func() { nc.Drain() })

	ctxReq, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. Header-less RoomsInfoBatch (relaxed, base router) — succeeds.
	batchData, err := json.Marshal(model.RoomsInfoBatchRequest{RoomIDs: []string{"r1"}})
	require.NoError(t, err)
	msg, err := nc.RequestWithContext(ctxReq, subject.RoomsInfoBatch("site-a"), batchData)
	require.NoError(t, err, "RoomsInfoBatch must answer a header-less request")
	var resp model.RoomsInfoBatchResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Len(t, resp.Rooms, 1)
	assert.Equal(t, "r1", resp.Rooms[0].RoomID)
	assert.True(t, resp.Rooms[0].Found)

	// 2. Header-less RoomKeyEnsure (strict group) — rejected with BadRequest.
	ensureData, err := json.Marshal(model.RoomKeyEnsureRequest{RoomID: "r1"})
	require.NoError(t, err)
	ensureMsg, err := nc.RequestWithContext(ctxReq, subject.RoomKeyEnsure("site-a"), ensureData)
	require.NoError(t, err, "expected an error envelope reply, not a transport failure")
	e, ok := errcode.Parse(ensureMsg.Data)
	require.True(t, ok, "strict route must reply with an errcode envelope: %s", ensureMsg.Data)
	assert.Equal(t, errcode.CodeBadRequest, e.Code, "missing X-Request-ID must be a bad_request")
}
```

Notes for the implementer:
- `errcode` is already imported by `room-service` package code; if the test file lacks the import, add `"github.com/hmchangw/chat/pkg/errcode"`.
- If `model.RoomKeyEnsureRequest` has a different name/shape, marshal whatever minimal body that subject expects — the body is irrelevant because `RequireRequestID` aborts *before* the handler reads it. A minimal `[]byte("{}")` is acceptable if the request type is uncertain: replace `ensureData` with `[]byte("{}")`.

- [ ] **Step 2: Run the test to verify it fails**

Run (needs Docker): `make test-integration SERVICE=room-service -run TestRoomsInfoBatchRPC_NoRequestID` (or `go test -race -tags integration ./room-service/ -run TestRoomsInfoBatchRPC_NoRequestID`)
Expected: FAIL on assertion 1 — with the current `main.go` the test builds its own router, but `Register` currently puts `RoomsInfoBatch` on the bare router which (in this test) has only minting middleware, so assertion 1 may already pass; the **decisive Red** is that without the Task-3 `Register` change, `RoomKeyEnsure` is ALSO on the bare minting router, so assertion 2 fails (no BadRequest — the handler runs and returns something else).

If Docker is unavailable: skip the run, proceed to Step 3, and rely on the Task-1 `pkg/natsrouter` unit tests (`TestChain_BaseMintsAndRuns_NoHeader`, `TestChain_StrictGroupRejects_NoHeader`) which prove the same middleware semantics. Note this in the commit summary.

- [ ] **Step 3: Change `room-service/main.go` base middleware**

Change `room-service/main.go:184` from:
```go
	router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
```
to:
```go
	router.Use(natsrouter.Recovery(), natsrouter.RequestID(), natsrouter.Logging())
```

- [ ] **Step 4: Rewire `Register` in `room-service/handler.go`**

Replace the body of `Register` (`room-service/handler.go:78-99`) so every route is registered on a strict group **except** `RoomsInfoBatch`, which stays on the base router:

```go
// Register wires every room-service RPC onto the natsrouter Router.
// All user-facing and dedup-critical routes go through the strict group, which
// re-applies RequireRequestID on top of the base (minting) middleware. The
// server-to-server RoomsInfoBatch read RPC is registered on the base router so
// it tolerates a missing X-Request-ID (one is minted for logging).
// Register/RegisterNoBody panic on subscription failure (fatal at startup).
func (h *Handler) Register(r *natsrouter.Router) {
	strict := r.Group(natsrouter.RequireRequestID())

	natsrouter.RegisterNoBody(strict, subject.MuteTogglePattern(h.siteID), h.muteToggle)
	natsrouter.RegisterNoBody(strict, subject.FavoriteTogglePattern(h.siteID), h.favoriteToggle)
	natsrouter.RegisterNoBody(strict, subject.RoomAppTabsPattern(h.siteID), h.getRoomAppTabs)
	natsrouter.RegisterNoBody(strict, subject.RoomAppCmdMenuPattern(h.siteID), h.getRoomAppCommandMenu)
	natsrouter.RegisterNoBody(strict, subject.OrgMembersPattern(h.siteID), h.listOrgMembers)
	natsrouter.RegisterNoBody(strict, subject.MemberListPattern(h.siteID), h.listMembers)
	natsrouter.RegisterNoBody(strict, subject.MemberStatusesPattern(h.siteID), h.listMemberStatuses)
	natsrouter.RegisterNoBody(strict, subject.MentionableSubscriptionsPattern(h.siteID), h.listMentionableSubscriptions)
	natsrouter.RegisterNoBody(strict, subject.RoomKeyGetPattern(h.siteID), h.getRoomKey)
	natsrouter.RegisterNoBody(strict, subject.MessageReadPattern(h.siteID), h.messageRead)
	natsrouter.Register(strict, subject.MessageReadReceiptPattern(h.siteID), h.messageReadReceipt)
	natsrouter.Register(strict, subject.MessageThreadReadPattern(h.siteID), h.messageThreadRead)
	natsrouter.Register(strict, subject.MemberRoleUpdatePattern(h.siteID), h.updateRole)
	natsrouter.Register(strict, subject.MemberRemovePattern(h.siteID), h.removeMember)
	natsrouter.Register(strict, subject.MemberAddPattern(h.siteID), h.addMembers)
	natsrouter.Register(strict, subject.RoomRenamePattern(h.siteID), h.roomRename)
	natsrouter.Register(strict, subject.RoomRestricted(h.siteID), h.roomRestricted)
	natsrouter.Register(strict, subject.RoomKeyEnsure(h.siteID), h.ensureRoomKey)
	natsrouter.Register(strict, subject.RoomCreatePattern(h.siteID), h.createRoom)

	// Server-to-server read RPC: tolerates a missing X-Request-ID (base router mints).
	natsrouter.Register(r, subject.RoomsInfoBatchSubscribe(h.siteID), h.roomsInfoBatch)
}
```

(Every line that was on `r` is now on `strict`, except the final `RoomsInfoBatch` line which stays on `r`. `RoomKeyEnsure` stays strict — out of scope per the spec.)

- [ ] **Step 5: Run the test to verify it passes**

Run (needs Docker): `go test -race -tags integration ./room-service/ -run TestRoomsInfoBatchRPC_NoRequestID`
Expected: PASS — header-less `RoomsInfoBatch` returns the room; header-less `RoomKeyEnsure` returns a `bad_request` envelope.

Also confirm the existing `TestRoomsInfoBatchRPC` (which builds its own `RequireRequestID()` base router and sends a valid ID) still passes — it does: `RoomsInfoBatch` on its bare base inherits that router's `RequireRequestID`, and the request carries a valid ID.

If Docker is unavailable: rely on Task-1 unit proofs; record that the integration test was not run locally.

- [ ] **Step 6: Build + lint**

Run: `make build SERVICE=room-service`
Expected: builds clean.
Run: `make test SERVICE=room-service`
Expected: PASS (unit tests unaffected).
Run: `make lint`
Expected: no new findings.

- [ ] **Step 7: Commit**

```bash
git add room-service/main.go room-service/handler.go room-service/integration_test.go
git commit -m "feat(room-service): relax X-Request-ID for RoomsInfoBatch via strict route group"
```

---

## Task 4: Docs — `docs/error-handling.md` §3a

**Files:**
- Modify: `docs/error-handling.md:170-177`

- [ ] **Step 1: Update the strict-callers list**

Replace `docs/error-handling.md:170-177` (the "Strict callers today" bullet and the JetStream-consume-loop bullet) with:

```markdown
- **Strict callers today**: every **user-facing** room-service handler (the
  strict route group installed in `room-service/handler.go`'s `Register`) and the
  dedup-critical member RPCs. The base room-service router now **mints** via
  `RequestID()`; the strict group re-applies `RequireRequestID` to all routes
  except the server-to-server `RoomsInfoBatch` read RPC, which tolerates a
  missing header.
- **Relaxed server-to-server RPCs**: `room-service.RoomsInfoBatch`
  (`chat.server.request.room.{siteID}.info.batch`, read-only) and
  `room-worker.serverCreateDM` (`chat.server.request.room.{siteID}.create.dm`)
  mint a request ID when absent. `serverCreateDM` keeps cross-site OUTBOX dedup
  safe by deriving the dedup key from a deterministic payload seed (room id +
  createdAt + destination site) instead of the request ID.
- **The room-worker JetStream consume loop** keeps the default mint policy
  defensively — by the time a message lands on the ROOMS stream, room-service
  validated the header at publish time. The consume loop logs an `slog.Error`
  if it ever has to mint, because that indicates an upstream contract
  violation (and downstream dedup will be broken for that message).
```

- [ ] **Step 2: Sanity-check the surrounding text**

Read `docs/error-handling.md:153-186` and confirm the "Client contract" paragraph still reads correctly after the edit (it does — clients SHOULD still send a stable ID; the relaxation is server-internal). No further change needed.

- [ ] **Step 3: Commit**

```bash
git add docs/error-handling.md
git commit -m "docs(error-handling): note relaxed X-Request-ID for server-to-server room RPCs"
```

---

## Final verification

- [ ] **Step 1: Full lint + touched-service unit tests**

Run: `make lint`
Run: `make test SERVICE=room-worker` and `make test SERVICE=room-service` and `go test -race ./pkg/natsrouter/`
Expected: all PASS.

- [ ] **Step 2: Integration (if Docker available)**

Run: `make test-integration SERVICE=room-worker` and `make test-integration SERVICE=room-service`
Expected: PASS. If Docker is unavailable, state clearly in the final summary that integration tests were written but not run locally (they run in CI).

- [ ] **Step 3: Push**

```bash
git push -u origin claude/friendly-knuth-WXYQY
```

---

## Self-review

**Spec coverage:**
- §3.1 natsrouter Group → Task 1. ✓
- §3.2 room-service base mints + strict group (incl. accepted double-log) → Task 3 (+ Task-1 unit proof of the chain semantics). ✓
- §3.3 room-worker global swap + payload-derived dedup → Task 2. ✓
- §3.4 docs → Task 4. ✓
- §4 tests: natsrouter Group/composition (Task 1), room-service header-less + strict reject (Task 3), room-worker header-less + payload-derived dedup (Task 2). ✓
- Non-goals (RoomKeyEnsure stays strict, async ROOMS path untouched, no client-api.md change): honored — `RoomKeyEnsure` registered on `strict`; no room-worker consume-loop or `client-api.md` edits. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full code. The one conditional (`RoomKeyEnsureRequest` body) has an explicit `[]byte("{}")` fallback.

**Type consistency:** `registrar` (interface), `Group{parent registrar; middleware []HandlerFunc}`, `(*Router).Group(mw ...HandlerFunc) *Group`, `Group.addRoute` — consistent across Task 1 definition and Task 3 usage (`r.Group(natsrouter.RequireRequestID())`). Dedup format `"%s:%s:%d"` + `":"+other.SiteID` matches the assertion `wantSeed+":site-b"` in Task 2.
