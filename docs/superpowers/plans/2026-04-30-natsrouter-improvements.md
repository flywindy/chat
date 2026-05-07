# natsrouter Concurrency Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `pkg/natsrouter`'s implicit per-subscription serialization (one in-flight handler per route per pod) with a Gin-style admission-controlled concurrency model: spawn-per-message optionally guarded by a semaphore, with a 503-busy reply when the semaphore is saturated.

**Architecture:** Each `*nats.Subscription` still has one dispatcher goroutine inside nats.go. The router's callback no longer runs the handler chain inline — it acquires a router-level semaphore (non-blocking) when one is configured, and on success spawns a goroutine that runs the chain. On semaphore-full, the callback publishes a `{"error":"service busy","code":"unavailable"}` reply and returns. The router-level `WithMaxConcurrency` option installs the semaphore; **default behavior is unbounded** (no admission control, gin-style). A new `HandlerTimeout(d)` middleware enforces a per-handler context deadline.

**Note (post-implementation):** Earlier drafts of this plan referenced a per-route `WithConcurrency` bulkhead and a default cap of 100. Both were dropped during implementation — the shipped surface is a single router-level `WithMaxConcurrency` option with unbounded default. References to `WithConcurrency` and "default 100" in the task bodies below are superseded by the actual implementation.

**Tech Stack:** Go 1.25, `nats-io/nats.go`, `Marz32onE/instrumentation-go/otel-nats`, `stretchr/testify`.

**Out of scope:**
- Replacing the router's NATS transport. Subjects, queue groups, and subscription semantics are unchanged.
- Any service-level wiring (history-service, message-worker, etc.) — this plan only changes `pkg/natsrouter`. Service-level adoption is tracked in each service's own plan (e.g. history-service Task 11 wires `WithMaxConcurrency`).
- Per-route ordering guarantees. The new model loses per-subject FIFO; documented in Task 7. (Earlier drafts proposed `WithConcurrency(1)` per route as the opt-in; that option was not shipped — see post-implementation note above.)

---

## Task 1: Add `WithMaxConcurrency` constructor option and `ErrUnavailable` error

**Why:** Establish the public API and types the rest of the plan builds on. Add a router-level semaphore (sized by `WithMaxConcurrency(N)`, default 100), plus a new `ErrUnavailable` error type for the saturation-reply path. No behavior change yet — Task 2 wires the semaphore into the dispatch path.

**Files:**
- Modify: `pkg/natsrouter/errors.go` (add `CodeUnavailable`, `ErrUnavailable`)
- Modify: `pkg/natsrouter/router.go` (add `Option`, `WithMaxConcurrency`, semaphore + WaitGroup fields, default constant)

- [ ] **Step 1: Write the failing tests**

Append to `pkg/natsrouter/errors_test.go` (create the file if missing — the existing `errors.go` has no test pair):

```go
package natsrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrUnavailable_HasCodeAndMessage(t *testing.T) {
	err := ErrUnavailable("service busy")
	assert.Equal(t, "unavailable", err.Code)
	assert.Equal(t, "service busy", err.Message)
}

func TestCodeUnavailable_Constant(t *testing.T) {
	assert.Equal(t, "unavailable", CodeUnavailable)
}
```

Append to `pkg/natsrouter/router_test.go`:

```go
func TestRouter_DefaultMaxConcurrency(t *testing.T) {
	r := New(nil, "test")
	assert.Equal(t, defaultMaxConcurrency, cap(r.sem))
}

func TestRouter_WithMaxConcurrency_Overrides(t *testing.T) {
	r := New(nil, "test", WithMaxConcurrency(7))
	assert.Equal(t, 7, cap(r.sem))
}

func TestRouter_WithMaxConcurrency_IgnoresNonPositive(t *testing.T) {
	r := New(nil, "test", WithMaxConcurrency(0))
	assert.Equal(t, defaultMaxConcurrency, cap(r.sem))
	r2 := New(nil, "test", WithMaxConcurrency(-1))
	assert.Equal(t, defaultMaxConcurrency, cap(r2.sem))
}
```

If `router_test.go` doesn't import `assert` already, add `"github.com/stretchr/testify/assert"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/natsrouter/...`
Expected: FAIL — `ErrUnavailable`, `CodeUnavailable`, `Option`, `WithMaxConcurrency`, `defaultMaxConcurrency`, and `r.sem` all undefined.

- [ ] **Step 3: Add `CodeUnavailable` and `ErrUnavailable` to errors.go**

In `pkg/natsrouter/errors.go`, after the existing `CodeInternal` constant, append:

```go
	// CodeUnavailable signals the service is temporarily over capacity and
	// the caller should retry. Used by the router's admission control when
	// the per-pod handler concurrency cap is reached.
	CodeUnavailable = "unavailable"
```

The block becomes:
```go
const (
	CodeBadRequest  = "bad_request"
	CodeNotFound    = "not_found"
	CodeForbidden   = "forbidden"
	CodeConflict    = "conflict"
	CodeInternal    = "internal"
	CodeUnavailable = "unavailable"
)
```

After the existing `ErrInternal` helper, add:

```go
// ErrUnavailable creates a user-facing service-busy error. Returned by the
// router's admission control when the per-pod handler concurrency cap is
// reached. Callers should retry with backoff.
func ErrUnavailable(message string) *RouteError { return ErrWithCode(CodeUnavailable, message) }
```

- [ ] **Step 4: Add semaphore + WaitGroup fields and `WithMaxConcurrency` to router.go**

In `pkg/natsrouter/router.go`, add an import for `sync` if not already present (it is — used by `sync.Mutex`).

Add this constant near the top (after the `package` line and imports):

```go
// defaultMaxConcurrency is the default per-pod handler concurrency cap. Sized
// to match the project-wide MAX_WORKERS convention used by JetStream worker
// services. Override with WithMaxConcurrency.
const defaultMaxConcurrency = 100
```

Replace the `Router` struct:

```go
type Router struct {
	nc         *otelnats.Conn
	queue      string
	middleware []HandlerFunc

	mu   sync.Mutex
	subs []*nats.Subscription
}
```

with:

```go
type Router struct {
	nc         *otelnats.Conn
	queue      string
	middleware []HandlerFunc

	// sem gates handler concurrency: every handler invocation acquires a
	// slot before running and releases it on return. cap(sem) is the
	// per-pod concurrency ceiling. Configured by WithMaxConcurrency.
	sem chan struct{}
	// wg tracks in-flight handler goroutines so Shutdown can wait for
	// them to finish.
	wg sync.WaitGroup

	mu   sync.Mutex
	subs []*nats.Subscription
}

// Option configures a Router on construction.
type Option func(*Router)

// WithMaxConcurrency sets the maximum number of in-flight handler
// invocations across all routes registered on this router. Defaults to
// defaultMaxConcurrency. Non-positive values are ignored. Saturation
// triggers a 503-style ErrUnavailable reply.
func WithMaxConcurrency(n int) Option {
	return func(r *Router) {
		if n > 0 {
			r.sem = make(chan struct{}, n)
		}
	}
}
```

Replace the existing `New` function:

```go
func New(nc *otelnats.Conn, queue string) *Router {
	return &Router{nc: nc, queue: queue}
}
```

with:

```go
func New(nc *otelnats.Conn, queue string, opts ...Option) *Router {
	r := &Router{
		nc:    nc,
		queue: queue,
		sem:   make(chan struct{}, defaultMaxConcurrency),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}
```

- [ ] **Step 5: Tighten the `Registrar` doc comment to signal composition intent**

This is forward-looking documentation only — no API change. The minimal interface is intentionally preserved so future wrappers (e.g. a route group that prepends a subject prefix and shared middleware) can implement `Registrar` and delegate to a parent without breaking changes.

In `pkg/natsrouter/router.go`, replace the existing block:

```go
// Registrar is the interface for registering route handlers.
type Registrar interface {
	addRoute(pattern string, handlers []HandlerFunc)
}
```

with:

```go
// Registrar is the interface that Register/RegisterNoBody/RegisterVoid use
// to attach handlers. Implemented by *Router today.
//
// The contract is intentionally minimal so future wrappers (for example a
// route-group type that prepends a shared subject prefix and shared
// middleware) can compose by implementing the same interface and
// delegating to a parent Registrar. addRoute receives the
// fully-resolved subject pattern and the complete middleware-chain-plus-
// handler slice; the implementation owns the NATS subscription lifecycle.
type Registrar interface {
	addRoute(pattern string, handlers []HandlerFunc)
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./pkg/natsrouter/...`
Expected: PASS — three new router tests, two new errors tests, all existing tests still green.

- [ ] **Step 7: Verify no service breaks (variadic options means existing call sites still compile)**

Run: `go build ./...` from repo root.
Expected: PASS — every existing `natsrouter.New(nc, queue)` call still typechecks.

- [ ] **Step 8: Commit**

```bash
git add pkg/natsrouter/errors.go pkg/natsrouter/errors_test.go pkg/natsrouter/router.go pkg/natsrouter/router_test.go
git commit -m "feat(natsrouter): add WithMaxConcurrency option and ErrUnavailable error"
```

---

## Task 2: Spawn handlers and reply busy on saturation

**Why:** The dispatcher goroutine currently runs the handler chain inline, which serializes everything to one in-flight handler per route. Wire the semaphore from Task 1 into `addRoute`'s callback: try a non-blocking semaphore acquire; if it succeeds, spawn a goroutine to run the chain (releasing on return); if it fails, publish an `ErrUnavailable` reply immediately and return. The dispatcher goroutine becomes a thin admission-control loop.

**Files:**
- Modify: `pkg/natsrouter/router.go` (rewrite `addRoute` callback; add `replyBusy` helper)
- Modify: `pkg/natsrouter/integration_test.go` (existing heavy-concurrency test must opt into a higher cap; add new busy-reply integration test)

- [ ] **Step 1: Write the failing busy-reply integration test**

Append to `pkg/natsrouter/integration_test.go`:

```go
// TestIntegration_BusyReplyOnSaturation verifies that requests arriving
// while the per-pod concurrency cap is exhausted receive an ErrUnavailable
// reply rather than blocking.
func TestIntegration_BusyReplyOnSaturation(t *testing.T) {
	nc := setupNATS(t)
	r := natsrouter.New(nc, "integration-busy", natsrouter.WithMaxConcurrency(1))

	gate := make(chan struct{})
	// Safety net: if any assertion below fails before we close the gate,
	// the spawned client and handler goroutines would block on `<-gate`
	// forever (bounded only by nc.Request's 5s timeout). This idempotent
	// closer guarantees release on every test exit path.
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()

	// Synchronize on real handler entry instead of polling: the handler
	// signals `entered` before blocking on `gate`, so the busy-reply poll
	// only starts once the slot is genuinely held.
	entered := make(chan struct{}, 1)
	natsrouter.Register(r, "busy.{id}",
		func(c *natsrouter.Context, req echoReq) (*echoResp, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
			return &echoResp{Seq: req.Seq}, nil
		})

	// First request occupies the only slot.
	first := make(chan struct {
		resp []byte
		err  error
	}, 1)
	go func() {
		data, _ := json.Marshal(echoReq{Seq: 1})
		resp, err := nc.Request(context.Background(), "busy.1", data, 5*time.Second)
		var b []byte
		if resp != nil {
			b = resp.Data
		}
		first <- struct {
			resp []byte
			err  error
		}{b, err}
	}()

	// Wait for handler to actually be in the gate before polling for busy.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler never entered the chain")
	}

	// A second request must now get busy because the slot is held.
	data, _ := json.Marshal(echoReq{Seq: 2})
	resp, err := nc.Request(context.Background(), "busy.2", data, 2*time.Second)
	require.NoError(t, err)
	var re natsrouter.RouteError
	require.NoError(t, json.Unmarshal(resp.Data, &re))
	assert.Equal(t, natsrouter.CodeUnavailable, re.Code, "expected busy reply once slot is held")

	// Release the gate; first request must complete normally.
	close(gate)
	got := <-first
	require.NoError(t, got.err)
	var ok echoResp
	require.NoError(t, json.Unmarshal(got.resp, &ok))
	assert.Equal(t, 1, ok.Seq)
}

// TestRouter_replyBusy_NoReplySubject verifies that draft-and-forget
// messages (empty Reply subject) trigger the silent-drop path with a
// Warn log and no panic. In-package unit test — no NATS connection
// required because replyBusy short-circuits before touching the wire.
func TestRouter_replyBusy_NoReplySubject(t *testing.T) {
	r := New(nil, "test")
	msg := &nats.Msg{Subject: "void.subject", Reply: ""}
	// Must not panic. Reply == "" hits the early-return path.
	r.replyBusy(msg)
}

// TestIntegration_SpawnSitePanicBackstop verifies that a handler panic
// without Recovery middleware is caught by the spawn-site backstop:
// the process survives, the caller receives an "internal error" reply,
// and subsequent requests still work (semaphore slot released, WG
// decremented).
func TestIntegration_SpawnSitePanicBackstop(t *testing.T) {
	nc := setupNATS(t)
	// Note: NO Recovery middleware installed. We're testing the spawn-site
	// backstop, not the middleware path.
	r := natsrouter.New(nc, "integration-panic-backstop", natsrouter.WithMaxConcurrency(2))

	natsrouter.Register(r, "boom.{id}",
		func(c *natsrouter.Context, req echoReq) (*echoResp, error) {
			panic("intentional handler panic")
		})

	// Panicking request must receive a reply (not time out) and the
	// reply must indicate an error.
	data, _ := json.Marshal(echoReq{Seq: 1})
	resp, err := nc.Request(context.Background(), "boom.1", data, 5*time.Second)
	require.NoError(t, err, "panicking handler should still produce a reply via backstop")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(resp.Data, &payload))
	assert.Equal(t, "internal error", payload["error"], "expected internal error reply from backstop")

	// Process survived: a follow-up normal request must succeed.
	natsrouter.Register(r, "ok.{id}",
		func(c *natsrouter.Context, req echoReq) (*echoResp, error) {
			return &echoResp{Seq: req.Seq}, nil
		})
	data, _ = json.Marshal(echoReq{Seq: 2})
	resp, err = nc.Request(context.Background(), "ok.42", data, 5*time.Second)
	require.NoError(t, err)
	var ok echoResp
	require.NoError(t, json.Unmarshal(resp.Data, &ok))
	assert.Equal(t, 2, ok.Seq)
}
```

Add `"github.com/nats-io/nats.go"` to the test file's imports if not already present (needed by `TestRouter_replyBusy_NoReplySubject`). That test belongs in `pkg/natsrouter/router_test.go` (package `natsrouter`, in-package, accesses unexported `replyBusy`); place it alongside the other `TestRouter_*` tests rather than in `integration_test.go`.

- [ ] **Step 2: Run the new test to verify it fails**

Run: `go test -tags=integration ./pkg/natsrouter/ -run TestIntegration_BusyReplyOnSaturation -v`
Expected: FAIL — the second request blocks rather than getting a busy reply (current code serializes inside the dispatcher).

- [ ] **Step 3: Add the `replyBusy` helper in router.go**

Add to `pkg/natsrouter/router.go`, after the `New` constructor:

```go
// replyBusy publishes an ErrUnavailable reply on m.Reply, used when the
// router's admission control rejects a message. For request/reply
// messages the caller observes the busy code and can retry. For
// fire-and-forget messages (empty Reply subject — typically
// RegisterVoid routes) the message is silently dropped at this point;
// we emit a Warn log so operators can correlate drops with the
// busy-reply rate.
func (r *Router) replyBusy(msg *nats.Msg) {
	if msg.Reply == "" {
		slog.Warn("natsrouter: dropped fire-and-forget message under saturation",
			"subject", msg.Subject)
		return
	}
	natsutil.ReplyJSON(msg, ErrUnavailable("service busy"))
}
```

Add the imports `"github.com/hmchangw/chat/pkg/natsutil"` and `"log/slog"` to the file.

- [ ] **Step 4: Rewrite the `natsHandler` closure inside `addRoute`**

In `pkg/natsrouter/router.go`, replace the existing `natsHandler` closure inside `addRoute`:

```go
	natsHandler := func(m otelnats.Msg) {
		c := acquireContext(m.Context(), m.Msg, rt.extractParams(m.Msg.Subject), all)
		c.Next()
		releaseContext(c)
	}
```

with:

```go
	natsHandler := func(m otelnats.Msg) {
		select {
		case r.sem <- struct{}{}:
		default:
			r.replyBusy(m.Msg)
			return
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() { <-r.sem }()
			// Process-safety backstop: catch any panic that bypassed
			// user-installed Recovery middleware. Recovery middleware (when
			// configured via r.Use) catches first and sends a structured
			// reply; this defer only fires if Recovery is absent or if a
			// panic somehow escapes it. Either way, the process survives
			// and the deferred semaphore/WG cleanup below still runs.
			defer func() {
				if rec := recover(); rec != nil {
					// Warn, not Error: a hit here means Recovery middleware
					// is misconfigured or absent (Recovery would have caught
					// it earlier and produced a structured ErrInternal
					// reply). The process survived, so the severity matches
					// "operator should fix the middleware setup", not
					// "production incident".
					slog.Warn("natsrouter: panic in handler caught by spawn backstop",
						"subject", m.Msg.Subject,
						"panic", rec,
						"stack", string(debug.Stack()))
					if m.Msg.Reply != "" {
						natsutil.ReplyError(m.Msg, "internal error")
					}
				}
			}()
			c := acquireContext(m.Context(), m.Msg, rt.extractParams(m.Msg.Subject), all)
			defer releaseContext(c)
			c.Next()
		}()
	}
```

Add the imports `"log/slog"` and `"runtime/debug"` to `pkg/natsrouter/router.go` if not already present.

**Defer ordering note (LIFO):** on goroutine exit (normal return or panic), the defers fire in reverse-registration order:
1. `releaseContext(c)` — chainState back to the pool.
2. `recover()` — catches any panic, logs, optionally publishes "internal error" reply.
3. `<-r.sem` — releases the admission slot.
4. `r.wg.Done()` — decrements the shutdown WaitGroup.

Recovery middleware (registered via `r.Use(Recovery())`) catches panics earlier in the chain and never lets them reach this defer; that's the intended path. The spawn-level recover is purely a process-safety net so a misconfigured service can't be crashed by a handler bug.

- [ ] **Step 5: Update `TestIntegration_ConcurrentRequestsWithCopy` to opt into a higher cap**

The existing test fires 300 concurrent requests and asserts all succeed. With the default cap of 100, some would be busy-replied. The test's intent is heavy-concurrency stress (context safety, not admission control), so bump the cap.

In `pkg/natsrouter/integration_test.go`, replace:

```go
	r := natsrouter.New(nc, "integration-concurrent")
```

with:

```go
	r := natsrouter.New(nc, "integration-concurrent", natsrouter.WithMaxConcurrency(500))
```

- [ ] **Step 6: Run all tests to verify they pass**

Run:
```bash
go test ./pkg/natsrouter/...
go test -tags=integration ./pkg/natsrouter/...
```
Expected: PASS — unit tests still green; integration tests including the new busy-reply test pass.

- [ ] **Step 7: Run with race detector**

Run: `go test -tags=integration -race ./pkg/natsrouter/...`
Expected: PASS — no data races introduced by the spawn-per-message model.

- [ ] **Step 8: Commit**

```bash
git add pkg/natsrouter/router.go pkg/natsrouter/integration_test.go
git commit -m "feat(natsrouter): spawn handlers with semaphore admission control"
```

---

## Task 3: Wait for in-flight handler goroutines on Shutdown

**Why:** With handlers running in spawned goroutines, `sub.Drain()` followed by `SubscriptionClosed` only guarantees that the *dispatcher* has stopped — handler goroutines may still be running. Add a `WaitGroup` wait after the close signal so `Router.Shutdown` returns only when all spawned handlers have finished (or `ctx` expires).

**Execution order requirement:** This task's failing-test phase ONLY produces the expected `FAIL` once Task 2 has been committed. Until Task 2 is in place, handlers run inline on the dispatcher and `sub.Drain()` naturally waits for them — the new test would pass by accident. Run the tasks sequentially: 1 → 2 → 3.

**Files:**
- Modify: `pkg/natsrouter/router.go` (extend `Shutdown` to wait on `r.wg`)
- Modify: `pkg/natsrouter/shutdown_test.go` (add WG-wait assertion if not already covered)

- [ ] **Step 1: Inspect the existing shutdown test for guidance**

Read `pkg/natsrouter/shutdown_test.go` to see the existing patterns. The integration-level test `TestIntegration_ShutdownUnderLoad` already validates that Shutdown blocks while handlers are running. With the new spawn model, that test must continue to pass — and the new WG wait is what makes it pass reliably.

- [ ] **Step 2: Write a failing test that proves Shutdown waits for spawned handlers**

Append to `pkg/natsrouter/integration_test.go`:

```go
// TestIntegration_ShutdownWaitsForSpawnedHandlers verifies that Shutdown
// blocks until handler goroutines (spawned by the semaphore admission
// model) have returned, not merely until the dispatcher has stopped.
func TestIntegration_ShutdownWaitsForSpawnedHandlers(t *testing.T) {
	nc := setupNATS(t)
	r := natsrouter.New(nc, "integration-shutdown-wg", natsrouter.WithMaxConcurrency(8))

	gate := make(chan struct{})
	// Safety net: any test failure before close(gate) below would pin
	// the spawned client goroutines and the gated handler goroutines
	// for up to nc.Request's 5s timeout. This idempotent closer
	// guarantees release on every exit path (success, t.Fatal, or
	// require failure).
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	var entered atomic.Int64
	var completed atomic.Int64
	natsrouter.Register(r, "wg.{id}",
		func(c *natsrouter.Context, req echoReq) (*echoResp, error) {
			entered.Add(1)
			<-gate
			completed.Add(1)
			return &echoResp{Seq: req.Seq}, nil
		})

	const inflight = 4
	for i := 0; i < inflight; i++ {
		go func(i int) {
			data, _ := json.Marshal(echoReq{Seq: i})
			_, _ = nc.Request(context.Background(), fmt.Sprintf("wg.%d", i), data, 5*time.Second)
		}(i)
	}

	// Synchronise on a real signal: every handler increments `entered` on
	// arrival and then blocks on `gate`. Once entered==inflight, all four
	// goroutines are inside the chain and Shutdown will have to wait on
	// the WaitGroup for them.
	require.Eventually(t, func() bool {
		return entered.Load() == int64(inflight)
	}, 5*time.Second, 20*time.Millisecond, "all %d handlers must enter before Shutdown is called", inflight)

	// Shutdown in a goroutine; it must NOT return before we close gate.
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- r.Shutdown(ctx)
	}()

	// Give Shutdown 200ms to (incorrectly) return early.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before handlers completed: err=%v", err)
	case <-time.After(200 * time.Millisecond):
		// expected — Shutdown is still blocked on the WaitGroup.
	}

	close(gate)

	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after handlers completed")
	}
	assert.Equal(t, int64(inflight), completed.Load(), "every gated handler must complete")
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags=integration ./pkg/natsrouter/ -run TestIntegration_ShutdownWaitsForSpawnedHandlers -v`
Expected: FAIL — `Shutdown` currently returns once `SubscriptionClosed` fires, before spawned handlers have completed. The test detects the early return.

- [ ] **Step 4: Extend `Shutdown` to wait on `r.wg`**

In `pkg/natsrouter/router.go`, locate the end of `Shutdown` (just before `return errors.Join(errs...)`) and replace this:

```go
	for i, ch := range closed {
		select {
		case <-ch:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("waiting for %q close: %w", subs[i].Subject, ctx.Err()))
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
```

with:

```go
	// Wait for each subscription's dispatcher to finish. On ctx expiry,
	// record the error and stop waiting on remaining subscriptions — but
	// DO NOT return early. We must fall through to the WaitGroup wait
	// below so in-flight handler goroutines are not abandoned. The
	// WaitGroup wait itself also respects ctx and will short-circuit.
closeLoop:
	for i, ch := range closed {
		select {
		case <-ch:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("waiting for %q close: %w", subs[i].Subject, ctx.Err()))
			break closeLoop
		}
	}

	// Wait for in-flight handler goroutines (admission-control model) to
	// return. ctx-bounded so a wedged handler can't pin Shutdown forever.
	handlersDone := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("waiting for in-flight handlers: %w", ctx.Err()))
	}
	return errors.Join(errs...)
```

**Important — why this matters:** the previous draft of this task used `return errors.Join(errs...)` inside the close-loop's `ctx.Done()` branch. That return would skip the `wg.Wait()` block below, abandoning in-flight handler goroutines if shutdown's deadline expired mid-drain. The labeled `break closeLoop` falls through correctly so abandoned goroutines are at least attempted-waited.

- [ ] **Step 5: Run the integration tests to verify they pass**

Run: `go test -tags=integration -race ./pkg/natsrouter/...`
Expected: PASS — `TestIntegration_ShutdownWaitsForSpawnedHandlers` passes; `TestIntegration_ShutdownUnderLoad` passes; everything else still green.

- [ ] **Step 6: Run unit tests**

Run: `go test -race ./pkg/natsrouter/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/natsrouter/router.go pkg/natsrouter/integration_test.go
git commit -m "feat(natsrouter): Shutdown waits for in-flight handler goroutines"
```

---

## Task 4: `HandlerTimeout` middleware

**Why:** Without a ceiling, a slow Cassandra/Mongo query can keep a handler running for the full driver timeout, holding a semaphore slot that turns away subsequent requests with `ErrUnavailable`. A router-level handler-timeout middleware sets a ceiling — once exceeded, the handler's `ctx.Done()` fires and downstream calls (gocql, mongo-driver) abort their in-flight queries. Services apply it via `Router.Use(HandlerTimeout(5 * time.Second))` alongside `Recovery` and `Logging`.

**Files:**
- Modify: `pkg/natsrouter/middleware.go` (add `HandlerTimeout`)
- Modify: `pkg/natsrouter/middleware_test.go` (or create — add unit tests)

- [ ] **Step 1: Check whether `middleware_test.go` exists**

```bash
ls pkg/natsrouter/middleware_test.go
```
If it doesn't exist, create it in Step 2 with the package declaration and imports. If it does, append.

- [ ] **Step 2: Write failing tests**

In `pkg/natsrouter/middleware_test.go`:

```go
package natsrouter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerTimeout_SetsDeadline(t *testing.T) {
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	var observedDeadline time.Time
	var ok bool
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(50 * time.Millisecond),
		func(c *Context) {
			observedDeadline, ok = c.Deadline()
		},
	}
	c.Next()

	require.True(t, ok, "deadline must be set inside the chain")
	assert.WithinDuration(t, time.Now().Add(50*time.Millisecond), observedDeadline, 30*time.Millisecond)
}

func TestHandlerTimeout_DoneFiresAfterExpiry(t *testing.T) {
	c := &Context{ctx: context.Background(), chain: &chainState{index: -1}}
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(20 * time.Millisecond),
		func(c *Context) {
			// Generous outer budget (2s) to absorb CI scheduler jitter — the
			// 20ms timer is what we're verifying fires; the outer bound only
			// catches a totally broken implementation.
			select {
			case <-c.Done():
				// expected
			case <-time.After(2 * time.Second):
				t.Fatal("ctx.Done() did not fire within 2s after a 20ms timeout")
			}
		},
	}
	c.Next()
}

// TestHandlerTimeout_DoesNotCancelParentContext verifies that the
// timeout middleware's defer-cancel only cancels its own derived ctx,
// never the parent ctx supplied to the handler chain. The earlier
// name ("DoesNotLeakDeadlineToCallerAfterChainEnds") was misleading
// — what's actually being asserted is parent-ctx isolation.
func TestHandlerTimeout_DoesNotCancelParentContext(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	c := &Context{ctx: parent, chain: &chainState{index: -1}}
	c.chain.handlers = []HandlerFunc{
		HandlerTimeout(20 * time.Millisecond),
		func(c *Context) {
			// no-op, return immediately
		},
	}
	c.Next()
	// HandlerTimeout's `defer cancel()` only cancels the derived ctx it
	// installed via SetContext. The parent supplied at acquireContext
	// time must remain unaffected — verify that here.
	select {
	case <-parent.Done():
		t.Fatal("parent context must not be cancelled by HandlerTimeout")
	default:
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./pkg/natsrouter/...`
Expected: FAIL — `HandlerTimeout` undefined.

- [ ] **Step 4: Add `HandlerTimeout` to middleware.go**

Append to `pkg/natsrouter/middleware.go`:

```go
// HandlerTimeout returns middleware that wraps the handler context with a
// deadline of d. Downstream calls that respect context (Cassandra/Mongo
// drivers, otelnats.Conn.Publish, etc.) will abort if the chain runs longer
// than d. The deadline is released when the chain returns.
//
// Caveat — the timeout does NOT actively interrupt a running handler. A
// handler doing pure CPU work or calling a non-context-aware library will
// run to completion past the deadline, holding its admission slot the
// whole time. Make sure handlers either propagate ctx into every blocking
// call or are short by construction.
//
// Reply mapping — when a context-aware downstream call returns
// context.DeadlineExceeded and the handler returns
// `fmt.Errorf("...: %w", err)`, the router's replyErr path falls through
// to `"internal error"` (no RouteError match). Recommended pattern: in
// the handler, map the deadline-expired sentinel explicitly, e.g.
//   if errors.Is(err, context.DeadlineExceeded) {
//       return nil, natsrouter.ErrUnavailable("request timed out")
//   }
// so the caller sees a structured "unavailable" code instead of a
// generic internal error.
//
// Place this AFTER RequestID and BEFORE Logging so the duration logged by
// Logging includes any time spent waiting for the deadline.
func HandlerTimeout(d time.Duration) HandlerFunc {
	return func(c *Context) {
		ctx, cancel := context.WithTimeout(c.ctx, d)
		defer cancel()
		c.SetContext(ctx)
		c.Next()
	}
}
```

Add `"context"` to the file's imports if not already present.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./pkg/natsrouter/...`
Expected: PASS — three new middleware tests, all existing tests still green.

- [ ] **Step 6: Commit**

```bash
git add pkg/natsrouter/middleware.go pkg/natsrouter/middleware_test.go
git commit -m "feat(natsrouter): HandlerTimeout middleware"
```

---

## Task 5: Document the concurrency model and ordering trade-off

**Why:** The behavior change introduced by Tasks 2–3 (semaphore admission, busy-reply, ordering loss) is invisible from the API surface. Document it in the package-level `doc.go` so adopters know what they're opting into.

**Files:**
- Modify: `pkg/natsrouter/doc.go`

- [ ] **Step 1: Replace the existing `doc.go` content**

Replace `pkg/natsrouter/doc.go` with:

```go
// Package natsrouter provides Gin-style pattern-based routing for NATS
// request/reply services with typed handlers, middleware, and automatic
// JSON marshal/unmarshal.
//
// # Concurrency model
//
// The router admits at most N concurrent handler invocations per process
// across all routes registered on it (default 100, override with
// WithMaxConcurrency at construction). Admission is enforced by a
// non-blocking acquire on a semaphore inside the per-subscription
// dispatcher callback:
//
//   - On acquire success, the router spawns a goroutine that runs the
//     middleware chain + handler, releasing the semaphore on return.
//   - On acquire failure (semaphore full), the router publishes an
//     ErrUnavailable reply (`{"code":"unavailable","error":"service busy"}`)
//     immediately and returns. Callers should retry with backoff.
//
// Per-route concurrency overrides are not supported today. The Registrar
// interface is intentionally minimal so a future wrapper (e.g. a route
// group that prepends a subject prefix and shared middleware, or a
// bulkhead with its own admission semaphore) can be added without
// breaking the existing API. Route-level isolation should wait until
// real evidence of noisy-neighbor contention surfaces in production.
//
// # Fire-and-forget routes
//
// RegisterVoid handlers have no NATS reply subject by definition. When a
// fire-and-forget message arrives while the semaphore is saturated, the
// router has no reply channel on which to publish ErrUnavailable, so the
// message is SILENTLY DROPPED. Callers that publish to a void route via
// nc.Publish (rather than nc.Request) get no signal that the message
// was dropped. Size MaxConcurrency conservatively for services that
// expose RegisterVoid endpoints, or front them with JetStream so
// dropped messages can be redelivered.
//
// # Queue-group fairness under saturation
//
// NATS queue-group routing distributes messages among subscribers
// without knowing whether any individual subscriber's process-level
// admission control is full. A saturated pod will continue to receive
// (and busy-reply) its share of messages even while other pods in the
// queue group sit idle. Operators should monitor the per-pod
// busy-reply rate (or set up server-side auto-scaling on it) rather
// than assume queue-group routing alone provides load balancing.
//
// # Ordering
//
// Per-subject FIFO ordering is NOT preserved. Two messages that arrive
// on the same subscription are spawned into independent goroutines and
// race; whichever wins the goroutine schedule runs first. Handlers must
// be idempotent or use external coordination (e.g. Cassandra LWTs,
// Mongo conditional updates) to ensure correctness under concurrent
// invocation.
//
// # Panic safety
//
// The router installs a process-safety backstop in every spawned
// handler goroutine: an unrecovered panic is caught at the spawn
// site, logged with stack trace, and (if the message has a Reply
// subject) replied to with "internal error". This guarantees the
// process cannot be crashed by a single bad handler regardless of
// middleware configuration. Recovery middleware (registered via
// r.Use(Recovery())) is still the recommended path because it
// produces structured ErrInternal replies enriched with request-ID
// and other middleware-set fields; the spawn-site backstop is
// strictly a defense-in-depth catch.
//
// # Shutdown
//
// Router.Shutdown drains every subscription, waits for the dispatcher
// goroutines to exit (SubscriptionClosed), and then waits on a
// WaitGroup that tracks every spawned handler goroutine. The full
// shutdown returns only after all in-flight handlers have completed or
// the context expires (whichever comes first).
//
// See README.md in this directory for full documentation and examples.
package natsrouter
```

- [ ] **Step 2: Verify the package still builds and `go doc` renders cleanly**

Run:
```bash
go build ./pkg/natsrouter/
go doc ./pkg/natsrouter/
```
Expected: PASS — the rewritten doc comment appears in `go doc` output.

- [ ] **Step 3: Run all tests as a final sanity check**

Run:
```bash
go test -race ./pkg/natsrouter/...
go test -tags=integration -race ./pkg/natsrouter/...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/natsrouter/doc.go
git commit -m "docs(natsrouter): document concurrency model and ordering trade-off"
```

---

## Self-Review

After all 5 tasks are complete:

- [ ] Run the full quality gate
  ```bash
  go test -race ./pkg/natsrouter/...
  go test -tags=integration -race ./pkg/natsrouter/...
  go vet ./pkg/natsrouter/...
  golangci-lint run ./pkg/natsrouter/...
  ```
  Expected: all PASS.

- [ ] Confirm every existing service that imports natsrouter still builds:
  ```bash
  go build ./...
  ```
  Expected: PASS.

- [ ] Confirm the public API surface added:
  ```bash
  go doc ./pkg/natsrouter | grep -E "WithMaxConcurrency|HandlerTimeout|ErrUnavailable|CodeUnavailable"
  ```
  Expected: all four symbols visible.

- [ ] Verify the package's stated invariants by re-reading the new `doc.go` and tracing each claim through the code:
  - "non-blocking acquire" → `select { case r.sem <- struct{}{}: default: ... }` in router.go
  - "publishes an ErrUnavailable reply" → `r.replyBusy(m.Msg)` in router.go
  - "WaitGroup tracks every spawned handler" → `r.wg.Add(1)` in addRoute, `r.wg.Wait()` in Shutdown
  - "fire-and-forget routes drop silently" → `if msg.Reply == "" { return }` early-return in `replyBusy`
  - "process-safety backstop on panic" → `defer func() { if rec := recover(); rec != nil { ... } }()` in addRoute spawn closure
  - "Registrar contract is intentionally minimal" → `addRoute(pattern, handlers)` unchanged signature, ready for future Group composition

- [ ] Verify a downstream service can adopt the new API in one line:
  ```bash
  grep -n "natsrouter.WithMaxConcurrency\|natsrouter.HandlerTimeout" history-service/cmd/main.go
  ```
  Expected: matches once the history-service plan's Task 11 ships.

