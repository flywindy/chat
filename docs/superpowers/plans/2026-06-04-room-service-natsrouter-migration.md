# room-service (+ room-worker RPC) natsrouter Migration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move all 20 room-service request/reply RPCs (including `member.statuses` and `subscription.mentionable`) and room-worker's one RPC (`natsServerCreateDM`) off raw `nc.QueueSubscribe` + hand-written wrappers onto `pkg/natsrouter`, gaining per-message concurrency and centralized marshal/error/recovery/request-ID/logging, with no wire changes.

**Architecture:** Add a strict `RequireRequestID()` middleware and `{name}` `*Pattern` subject builders, convert each handler core from `handleXxx(ctx, subj, data)` to a typed `xxx(c *natsrouter.Context, req)` (or `xxx(c)` for body-less), register them via `natsrouter.Register`/`RegisterNoBody`, then cut `main.go` from `RegisterCRUD` to a router. Subjects, request/response JSON, and error envelopes are preserved.

**Tech Stack:** Go 1.25, `pkg/natsrouter`, `pkg/subject`, `pkg/model`, `pkg/natsutil`, `pkg/errcode`, `go.uber.org/mock`, `testify`.

**Spec:** `docs/superpowers/specs/2026-06-04-room-service-natsrouter-migration-design.md`

---

## Conventions used throughout

- Run a single test: `go test ./pkg/natsrouter/ -run TestName -race -count=1`
- Run a service's unit tests: `make test SERVICE=room-service`
- Run a package's tests: `go test ./pkg/subject/ -race` (Makefile wraps `-race`; raw `go test` is fine for a single package during TDD).
- Commit after every green step. Branch is already `claude/zen-brown-Atb7v`.
- **Keep `make test` green at every commit.** Integration tests (`//go:build integration`) only go fully green after the `main.go` cutover (Task 11) — that is expected and called out there.

## File structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `pkg/natsrouter/middleware.go` | Add `RequireRequestID()` | 1 |
| `pkg/natsrouter/middleware_test.go` | Tests for `RequireRequestID()` | 1 |
| `pkg/subject/subject.go` | Add 17 `*Pattern` builders | 2 |
| `pkg/subject/subject_test.go` | Pattern↔Wildcard equivalence tests | 2 |
| `pkg/model/event.go` | Add `StatusReply`, `StatusWithRequestReply`, `RoomRenameRequest` | 3 |
| `pkg/model/model_test.go` | Round-trip tests for the new types | 3 |
| `room-service/handler.go` | Convert 20 handlers; add `Register`; delete `RegisterCRUD`/wrappers/`wrappedCtx` | 4–11 |
| `room-service/handler_test.go` | Convert handler tests; delete dead tests | 4–11 |
| `room-service/main.go` | Router wiring + shutdown | 11 |
| `room-worker/handler.go` | Convert `natsServerCreateDM`→`serverCreateDM`; delete `requireDedupRequestID` | 12 |
| `room-worker/handler_test.go` | Convert sync-DM tests | 12 |
| `room-worker/main.go` | Router for the one RPC + shutdown | 12 |
| `docs/client-api.md` | Update rename malformed-body error | 13 |

---

## Phase 1 — Foundation (no room-service changes yet)

### Task 1: `RequireRequestID()` middleware

**Files:**
- Modify: `pkg/natsrouter/middleware.go`
- Test: `pkg/natsrouter/middleware_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `pkg/natsrouter/middleware_test.go` (add `"github.com/nats-io/nats.go"` and `"github.com/hmchangw/chat/pkg/natsutil"` to imports):

```go
func TestRequireRequestID_ValidPasses(t *testing.T) {
	const id = "01970a4f-8c2d-7c9a-abcd-e0123456789f"
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{natsutil.RequestIDHeader: []string{id}}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	require.True(t, ran, "handler must run when request ID is a valid UUID")
	got, ok := c.Get(requestIDKey)
	require.True(t, ok)
	assert.Equal(t, id, got)
	assert.Equal(t, id, natsutil.RequestIDFromContext(c))
}

func TestRequireRequestID_MissingAborts(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "handler must NOT run when request ID is missing")
	assert.True(t, c.IsAborted())
}

func TestRequireRequestID_InvalidAborts(t *testing.T) {
	c := &Context{
		ctx:   context.Background(),
		Msg:   &nats.Msg{Subject: "x", Header: nats.Header{natsutil.RequestIDHeader: []string{"not-a-uuid"}}},
		chain: &chainState{index: -1},
	}
	var ran bool
	c.chain.handlers = []HandlerFunc{
		RequireRequestID(),
		func(c *Context) { ran = true },
	}
	c.Next()

	assert.False(t, ran, "handler must NOT run when request ID is malformed")
	assert.True(t, c.IsAborted())
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/natsrouter/ -run TestRequireRequestID -race -count=1`
Expected: FAIL — `undefined: RequireRequestID`.

- [ ] **Step 3: Implement the middleware**

Add to `pkg/natsrouter/middleware.go` directly below `RequestID()` (the file already imports `nats`, `errcode`, `errnats`, `natsutil`):

```go
// RequireRequestID is the strict variant of RequestID: a missing/non-UUID
// X-Request-ID is rejected (BadRequest, reason RequestIDRequired) and aborts; never mints.
func RequireRequestID() HandlerFunc {
	return func(c *Context) {
		var (
			headers nats.Header
			subj    string
		)
		if c.Msg != nil {
			headers = c.Msg.Header
			subj = c.Msg.Subject
		}
		ctx, id, err := natsutil.RequireRequestID(c.ctx, headers, subj)
		if err != nil {
			// c.Msg is set in production; guard the nil-Msg unit-test context.
			if c.Msg != nil {
				errnats.Reply(c, c.Msg, err)
			}
			c.Abort()
			return
		}
		c.Set(requestIDKey, id)
		c.SetContext(ctx)
		c.WithLogValues("request_id", id)
		c.Next()
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/natsrouter/ -run TestRequireRequestID -race -count=1`
Expected: PASS (3 tests).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add pkg/natsrouter/middleware.go pkg/natsrouter/middleware_test.go
git commit -m "feat(natsrouter): add strict RequireRequestID middleware"
```

---

### Task 2: `*Pattern` subject builders

**Files:**
- Modify: `pkg/subject/subject.go`
- Test: `pkg/subject/subject_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/subject/subject_test.go` (ensure `"strings"` and testify imports exist):

```go
func TestRoomPatternsMatchWildcards(t *testing.T) {
	const site = "site-a"
	repl := strings.NewReplacer("{account}", "*", "{roomID}", "*", "{orgID}", "*")
	cases := []struct{ name, pattern, wildcard string }{
		{"create", RoomCreatePattern(site), RoomCreateWildcard(site)},
		{"role-update", MemberRoleUpdatePattern(site), MemberRoleUpdateWildcard(site)},
		{"remove", MemberRemovePattern(site), MemberRemoveWildcard(site)},
		{"add", MemberAddPattern(site), MemberAddWildcard(site)},
		{"list", MemberListPattern(site), MemberListWildcard(site)},
		{"org-members", OrgMembersPattern(site), OrgMembersWildcard(site)},
		{"message-read", MessageReadPattern(site), MessageReadWildcard(site)},
		{"read-receipt", MessageReadReceiptPattern(site), MessageReadReceiptWildcard(site)},
		{"thread-read", MessageThreadReadPattern(site), MessageThreadReadWildcard(site)},
		{"key-get", RoomKeyGetPattern(site), RoomKeyGetWildcard(site)},
		{"mute", MuteTogglePattern(site), MuteToggleWildcard(site)},
		{"favorite", FavoriteTogglePattern(site), FavoriteToggleWildcard(site)},
		{"rename", RoomRenamePattern(site), RoomRenameWildcard(site)},
		{"app-tabs", RoomAppTabsPattern(site), RoomAppTabsWildcard(site)},
		{"app-cmd-menu", RoomAppCmdMenuPattern(site), RoomAppCmdMenuWildcard(site)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wildcard, repl.Replace(tc.pattern),
				"pattern with params replaced by * must equal the existing wildcard subscription subject")
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/subject/ -run TestRoomPatternsMatchWildcards -race -count=1`
Expected: FAIL — `undefined: RoomCreatePattern` (etc.).

- [ ] **Step 3: Add the builders**

Append to `pkg/subject/subject.go` (after the existing `--- natsrouter patterns ---` group, e.g. near line 524):

```go
// --- room-service natsrouter pattern builders (siteID baked in) ---

func RoomCreatePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.%s.create", siteID)
}

func MemberRoleUpdatePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.member.role-update", siteID)
}

func MemberRemovePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.member.remove", siteID)
}

func MemberAddPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.member.add", siteID)
}

func MemberListPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.member.list", siteID)
}

func OrgMembersPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.orgs.{orgID}.%s.members", siteID)
}

func MessageReadPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.message.read", siteID)
}

func MessageReadReceiptPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.message.read-receipt", siteID)
}

func MessageThreadReadPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.message.thread.read", siteID)
}

func RoomKeyGetPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.key.get", siteID)
}

func MuteTogglePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.mute.toggle", siteID)
}

func FavoriteTogglePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.favorite.toggle", siteID)
}

func RoomRenamePattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.room.rename", siteID)
}

func RoomAppTabsPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.app.tabs", siteID)
}

func RoomAppCmdMenuPattern(siteID string) string {
	return fmt.Sprintf("chat.user.{account}.request.room.{roomID}.%s.app.cmd-menu", siteID)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/subject/ -run TestRoomPatternsMatchWildcards -race -count=1`
Expected: PASS (17 subtests).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add pkg/subject/subject.go pkg/subject/subject_test.go
git commit -m "feat(subject): add room-service natsrouter pattern builders"
```

---

### Task 3: Typed status replies + rename request

**Files:**
- Modify: `pkg/model/event.go`
- Test: `pkg/model/model_test.go`

- [ ] **Step 1: Write the failing test**

`pkg/model/model_test.go` uses a generic `roundTrip` helper. Append:

```go
func TestStatusReply_RoundTrip(t *testing.T) {
	roundTrip(t, model.StatusReply{Status: "ok"})
	roundTrip(t, model.StatusReply{Status: "accepted"})
}

func TestStatusWithRequestReply_RoundTrip(t *testing.T) {
	roundTrip(t, model.StatusWithRequestReply{Status: "accepted", RequestID: "01970a4f-8c2d-7c9a-abcd-e0123456789f"})
}

func TestRoomRenameRequest_RoundTrip(t *testing.T) {
	roundTrip(t, model.RoomRenameRequest{NewName: "New Name"})
}

func TestStatusReply_JSONShape(t *testing.T) {
	b, err := json.Marshal(model.StatusReply{Status: "accepted"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"accepted"}`, string(b))
}

func TestStatusWithRequestReply_JSONShape(t *testing.T) {
	b, err := json.Marshal(model.StatusWithRequestReply{Status: "ok", RequestID: "rid"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"ok","requestId":"rid"}`, string(b))
}
```

(If `json`, `require`, or `assert` are not already imported in `model_test.go`, add them.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/model/ -run 'TestStatusReply|TestStatusWithRequestReply|TestRoomRenameRequest' -race -count=1`
Expected: FAIL — `undefined: model.StatusReply` (etc.).

- [ ] **Step 3: Add the types**

Append to `pkg/model/event.go`:

```go
// StatusReply is the response for fire-and-forget RPCs that only confirm
// acceptance. Status is "ok" or "accepted" depending on the endpoint.
type StatusReply struct {
	Status string `json:"status"`
}

// StatusWithRequestReply is StatusReply plus the echoed request ID, for RPCs
// whose clients correlate the async result by request ID (rename, restricted).
type StatusWithRequestReply struct {
	Status    string `json:"status"`
	RequestID string `json:"requestId"`
}

// RoomRenameRequest is the rename RPC body. NewName-only: roomID is taken from
// the subject, never the body.
type RoomRenameRequest struct {
	NewName string `json:"newName"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/model/ -run 'TestStatusReply|TestStatusWithRequestReply|TestRoomRenameRequest' -race -count=1`
Expected: PASS.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add pkg/model/event.go pkg/model/model_test.go
git commit -m "feat(model): add StatusReply, StatusWithRequestReply, RoomRenameRequest"
```

---

## Phase 2 — room-service handler conversion

### Conversion recipe (read once, applied per handler in Tasks 4–10)

Every handler today has a thin wrapper `natsXxx(m otelnats.Msg)` and a core `handleXxx(ctx, subj, data)`. The conversion **deletes the wrapper** and **reshapes the core** into the typed handler natsrouter calls.

**Three flavors:**

**(A) Body handler → `Register`** (e.g. role-update, remove, add, read-receipt, thread-read, rename, restricted, rooms-info-batch, ensure-key, create):

Before:
```go
func (h *Handler) natsUpdateRole(m otelnats.Msg) {
	ctx, err := wrappedCtx(m)
	if err != nil { errnats.Reply(ctx, m.Msg, err); return }
	resp, err := h.handleUpdateRole(ctx, m.Msg.Subject, m.Msg.Data)
	if err != nil { errnats.Reply(ctx, m.Msg, err); return }
	natsutil.ReplyJSON(m.Msg, resp)
}

func (h *Handler) handleUpdateRole(ctx context.Context, subj string, data []byte) ([]byte, error) {
	account, roomID, ok := subject.ParseUserRoomSubject(subj)
	if !ok { return nil, fmt.Errorf("invalid role-update subject") }
	var req model.UpdateRoleRequest
	if err := json.Unmarshal(data, &req); err != nil { return nil, errcode.BadRequest("invalid request") }
	// …business logic using ctx, account, roomID, req…
	return json.Marshal(map[string]string{"status": "ok"})
}
```

After (delete the wrapper entirely; reshape the core):
```go
func (h *Handler) updateRole(c *natsrouter.Context, req model.UpdateRoleRequest) (*model.StatusReply, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …same business logic, unchanged…
	return &model.StatusReply{Status: "ok"}, nil
}
```

Mechanical changes:
1. Delete `natsXxx`.
2. Rename `handleXxx` → `xxx` (unexported, no `Nats`/`handle` prefix). Signature → `(c *natsrouter.Context, req <ReqType>) (*<RespType>, error)`.
3. First line: `var ctx context.Context = c` (keeps every internal `ctx` reference working; `*Context` implements `context.Context`).
4. Delete the `subject.ParseXxx` block; replace with `account := c.Param("account")` / `roomID := c.Param("roomID")` as the body uses.
5. Delete the `json.Unmarshal(data, &req)` block — `req` is now the typed parameter.
6. Convert returns: `return json.Marshal(X)` → `return &X, nil`; every `return nil, err` stays.
7. Add the registration line to `Register` (Task 4 introduces the method).

**(B) Body-less handler → `RegisterNoBody`** (mute, favorite, message-read, app-tabs, app-cmd-menu, list-org-members): identical to (A) but the signature is `func (h *Handler) xxx(c *natsrouter.Context) (*<RespType>, error)` (no `req`), and there is no unmarshal block to delete (the core took `_ []byte` or no data arg).

**(C) Optional-body handler → `RegisterNoBody` + manual unmarshal** (list-members, get-room-key): signature `func (h *Handler) xxx(c *natsrouter.Context) (*<RespType>, error)`; **keep** the existing `var req …; if len(c.Msg.Data) > 0 { json.Unmarshal(c.Msg.Data, &req) }` guard (was `if len(data) > 0`). Do **not** use `Register` — it rejects an empty body.

**Test conversion (all flavors).** Tests currently call e.g. `h.handleUpdateRole(ctxWithReqID(), subj, body)`. Convert to build a `*natsrouter.Context` and pass the typed request. Add this helper once to `room-service/handler_test.go`:

```go
// ctxParams builds a *natsrouter.Context with subject params and a valid
// request ID on the underlying ctx (for handlers that echo/read it).
func ctxParams(params map[string]string) *natsrouter.Context {
	c := natsrouter.NewContext(params)
	c.SetContext(natsutil.WithRequestID(context.Background(), "01970a4f-8c2d-7c9a-abcd-e0123456789f"))
	return c
}
```

For optional-body / no-body handlers that read `c.Msg.Data`, also set `c.Msg`:
```go
c := ctxParams(map[string]string{"account": "alice", "roomID": "r1"})
c.Msg = &nats.Msg{Data: body} // body may be nil for the empty-body case
```

Then call the typed handler and assert on the struct:
```go
resp, err := h.updateRole(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.UpdateRoleRequest{/*…*/})
require.NoError(t, err)
assert.Equal(t, "ok", resp.Status)
```

**Deleted test categories (across the tasks):** (1) `*_InvalidSubject` cases — the handler no longer parses the subject, so the branch is unreachable; (2) `TestWrappedCtx_*` (deleted with `wrappedCtx` in Task 10) and room-worker's `TestRequireDedupRequestID` (Task 12) — request-ID validation now lives in `RequireRequestID` (Task 1 tests cover it); (3) malformed-JSON-body cases for `Register` handlers — unmarshalling happens in `natsrouter.Register` before the handler runs, so the handler can no longer receive malformed JSON. For (3), verify `pkg/natsrouter/router_test.go` (or `register` tests) already asserts a malformed body yields `errcode.BadRequest("invalid request payload")`; if it does not, add that one test there rather than keeping it per-handler.

**Per-handler tasks below give the exact new signature, registration line, params, and any handler-specific notes.** Apply the recipe; the business logic between the parsing prologue and the return is unchanged unless a note says otherwise.

---

### Task 4: Introduce `Register`; convert the toggles (mute, favorite)

**Files:**
- Modify: `room-service/handler.go`
- Test: `room-service/handler_test.go`

- [ ] **Step 1: Add the `Register` method skeleton + the `natsrouter` import**

In `room-service/handler.go`, add `"github.com/hmchangw/chat/pkg/natsrouter"` to imports and add (above `RegisterCRUD`):

```go
// Register wires every room-service RPC onto the natsrouter Router. Replaces
// RegisterCRUD. Register/RegisterNoBody panic on subscription failure (fatal at startup).
func (h *Handler) Register(r *natsrouter.Router) {
	natsrouter.RegisterNoBody(r, subject.MuteTogglePattern(h.siteID), h.muteToggle)
	natsrouter.RegisterNoBody(r, subject.FavoriteTogglePattern(h.siteID), h.favoriteToggle)
}
```

- [ ] **Step 2: Convert the `muteToggle` test (write failing)**

In `room-service/handler_test.go`: add the `ctxParams` helper from the recipe (and imports `context`, `github.com/nats-io/nats.go`, `github.com/hmchangw/chat/pkg/natsrouter`, `github.com/hmchangw/chat/pkg/natsutil` if absent). Rewrite each `h.handleMuteToggle(ctx, subj, nil)` call site (see spec: `handler_test.go:4063,4120,4154,4164,4183,4210,4240,4692`) to:

```go
resp, err := h.muteToggle(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}))
```

Delete the `*_InvalidSubject` mute case (the `"garbage.subject"` test at `handler_test.go:4164`) — subject parsing no longer happens in the handler. Keep all other assertions; change `resp` usage from the unmarshalled `MuteToggleResponse` to the returned `*model.MuteToggleResponse` (fields identical).

- [ ] **Step 3: Run to verify failure**

Run: `make test SERVICE=room-service`
Expected: FAIL to compile — `h.muteToggle undefined` / `h.handleMuteToggle` signature mismatch.

- [ ] **Step 4: Convert `muteToggle` + `favoriteToggle` cores (flavor B)**

Apply recipe (B). Delete `natsMuteToggle` and `natsFavoriteToggle`. Reshape:

```go
func (h *Handler) muteToggle(c *natsrouter.Context) (*model.MuteToggleResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …unchanged business logic…
	return &model.MuteToggleResponse{Status: "ok", Muted: sub.Muted}, nil
}

func (h *Handler) favoriteToggle(c *natsrouter.Context) (*model.FavoriteToggleResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …unchanged business logic…
	return &model.FavoriteToggleResponse{Status: "ok", Favorite: sub.Favorite}, nil
}
```

Remove the `MuteToggleWildcard`/`FavoriteToggleWildcard` lines from `RegisterCRUD` (they now live in `Register`).

- [ ] **Step 5: Run to verify pass**

Run: `make test SERVICE=room-service`
Expected: PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint
git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate mute/favorite toggles to natsrouter"
```

---

### Task 5: Convert simple no-body reads (app-tabs, app-cmd-menu, list-org-members)

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.RegisterNoBody(r, subject.RoomAppTabsPattern(h.siteID), h.getRoomAppTabs)
	natsrouter.RegisterNoBody(r, subject.RoomAppCmdMenuPattern(h.siteID), h.getRoomAppCommandMenu)
	natsrouter.RegisterNoBody(r, subject.OrgMembersPattern(h.siteID), h.listOrgMembers)
```

- [ ] **Step 2: Convert tests (write failing)** — update call sites to:

```go
resp, err := h.getRoomAppTabs(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}))
resp, err := h.getRoomAppCommandMenu(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}))
resp, err := h.listOrgMembers(ctxParams(map[string]string{"account": "alice", "orgID": "eng"}))
```

Delete any `*_InvalidSubject` cases for these three (`handler_test.go:5403,5537` cover app tabs/cmd-menu; the org-members invalid-subject case if present). Adjust `resp` to the returned struct pointer (fields identical to the previously-unmarshalled responses).

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL (undefined methods).

- [ ] **Step 4: Convert cores (flavor B).** Signatures + return types:

```go
func (h *Handler) getRoomAppTabs(c *natsrouter.Context) (*model.GetRoomAppTabsResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …unchanged; final return becomes &model.GetRoomAppTabsResponse{Apps: out}, nil
}

func (h *Handler) getRoomAppCommandMenu(c *natsrouter.Context) (*model.GetRoomAppCommandMenuResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …unchanged, return &model.GetRoomAppCommandMenuResponse{...}, nil…
}

func (h *Handler) listOrgMembers(c *natsrouter.Context) (*model.ListOrgMembersResponse, error) {
	var ctx context.Context = c
	orgID := c.Param("orgID")
	// …unchanged (drop ParseOrgMembersSubject; use orgID)…
	return &model.ListOrgMembersResponse{Members: members}, nil
}
```

Note (list-org-members): reads only `orgID`; no requester check (per spec decision). Delete `natsGetRoomAppTabs`, `natsGetRoomAppCommandMenu`, `natsListOrgMembers` and their three `RegisterCRUD` lines.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate app-tabs, app-cmd-menu, list-org-members to natsrouter"
```

---

### Task 6: Convert message-read + read-receipt + thread-read

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.RegisterNoBody(r, subject.MessageReadPattern(h.siteID), h.messageRead)
	natsrouter.Register(r, subject.MessageReadReceiptPattern(h.siteID), h.messageReadReceipt)
	natsrouter.Register(r, subject.MessageThreadReadPattern(h.siteID), h.messageThreadRead)
```

- [ ] **Step 2: Convert tests (write failing).** Call sites become:

```go
resp, err := h.messageRead(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}))
resp, err := h.messageReadReceipt(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.ReadReceiptRequest{MessageID: "m1"})
resp, err := h.messageThreadRead(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.MessageThreadReadRequest{ThreadID: "t1"})
```

Delete the malformed-JSON-body test cases for read-receipt/thread-read (now covered by natsrouter `Register` tests) and any `*_InvalidSubject` cases (`handler_test.go:1825,1997` per spec). `messageRead` returns `*model.StatusReply` (`Status: "accepted"`); `messageThreadRead` returns `*model.StatusReply` (`Status: "accepted"`); `messageReadReceipt` returns `*model.ReadReceiptResponse`.

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL.

- [ ] **Step 4: Convert cores.**
- `messageRead` — flavor B (was `_ []byte`):
```go
func (h *Handler) messageRead(c *natsrouter.Context) (*model.StatusReply, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …unchanged… return &model.StatusReply{Status: "accepted"}, nil
}
```
- `messageReadReceipt` — flavor A, `req model.ReadReceiptRequest`, returns `*model.ReadReceiptResponse` (`return &model.ReadReceiptResponse{Readers: entries}, nil`).
- `messageThreadRead` — flavor A, `req model.MessageThreadReadRequest`, returns `*model.StatusReply{Status: "accepted"}`.

Delete `natsMessageRead`, `natsMessageReadReceipt`, `natsMessageThreadRead` and their `RegisterCRUD` lines.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate message read/read-receipt/thread-read to natsrouter"
```

---

### Task 7: Convert optional-body reads (list-members, get-room-key)

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.RegisterNoBody(r, subject.MemberListPattern(h.siteID), h.listMembers)
	natsrouter.RegisterNoBody(r, subject.RoomKeyGetPattern(h.siteID), h.getRoomKey)
```

- [ ] **Step 2: Convert tests (write failing).** Call sites build a Context with `c.Msg` so the optional body is reachable:

```go
c := ctxParams(map[string]string{"account": "alice", "roomID": "r1"})
c.Msg = &nats.Msg{Data: body} // body may be nil
resp, err := h.listMembers(c)
```

**Add an explicit empty-body test for each** (locks the optional-body contract):

```go
func TestHandler_ListMembers_EmptyBody(t *testing.T) {
	// …set up store mock for a member alice in r1…
	c := ctxParams(map[string]string{"account": "alice", "roomID": "r1"})
	c.Msg = &nats.Msg{Data: nil}
	resp, err := h.listMembers(c)
	require.NoError(t, err)
	assert.NotNil(t, resp)
}
```
(Mirror for `getRoomKey`.) Delete the list-members/get-room-key `*_InvalidSubject` cases.

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL.

- [ ] **Step 4: Convert cores (flavor C — keep the `len > 0` guard).**

```go
func (h *Handler) listMembers(c *natsrouter.Context) (*model.ListRoomMembersResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …membership check unchanged (GetSubscription → errNotRoomMember)…
	var req model.ListRoomMembersRequest
	if len(c.Msg.Data) > 0 {
		if err := json.Unmarshal(c.Msg.Data, &req); err != nil {
			return nil, errcode.BadRequest("invalid request")
		}
	}
	// …unchanged… return &model.ListRoomMembersResponse{Members: members}, nil
}

func (h *Handler) getRoomKey(c *natsrouter.Context) (*model.RoomKeyGetResponse, error) {
	var ctx context.Context = c
	account := c.Param("account")
	roomID := c.Param("roomID")
	// …membership check unchanged…
	var req model.RoomKeyGetRequest
	if len(c.Msg.Data) > 0 {
		if err := json.Unmarshal(c.Msg.Data, &req); err != nil {
			return nil, errcode.BadRequest("invalid request")
		}
	}
	// …unchanged; the two return json.Marshal(model.RoomKeyGetResponse{...}) become
	//    return &model.RoomKeyGetResponse{...}, nil  (keep the #nosec G117 comments)…
}
```

Note: `getRoomKey` previously took `data []byte` and used `requesterAccount`/`roomID` from the subject — replace those with `account`/`roomID` from `c.Param`. Delete `natsListMembers`, `natsGetRoomKey`, and their `RegisterCRUD` lines.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate list-members and get-room-key (optional body) to natsrouter"
```

---

### Task 8: Convert async-accept mutations (remove-member, add-members, role-update)

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.Register(r, subject.MemberRoleUpdatePattern(h.siteID), h.updateRole)
	natsrouter.Register(r, subject.MemberRemovePattern(h.siteID), h.removeMember)
	natsrouter.Register(r, subject.MemberAddPattern(h.siteID), h.addMembers)
```

- [ ] **Step 2: Convert tests (write failing).** Call sites:

```go
resp, err := h.updateRole(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.UpdateRoleRequest{/*…*/})
resp, err := h.removeMember(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.RemoveMemberRequest{/*…*/})
resp, err := h.addMembers(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.AddMembersRequest{/*…*/})
```

`updateRole` → `*model.StatusReply{Status: "ok"}`; `removeMember`/`addMembers` → `*model.StatusReply{Status: "accepted"}`. Delete malformed-JSON-body cases and `*_InvalidSubject` cases for these three (`handler_test.go:2431,2940,3376,3742` per spec). The `NatsHandleRemoveMember` wrapper-level tests (`otelnats.Msg{...}`) are deleted.

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL.

- [ ] **Step 4: Convert cores (flavor A).** Reshape `handleUpdateRole`, `handleRemoveMember`, `handleAddMembers`: signature `(c *natsrouter.Context, req <T>) (*model.StatusReply, error)`, `var ctx context.Context = c`, `account`/`roomID` from `c.Param`, drop the unmarshal block, convert the trailing `json.Marshal(map[string]string{"status": …})` to `&model.StatusReply{Status: …}, nil`. Note these handlers also cross-check `req.RoomID` against the subject roomID (`if req.RoomID != "" && req.RoomID != roomID`) — keep that. Delete `natsUpdateRole`, `NatsHandleRemoveMember`, `natsAddMembers` and their `RegisterCRUD` lines.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate role-update/remove/add members to natsrouter"
```

---

### Task 9: Convert rename + restricted (request-ID echo; bad-body fix)

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.Register(r, subject.RoomRenamePattern(h.siteID), h.roomRename)
	natsrouter.Register(r, subject.RoomRestricted(h.siteID), h.roomRestricted) // concrete subject, no params
```

- [ ] **Step 2: Convert tests (write failing).** Call sites:

```go
resp, err := h.roomRename(ctxParams(map[string]string{"account": "alice", "roomID": "r1"}), model.RoomRenameRequest{NewName: "X"})
// restricted has no subject params:
resp, err := h.roomRestricted(ctxParams(map[string]string{}), model.RoomRestrictedRequest{RoomID: "r1", Account: "alice"})
```

Both return `*model.StatusWithRequestReply` — `resp.RequestID` equals the request ID set by `ctxParams` (`01970a4f-…`); `resp.Status` is `"accepted"` (rename) / `"ok"` (restricted). Delete the old malformed-body tests that asserted an `internal`/`fmt.Errorf("invalid request")` result — that path now returns a router `bad_request`.

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL.

- [ ] **Step 4: Convert cores (flavor A).**
- `roomRename`: signature `(c *natsrouter.Context, req model.RoomRenameRequest) (*model.StatusWithRequestReply, error)`. Delete the inline `var body struct{ NewName string }` + its `json.Unmarshal` + `fmt.Errorf("invalid request: %w", err)` (the bad-body fix — router now handles it). Use `req.NewName`. Read the request ID once: `requestID := natsutil.RequestIDFromContext(c)`. Convert the final `json.Marshal(map[string]string{"status":"accepted","requestId":requestID})` → `&model.StatusWithRequestReply{Status: "accepted", RequestID: requestID}, nil`.
- `roomRestricted`: signature `(c *natsrouter.Context, req model.RoomRestrictedRequest) (*model.StatusWithRequestReply, error)`. Delete the `json.Unmarshal` + `fmt.Errorf("invalid request: %w", err)` block. `requestID := natsutil.RequestIDFromContext(c)`. Final return → `&model.StatusWithRequestReply{Status: "ok", RequestID: requestID}, nil`. No `c.Param` (concrete subject).

Delete `natsRoomRename`, `natsRoomRestricted` and their `RegisterCRUD` lines.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS.

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate rename/restricted to natsrouter (fix bad-body to bad_request)"
```

---

### Task 10: Convert rooms-info-batch, ensure-room-key, and create

**Files:** `room-service/handler.go`, `room-service/handler_test.go`

- [ ] **Step 1: Add registration lines** to `Register`:

```go
	natsrouter.Register(r, subject.RoomsInfoBatchSubscribe(h.siteID), h.roomsInfoBatch) // concrete subject
	natsrouter.Register(r, subject.RoomKeyEnsure(h.siteID), h.ensureRoomKey)            // concrete subject
	natsrouter.Register(r, subject.RoomCreatePattern(h.siteID), h.createRoom)
```

- [ ] **Step 2: Convert tests (write failing).** Call sites:

```go
resp, err := h.roomsInfoBatch(ctxParams(map[string]string{}), model.RoomsInfoBatchRequest{RoomIDs: []string{"r1"}})
resp, err := h.ensureRoomKey(ctxParams(map[string]string{}), model.RoomKeyEnsureRequest{/*…*/})
resp, err := h.createRoom(ctxParams(map[string]string{"account": "alice"}), body) // body is model.CreateRoomRequest
```

`roomsInfoBatch` → `*model.RoomsInfoBatchResponse`; `ensureRoomKey` → `*model.RoomKeyEnsureResponse`; `createRoom` → `*model.CreateRoomReply`. The existing `handleCreateRoom` tests pass a `[]byte` body and `createRoomSubj("alice","site-a")`; convert them to pass `model.CreateRoomRequest{}` and `ctxParams({"account":"alice"})`, asserting on `resp` fields (was: unmarshal the returned `[]byte`). Delete the create `*_InvalidSubject` (`handler_test.go:2437`) and invalid-JSON (`handler_test.go:2817`) cases.

- [ ] **Step 3: Run to verify failure** — `make test SERVICE=room-service` → FAIL.

- [ ] **Step 4: Convert cores.**
- `roomsInfoBatch` (flavor A): `(c *natsrouter.Context, req model.RoomsInfoBatchRequest) (*model.RoomsInfoBatchResponse, error)`; drop unmarshal; `var ctx context.Context = c`; final `json.Marshal(model.RoomsInfoBatchResponse{Rooms: infos})` → `&model.RoomsInfoBatchResponse{Rooms: infos}, nil`. (No subject params — it's a batch endpoint.)
- `ensureRoomKey` (flavor A): `(c *natsrouter.Context, req model.RoomKeyEnsureRequest) (*model.RoomKeyEnsureResponse, error)`; drop unmarshal (keep the comment block about not using `WithCause` on parse errors — but the parse now happens in the router, so that whole `json.Unmarshal` block is deleted); convert the two `json.Marshal(model.RoomKeyEnsureResponse{...})` returns → `&…, nil`.
- `createRoom` (flavor A, **plus sub-handler return-type changes**):
  ```go
  func (h *Handler) createRoom(c *natsrouter.Context, req model.CreateRoomRequest) (*model.CreateRoomReply, error) {
      var ctx context.Context = c
      requesterAccount := c.Param("account")
      // delete ParseRoomCreateSubject + json.Unmarshal blocks
      roomType, err := classifyAndValidate(&req, requesterAccount)
      if err != nil { return nil, err }
      // …GetUser + name checks unchanged…
      switch roomType {
      case model.RoomTypeChannel:
          return h.handleCreateRoomChannel(ctx, &req, requester, requesterAccount, roomType)
      case model.RoomTypeDM, model.RoomTypeBotDM:
          return h.handleCreateRoomDMOrBotDM(ctx, &req, requester, roomType)
      default:
          return nil, fmt.Errorf("unknown room type: %s", roomType)
      }
  }
  ```
  Change `handleCreateRoomChannel` and `handleCreateRoomDMOrBotDM` return types from `([]byte, error)` to `(*model.CreateRoomReply, error)`: each ends in `return json.Marshal(model.CreateRoomReply{…})` — change to `return &model.CreateRoomReply{…}, nil`. Their other `return nil, err` lines are unchanged.

Delete `natsCreateRoom`, `natsRoomsInfoBatch`, `NatsHandleEnsureRoomKey` and their `RegisterCRUD` lines. `RegisterCRUD` is now an empty method (`return nil`) — **leave it for now**; `main.go` still calls it until Task 11 (deleting it here would break `main.go`'s compile). Delete the now-unused `wrappedCtx` helper **and** the `TestWrappedCtx_*` trio (`handler_test.go:2351-2402`) in this same step — `wrappedCtx` becomes unreferenced once the last wrapper is gone (golangci-lint `unused` would flag it), and its behavior is already covered by Task 1's `RequireRequestID` tests.

- [ ] **Step 5: Run to verify pass** — `make test SERVICE=room-service` → PASS. (Integration tests are intentionally still red until Task 11: `RegisterCRUD` is now an empty no-op and `Register` is not wired into `main.go` yet.)

- [ ] **Step 6: Lint + commit**

```bash
make lint && git add room-service/handler.go room-service/handler_test.go
git commit -m "refactor(room-service): migrate rooms-info-batch, ensure-room-key, create to natsrouter; remove wrappedCtx"
```

---

### Task 11: `room-service/main.go` cutover

**Files:** Modify `room-service/main.go`

- [ ] **Step 1: Replace registration + add router**

Add imports `"github.com/hmchangw/chat/pkg/natsrouter"`. Replace the `handler.RegisterCRUD(nc)` block (`main.go:182-185`) with:

```go
	router := natsrouter.New(nc, "room-service")
	router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
	handler.Register(router)
```

Then delete the now-empty `RegisterCRUD` method from `room-service/handler.go`, and remove the `otelnats` import (`github.com/Marz32onE/instrumentation-go/otel-nats/otelnats`) from `handler.go` if it is no longer referenced there (it was only used by the deleted wrappers and `RegisterCRUD`'s signature).

- [ ] **Step 2 (OPTIONAL — recommended): per-handler timeout backpressure**

Only if adopting the §6.4 recommendation. Add config field to `type config`:

```go
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"10s"`
```

and a guard + middleware line (after the `router.Use(...)` above):

```go
	if cfg.RequestTimeout <= 0 {
		slog.Error("invalid REQUEST_TIMEOUT: must be > 0", "value", cfg.RequestTimeout)
		os.Exit(1)
	}
	router.Use(natsrouter.HandlerTimeout(cfg.RequestTimeout))
```

If adopting, also add `REQUEST_TIMEOUT=10s` to `room-service/deploy/docker-compose.yml`. **Skip this entire step if the timeout is not wanted** — the migration is correct without it.

- [ ] **Step 3: Add `router.Shutdown` as the first shutdown hook**

Make the first hook in `shutdown.Wait(...)` (before `nc.Drain()`):

```go
		func(ctx context.Context) error { return router.Shutdown(ctx) },
		func(ctx context.Context) error { return nc.Drain() },
```

- [ ] **Step 4: Build + run unit tests**

Run: `make build SERVICE=room-service && make test SERVICE=room-service`
Expected: build OK; tests PASS. (`RegisterCRUD` is gone; confirm no references remain: `grep -rn RegisterCRUD room-service` returns nothing.)

- [ ] **Step 5: Integration tests**

Run: `make test-integration SERVICE=room-service`
Expected: PASS — subjects/replies are unchanged, so request/reply assertions hold. Fix only failures caused by the wiring/signature change (e.g., a test that called the removed `RegisterCRUD`).

- [ ] **Step 6: Lint + commit**

```bash
make lint
git add room-service/main.go room-service/deploy/docker-compose.yml
git commit -m "refactor(room-service): cut main.go over to natsrouter Router"
```

---

## Phase 3 — room-worker RPC + docs

### Task 12: room-worker `natsServerCreateDM` → natsrouter

**Files:** `room-worker/handler.go`, `room-worker/handler_test.go`, `room-worker/main.go`

- [ ] **Step 1: Convert the test (write failing)**

In `room-worker/handler_test.go`, rewrite `handleSyncCreateDM(ctx, data)` call sites to the typed handler, and **move** `TestRequireDedupRequestID` (`handler_test.go:4345`) out — it tested `requireDedupRequestID`, which is deleted; the strict-request-ID behavior is now covered by `pkg/natsrouter`'s `TestRequireRequestID_*` (Task 1). Add the same `ctxParams` helper (no subject params needed — concrete subject), with `c.Msg` set for body access if a test needs it:

```go
resp, err := h.serverCreateDM(ctxParams(map[string]string{}), model.SyncCreateDMRequest{/*…*/})
```

- [ ] **Step 2: Run to verify failure** — `make test SERVICE=room-worker` → FAIL.

- [ ] **Step 3: Convert the handler**

Reshape `handleSyncCreateDM(ctx, data []byte)` → `serverCreateDM(c *natsrouter.Context, req model.SyncCreateDMRequest) (*model.SyncCreateDMReply, error)`: `var ctx context.Context = c`; delete the internal `json.Unmarshal(data, &req)` (req is the param) — **keep** the field validation that returns `errInvalidSyncDMRequest`. Delete `natsServerCreateDM` and `requireDedupRequestID`. If `otelnats` is no longer referenced in `room-worker/handler.go` after removing `natsServerCreateDM`, remove its import (note: `room-worker/main.go` still imports `oteljetstream`/`otelnats` for the JetStream loop — leave those).

- [ ] **Step 4: Run to verify pass** — `make test SERVICE=room-worker` → PASS.

- [ ] **Step 5: Wire the router in `room-worker/main.go`**

Add import `"github.com/hmchangw/chat/pkg/natsrouter"`. Replace the `nc.QueueSubscribe(subject.RoomCreateDMSync(...), "room-worker", handler.natsServerCreateDM)` block (`main.go:152-155`) with:

```go
	router := natsrouter.New(nc, "room-worker")
	router.Use(natsrouter.Recovery(), natsrouter.RequireRequestID(), natsrouter.Logging())
	natsrouter.Register(router, subject.RoomCreateDMSync(cfg.SiteID), handler.serverCreateDM)
```

Add a `router.Shutdown(ctx)` hook **before** `nc.Drain()` in the existing `hooks` slice (after the `iter.Stop()` + `wg.Wait()` hooks that drain the JetStream loop):

```go
		func(ctx context.Context) error { return router.Shutdown(ctx) },
		func(ctx context.Context) error { return nc.Drain() },
```

- [ ] **Step 6: Build, test, lint, commit**

```bash
make build SERVICE=room-worker && make test SERVICE=room-worker
make test-integration SERVICE=room-worker   # subjects unchanged → should pass
make lint
git add room-worker/handler.go room-worker/handler_test.go room-worker/main.go
git commit -m "refactor(room-worker): migrate natsServerCreateDM to natsrouter"
```

---

### Task 13: docs/client-api.md + final verification

**Files:** Modify `docs/client-api.md`

- [ ] **Step 1: Update the rename malformed-body error**

At `docs/client-api.md:622`, the rename section documents the malformed-body error as `"invalid request"` with (effectively) an internal failure. Change it to reflect the new behavior: the wire message is `"invalid request payload"` and the code is `bad_request` (the router now classifies a malformed body as a client error). Verify the request-ID-required note is present for the migrated RPCs (create §211 and rename §608 already document it; the behavior is unchanged — enforced on all 20 — so no schema changes are needed elsewhere).

- [ ] **Step 2: Full-repo lint + tests**

```bash
make lint
make test
```
Expected: PASS across all packages.

- [ ] **Step 3: Confirm no dead references**

```bash
grep -rn "RegisterCRUD\|wrappedCtx\|requireDedupRequestID\|natsCreateRoom\|natsServerCreateDM" room-service room-worker
```
Expected: no matches.

- [ ] **Step 4: Commit**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): rename malformed-body error now bad_request/invalid request payload"
```

- [ ] **Step 5: SAST (blocking CI gate)**

```bash
make sast
```
Expected: no new medium+ findings. (Migration removes manual `errnats.Reply` sites and keeps the existing `#nosec G117` comments inside the preserved `getRoomKey` core; no new SAST surface.)

---

## Self-review notes (already reconciled against the spec)

- **Every spec section maps to a task:** RequireRequestID → T1; Pattern builders → T2; typed replies + RoomRenameRequest → T3; the 20 handler flavors (10 Register / 6 NoBody / 4 NoBody+optional) → T4–T10; main.go wiring + shutdown + optional HandlerTimeout → T11; room-worker RPC → T12; wire-compat client-api.md + error-model bad-body fix → T9/T13; observability parity → preserved by `var ctx context.Context = c` (recipe step 3); test deletions (invalid-subject, wrappedCtx, RequireDedupRequestID) → T4–T10, T12.
- **Type/name consistency:** handler methods are unexported `xxx(c, …)`; builders are `XxxPattern(siteID)`; responses are `*model.StatusReply` / `*model.StatusWithRequestReply` / existing `*model.XxxResponse`; `c.Param("account"|"roomID"|"orgID")` matches the `{account}`/`{roomID}`/`{orgID}` placeholders in the Task 2 builders.
- **Concrete subjects** (rooms-info-batch, ensure-room-key, restricted, create-dm) register with their existing builders and take **no** `c.Param`.
