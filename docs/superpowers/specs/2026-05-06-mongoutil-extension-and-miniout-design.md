# pkg/mongoutil Extension and pkg/minioutil — Design Spec

**Date:** 2026-05-06
**Status:** approved (post-review)
**Reviewers consulted:** bug-focused, Go-architecture + MinIO/S3 expert, MongoDB / mongo-driver expert.

## Goal

Extract the generic Mongo helpers currently living in `history-service/internal/mongorepo` into the shared `pkg/mongoutil` package, extend them with bulk-write primitives, and add a new minimal `pkg/minioutil` package for storing/retrieving JSON blobs in MinIO. Both packages serve a future service that uses Mongo (heavy bulk-upsert workload, ~100 records per call) and MinIO (typed JSON object storage).

## Background

`pkg/mongoutil` currently exposes only `Connect` / `Disconnect`. Every typed collection helper lives in `history-service/internal/mongorepo`:

- `Collection[T]` — generic typed wrapper (FindOne / FindByID / FindMany / Aggregate / AggregatePaged / Raw).
- `OffsetPageRequest`, `OffsetPage[T]`, `EmptyPage[T]`, `NewOffsetPageRequest` — offset-pagination types.
- `QueryOption` — functional options (`WithProjection` / `WithSort` / `WithLimit` / `WithSkip`).

These have zero domain knowledge and are reusable across every Mongo-using service in the monorepo. The future service explicitly needs them plus a bulk-upsert layer.

`pkg/minioutil` does not exist. The codebase has no MinIO/S3 dependency or code today.

## Architecture overview

Two packages change:

1. **`pkg/mongoutil/`** — single existing package, extended (Option A: keep one folder, no sibling packages).
2. **`pkg/minioutil/`** — new package, follows the `pkg/<provider>util` naming convention used by `cassutil` / `mongoutil` / `natsutil` / `valkeyutil`.

History-service migrates: domain-specific code stays in `internal/mongorepo`, generic helpers (and their tests) move out and imports update.

---

## `pkg/mongoutil` extension

### Files

- `mongo.go` — unchanged (`Connect`, `Disconnect`, `buildClientOptions`).
- `collection.go` — new, contents moved verbatim from `history-service/internal/mongorepo/collection.go`. Adds `BulkWrite`, `BulkUpsert`, `BulkUpsertByID`, `InsertMany` methods.
- `pagination.go` — new, moved from `history-service/internal/mongorepo/pagination.go`.
- `options.go` — new, moved from `history-service/internal/mongorepo/options.go`.
- `bulk.go` — new, contains `BulkResult`, `UpsertModel`, `DeleteModel`.

### Existing API (relocated, signatures unchanged)

```go
type Collection[T any] struct { /* unexported */ }

func NewCollection[T any](col *mongo.Collection) *Collection[T]

func (c *Collection[T]) FindOne(ctx context.Context, filter any, opts ...QueryOption) (*T, error)
func (c *Collection[T]) FindByID(ctx context.Context, id string, opts ...QueryOption) (*T, error)
func (c *Collection[T]) FindMany(ctx context.Context, filter any, opts ...QueryOption) ([]T, error)
func (c *Collection[T]) Aggregate(ctx context.Context, pipeline bson.A) ([]T, error)
func (c *Collection[T]) AggregatePaged(ctx context.Context, pipeline bson.A, req OffsetPageRequest) (OffsetPage[T], error)
func (c *Collection[T]) Raw() *mongo.Collection

type OffsetPageRequest struct { Offset, Limit int64 }
type OffsetPage[T any] struct { Data []T; Total int64 }
func EmptyPage[T any]() OffsetPage[T]
func NewOffsetPageRequest(offset, limit int) OffsetPageRequest

type QueryOption func(*queryOptions)
func WithProjection(projection any) QueryOption
func WithSort(sort any) QueryOption
func WithLimit(limit int64) QueryOption
func WithSkip(skip int64) QueryOption
```

Conventions preserved:
- `(*T, error)` with `(nil, nil)` for not-found is the codebase convention.
- `[]T` returns `[]T{}` not `nil` so JSON marshals `[]` not `null`.
- `*Collection[T]` is goroutine-safe (it wraps `*mongo.Collection`, which is goroutine-safe per the driver docs).

### `Collection[T]` is goroutine-safe

The wrapper holds a `*mongo.Collection` and a name string; both are goroutine-safe to share. State this in the package godoc.

### `AggregatePaged` 16 MB caveat

`AggregatePaged` appends a `$facet` stage emitting one document containing both `data` and `total` branches. The `data` branch is bounded by the caller's `OffsetPageRequest.Limit`. Mongo enforces a 16 MB BSON document limit on `$facet` output. `NewOffsetPageRequest` clamps `Limit` to 100, which provides the practical guard for typical pages. Callers passing `OffsetPageRequest` directly with a large `Limit` and large documents must keep the product under 16 MB. Document this as a godoc note on `AggregatePaged`.

### New bulk-write API

Three layers, mirroring the existing `FindOne` → `FindByID` layering pattern:

```go
// pkg/mongoutil/bulk.go

// BulkResult mirrors the relevant fields of mongo.BulkWriteResult in a typed
// shape. Returned by BulkWrite/BulkUpsert/BulkUpsertByID.
type BulkResult struct {
    Matched      int64           // existing docs matched (already excludes upserted-new docs; see note below)
    Modified     int64           // docs whose contents actually changed (Modified <= Matched)
    Upserted     int64           // new docs inserted via upsert (matched=0 path)
    Inserted     int64           // pure inserts via InsertOneModel (rare; from BulkWrite callers)
    Deleted      int64           // docs deleted
    UpsertedIDs  map[int64]any   // ordinal -> _id of newly-inserted docs (driver populates; useful for callers needing assigned IDs)
    Acknowledged bool            // false only when w:0 write concerns are in use; counts are non-deterministic when false
}

// UpsertModel constructs an UpdateOne write model with Upsert=true. Pure
// stateless constructor; safe to call repeatedly. Filter is typically
// bson.M{"_id": id}; update is typically bson.M{"$set": item} or
// bson.M{"$set": ..., "$setOnInsert": ...}.
func UpsertModel(filter, update any) mongo.WriteModel

// DeleteModel constructs a DeleteOne write model. Pure stateless constructor.
func DeleteModel(filter any) mongo.WriteModel
```

```go
// pkg/mongoutil/collection.go (additions)

// BulkWrite executes a slice of write models as a single batched operation.
// The wrapper sets options.BulkWrite().SetOrdered(false) explicitly to
// override mongo-driver's default of ordered=true. Failed individual ops
// do not block the rest under unordered execution; partial success
// returns a non-nil *BulkResult alongside the error so callers can
// inspect WriteErrors via errors.As(err, &mongo.BulkWriteException{}).
//
// Empty input is a no-op: returns (nil, nil) without calling the driver.
// Without this short-circuit the driver returns mongo.ErrEmptySlice.
//
// Sessions/transactions: BulkWrite picks up a session-bearing context
// transparently (via mongo.NewSessionContext or sess.WithTransaction).
// For atomic-across-documents semantics, wrap callers in WithTransaction.
func (c *Collection[T]) BulkWrite(ctx context.Context, models []mongo.WriteModel) (*BulkResult, error)

// BulkUpsert is a typed convenience layer over BulkWrite for the canonical
// "upsert these items by filter" case. Each item is upserted with update
// document {"$set": item}.
//
// MERGE semantics, NOT REPLACE. $set updates the listed fields only:
//   - If a document matches the filter, $set merges the fields from item
//     onto the stored document. Fields in the stored document that are
//     absent from item are PRESERVED.
//   - If no document matches, a new document is inserted containing the
//     filter fields plus everything in item.
//
// BSON omitempty caveat: a struct field tagged `bson:"foo,omitempty"`
// whose Go zero value is empty WILL NOT be present in the marshaled
// $set payload, meaning the stored value is preserved (not cleared).
// Callers that need to clear fields must either drop omitempty, use
// pointers (where nil means "absent" and *T (e.g., empty string) means
// "set explicitly"), or fall back to BulkWrite with explicit models.
//
// Empty input is a no-op (returns (nil, nil)).
func (c *Collection[T]) BulkUpsert(ctx context.Context, items []T, filter func(T) any) (*BulkResult, error)

// BulkUpsertByID is the most ergonomic layer — analogous to FindByID over
// FindOne. The id function extracts a string identifier; the filter
// bson.M{"_id": idFn(item)} is applied internally. Same MERGE semantics
// and omitempty caveat as BulkUpsert. For non-string IDs (e.g.
// bson.ObjectID) or composite keys, fall back to BulkUpsert with a
// custom filter.
//
// Empty input is a no-op (returns (nil, nil)).
func (c *Collection[T]) BulkUpsertByID(ctx context.Context, items []T, idFn func(T) string) (*BulkResult, error)

// InsertMany inserts a slice of items in a single batched operation. The
// wrapper sets options.InsertMany().SetOrdered(false) explicitly: a
// duplicate-key error on one item does not abort the rest, and the
// returned error reports all collisions.
//
// Returns the count of successfully inserted documents. On partial
// failure (some items collided, others succeeded under unordered
// execution), the count reflects the successes and the error carries
// the per-item write errors. Inserted will be 0 only when every item
// failed or the call returned a transport-level error before any
// inserts were attempted.
//
// Faster than BulkUpsert when every item is known to be new — Mongo
// skips the upsert filter lookup.
//
// Detect duplicate-key collisions with mongo.IsDuplicateKeyError(err) —
// the canonical helper used elsewhere in this codebase. Note: there is
// NO mongo.ErrDuplicateKey sentinel in mongo-driver/v2.
//
// The driver's *mongo.InsertManyResult.InsertedIDs is intentionally
// dropped: this codebase exclusively uses caller-assigned application
// IDs (UUIDv7 hex / base62 — see pkg/idgen), so the result's IDs add
// no information. If a future service needs them, add a sibling
// InsertManyWithIDs method.
//
// Empty input is a no-op (returns (0, nil)).
func (c *Collection[T]) InsertMany(ctx context.Context, items []T) (int64, error)
```

#### Layering

```text
BulkUpsertByID(items, idFn)
    → BulkUpsert(items, func(x) any { return bson.M{"_id": idFn(x)} })
        → BulkWrite(buildModels(items, filterFn))
```

Each layer is a thin pass-through over the next; the foundation lives in `BulkWrite`. Pure builds the `bson.M{"$set": item}` update once per item and produces an `UpsertModel`.

#### Why unordered

For idempotent upsert workloads, an unordered batch is preferable: a single failing record (e.g., a unique-index conflict due to a race) doesn't block the remaining records. The `BulkResult` reports counts even when some operations fail. The driver's per-batch grouping by operation type (under unordered execution) makes count merging deterministic via `mergeResults`. Ordered execution would only make sense when records have causal dependencies — for that, callers should reach for sessions/transactions.

#### Why `(*BulkResult, error)` even on partial failure

`Collection.BulkWrite` returns the partial-success counts as the function's first return value, alongside a `mongo.BulkWriteException` error. The wrapper preserves both: the `*BulkResult` carries successful counts and `UpsertedIDs`; the error carries `WriteErrors` for callers who need them via `errors.As(err, &bwe)`. This pattern lets callers ignore the error if they're OK with partial success or inspect `bwe.WriteErrors` if not. The error is wrapped with `fmt.Errorf("...: %w", err)` so `errors.As` traverses the chain correctly.

#### Empty-input short-circuit

mongo-driver's `BulkWrite` and `InsertMany` both return wrapped `mongo.ErrEmptySlice` on empty input. The wrapper returns `(nil, nil)` (or `nil` for `InsertMany`) to make empty batches a clean no-op instead of an error. Implementations must explicitly check `len(...) == 0` and return early before calling the driver.

### Test coverage

#### Unit tests

In-package, no Docker:
- `bulk_test.go` — `UpsertModel` / `DeleteModel` constructed-shape assertions (verify that the returned `mongo.WriteModel` has the expected filter/update/upsert flag).
- `pagination_test.go` (moved from `mongorepo`) — clamping logic in `NewOffsetPageRequest`, `EmptyPage` non-nil-Data invariant.
- Empty-input short-circuit tests for `BulkWrite` / `BulkUpsert` / `BulkUpsertByID` / `InsertMany`: pass empty slice, assert `(nil, nil)` / `(0, nil)` returns, no driver call attempted (use a `*Collection[T]` wrapping a nil `*mongo.Collection` — the short-circuit returns before dereferencing).

#### Integration tests

`//go:build integration`, testcontainers Mongo (already in `go.mod`):
- `collection_integration_test.go` — moved from `history-service/internal/mongorepo/subscription_test.go`. Covers `Collection[T]` Find / Aggregate / AggregatePaged round-trips with the existing `testDoc` fixture and `setupMongo` helper.
- `bulk_integration_test.go` — `BulkWrite` (mixed UpsertModel/DeleteModel), `BulkUpsert`, `BulkUpsertByID` (each with 100-record batches verifying `BulkResult` counts), `InsertMany` (success + duplicate-key detection via `mongo.IsDuplicateKeyError`), partial-failure scenarios (unique-index violation on one record under unordered execution).

---

## `pkg/minioutil` (new package)

### Files

- `minio.go` — `Connect`, `Bucket[T]`, `NewBucket`, `Put`, `Get`, `List`, `Delete`.
- `minio_test.go` — unit tests (no Docker).
- `minio_integration_test.go` — `//go:build integration`, uses `testcontainers-go/modules/minio` (one new dependency).

### Public API

```go
package minioutil

import (
    "context"

    "github.com/minio/minio-go/v7"
)

// Connect constructs a MinIO client. Unlike mongoutil.Connect /
// valkeyutil.Connect, it does NOT issue a connectivity probe at
// construction time. A probe via ListBuckets requires
// s3:ListAllMyBuckets (an account-wide IAM permission); real production
// deployments scope credentials to the one bucket the service uses
// (s3:ListBucket on that bucket only), so probing here would force
// callers to grant broader IAM than they need.
//
// NewBucket carries the actual fail-fast probe via client.BucketExists,
// which only requires bucket-scoped s3:ListBucket. Standard wiring:
//
//     client, err := minioutil.Connect(ctx, endpoint, useSSL, key, secret)
//     if err != nil { ... } // construction-only failures (cred parse, etc.)
//     bucket, err := minioutil.NewBucket[T](ctx, client, cfg.Bucket)
//     if err != nil { ... } // misconfig surfaces here, bucket-scoped
//
// The endpoint must be host:port or hostname WITHOUT a scheme.
// Examples: "localhost:9000", "minio.example.com",
// "s3.us-east-1.amazonaws.com". Do NOT include "http://" or "https://".
// The useSSL parameter controls the scheme internally.
//
// The returned client is goroutine-safe (wraps an http.Client).
// Region defaults to "us-east-1" — irrelevant for MinIO, may matter for
// AWS S3 in non-us-east-1; if needed in the future, add a region option.
// Custom TLS (custom CA, mTLS, skip-verify) is not configurable via
// Connect; for those cases callers can construct *minio.Client directly
// and pass it to NewBucket.
//
// The ctx parameter is retained for signature symmetry with
// valkeyutil.Connect even though no probe uses it; future probe
// additions (if any) can adopt it without breaking the API.
func Connect(ctx context.Context, endpoint string, useSSL bool, accessKey, secretKey string) (*minio.Client, error)

// Bucket is a typed wrapper that binds a MinIO client to a single bucket
// and a JSON-marshalable payload type T. T has no static constraint;
// JSON marshaling determines suitability at runtime.
//
// *Bucket[T] is goroutine-safe.
type Bucket[T any] struct { /* unexported */ }

// NewBucket binds a client to a bucket name. Verifies bucket existence
// via client.BucketExists(ctx, name) at construction time so a
// misconfigured MINIO_BUCKET env var fails the service at startup
// rather than failing every Get/Put silently. Returns an error if the
// bucket does not exist or the existence check fails.
//
// NewBucket does NOT create the bucket — bucket provisioning is owned
// by ops/IaC.
func NewBucket[T any](ctx context.Context, client *minio.Client, name string) (*Bucket[T], error)

// Raw returns the underlying *minio.Client. Mirrors Collection[T].Raw().
// Escape hatch for features the wrapper does not surface (presigned
// URLs, multipart uploads, conditional Put, object tagging, versioning,
// region-specific operations). Pairs with Name() so callers can build
// arbitrary minio-go calls scoped to this bucket without re-threading
// the bucket name.
func (b *Bucket[T]) Raw() *minio.Client

// Name returns the bucket name.
func (b *Bucket[T]) Name() string

// Put marshals v as JSON and stores it under key with
// Content-Type: application/json; charset=utf-8.
func (b *Bucket[T]) Put(ctx context.Context, key string, v T) error

// Get fetches the object at key and unmarshals it from JSON. Returns
// (nil, nil) when the key does not exist (matches Collection.FindOne).
//
// Implementation pattern (load-bearing — implementer must follow exactly):
//   obj, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
//   if err != nil { return nil, fmt.Errorf("...: %w", err) }
//   defer obj.Close()
//   info, err := obj.Stat()
//   if err != nil {
//       var minioErr minio.ErrorResponse
//       if errors.As(err, &minioErr) && minioErr.Code == "NoSuchKey" {
//           return nil, nil
//       }
//       return nil, fmt.Errorf("...: %w", err)
//   }
//   _ = info // headers were read by Stat; body still streamable
//   var v T
//   if err := json.NewDecoder(obj).Decode(&v); err != nil { ... }
//   return &v, nil
//
// The Stat() call reuses the GetObject HTTP response (no extra round
// trip) and is the correct way to surface NoSuchKey synchronously.
func (b *Bucket[T]) Get(ctx context.Context, key string) (*T, error)

// List returns up to maxKeys keys whose names start with prefix.
// maxKeys=0 defaults to 1000 (sane cap to prevent unbounded scans).
// To drain a bucket pass math.MaxInt explicitly — but be aware that
// loads all keys into memory and may make many round trips on large
// buckets.
//
// Implementation pattern (load-bearing — minio-go's ListObjects yields
// a channel that must be drained or the goroutine leaks):
//   ctx, cancel := context.WithCancel(ctx)
//   defer cancel()
//   keys := make([]string, 0)
//   for obj := range b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{
//       Prefix: prefix, Recursive: true,
//   }) {
//       if obj.Err != nil { return nil, fmt.Errorf("...: %w", obj.Err) }
//       keys = append(keys, obj.Key)
//       if len(keys) >= maxKeys { break }  // defer cancel() stops the listing goroutine
//   }
//   return keys, nil
func (b *Bucket[T]) List(ctx context.Context, prefix string, maxKeys int) ([]string, error)

// Delete removes key. Idempotent for non-versioned buckets — returns
// nil if the key does not exist (S3 / MinIO native semantics: DELETE
// returns 204 regardless of prior existence). Versioning is out of
// scope; on a versioned bucket Delete creates a delete-marker rather
// than performing a true delete, and subsequent reads see the marker.
func (b *Bucket[T]) Delete(ctx context.Context, key string) error
```

### Configuration shape (consumed by services, not by `pkg/minioutil` itself)

`pkg/minioutil` does not own configuration parsing. The future service will define:

```go
type MinioConfig struct {
    Endpoint  string `env:"MINIO_ENDPOINT"   required:"true"`
    UseSSL    bool   `env:"MINIO_USE_SSL"    envDefault:"false"`
    AccessKey string `env:"MINIO_ACCESS_KEY" required:"true"`
    SecretKey string `env:"MINIO_SECRET_KEY" required:"true"`
    Bucket    string `env:"MINIO_BUCKET"     required:"true"`
}
```

And wire it as:

```go
client, err := minioutil.Connect(ctx, cfg.Minio.Endpoint, cfg.Minio.UseSSL, cfg.Minio.AccessKey, cfg.Minio.SecretKey)
if err != nil { ... }
bucket, err := minioutil.NewBucket[MyDocument](ctx, client, cfg.Minio.Bucket)
if err != nil { ... }
```

### New dependencies

- `github.com/minio/minio-go/v7` — MinIO Go SDK. Pure Go, well-maintained, **Apache-2.0** licensed.
- `github.com/testcontainers/testcontainers-go/modules/minio` — for integration tests only. Apache-2.0 licensed.

### Test coverage

#### Unit tests

`minio_test.go` — only the trivial accessors (`Bucket[T].Raw`, `Bucket[T].Name`) are unit-tested. The full behavioral coverage (NewBucket, Put, Get, List, Delete) lives in the integration tests against a real MinIO via testcontainers. A hand-rolled S3 stub server was considered and rejected: emulating S3's wire format (XML shapes, headers, error codes) accurately is fragile against minio-go upgrades and provides false confidence -- it tests "the wrapper produces the verbs we expect against a fake we control," not "the wrapper interoperates with an S3-compliant server." testcontainers MinIO is fast enough that the integration suite is the right backstop.

#### Integration tests

`minio_integration_test.go` (`//go:build integration`):
- testcontainers MinIO module spins up a real server.
- Pre-create the bucket via raw `client.MakeBucket(ctx, ...)` (the test owns provisioning, the package does not).
- Round-trips Put / Get / List / Delete.
- Get on missing key returns `(nil, nil)`.
- Delete idempotency on missing key.
- List with prefix filter + maxKeys cap; assert ordering (lexicographic per S3) and goroutine cleanup (no goroutine leak after early break).

---

## history-service migration

### Imports

Every file in `history-service/internal/mongorepo/` and `history-service/internal/service/` that references `mongorepo.Collection`, `mongorepo.OffsetPage`, `mongorepo.OffsetPageRequest`, `mongorepo.NewOffsetPageRequest`, `mongorepo.EmptyPage`, `mongorepo.QueryOption`, `mongorepo.WithProjection`, etc., switches to the corresponding `mongoutil` symbol.

### Files moved from `history-service/internal/mongorepo/` to `pkg/mongoutil/`

- `collection.go` → `pkg/mongoutil/collection.go`.
- `options.go` → `pkg/mongoutil/options.go`.
- `pagination.go` → `pkg/mongoutil/pagination.go`.
- `pagination_test.go` → `pkg/mongoutil/pagination_test.go`.
- `TestCollection_*` integration tests + `testDoc` fixture + `setupMongo` helper from `subscription_test.go` → `pkg/mongoutil/collection_integration_test.go`.

### Files retained in `history-service/internal/mongorepo/`

- `pipelines.go` — domain pipelines (chat threads).
- `subscription.go` — domain repo (uses `mongoutil.NewCollection[model.Subscription]` after migration).
- `threadroom.go` — domain repo (same pattern).
- `subscription_test.go` (slimmed) — only the subscription-specific tests remain; `TestCollection_*`, `testDoc`, and `setupMongo` are extracted to `pkg/mongoutil`. Subscription tests need a fixture; they keep their own minimal helper.
- `threadroom_test.go` — unchanged in behavior, only imports flip.

### Behavior change

None. Pure refactor at the import level. All existing tests pass before and after.

---

## Testing strategy summary

### Unit tests — pass without Docker

- `pkg/mongoutil/pagination_test.go` (moved).
- `pkg/mongoutil/options_test.go` — query-option apply-order and produced builder fields.
- `pkg/mongoutil/bulk_test.go` — `UpsertModel`/`DeleteModel` shape.
- `pkg/mongoutil/empty_input_test.go` — `BulkWrite` / `BulkUpsert` / `BulkUpsertByID` / `InsertMany` early-return on empty input (uses `*Collection[T]` with nil underlying collection — the short-circuit returns before dereferencing).
- `pkg/minioutil/minio_test.go` — `Bucket[T].Raw` / `Bucket[T].Name` accessor tests only. Behavioral coverage of NewBucket / Put / Get / List / Delete lives in the integration tests; see "Unit tests" rationale.

### Integration tests — Docker-gated

- `pkg/mongoutil/collection_integration_test.go` (moved from history-service `subscription_test.go`).
- `pkg/mongoutil/bulk_integration_test.go` — bulk operations on testcontainers Mongo with 100-record batches; partial-failure verification via duplicate-key collision under unordered execution.
- `pkg/minioutil/minio_integration_test.go` — full Put / Get / List / Delete roundtrip on testcontainers MinIO; goroutine-leak guard around early-break List.

### CI gate

`make test` (unit) and `make test-integration` (integration, Docker-required) both run as gating checks per project convention.

---

## Observability

Out of scope for v1. Both packages stay observability-light, matching `pkg/cassutil` / `pkg/natsutil` conventions. Services that want OTel spans around `BulkUpsert` / `Put` / `Get` wrap at their own layer. Adding instrumentation to `pkg/mongoutil` and `pkg/minioutil` is a separate effort.

---

## Out of scope

- MinIO presigned URLs (`PresignPut` / `PresignGet`).
- MinIO multipart upload helpers.
- MinIO object metadata / tagging / versioning / SSE.
- MinIO event notifications.
- MinIO custom TLS (mTLS, custom CA) — callers needing it construct `*minio.Client` themselves.
- MinIO region option — `minio-go` defaults to `us-east-1`; sufficient for MinIO, may need revisiting for AWS S3 in non-us-east-1.
- MinIO bucket creation — provisioned by ops; `NewBucket` only verifies existence.
- Mongo change streams.
- Mongo session / transaction wrappers (callers use `mongo.Client.StartSession()` directly if needed; `BulkWrite` already picks up session-bearing contexts transparently).
- `Collection[T]` `UpdateOne` / `DeleteOne` single-document writes — current spec is bulk-focused; deferred to a follow-up commit when a real caller appears (`Raw().UpdateOne(...)` is the current escape hatch).
- `BulkUpsertByID` with non-string IDs — `BulkUpsert` with a custom filter handles `bson.ObjectID` etc.
- A `BulkReplace` method using `ReplaceOneModel` for whole-document replacement — adds when needed.
- A `WithOrdered() Option` for opting into ordered execution — adds when needed.
- Observability / metrics / tracing in either package.

These can be added in follow-up work as concrete needs arise.

---

## Risk and reversibility

- **Mongo migration** is purely additive in `pkg/mongoutil` and a path-flip in history-service. Reversible by reverting the commits. No behavior change.
- **MinIO package** is brand-new code. Reversible by deleting the package; nothing depends on it yet.
- **New dependencies** (`minio-go/v7`, `testcontainers-go/modules/minio`) are well-maintained, Apache-2.0 licensed, widely used. Adding them carries low ongoing-maintenance risk.
- **Bulk-upsert behavior** is delegated to the official driver; no custom batching/chunking logic. Failures in production are bounded by the driver's contract.
- **NewBucket fail-fast probe** adds a single round-trip per service startup. Negligible cost; matches the codebase pattern.

---

## Reviewer feedback resolution log

This spec was reviewed by three agents (bug-focused, Go-architecture + MinIO/S3 expert, MongoDB / mongo-driver expert). Resolutions:

**Critical fixes applied:**
- `mongo.ErrDuplicateKey` corrected to `mongo.IsDuplicateKeyError(err)` (no such sentinel exists in mongo-driver/v2).
- `BulkUpsert` semantics clarified — kept as `$set` + upsert (MERGE, not replace); doc rewritten to remove "replacing all fields" misclaim and to flag the `omitempty` BSON tag caveat.
- `pkg/minioutil.Connect` originally specified a `client.ListBuckets(ctx)` 5-second probe (replacing the round-1 "no-op HealthCheck" claim). This was further amended post-implementation: see "Post-merge amendments" below — the probe was removed entirely because `ListBuckets` requires `s3:ListAllMyBuckets` (account-wide IAM) while `NewBucket`'s `BucketExists` probe is bucket-scoped and sufficient.
- `pkg/minioutil.Get` documents the `obj.Stat()` + `errors.As(&minio.ErrorResponse{})` + `Code=="NoSuchKey"` pattern explicitly.
- `BulkWrite` empty-input short-circuit made explicit.
- `subscription_test.go` and `testDoc`/`setupMongo` migration plan corrected — `TestCollection_*` tests move to `pkg/mongoutil/collection_integration_test.go`.
- `BulkWrite` explicitly calls `options.BulkWrite().SetOrdered(false)`; `InsertMany` similarly.

**Important fixes applied:**
- `BulkResult` gains `UpsertedIDs map[int64]any` and `Acknowledged bool`.
- `BulkWriteException carries the result` mechanism description corrected (the result is returned alongside the error, not embedded in the exception).
- `pkg/minioutil.List` documents the `context.WithCancel` + `defer cancel()` + per-item `.Err` pattern.
- `pkg/minioutil.NewBucket` returns `(*Bucket[T], error)` and calls `BucketExists(ctx)` for fail-fast startup matching the codebase convention.

**Minor fixes applied:**
- Package renamed `miniout` → `minioutil` for `<provider>util` convention.
- License corrected from "MIT" to "Apache-2.0".
- Concurrency safety stated explicitly for `*Collection[T]`, `*minio.Client`, `*Bucket[T]`.
- Endpoint format documented (host:port without scheme).
- Content-Type set as `application/json; charset=utf-8`.
- Error wrapping note on `BulkWriteException` preservation.
- `AggregatePaged` 16 MB caveat documented.

**Resolutions on open questions:**
- Q1 (three-layer shape) — kept; layering mirrors `FindOne`/`FindByID`.
- Q2 (`func(T) string` vs `func(T) any`) — kept string for codebase consistency.
- Q3 (`UpdateOne`/`DeleteOne` on Collection) — deferred to a follow-up; out of scope.
- Q4 (ordered vs unordered) — unordered, with explicit `SetOrdered(false)`.
- Q5 (empty-input semantics) — `(nil, nil)` no-op, explicit short-circuit, documented as contract.
- Q6 (`maxKeys=0 → 1000`) — kept; `math.MaxInt` documented as drain escape hatch with footnote.
- Q7 (`Bucket[T]` vs `[]byte`) — kept typed.
- Q8 (Connect verification) — initially decided as `ListBuckets(ctx)` synchronous probe (rejecting the round-1 background `HealthCheck` proposal). **Subsequently amended post-implementation**: the probe was removed entirely. `Connect` is now construct-only; `NewBucket`'s `BucketExists` probe is the bucket-scoped fail-fast hook. See "Post-merge amendments" for rationale.

**Kept open / accepted as scope notes:**
- Region, custom TLS, presigned URLs, multipart, versioning — explicit out-of-scope items, added options later when a real caller appears.

---

## Second-round reviewer feedback resolution log

After the implementation plan was written, four reviewers (bug, spec-consistency, senior-engineer-architecture, mongo-expert) reviewed it. The findings flowed back into both spec and plan:

**Spec amendments (this commit):**
- `InsertMany` signature changed from `error` to `(int64, error)` so callers see how many items were inserted on partial failure under unordered execution. The driver returns `(*InsertManyResult, BulkWriteException)` on partial failure; previously the result was discarded. Documented that the count reflects successes and the error carries per-item write errors.
- Empty-input short-circuit doc updated to `(0, nil)` for `InsertMany`.

**Spec items kept as-is despite reviewer pushback (decided):**
- Empty-input contract for `BulkWrite`/`BulkUpsert`/`BulkUpsertByID` stays as `(nil, nil)`. Reviewers (architecture M1, mongo #2) flagged that callers writing `res.Upserted` without nil-check will panic on the empty-batch path. Decided: keep the spec contract, document the requirement in the plan's per-method godocs.

**Plan-only fixes (do not change spec):**
- Critical: `BulkUpsert` `bson.M{"$set": item}` includes `_id` and MongoDB rejects updates that try to modify the immutable `_id`. The plan implementation must drop `_id` from the marshaled `$set` payload before calling `UpsertModel`.
- Critical: `mongo.IndexOptionsBuilder{}.Build()` API doesn't exist in v2; plan now uses `options.Index().SetUnique(true)`.
- Critical: `setupMinIO` bucket-name sanitization extended (replace `_` with `-`, length cap) so S3 doesn't reject bucket names derived from `t.Name()`.
- Critical: `TestConnect_RejectsInvalidEndpoint` removed — `minio.New` doesn't validate scheme'd endpoints synchronously, and the test was unreliable. Behavioral coverage of `Connect` now lives entirely in the testcontainers smoke test.
- Major: `Bucket[T]` gains `Raw() *minio.Client` and `Name() string` mirroring `Collection[T].Raw()`. Spec's long out-of-scope list (presigned URLs, multipart, conditional Put, tagging, versioning) is workable only if callers can reach the underlying client.
- Major: `setupMinIO` reuses `sync.Once` + per-test bucket pattern via a new `pkg/testutil.MinIO` helper, mirroring the existing `testutil.MongoDB`. Avoids spinning a fresh container per test.
- Major: A goroutine-leak guard is added to the `List` integration test (using `go.uber.org/goleak`, a small Apache-2.0 test-only dep) to verify the `context.WithCancel`/`defer cancel()` pattern actually does its job. The `-race` flag does not detect goroutine leaks. `runtime.NumGoroutine()` was considered first but rejected as flaky -- HTTP transport keepalive goroutines and `require.Eventually`'s own polling goroutine both contaminate the count.
- Major: `Connect` now takes `context.Context` as its first parameter, matching `pkg/valkeyutil.Connect`. The 5-second probe timeout is derived from the supplied ctx, so callers can cancel startup probes via interrupt or deadline.
- Major: Hand-rolled S3 stub-server unit tests rejected. The spec previously called for them but the implementation effort (emulating S3's wire format, XML shapes, headers, error codes) is fragile against minio-go upgrades and provides false confidence. testcontainers MinIO is fast enough to be the sole behavioral backstop. Only the trivial accessors (`Bucket[T].Raw`, `Bucket[T].Name`) are unit-tested.
- Minor: `setupMinIO` uses `pkg/testutil/testimages` for the MinIO tag (codebase canonical) rather than hardcoding.
- Minor: `BulkUpsert` godoc adds `createdAt`-clobber footgun warning, filter-vs-item `_id` consistency note, and filter-field index requirement note.
- Minor: `BulkUpsertByID` godoc explicitly notes that `_id` is always indexed (the cheapest possible bulk-upsert pattern, no risk of collection scan).
- Minor: `BulkResult.UpsertedIDs` map keys are documented as potentially non-contiguous under partial-failure scenarios.
- Minor: Empty-input return-shape inconsistency between `BulkWrite`/`BulkUpsert`/`BulkUpsertByID` (`(nil, nil)`) and `InsertMany` (`(0, nil)`) is documented in the package overview.
- Minor: `InsertMany` godoc tightened: the count claim is correct under acknowledged writes only.


---

## Post-merge amendments

These changes landed in the implementation after the round-2 resolution log was finalized. They are documented here for future readers — the live code is the source of truth, but this section explains *why* the implementation diverged from the earlier sections of this spec.

**`minioutil.Connect` is construct-only — no startup probe** (commit `b2e31ad`).
- Earlier sections of this spec describe a `client.ListBuckets(ctx)` probe with a 5-second timeout.
- CodeRabbit review of PR #157 flagged that `ListBuckets` requires `s3:ListAllMyBuckets`, an account-wide IAM permission. Real production deployments scope credentials to one bucket via `s3:ListBucket` on that bucket's ARN; the probe would force broader IAM than the package actually needs.
- `NewBucket` already performs `client.BucketExists(ctx, name)`, which only requires bucket-scoped `s3:ListBucket` and is the natural fail-fast hook (the bucket name is what callers misconfigure, not the credentials).
- Resolution: the `ListBuckets` probe was removed entirely from `Connect`. The `ctx` parameter is preserved for signature symmetry with `valkeyutil.Connect` even though it's currently unused (named `_` in the function body). `Connect` now only fails on `minio.New` construction errors (malformed credentials, etc.).
- Net effect: `minioutil` works under least-privilege IAM with no functional regression — the bucket-scoped probe in `NewBucket` catches everything the broad probe in `Connect` would have caught.

**`Bucket[T].List` was given a server-side `MaxKeys` hint** (commit `9f0ab12`).
- Earlier sections describe `List` calling `b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})` and breaking out client-side at `len(keys) >= maxKeys`.
- CodeRabbit (and an S3 expert review) flagged that without setting `MaxKeys` on `ListObjectsOptions`, the server returns up to 1000 objects per page regardless of the caller's `maxKeys`, causing over-fetch on small lists.
- Resolution: `MaxKeys: maxKeys` is now passed through to the SDK. `minio-go` clamps internally at 1000 per page, so passing `math.MaxInt` remains safe; passing small values trims the wire payload.
- Net effect: same client-side semantics, fewer wasted bytes on small list calls.

**`List` integration test uses `goleak.IgnoreCurrent()` baseline, not `defer goleak.VerifyNone(t)`** (commit `9f0ab12`).
- Earlier sections describe `defer goleak.VerifyNone(t)` as the goroutine-leak verification.
- An S3 expert verified against minio-go v7.1.0 source: `minio-go`'s default transport sets `IdleConnTimeout: 60*time.Second`, so HTTP keepalive `net/http.(*persistConn).readLoop` goroutines linger far longer than goleak's drain budget. Goleak's default ignore list does not cover these goroutines.
- Resolution: `goleak.IgnoreCurrent()` is snapshotted *before* the `List` call, then `goleak.VerifyNone(t, preList)` runs after assertions. This pattern ignores any goroutine that already existed (including the keepalive goroutines from prior `Put` calls) and only flags new ones spawned by `List`.
- Net effect: the leak guard is reliable in CI rather than false-positive against the HTTP transport pool.

**`Bucket[T].Get` godoc honestly admits two HTTP round trips** (commit `658719c`).
- The earlier "Implementation pattern" comment in this spec claims "The Stat() call reuses the GetObject HTTP response (no extra round trip) and is the correct way to surface NoSuchKey synchronously."
- The S3 expert verified against minio-go v7.1.0: `GetObject` is lazy (no HTTP request until first touch), so calling `Stat()` first issues a HEAD, and the subsequent `json.NewDecoder(obj).Decode(...)` triggers a separate GET. Two round trips total per Get.
- Resolution: the `Get` method's godoc was rewritten to admit two round trips and explain why this is acceptable for the small-JSON-blob workload. The behavior is correct (the load-bearing pattern is preserved); only the documented round-trip cost was inaccurate.

**`bsonSetWithoutID` lives in `pkg/mongoutil/bulk.go`, not `collection.go`** (commit `660e806`).
- Earlier sections show the helper inline next to `BulkUpsert` in `collection.go`.
- A code-quality reviewer flagged that the helper's natural home is alongside `UpsertModel` / `DeleteModel` / `BulkResult` / `fromDriverResult` in `bulk.go` — all bulk-API supporting code in one file.
- Resolution: the helper was moved to `bulk.go`; its in-helper `bson.Marshal` / `bson.Unmarshal` errors are now wrapped per CLAUDE.md `"never return bare err"` (the inline version returned bare errors). Three new unit tests were added in `bulk_test.go` covering happy path, no-`_id`-field no-op, and marshal-error.

**Test renames for clarity:**
- `TestIntegration_List_DefaultCap` → `TestIntegration_List_ZeroMaxKeysReturnsAll` (commit `b2e31ad`). The original name implied the test exercised the 1000-key cap engaging, but seeding 1001 keys would add ~30s of CI Put traffic; the rename + comment is honest about the test's narrower scope (the cap-engages behavior is exercised at small N by `TestIntegration_List_MaxKeysCap`).
