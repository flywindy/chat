# Portal-service: robust `userId` projection in `ListEmployees`

**Date:** 2026-07-14
**Service:** `portal-service`
**Status:** Approved — ready for implementation plan

## Problem

`portal-service` fails to load its employee directory in production with:

```
list employee directory: decode employees: error decodeing key userId:
decoding an object ID into a string is not supported by default
```

`mongoDirectoryStore.ListEmployees` runs an aggregation over the `users`
collection and, in its final `$project` stage, copies each document's `_id`
into an output field named `userId`:

```go
{Key: "userId", Value: "$_id"},
```

That value is decoded into `employee.UserID`, a Go `string`:

```go
UserID string `json:"userId" bson:"userId"`
```

In production, at least one `users` document has a native MongoDB `ObjectID`
`_id` (written by an upstream system) rather than a string. The MongoDB Go
driver refuses to decode an `ObjectID` into a `string`, so `cursor.All` errors
out. Because `cursor.All` decodes documents one at a time, a **single**
ObjectID row fails the entire directory load — and a failed load leaves the
in-memory cache empty, which fails the portal's readiness signal.

Note: excluding `_id` from the projection output (`{_id: 0}`) does **not**
prevent this — that only suppresses the output field named `_id`. The
expression `"$_id"` still reads the input document's `_id` value and routes it
into the `userId` key, which is exactly the key the decoder reports.

## Goal

Make the directory load resilient to the BSON type of `users._id` so an
ObjectID `_id` can no longer crash `ListEmployees`, while string `_id` values
continue to decode unchanged.

## Non-goals

- Auditing other services or other code paths that read `users._id` into a Go
  string. Scope is `ListEmployees` only.
- Investigating or migrating the ObjectID `_id` rows at the data source.
- Changing `GetByAccount` (it does not project `_id`).
- Any change to `docs/client-api.md` — `ListEmployees` is a store method, not a
  `chat.user.*` client-facing handler, and `UserID` is not returned to clients.

## Approach

Coerce the `_id` value to a string inside the aggregation using `$toString`:

```go
{Key: "userId", Value: bson.D{{Key: "$toString", Value: "$_id"}}},
```

`$toString`:
- hex-encodes an `ObjectID` `_id` into its 24-character hex string, and
- passes a `string` `_id` through unchanged.

This removes `ListEmployees`'s dependency on `users._id` being any specific
BSON type, so a mixed-type collection no longer breaks the decode. Only the one
projection line changes; the `$lookup`, `$unwind`, and all other projected
fields are untouched.

### Considered alternative (rejected)

Dropping the `userId` projection (and the unused `UserID` field) entirely would
also fix the crash and satisfy the "project precisely" rule, since `UserID` is
currently unread by production code. It was rejected in favor of `$toString`:
keeping the field defended against the crash preserves the value for possible
future use and keeps the change minimal.

## Known, intentional behavior (documented, not bugs)

1. **`employee.UserID` is currently unused by production code.** The login and
   userInfo handlers read only `SiteID`, `Roles`, and `EmployeeID` from the
   cached `employee`; the `userId` returned to clients comes from the upstream
   botplatform login response, not from this projected field. The coerced value
   is therefore never surfaced to a client.
2. **For an ObjectID row, the hex string will not match this system's
   `"u9"`-style user IDs.** This is acceptable because the goal is
   crash-resilience of the directory load, and the field is unread. A short
   code comment on the projection line records both facts.

## Testing

Integration test in `portal-service/integration_test.go` (the store executes a
real Mongo aggregation, so this is exercised at the integration level, matching
the existing tests). Docker-backed via `testutil.MongoDB`; runs under
`make test-integration SERVICE=portal-service` in CI.

Two scenarios, asserted separately:

1. **ObjectID `_id`** — insert a `users` row whose `_id` is a
   `bson.NewObjectID()`. Assert `ListEmployees` returns no error and the row's
   `UserID` equals `oid.Hex()`.
2. **String `_id`** — insert a `users` row whose `_id` is a plain string.
   Assert `UserID` equals that string unchanged. This is a standalone
   assertion alongside the ObjectID case (the existing
   `TestMongoDirectoryStore_ListEmployees_UsersPrimary` also exercises
   string-`_id` passthrough, but the two target scenarios are asserted
   explicitly here).

No mixed-collection (both types in one collection) case is required.

## Files touched

- `portal-service/store_mongo.go` — the one projection line, plus a short
  clarifying comment.
- `portal-service/integration_test.go` — the ObjectID (and string) regression
  test(s).

## Acceptance criteria

- `ListEmployees` returns successfully when a `users` document has an ObjectID
  `_id`, with `UserID` set to the hex encoding.
- String `_id` values still decode unchanged.
- `make build SERVICE=portal-service` passes.
- `make test-integration SERVICE=portal-service` passes (including the new
  ObjectID regression test).
- No other files change.
