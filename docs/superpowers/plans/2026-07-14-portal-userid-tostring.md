# Portal-service `$toString` userId Projection Fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `portal-service`'s `ListEmployees` aggregation resilient to `users._id` being a MongoDB ObjectID by coercing it to a string with `$toString`, so an ObjectID row can no longer crash the directory load.

**Architecture:** Single-line change in the aggregation's final `$project` stage (`userId: "$_id"` → `userId: {$toString: "$_id"}`), plus an integration regression test that inserts an ObjectID `_id` and a string `_id` and asserts both decode into the Go `string` `UserID` field.

**Tech Stack:** Go 1.25, `go.mongodb.org/mongo-driver/v2` (bson + aggregation), testcontainers via `pkg/testutil.MongoDB`, `testify` assertions.

## Global Constraints

- Go 1.25; single root `go.mod`; service is flat `package main` at repo root.
- Use `make` targets, never raw `go` commands (`make build SERVICE=portal-service`, `make test-integration SERVICE=portal-service`).
- Integration tests use the `//go:build integration` tag, live in `package main`, and get Mongo from `testutil.MongoDB(t, prefix)` — never start a container directly.
- TDD: Red → Green → Refactor → Commit. Write the failing test first.
- The `DirectoryStore` interface signature does NOT change, so no `make generate` / mock regeneration is required.
- No `docs/client-api.md` change — `ListEmployees` is a store method, not a `chat.user.*` handler, and `UserID` is not client-facing.
- Scope is `ListEmployees` only. Do not touch `GetByAccount`, other services, or the data source.

---

### Task 1: Coerce `users._id` to string in the `ListEmployees` projection

**Files:**
- Modify: `portal-service/store_mongo.go` (final `$project` stage inside `ListEmployees`, the `userId` line ~73)
- Test: `portal-service/integration_test.go` (add one new integration test function)

**Interfaces:**
- Consumes: `mongoDirectoryStore.ListEmployees(ctx) ([]employee, error)` (unchanged signature); `employee.UserID string` with `bson:"userId"`; `testutil.MongoDB(t, "portal") *mongo.Database`.
- Produces: no new exported symbols. Behavior change only: `employee.UserID` is now the string form of `users._id` regardless of whether `_id` is stored as a BSON string or ObjectID.

- [ ] **Step 1: Write the failing test**

Add this function to `portal-service/integration_test.go` (after `TestMongoDirectoryStore_ListEmployees_UsersPrimary`, before `TestMongoDirectoryStore_ListEmployees_Empty`). It requires `bson.NewObjectID()`; `bson` is already imported in this file.

```go
// TestMongoDirectoryStore_ListEmployees_ObjectIDUserID is the regression test
// for the "decoding an object ID into a string is not supported" crash:
// production users rows can have a native MongoDB ObjectID _id (written by an
// upstream system), but UserID is a Go string. Projecting $_id straight into
// userId made cur.All fail to decode the ObjectID. The fix ($toString on $_id)
// hex-encodes an ObjectID so it decodes, and passes a string _id through
// unchanged. The two _id types are asserted separately.
func TestMongoDirectoryStore_ListEmployees_ObjectIDUserID(t *testing.T) {
	db := testutil.MongoDB(t, "portal")
	store := newMongoDirectoryStore(db)
	ctx := context.Background()

	oid := bson.NewObjectID()
	_, err := db.Collection("users").InsertMany(ctx, []any{
		// Native ObjectID _id — the production shape that triggered the crash.
		bson.M{"_id": oid, "account": "alice", "siteId": "site-a"},
		// String _id — must pass through $toString unchanged.
		bson.M{"_id": "u-bob", "account": "bob", "siteId": "site-b"},
	})
	require.NoError(t, err)

	emps, err := store.ListEmployees(ctx)
	require.NoError(t, err)

	byAccount := make(map[string]employee, len(emps))
	for _, e := range emps {
		byAccount[e.Account] = e
	}

	// ObjectID _id: UserID is the 24-char hex encoding.
	assert.Equal(t, employee{Account: "alice", SiteID: "site-a", UserID: oid.Hex()}, byAccount["alice"])
	// String _id: UserID is the string unchanged.
	assert.Equal(t, employee{Account: "bob", SiteID: "site-b", UserID: "u-bob"}, byAccount["bob"])
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-integration SERVICE=portal-service`
Expected: `TestMongoDirectoryStore_ListEmployees_ObjectIDUserID` FAILS on `require.NoError(t, err)` after `ListEmployees` with a decode error like `decode employees: ... decoding an object ID into a string is not supported by default`. (Requires Docker; in CI this runs on the integration job.)

- [ ] **Step 3: Write the minimal implementation**

In `portal-service/store_mongo.go`, inside `ListEmployees`'s final `$project` stage, replace this line:

```go
			{Key: "userId", Value: "$_id"},
```

with:

```go
			// $toString hex-encodes an ObjectID _id (some production users rows
			// are written with a native ObjectID) and passes a string _id
			// through unchanged, so _id decodes cleanly into UserID (a Go
			// string). UserID is currently unread by the handlers, so the hex
			// form for ObjectID rows is acceptable — the goal is a crash-free
			// directory load.
			{Key: "userId", Value: bson.D{{Key: "$toString", Value: "$_id"}}},
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `make test-integration SERVICE=portal-service`
Expected: PASS — `TestMongoDirectoryStore_ListEmployees_ObjectIDUserID` and all existing portal-service integration tests green. `TestMongoDirectoryStore_ListEmployees_UsersPrimary` still passes because `$toString` on its string `_id`s returns them unchanged.

- [ ] **Step 5: Verify build and lint**

Run: `make build SERVICE=portal-service && make lint`
Expected: both succeed, no new findings.

- [ ] **Step 6: Commit**

```bash
git add portal-service/store_mongo.go portal-service/integration_test.go
git commit -m "fix(portal-service): coerce users._id to string in ListEmployees

ListEmployees projected userId directly from \$_id. When users._id is a
native MongoDB ObjectID (some production rows), cur.All failed with
\"decoding an object ID into a string is not supported by default\",
which emptied the directory cache and failed readiness.

Wrap \$_id in \$toString so an ObjectID is hex-encoded and a string _id
passes through unchanged. Add an integration regression test covering
both _id types."
```

---

## Self-Review

**1. Spec coverage:**
- Problem / goal (ObjectID `_id` crash) → Task 1 Step 3 (the `$toString` change). ✓
- Approach (`$toString`, only the one line) → Task 1 Step 3. ✓
- Known behavior (hex for ObjectID, unread field) → captured in the code comment (Step 3) and commit message. ✓
- Testing (ObjectID + string, asserted separately, integration) → Task 1 Steps 1–4. ✓
- Non-goals (no `GetByAccount`, no client-api, no mock regen, no data migration) → Global Constraints. ✓

**2. Placeholder scan:** No TBD/TODO; every code and command step shows exact content. ✓

**3. Type consistency:** `employee{Account, SiteID, UserID}` fields match `store.go`; `oid.Hex()` is the `bson.ObjectID` method; `bson.D`/`bson.M`/`bson.NewObjectID` are v2 driver APIs already used in this file. Interface signature unchanged. ✓
