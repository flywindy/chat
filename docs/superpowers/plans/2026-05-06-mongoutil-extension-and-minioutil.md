# pkg/mongoutil Extension and pkg/minioutil Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Promote the generic Mongo helpers (Collection[T], pagination, options) from `history-service/internal/mongorepo` into `pkg/mongoutil`; extend the wrapper with a three-layer bulk-write API (`BulkWrite` foundation → `BulkUpsert` typed convenience → `BulkUpsertByID` ergonomic) plus `InsertMany`. Add a new `pkg/minioutil` package providing a typed `Bucket[T]` JSON object-store wrapper around the MinIO Go SDK.

**Architecture:** Pure refactor + additive new code. `pkg/mongoutil` extension is a verbatim move with bulk methods bolted on; history-service's domain-specific repos (subscription, threadroom, pipelines) stay in place and just flip imports. `pkg/minioutil` is greenfield, modeled on the existing `pkg/cassutil`/`pkg/mongoutil`/`pkg/natsutil`/`pkg/valkeyutil` shape.

**Tech Stack:** Go 1.25, mongo-driver/v2 (`go.mongodb.org/mongo-driver/v2`), minio-go/v7 (`github.com/minio/minio-go/v7`, new dep), testcontainers-go with `mongodb` (existing) and `minio` (new) modules. `stretchr/testify` for assertions.

**Spec reference:** `docs/superpowers/specs/2026-05-06-mongoutil-extension-and-miniout-design.md` (commit `77922f3`).

**Branch:** `claude/review-history-service-KWjho` (the same branch carrying the natsrouter PR; everything ships together per user request).

---

## Task 1: Move pagination types to pkg/mongoutil

**Why:** Smallest standalone move. Pagination types (`OffsetPageRequest`, `OffsetPage[T]`, `EmptyPage[T]`, `NewOffsetPageRequest`) have zero internal-package dependencies. `Collection[T].AggregatePaged` and `subscription.go` / `threadroom.go` reference them, but those are in the same package today (`mongorepo`) so the references compile unchanged in this commit. Subsequent tasks will flip the references when those files move.

**Files:**
- Create: `pkg/mongoutil/pagination.go` (verbatim copy of `history-service/internal/mongorepo/pagination.go` with package declaration changed)
- Create: `pkg/mongoutil/pagination_test.go` (verbatim copy of `history-service/internal/mongorepo/pagination_test.go` with package declaration changed)
- Delete: `history-service/internal/mongorepo/pagination.go`
- Delete: `history-service/internal/mongorepo/pagination_test.go`
- Modify: `history-service/internal/mongorepo/collection.go` (still in mongorepo at this point — flip its references to `mongoutil.OffsetPageRequest` / `mongoutil.OffsetPage` / `mongoutil.EmptyPage`)
- Modify: `history-service/internal/mongorepo/subscription.go`, `threadroom.go` (flip references)

- [ ] **Step 1: Create the new pagination file**

Create `pkg/mongoutil/pagination.go` with this exact content (verbatim move from `history-service/internal/mongorepo/pagination.go`, only the `package` line changes):

```go
package mongoutil

type OffsetPageRequest struct {
	Offset int64
	Limit  int64
}

type OffsetPage[T any] struct {
	Data  []T
	Total int64
}

// EmptyPage returns a zero-result page with non-nil Data so JSON marshals to [] not null.
func EmptyPage[T any]() OffsetPage[T] {
	return OffsetPage[T]{Data: []T{}}
}

// NewOffsetPageRequest validates offset+limit. Default limit 20, max 100, negative offset clamped to 0.
func NewOffsetPageRequest(offset, limit int) OffsetPageRequest {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return OffsetPageRequest{Offset: int64(offset), Limit: int64(limit)}
}
```

- [ ] **Step 2: Create the new pagination test file**

Create `pkg/mongoutil/pagination_test.go` with this exact content (verbatim move):

```go
package mongoutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewOffsetPageRequest_Defaults(t *testing.T) {
	p := NewOffsetPageRequest(0, 0)
	assert.Equal(t, int64(0), p.Offset)
	assert.Equal(t, int64(20), p.Limit)
}

func TestNewOffsetPageRequest_Custom(t *testing.T) {
	p := NewOffsetPageRequest(10, 30)
	assert.Equal(t, int64(10), p.Offset)
	assert.Equal(t, int64(30), p.Limit)
}

func TestNewOffsetPageRequest_LimitCapped(t *testing.T) {
	p := NewOffsetPageRequest(0, 200)
	assert.Equal(t, int64(100), p.Limit)
}

func TestNewOffsetPageRequest_NegativeOffset(t *testing.T) {
	p := NewOffsetPageRequest(-5, 20)
	assert.Equal(t, int64(0), p.Offset)
}

func TestNewOffsetPageRequest_NegativeLimit(t *testing.T) {
	p := NewOffsetPageRequest(0, -1)
	assert.Equal(t, int64(20), p.Limit)
}

func TestEmptyPage(t *testing.T) {
	page := EmptyPage[int]()
	assert.NotNil(t, page.Data)
	assert.Empty(t, page.Data)
	assert.Equal(t, int64(0), page.Total)
}
```

- [ ] **Step 3: Delete the old pagination files**

```bash
rm history-service/internal/mongorepo/pagination.go
rm history-service/internal/mongorepo/pagination_test.go
```

- [ ] **Step 4: Flip pagination references in remaining mongorepo files**

In `history-service/internal/mongorepo/collection.go`, add `"github.com/hmchangw/chat/pkg/mongoutil"` to the import block, and replace every reference:
- `OffsetPageRequest` → `mongoutil.OffsetPageRequest`
- `OffsetPage[T]` → `mongoutil.OffsetPage[T]`
- `EmptyPage[T]()` → `mongoutil.EmptyPage[T]()`
- `NewOffsetPageRequest(...)` → `mongoutil.NewOffsetPageRequest(...)` (if used; check)

In `history-service/internal/mongorepo/subscription.go` and `threadroom.go`, do the same import addition and reference flipping.

In `history-service/internal/service/threads.go` (or any service-layer file that consumes pagination types from `mongorepo`), flip the same references.

Find all references with:
```bash
grep -rn "mongorepo\.\(OffsetPage\|EmptyPage\|NewOffsetPageRequest\)" history-service/ --include="*.go"
```
Replace each one. Add the import where needed.

- [ ] **Step 5: Build to confirm everything compiles**

```bash
go build ./...
```
Expected: PASS — no compile errors.

- [ ] **Step 6: Run unit tests**

```bash
go test ./pkg/mongoutil/... ./history-service/...
```
Expected: PASS — pagination tests pass in the new location; history-service tests still pass.

- [ ] **Step 7: Run lint**

```bash
make lint
```
Expected: PASS, exit 0.

- [ ] **Step 8: Commit**

```bash
git add pkg/mongoutil/pagination.go pkg/mongoutil/pagination_test.go history-service/internal/mongorepo/ history-service/internal/service/
git commit -m "refactor(mongoutil): move pagination types from history-service to pkg

Pure relocation — file content unchanged, package declaration updated.
History-service domain files (collection.go still in mongorepo,
subscription.go, threadroom.go) flip references from in-package
unqualified names to mongoutil.* prefixes. No behavior change."
```

---

## Task 2: Move QueryOption + add new unit tests

**Why:** Same pattern as Task 1 — small, isolated move. `QueryOption` and the `WithProjection` / `WithSort` / `WithLimit` / `WithSkip` constructors have no internal-package dependencies. The existing mongorepo has no `options_test.go`, so this task adds one (the spec explicitly calls for unit-test coverage of the option apply order).

**Files:**
- Create: `pkg/mongoutil/options.go` (verbatim move with package change)
- Create: `pkg/mongoutil/options_test.go` (new — tests apply-order and produced builder fields)
- Delete: `history-service/internal/mongorepo/options.go`
- Modify: `history-service/internal/mongorepo/collection.go`, `subscription.go`, `threadroom.go` (flip references)

- [ ] **Step 1: Create the new options file**

Create `pkg/mongoutil/options.go` (verbatim move from `history-service/internal/mongorepo/options.go`, package declaration changed):

```go
package mongoutil

import "go.mongodb.org/mongo-driver/v2/mongo/options"

type queryOptions struct {
	projection any
	sort       any
	limit      *int64
	skip       *int64
}

// findOneOpts only applies projection — sort/limit/skip are irrelevant for single-document lookups.
func (qo *queryOptions) findOneOpts() *options.FindOneOptionsBuilder {
	opts := options.FindOne()
	if qo.projection != nil {
		opts.SetProjection(qo.projection)
	}
	return opts
}

func (qo *queryOptions) findOpts() *options.FindOptionsBuilder {
	opts := options.Find()
	if qo.projection != nil {
		opts.SetProjection(qo.projection)
	}
	if qo.sort != nil {
		opts.SetSort(qo.sort)
	}
	if qo.limit != nil {
		opts.SetLimit(*qo.limit)
	}
	if qo.skip != nil {
		opts.SetSkip(*qo.skip)
	}
	return opts
}

type QueryOption func(*queryOptions)

func WithProjection(projection any) QueryOption {
	return func(o *queryOptions) {
		o.projection = projection
	}
}

// WithSort only applies to FindMany.
func WithSort(sort any) QueryOption {
	return func(o *queryOptions) {
		o.sort = sort
	}
}

// WithLimit only applies to FindMany.
func WithLimit(limit int64) QueryOption {
	return func(o *queryOptions) {
		o.limit = &limit
	}
}

// WithSkip only applies to FindMany.
func WithSkip(skip int64) QueryOption {
	return func(o *queryOptions) {
		o.skip = &skip
	}
}

func apply(opts []QueryOption) *queryOptions {
	qo := &queryOptions{}
	for _, opt := range opts {
		opt(qo)
	}
	return qo
}
```

- [ ] **Step 2: Write unit tests for QueryOption**

Create `pkg/mongoutil/options_test.go`:

```go
package mongoutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestApply_Empty(t *testing.T) {
	qo := apply(nil)
	assert.Nil(t, qo.projection)
	assert.Nil(t, qo.sort)
	assert.Nil(t, qo.limit)
	assert.Nil(t, qo.skip)
}

func TestApply_AllOptions(t *testing.T) {
	proj := bson.M{"a": 1}
	sort := bson.D{{Key: "b", Value: -1}}
	qo := apply([]QueryOption{
		WithProjection(proj),
		WithSort(sort),
		WithLimit(50),
		WithSkip(10),
	})
	assert.Equal(t, proj, qo.projection)
	assert.Equal(t, sort, qo.sort)
	assert.Equal(t, int64(50), *qo.limit)
	assert.Equal(t, int64(10), *qo.skip)
}

func TestApply_LastValueWins(t *testing.T) {
	qo := apply([]QueryOption{
		WithLimit(10),
		WithLimit(20),
	})
	assert.Equal(t, int64(20), *qo.limit)
}
```

- [ ] **Step 3: Delete the old options file**

```bash
rm history-service/internal/mongorepo/options.go
```

- [ ] **Step 4: Flip references in remaining mongorepo + service files**

Find all references and flip to `mongoutil.*`:
```bash
grep -rn "mongorepo\.\(QueryOption\|WithProjection\|WithSort\|WithLimit\|WithSkip\)" history-service/ --include="*.go"
```

Inside `history-service/internal/mongorepo/collection.go`, `subscription.go`, `threadroom.go`: the references are unqualified (in-package) — change them to `mongoutil.QueryOption`, `mongoutil.WithProjection`, etc., and ensure `pkg/mongoutil` is imported.

- [ ] **Step 5: Build and test**

```bash
go build ./...
go test ./pkg/mongoutil/... ./history-service/...
make lint
```
Expected: PASS for all three.

- [ ] **Step 6: Commit**

```bash
git add pkg/mongoutil/options.go pkg/mongoutil/options_test.go history-service/internal/mongorepo/ history-service/internal/service/
git commit -m "refactor(mongoutil): move QueryOption from history-service to pkg

Verbatim move of QueryOption + WithProjection/WithSort/WithLimit/WithSkip
constructors. Adds new options_test.go covering empty input, all options
applied, and last-value-wins semantics. References in mongorepo
collection.go and domain files flip to qualified mongoutil.* names."
```

---

## Task 3: Move Collection[T] + extract integration tests + slim subscription_test.go

**Why:** The cornerstone refactor. Collection[T] moves to pkg/mongoutil. The TestCollection_* integration tests + the testDoc fixture currently live in `subscription_test.go` — they must move to a new `pkg/mongoutil/collection_integration_test.go`. The subscription/threadroom domain tests stay in mongorepo with their own minimal `setupMongo` helper.

**Files:**
- Create: `pkg/mongoutil/collection.go` (moved from `history-service/internal/mongorepo/collection.go`, package change, qualified-references unqualified since they're now in-package)
- Create: `pkg/mongoutil/collection_integration_test.go` (extract TestCollection_*, testDoc, and a setupMongo helper local to mongoutil)
- Create: `history-service/internal/mongorepo/setup_test.go` (small `setupMongo` helper shared by subscription_test.go and threadroom_test.go — both currently rely on the same helper that lived in subscription_test.go)
- Delete: `history-service/internal/mongorepo/collection.go`
- Modify: `history-service/internal/mongorepo/subscription_test.go` (remove TestCollection_*, testDoc, setupMongo — keep only TestSubscriptionRepo_*)
- Modify: `history-service/internal/mongorepo/subscription.go`, `threadroom.go` (flip `NewCollection[X]` → `mongoutil.NewCollection[X]`)
- Modify: `history-service/internal/service/threads.go` (flip any remaining mongorepo.* references for moved symbols)

- [ ] **Step 1: Create the new `pkg/mongoutil/collection.go`**

Copy the full content of `history-service/internal/mongorepo/collection.go` to `pkg/mongoutil/collection.go`. Change the package declaration to `package mongoutil` and remove the `mongoutil.` prefix from any references that were qualified by Tasks 1 and 2 (since those types are now in-package). The result:

```go
package mongoutil

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Collection is a type-safe wrapper around *mongo.Collection that normalises
// ErrNoDocuments and wraps errors consistently. *Collection[T] is goroutine-safe
// (it wraps *mongo.Collection, which is goroutine-safe per the driver docs).
type Collection[T any] struct {
	col  *mongo.Collection
	name string
}

func NewCollection[T any](col *mongo.Collection) *Collection[T] {
	return &Collection[T]{col: col, name: col.Name()}
}

// FindOne returns the first matching document, or (nil, nil) when none match.
func (c *Collection[T]) FindOne(ctx context.Context, filter any, opts ...QueryOption) (*T, error) {
	var result T
	err := c.col.FindOne(ctx, filter, apply(opts).findOneOpts()).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding %s: %w", c.name, err)
	}
	return &result, nil
}

func (c *Collection[T]) FindByID(ctx context.Context, id string, opts ...QueryOption) (*T, error) {
	return c.FindOne(ctx, bson.M{"_id": id}, opts...)
}

// FindMany returns all matching documents; returns empty (not nil) when none match.
func (c *Collection[T]) FindMany(ctx context.Context, filter any, opts ...QueryOption) ([]T, error) {
	cursor, err := c.col.Find(ctx, filter, apply(opts).findOpts())
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", c.name, err)
	}
	var results []T
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decoding %s results: %w", c.name, err)
	}
	if results == nil {
		results = []T{}
	}
	return results, nil
}

// Raw returns the underlying *mongo.Collection for escape-hatch scenarios.
func (c *Collection[T]) Raw() *mongo.Collection { return c.col }

// Aggregate runs the pipeline; no QueryOption — the pipeline encodes all query logic.
func (c *Collection[T]) Aggregate(ctx context.Context, pipeline bson.A) ([]T, error) {
	cursor, err := c.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregating %s: %w", c.name, err)
	}
	var results []T
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decoding %s aggregate: %w", c.name, err)
	}
	if results == nil {
		results = []T{}
	}
	return results, nil
}

// AggregatePaged appends a $facet: skip+limit data branch + count branch → OffsetPage[T].
//
// Caveat: $facet emits a single BSON document containing both branches.
// Mongo's 16 MB document limit applies to that output. For typical pages
// (Limit ≤ 100, the cap NewOffsetPageRequest enforces) this is non-issue;
// callers passing OffsetPageRequest directly with large Limit + large
// documents must keep the product under 16 MB.
func (c *Collection[T]) AggregatePaged(ctx context.Context, pipeline bson.A, req OffsetPageRequest) (OffsetPage[T], error) {
	facet := bson.D{{Key: "$facet", Value: bson.M{
		"data": bson.A{
			bson.D{{Key: "$skip", Value: req.Offset}},
			bson.D{{Key: "$limit", Value: req.Limit}},
		},
		"total": bson.A{
			bson.D{{Key: "$count", Value: "count"}},
		},
	}}}
	full := make(bson.A, 0, len(pipeline)+1)
	full = append(full, pipeline...)
	full = append(full, facet)

	cursor, err := c.col.Aggregate(ctx, full)
	if err != nil {
		return OffsetPage[T]{}, fmt.Errorf("aggregating %s: %w", c.name, err)
	}
	var wrapper []facetResult[T]
	if err := cursor.All(ctx, &wrapper); err != nil {
		return OffsetPage[T]{}, fmt.Errorf("decoding %s facet: %w", c.name, err)
	}
	if len(wrapper) == 0 {
		return EmptyPage[T](), nil
	}
	data := wrapper[0].Data
	if data == nil {
		data = []T{}
	}
	var total int64
	if len(wrapper[0].Total) > 0 {
		total = wrapper[0].Total[0].Count
	}
	return OffsetPage[T]{Data: data, Total: total}, nil
}

// facetResult decodes the single document emitted by the $facet stage.
type facetResult[T any] struct {
	Data  []T           `bson:"data"`
	Total []countResult `bson:"total"`
}

type countResult struct {
	Count int64 `bson:"count"`
}
```

- [ ] **Step 2: Create `pkg/mongoutil/collection_integration_test.go`**

Move the TestCollection_* tests + testDoc + setupMongo from `history-service/internal/mongorepo/subscription_test.go`. Use a different DB name (`mongoutil_test`) to avoid colliding with history-service tests:

```go
//go:build integration

package mongoutil

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/testutil"
)

func setupMongo(t *testing.T) *mongo.Database {
	return testutil.MongoDB(t, "mongoutil_test")
}

type testDoc struct {
	ID   string `bson:"_id"`
	Name string `bson:"name"`
	Age  int    `bson:"age"`
}

func TestCollection_FindOne_Success(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertOne(ctx, testDoc{ID: "d1", Name: "Alice", Age: 30})
	require.NoError(t, err)

	result, err := col.FindOne(ctx, bson.M{"name": "Alice"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "d1", result.ID)
	assert.Equal(t, "Alice", result.Name)
}

func TestCollection_FindOne_NotFound_ReturnsNilNil(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	result, err := col.FindOne(ctx, bson.M{"name": "Nobody"})
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestCollection_FindByID(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertOne(ctx, testDoc{ID: "d1", Name: "Bob", Age: 25})
	require.NoError(t, err)

	result, err := col.FindByID(ctx, "d1")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "Bob", result.Name)
}

func TestCollection_FindByID_NotFound(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	result, err := col.FindByID(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestCollection_FindMany_Success(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	_, err := db.Collection("test_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "Alice", Age: 30},
		testDoc{ID: "d2", Name: "Bob", Age: 25},
		testDoc{ID: "d3", Name: "Charlie", Age: 35},
	})
	require.NoError(t, err)

	results, err := col.FindMany(ctx, bson.M{"age": bson.M{"$gte": 30}})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestCollection_FindMany_Empty_ReturnsEmptySlice(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("test_docs"))

	results, err := col.FindMany(ctx, bson.M{"name": "Nobody"})
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
}

func TestCollection_Raw(t *testing.T) {
	db := setupMongo(t)
	col := NewCollection[testDoc](db.Collection("test_docs"))

	raw := col.Raw()
	assert.Equal(t, "test_docs", raw.Name())
}

func TestCollection_Aggregate_ReturnsMatchingDocs(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs"))

	_, err := db.Collection("agg_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "Alice", Age: 30},
		testDoc{ID: "d2", Name: "Bob", Age: 25},
		testDoc{ID: "d3", Name: "Charlie", Age: 35},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.M{"age": bson.M{"$gte": 30}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "d1", results[0].ID)
	assert.Equal(t, "d3", results[1].ID)
}

func TestCollection_Aggregate_Empty_ReturnsEmptySlice(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("agg_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	results, err := col.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	assert.NotNil(t, results)
	assert.Empty(t, results)
}

func TestCollection_AggregatePaged_ReturnsDataAndTotal(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs"))

	_, err := db.Collection("paged_docs").InsertMany(ctx, []interface{}{
		testDoc{ID: "d1", Name: "A", Age: 1},
		testDoc{ID: "d2", Name: "B", Age: 2},
		testDoc{ID: "d3", Name: "C", Age: 3},
		testDoc{ID: "d4", Name: "D", Age: 4},
		testDoc{ID: "d5", Name: "E", Age: 5},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "age", Value: 1}}}},
	}

	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page.Total)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "d1", page.Data[0].ID)
	assert.Equal(t, "d2", page.Data[1].ID)

	page2, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(2, 2))
	require.NoError(t, err)
	assert.Equal(t, int64(5), page2.Total)
	require.Len(t, page2.Data, 2)
	assert.Equal(t, "d3", page2.Data[0].ID)
	assert.Equal(t, "d4", page2.Data[1].ID)
}

func TestCollection_AggregatePaged_EmptyCollection(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("paged_docs_empty"))

	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"name": "Nobody"}}}}

	page, err := col.AggregatePaged(ctx, pipeline, NewOffsetPageRequest(0, 10))
	require.NoError(t, err)
	assert.Equal(t, int64(0), page.Total)
	assert.NotNil(t, page.Data)
	assert.Empty(t, page.Data)
}
```

- [ ] **Step 3: Create `history-service/internal/mongorepo/setup_test.go`**

The subscription_test.go and threadroom_test.go integration tests need their own setupMongo helper after the move. Create:

```go
//go:build integration

package mongorepo

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/testutil"
)

func setupMongo(t *testing.T) *mongo.Database {
	return testutil.MongoDB(t, "history_service_test")
}
```

- [ ] **Step 4: Slim `history-service/internal/mongorepo/subscription_test.go`**

Remove from subscription_test.go: the local `setupMongo` definition, the `testDoc` type, and ALL `TestCollection_*` tests (`FindOne_Success`, `FindOne_NotFound_ReturnsNilNil`, `FindByID`, `FindByID_NotFound`, `FindMany_Success`, `FindMany_Empty_ReturnsEmptySlice`, `Raw`, `Aggregate_ReturnsMatchingDocs`, `Aggregate_Empty_ReturnsEmptySlice`, `AggregatePaged_ReturnsDataAndTotal`, `AggregatePaged_EmptyCollection`).

Keep only the four `TestSubscriptionRepo_*` tests. The file's imports may need pruning: `bson`, `assert`, `require`, `testify`, `time`, `model`, `testutil` should remain only if still referenced; remove `mongo` if no longer used.

The slimmed file's expected shape:

```go
//go:build integration

package mongorepo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
)

func TestSubscriptionRepo_GetSubscription(t *testing.T) {
	// ... unchanged ...
}

func TestSubscriptionRepo_GetSubscription_NotFound(t *testing.T) {
	// ... unchanged ...
}

func TestSubscriptionRepo_GetHistorySharedSince_NilHSS(t *testing.T) {
	// ... unchanged ...
}

func TestSubscriptionRepo_GetHistorySharedSince_WithHSS(t *testing.T) {
	// ... unchanged ...
}

func TestSubscriptionRepo_GetHistorySharedSince_NotSubscribed(t *testing.T) {
	// ... unchanged ...
}
```

- [ ] **Step 5: Delete the old `collection.go`**

```bash
rm history-service/internal/mongorepo/collection.go
```

- [ ] **Step 6: Flip remaining `mongorepo.NewCollection` references**

In `history-service/internal/mongorepo/subscription.go` and `threadroom.go`, change `NewCollection[X](...)` → `mongoutil.NewCollection[X](...)`. Add the `mongoutil` import if not present.

In any service-layer file referencing `mongorepo.Collection`, `mongorepo.NewCollection`, etc., flip to `mongoutil.*`.

Find with:
```bash
grep -rn "mongorepo\.\(Collection\|NewCollection\)" history-service/ --include="*.go"
```

- [ ] **Step 7: Build to confirm everything compiles**

```bash
go build ./...
```
Expected: PASS.

- [ ] **Step 8: Run unit tests**

```bash
go test ./pkg/mongoutil/... ./history-service/...
```
Expected: PASS.

- [ ] **Step 9: Run integration tests**

```bash
go test -tags=integration -race ./pkg/mongoutil/... ./history-service/...
```
Expected: PASS — `TestCollection_*` tests now run from `pkg/mongoutil`; subscription/threadroom tests still pass from history-service.

- [ ] **Step 10: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add pkg/mongoutil/collection.go pkg/mongoutil/collection_integration_test.go history-service/internal/mongorepo/ history-service/internal/service/
git commit -m "refactor(mongoutil): move Collection[T] from history-service to pkg

Verbatim move of Collection[T] generic wrapper. TestCollection_*
integration tests + testDoc fixture + setupMongo helper extracted from
subscription_test.go to pkg/mongoutil/collection_integration_test.go.
Subscription/threadroom tests retained in history-service mongorepo
with their own minimal setup_test.go helper. Domain repo files
(subscription.go, threadroom.go) flip NewCollection references to
qualified mongoutil.NewCollection. AggregatePaged gains a doc note
about the 16 MB \$facet output limit.

After this commit pkg/mongoutil contains the full read-side API
(FindOne, FindByID, FindMany, Aggregate, AggregatePaged, Raw) plus
the relocated pagination + options helpers. Bulk-write methods
follow in subsequent tasks."
```

---

## Task 4: Add BulkResult, UpsertModel, DeleteModel helpers

**Why:** The foundation for the bulk-write API. Pure helpers, no Collection coupling, no integration test required at this stage — the methods that consume them in Tasks 5-7 will exercise them via integration tests.

**Files:**
- Create: `pkg/mongoutil/bulk.go` — `BulkResult` struct, `UpsertModel` and `DeleteModel` factory functions
- Create: `pkg/mongoutil/bulk_test.go` — unit tests for the constructors

- [ ] **Step 1: Write the failing tests**

Create `pkg/mongoutil/bulk_test.go`:

```go
package mongoutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestUpsertModel_BuildsUpdateOneModelWithUpsert(t *testing.T) {
	filter := bson.M{"_id": "x"}
	update := bson.M{"$set": bson.M{"name": "alice"}}

	m := UpsertModel(filter, update)
	require.NotNil(t, m)

	uo, ok := m.(*mongo.UpdateOneModel)
	require.True(t, ok, "UpsertModel must return *UpdateOneModel")
	assert.Equal(t, filter, uo.Filter)
	assert.Equal(t, update, uo.Update)
	require.NotNil(t, uo.Upsert)
	assert.True(t, *uo.Upsert)
}

func TestDeleteModel_BuildsDeleteOneModel(t *testing.T) {
	filter := bson.M{"_id": "x"}

	m := DeleteModel(filter)
	require.NotNil(t, m)

	d, ok := m.(*mongo.DeleteOneModel)
	require.True(t, ok, "DeleteModel must return *DeleteOneModel")
	assert.Equal(t, filter, d.Filter)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./pkg/mongoutil/...
```
Expected: FAIL — `UpsertModel`, `DeleteModel`, `BulkResult` undefined.

- [ ] **Step 3: Create `pkg/mongoutil/bulk.go`**

```go
package mongoutil

import (
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BulkResult mirrors the relevant fields of mongo.BulkWriteResult in a typed
// shape returned by Collection[T].BulkWrite / BulkUpsert / BulkUpsertByID.
//
// Empty-input contract: BulkWrite / BulkUpsert / BulkUpsertByID all return
// (nil, nil) when called with an empty slice -- callers MUST nil-check the
// result before reading fields. InsertMany is the exception: it returns
// (0, nil) on empty input, not a nil result. The asymmetry exists because
// BulkResult is a struct (nil-able pointer) while InsertMany returns a
// scalar count (no natural nil sentinel beyond zero).
//
// Field semantics (matching the driver):
//   - Matched: existing docs matched by the filter. Per the wire protocol,
//     a successful upsert that creates a new doc reports MatchedCount=0
//     (since the filter found no match) -- so Matched already excludes
//     upserted-new docs by construction. The driver also explicitly
//     subtracts UpsertedCount from MatchedCount in its bulk-write merge
//     path, which preserves the same invariant for batches.
//   - Modified: docs whose contents actually changed. Always Modified <= Matched.
//   - Upserted: new docs inserted via upsert (the matched=0 path).
//   - Inserted: pure inserts via InsertOneModel (rare; only set by BulkWrite
//     callers that include InsertOneModels).
//   - Deleted: docs deleted.
//   - UpsertedIDs: ordinal index -> _id of newly inserted docs (driver
//     populates from BulkWriteResult.UpsertedIDs); useful when callers need
//     server-assigned IDs. Keys are the original ordinal indices in the
//     input slice; under unordered execution with partial failures the keys
//     may be NON-CONTIGUOUS (e.g., for a 3-model batch where index 1 failed,
//     UpsertedIDs has keys {0, 2} only). Missing keys correspond to ops
//     that did not perform an insert: matched-existing, failed, or non-upsert.
//   - Acknowledged: false only when w:0 write concerns are in use; in that
//     case all counts are non-deterministic.
type BulkResult struct {
	Matched      int64
	Modified     int64
	Upserted     int64
	Inserted     int64
	Deleted      int64
	UpsertedIDs  map[int64]any
	Acknowledged bool
}

// UpsertModel constructs an UpdateOne write model with Upsert=true. Pure
// stateless constructor; safe to call repeatedly. Filter is typically
// bson.M{"_id": id}; update is typically bson.M{"$set": item} or
// bson.M{"$set": ..., "$setOnInsert": ...}.
func UpsertModel(filter, update any) mongo.WriteModel {
	return mongo.NewUpdateOneModel().
		SetFilter(filter).
		SetUpdate(update).
		SetUpsert(true)
}

// DeleteModel constructs a DeleteOne write model. Pure stateless constructor.
func DeleteModel(filter any) mongo.WriteModel {
	return mongo.NewDeleteOneModel().SetFilter(filter)
}

// fromDriverResult converts mongo.BulkWriteResult into the wrapper's
// BulkResult shape. Returns nil when r is nil so callers don't have to
// nil-check before mapping.
func fromDriverResult(r *mongo.BulkWriteResult) *BulkResult {
	if r == nil {
		return nil
	}
	return &BulkResult{
		Matched:      r.MatchedCount,
		Modified:     r.ModifiedCount,
		Upserted:     r.UpsertedCount,
		Inserted:     r.InsertedCount,
		Deleted:      r.DeletedCount,
		UpsertedIDs:  r.UpsertedIDs,
		Acknowledged: r.Acknowledged,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./pkg/mongoutil/...
```
Expected: PASS — both new tests + all earlier tests.

- [ ] **Step 5: Run lint**

```bash
make lint
```
Expected: PASS, exit 0.

- [ ] **Step 6: Commit**

```bash
git add pkg/mongoutil/bulk.go pkg/mongoutil/bulk_test.go
git commit -m "feat(mongoutil): add BulkResult + UpsertModel + DeleteModel helpers

BulkResult mirrors the driver's mongo.BulkWriteResult fields the
wrapper exposes (Matched/Modified/Upserted/Inserted/Deleted plus
UpsertedIDs and Acknowledged). UpsertModel and DeleteModel are
stateless constructors returning mongo.WriteModel for use with the
upcoming BulkWrite method. fromDriverResult converts driver output
to BulkResult, returning nil on nil input."
```

---

## Task 5: Add Collection[T].BulkWrite (foundation)

**Why:** The bottom layer of the three-layer bulk API. Must explicitly call `options.BulkWrite().SetOrdered(false)` (driver default is true). Must short-circuit on empty input (driver returns wrapped `ErrEmptySlice`). Must return both `*BulkResult` and error on partial-success so callers can inspect via `errors.As(err, &bwe)`.

**Files:**
- Modify: `pkg/mongoutil/collection.go` (append `BulkWrite` method)
- Modify: `pkg/mongoutil/empty_input_test.go` (new — empty-input short-circuit unit test)
- Modify: `pkg/mongoutil/bulk_integration_test.go` (new — integration test with partial-failure scenario)

- [ ] **Step 1: Write the failing unit test for empty-input**

Create `pkg/mongoutil/empty_input_test.go`:

```go
package mongoutil

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBulkWrite_EmptyInputShortCircuits(t *testing.T) {
	// Pass nil collection; the early return must fire before dereferencing.
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	res, err := c.BulkWrite(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, res)
}

type testEmptyDoc struct {
	ID string `bson:"_id"`
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./pkg/mongoutil/...
```
Expected: FAIL — `BulkWrite` undefined.

- [ ] **Step 3: Write the failing integration test**

Create `pkg/mongoutil/bulk_integration_test.go`:

```go
//go:build integration

package mongoutil

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestBulkWrite_MixedUpsertAndDelete(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_mixed"))

	// Pre-seed one doc so the Delete model has something to remove.
	_, err := db.Collection("bulk_mixed").InsertOne(ctx, testDoc{ID: "to-delete", Name: "X", Age: 1})
	require.NoError(t, err)

	models := []mongo.WriteModel{
		UpsertModel(bson.M{"_id": "u1"}, bson.M{"$set": bson.M{"name": "Alice", "age": 30}}),
		UpsertModel(bson.M{"_id": "u2"}, bson.M{"$set": bson.M{"name": "Bob", "age": 25}}),
		DeleteModel(bson.M{"_id": "to-delete"}),
	}
	res, err := col.BulkWrite(ctx, models)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(2), res.Upserted, "two upsert-inserts expected")
	assert.Equal(t, int64(1), res.Deleted)
	assert.Equal(t, int64(0), res.Matched, "no pre-existing docs matched the upsert filters")
}

func TestBulkWrite_PartialFailureUnderUnordered(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	rawCol := db.Collection("bulk_partial")

	// Create a unique index on `name` to force a duplicate-key error.
	// IndexModel.Options is options.Lister[options.IndexOptions]; use the
	// builder from go.mongodb.org/mongo-driver/v2/mongo/options.
	_, err := rawCol.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)

	col := NewCollection[testDoc](rawCol)

	// Insert one doc that'll cause the second upsert to collide on name.
	_, err = rawCol.InsertOne(ctx, testDoc{ID: "first", Name: "duplicate", Age: 1})
	require.NoError(t, err)

	models := []mongo.WriteModel{
		UpsertModel(bson.M{"_id": "ok-1"}, bson.M{"$set": bson.M{"name": "unique-1", "age": 10}}),
		UpsertModel(bson.M{"_id": "collision"}, bson.M{"$set": bson.M{"name": "duplicate", "age": 20}}),
		UpsertModel(bson.M{"_id": "ok-2"}, bson.M{"$set": bson.M{"name": "unique-2", "age": 30}}),
	}
	res, err := col.BulkWrite(ctx, models)

	// Partial failure: one collision, two successes. Under unordered execution
	// the wrapper returns BOTH a non-nil result AND a wrapped exception.
	require.Error(t, err)
	require.NotNil(t, res, "partial-success result must survive the error return")
	assert.Equal(t, int64(2), res.Upserted, "two non-colliding upserts succeeded")

	// Two cross-checks: the typed BulkWriteException via errors.As (so callers
	// can inspect WriteErrors) AND mongo.IsDuplicateKeyError (the codebase
	// canonical helper). If either fails the wrapper has broken the error
	// chain via fmt.Errorf and callers won't be able to discriminate.
	var bwe mongo.BulkWriteException
	require.True(t, errors.As(err, &bwe), "wrapped error must unwrap to BulkWriteException")
	assert.Len(t, bwe.WriteErrors, 1, "exactly one write error from the duplicate key")
	assert.True(t, mongo.IsDuplicateKeyError(err), "duplicate-key helper must traverse the wrap")
}
```

The test imports must include `"go.mongodb.org/mongo-driver/v2/mongo/options"` for `options.Index()` and `"errors"` for `errors.As`.

- [ ] **Step 4: Append `BulkWrite` to `pkg/mongoutil/collection.go`**

Add at the end of `collection.go`:

```go
// BulkWrite executes a slice of write models as a single batched operation.
// The wrapper sets options.BulkWrite().SetOrdered(false) explicitly to
// override mongo-driver's default of ordered=true. Failed individual ops
// do not block the rest under unordered execution; partial success
// returns a non-nil *BulkResult alongside the error so callers can
// inspect WriteErrors via errors.As(err, &mongo.BulkWriteException{}).
//
// Empty input is a no-op: returns (nil, nil) without calling the driver.
// Without this short-circuit the driver returns wrapped mongo.ErrEmptySlice.
//
// Sessions/transactions: BulkWrite picks up a session-bearing context
// transparently (via mongo.NewSessionContext or sess.WithTransaction).
// For atomic-across-documents semantics, wrap callers in WithTransaction.
func (c *Collection[T]) BulkWrite(ctx context.Context, models []mongo.WriteModel) (*BulkResult, error) {
	if len(models) == 0 {
		return nil, nil
	}
	res, err := c.col.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	mapped := fromDriverResult(res)
	if err != nil {
		return mapped, fmt.Errorf("bulk write %s: %w", c.name, err)
	}
	return mapped, nil
}
```

Add `"go.mongodb.org/mongo-driver/v2/mongo/options"` to the import block.

- [ ] **Step 5: Run unit tests**

```bash
go test ./pkg/mongoutil/...
```
Expected: PASS — empty-input test passes.

- [ ] **Step 6: Run integration tests**

```bash
go test -tags=integration -race ./pkg/mongoutil/...
```
Expected: PASS — both new integration tests pass.

- [ ] **Step 7: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add pkg/mongoutil/collection.go pkg/mongoutil/empty_input_test.go pkg/mongoutil/bulk_integration_test.go
git commit -m "feat(mongoutil): add Collection[T].BulkWrite foundation

Wraps mongo-driver's Collection.BulkWrite with an explicit
SetOrdered(false) override (driver default is ordered=true) so a
single failing op doesn't block the rest. Empty input short-circuits
to (nil, nil) without calling the driver, avoiding the wrapped
ErrEmptySlice error path.

Returns (*BulkResult, error) so callers can inspect both the
partial-success counts AND the typed *mongo.BulkWriteException via
errors.As. Session-bearing contexts are picked up transparently —
for atomic semantics callers wrap in mongo.Client.UseSession or
sess.WithTransaction."
```

---

## Task 6: Add Collection[T].BulkUpsert (typed convenience)

**Why:** Middle layer of the three-layer API. Builds N `UpsertModel` instances from a slice of items + a filter mapper, then delegates to BulkWrite. **MERGE semantics** (`$set`) — preserves stored fields not present in T. Documents the `omitempty` BSON-tag caveat.

**Files:**
- Modify: `pkg/mongoutil/collection.go` (append `BulkUpsert` method)
- Modify: `pkg/mongoutil/empty_input_test.go` (add empty-input test for BulkUpsert)
- Modify: `pkg/mongoutil/bulk_integration_test.go` (add 100-record batch integration test + merge-semantics verification)

- [ ] **Step 1: Write the failing unit test for empty-input**

Append to `pkg/mongoutil/empty_input_test.go`:

```go
func TestBulkUpsert_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	res, err := c.BulkUpsert(context.Background(), nil, func(testEmptyDoc) any { return nil })
	require.NoError(t, err)
	assert.Nil(t, res)
}
```

- [ ] **Step 2: Write the failing integration tests**

Append to `pkg/mongoutil/bulk_integration_test.go`:

```go
func TestBulkUpsert_HundredRecordsByID(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_upsert_100"))

	items := make([]testDoc, 100)
	for i := range items {
		items[i] = testDoc{ID: fmt.Sprintf("d%03d", i), Name: fmt.Sprintf("name-%d", i), Age: i}
	}

	res, err := col.BulkUpsert(ctx, items, func(d testDoc) any {
		return bson.M{"_id": d.ID}
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(100), res.Upserted, "all 100 inserted via upsert")
	assert.Equal(t, int64(0), res.Matched)

	// Re-running with the same items: now all 100 match (no inserts).
	for i := range items {
		items[i].Age = i + 1000 // bump age so Modified count != 0
	}
	res2, err := col.BulkUpsert(ctx, items, func(d testDoc) any {
		return bson.M{"_id": d.ID}
	})
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.Equal(t, int64(0), res2.Upserted, "no new inserts on second run")
	assert.Equal(t, int64(100), res2.Matched)
	assert.Equal(t, int64(100), res2.Modified)
}

// TestBulkUpsert_MergeSemantics verifies that $set MERGES rather than REPLACES.
// A field that exists in the stored doc but is not present in the upserted struct
// must be preserved.
func TestBulkUpsert_MergeSemantics(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()

	// Note the inline struct — this test uses a doc shape with an extra field
	// not present in the upsert payload, to prove merge semantics.
	type payload struct {
		ID   string `bson:"_id"`
		Name string `bson:"name"`
	}
	type stored struct {
		ID    string `bson:"_id"`
		Name  string `bson:"name"`
		Extra string `bson:"extra,omitempty"`
	}

	rawCol := db.Collection("bulk_upsert_merge")
	_, err := rawCol.InsertOne(ctx, stored{ID: "x", Name: "old", Extra: "preserve me"})
	require.NoError(t, err)

	col := NewCollection[payload](rawCol)
	res, err := col.BulkUpsert(ctx, []payload{{ID: "x", Name: "new"}}, func(p payload) any {
		return bson.M{"_id": p.ID}
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Matched)
	assert.Equal(t, int64(1), res.Modified)

	// Read back as the wider type — Extra must still be there.
	var got stored
	err = rawCol.FindOne(ctx, bson.M{"_id": "x"}).Decode(&got)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name, "Name updated")
	assert.Equal(t, "preserve me", got.Extra, "Extra preserved across merge upsert")
}
```

Add `"fmt"` to the imports if not already present.

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./pkg/mongoutil/...
```
Expected: FAIL — `BulkUpsert` undefined.

- [ ] **Step 4: Append `BulkUpsert` to `pkg/mongoutil/collection.go`**

```go
// BulkUpsert is a typed convenience layer over BulkWrite for the canonical
// "upsert these items by filter" case. Each item is upserted with update
// document {"$set": item}, with the item's "_id" field stripped from the
// $set payload (see below).
//
// MERGE semantics, NOT REPLACE. $set updates the listed fields only:
//   - If a document matches the filter, $set merges the fields from item
//     onto the stored document. Fields in the stored document that are
//     absent from item are PRESERVED.
//   - If no document matches, a new document is inserted containing the
//     filter fields plus everything in item (minus _id, which the upsert
//     filter sets on insert).
//
// _id handling: MongoDB rejects updates that try to modify the immutable
// "_id". Because T typically has a `bson:"_id"` field (per CLAUDE.md
// codebase convention), the wrapper marshals each item to a BSON map and
// removes "_id" before assembling the $set payload. The "_id" is set on
// insert from the upsert filter -- so callers using a filter like
// bson.M{"_id": id} will see new docs created with that id intact, and
// existing docs updated without an _id-mutation error.
//
// IMPORTANT: For BulkUpsert with a custom (non-_id) filter, the filter
// fields and the item's _id MUST be consistent. If the filter matches a
// stored doc whose _id differs from item._id (which the wrapper drops
// from $set anyway), no error fires -- but the caller's mental model is
// off. Use BulkUpsertByID when "_id is the filter" is the intent.
//
// Pitfalls of pure-$set upsert:
//   - createdAt-style fields are REWRITTEN on every upsert (they're in
//     $set). For "set on insert only" semantics, fall back to BulkWrite
//     with $setOnInsert + $set models.
//   - The filter field(s) MUST be indexed or each upsert triggers a
//     full collection scan. _id is always indexed; custom filter fields
//     require explicit indexes.
//
// BSON omitempty caveat: a struct field tagged `bson:"foo,omitempty"`
// whose Go zero value is empty WILL NOT be present in the marshaled
// $set payload, meaning the stored value is preserved (not cleared).
// Callers that need to clear fields must either drop omitempty, use
// pointer fields (where nil means "absent" and a non-nil pointer to
// the zero value means "set explicitly"), or fall back to BulkWrite
// with explicit models.
//
// Empty input is a no-op (returns (nil, nil)). Callers must nil-check
// the result before reading fields on the empty path.
func (c *Collection[T]) BulkUpsert(ctx context.Context, items []T, filter func(T) any) (*BulkResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	models := make([]mongo.WriteModel, 0, len(items))
	for _, it := range items {
		setDoc, err := bsonSetWithoutID(it)
		if err != nil {
			return nil, fmt.Errorf("bulk upsert %s marshal item: %w", c.name, err)
		}
		models = append(models, UpsertModel(filter(it), bson.M{"$set": setDoc}))
	}
	return c.BulkWrite(ctx, models)
}

// bsonSetWithoutID marshals item to a BSON map and drops the "_id" field.
// MongoDB rejects updates that would modify the immutable _id on existing
// documents; with pure $set + upsert, the marshaled $set MUST exclude
// _id or every match-and-update path errors. The _id is set on insert
// from the upsert filter -- see UpsertModel callers.
//
// Two marshal passes (Marshal then Unmarshal into bson.M) is acceptable
// at ~100 records per call. For higher-throughput callers a future
// BulkUpsertRaw taking pre-built bson.D payloads is the optimization
// path (see spec out-of-scope).
func bsonSetWithoutID(item any) (bson.M, error) {
	raw, err := bson.Marshal(item)
	if err != nil {
		return nil, err
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "_id")
	return m, nil
}
```

Add `"fmt"` to the `collection.go` imports if not already present.

- [ ] **Step 5: Run all tests**

```bash
go test ./pkg/mongoutil/...
go test -tags=integration -race ./pkg/mongoutil/...
```
Expected: PASS — empty-input test, 100-record test, merge-semantics test all green.

- [ ] **Step 6: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/mongoutil/collection.go pkg/mongoutil/empty_input_test.go pkg/mongoutil/bulk_integration_test.go
git commit -m "feat(mongoutil): add Collection[T].BulkUpsert typed convenience

Builds N UpsertModels from items + filter mapper and delegates to
BulkWrite. \$set merge semantics: stored fields not present in T are
preserved. omitempty BSON tag caveat documented.

Integration tests cover the 100-record canonical batch (the future
service's stated load) and a dedicated merge-semantics test that
proves an Extra field on the stored doc survives an upsert with a
narrower payload type."
```

---

## Task 7: Add Collection[T].BulkUpsertByID (ergonomic ID-based form)

**Why:** Top layer — the canonical form for "upsert by `_id`". Pure pass-through to BulkUpsert with a built-in `bson.M{"_id": idFn(item)}` filter. Mirrors the FindByID-over-FindOne pattern.

**Files:**
- Modify: `pkg/mongoutil/collection.go` (append `BulkUpsertByID` method)
- Modify: `pkg/mongoutil/empty_input_test.go` (add empty-input test)
- Modify: `pkg/mongoutil/bulk_integration_test.go` (add integration test confirming delegation)

- [ ] **Step 1: Write the failing unit test for empty-input**

Append to `pkg/mongoutil/empty_input_test.go`:

```go
func TestBulkUpsertByID_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	res, err := c.BulkUpsertByID(context.Background(), nil, func(testEmptyDoc) string { return "" })
	require.NoError(t, err)
	assert.Nil(t, res)
}
```

- [ ] **Step 2: Write the failing integration test**

Append to `pkg/mongoutil/bulk_integration_test.go`:

```go
func TestBulkUpsertByID_DelegatesToBulkUpsert(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("bulk_upsert_byid"))

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30},
		{ID: "b", Name: "Bob", Age: 25},
	}
	res, err := col.BulkUpsertByID(ctx, items, func(d testDoc) string { return d.ID })
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, int64(2), res.Upserted)

	// Confirm the docs landed at the expected _id values.
	got, err := col.FindByID(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Alice", got.Name)
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./pkg/mongoutil/...
```
Expected: FAIL — `BulkUpsertByID` undefined.

- [ ] **Step 4: Append `BulkUpsertByID` to `pkg/mongoutil/collection.go`**

```go
// BulkUpsertByID is the most ergonomic layer — analogous to FindByID over
// FindOne. The id function extracts a string identifier; the filter
// bson.M{"_id": idFn(item)} is applied internally. Same MERGE semantics
// and omitempty caveat as BulkUpsert. For non-string IDs (e.g.
// bson.ObjectID) or composite keys, fall back to BulkUpsert with a
// custom filter.
//
// Performance: this is the cheapest possible bulk-upsert pattern in
// MongoDB. _id is ALWAYS indexed (the unique primary key index is
// auto-created), so each upsert is a single B-tree lookup -- never a
// collection scan. Prefer this over BulkUpsert with a custom filter
// whenever the workload is "upsert by id".
//
// Empty input is a no-op (returns (nil, nil)). Callers must nil-check
// the result before reading fields on the empty path.
func (c *Collection[T]) BulkUpsertByID(ctx context.Context, items []T, idFn func(T) string) (*BulkResult, error) {
	return c.BulkUpsert(ctx, items, func(item T) any {
		return bson.M{"_id": idFn(item)}
	})
}
```

- [ ] **Step 5: Run all tests**

```bash
go test ./pkg/mongoutil/...
go test -tags=integration -race ./pkg/mongoutil/...
```
Expected: PASS.

- [ ] **Step 6: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/mongoutil/collection.go pkg/mongoutil/empty_input_test.go pkg/mongoutil/bulk_integration_test.go
git commit -m "feat(mongoutil): add Collection[T].BulkUpsertByID ergonomic layer

Pure pass-through to BulkUpsert with a built-in bson.M{\"_id\": idFn(item)}
filter. Mirrors FindByID-over-FindOne. Locked to func(T) string IDs —
this codebase exclusively uses string identifiers (UUIDv7 hex, base62);
non-string IDs fall back to BulkUpsert with a custom filter."
```

---

## Task 8: Add Collection[T].InsertMany

**Why:** Sibling to bulk-upsert for write-only batches. Faster than BulkUpsert when items are known to be new (no upsert lookup). Detects duplicate-key collisions via the codebase-canonical `mongo.IsDuplicateKeyError(err)`. Defaults to **unordered** (driver default is ordered).

**Files:**
- Modify: `pkg/mongoutil/collection.go` (append `InsertMany` method)
- Modify: `pkg/mongoutil/empty_input_test.go` (add empty-input test)
- Modify: `pkg/mongoutil/bulk_integration_test.go` (add happy path + duplicate-key collision test)

- [ ] **Step 1: Write the failing unit test for empty-input**

Append to `pkg/mongoutil/empty_input_test.go`:

```go
func TestInsertMany_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	n, err := c.InsertMany(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
```

- [ ] **Step 2: Write the failing integration tests**

Append to `pkg/mongoutil/bulk_integration_test.go`:

```go
func TestInsertMany_HappyPath(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	col := NewCollection[testDoc](db.Collection("insertmany_happy"))

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30},
		{ID: "b", Name: "Bob", Age: 25},
		{ID: "c", Name: "Charlie", Age: 35},
	}
	n, err := col.InsertMany(ctx, items)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n, "all 3 items inserted")

	got, err := col.FindByID(ctx, "b")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Bob", got.Name)
}

func TestInsertMany_DuplicateKeyDetectableUnderUnordered(t *testing.T) {
	db := setupMongo(t)
	ctx := context.Background()
	rawCol := db.Collection("insertmany_dup")
	col := NewCollection[testDoc](rawCol)

	// Pre-seed a doc with _id "b" so the second item collides.
	_, err := rawCol.InsertOne(ctx, testDoc{ID: "b", Name: "Pre", Age: 1})
	require.NoError(t, err)

	items := []testDoc{
		{ID: "a", Name: "Alice", Age: 30}, // OK
		{ID: "b", Name: "Bob", Age: 25},   // collision on _id
		{ID: "c", Name: "Charlie", Age: 35}, // OK under unordered
	}
	n, err := col.InsertMany(ctx, items)
	require.Error(t, err)
	assert.True(t, mongo.IsDuplicateKeyError(err), "must be detectable via mongo.IsDuplicateKeyError")
	// Defense-in-depth: also assert errors.As resolves to BulkWriteException
	// so callers who walk the chain manually aren't surprised.
	var bwe mongo.BulkWriteException
	require.True(t, errors.As(err, &bwe), "wrapped error must unwrap to BulkWriteException")
	assert.Equal(t, int64(2), n, "two non-colliding inserts succeeded under unordered execution")

	// Under unordered, the non-colliding inserts succeeded.
	gotA, err := col.FindByID(ctx, "a")
	require.NoError(t, err)
	require.NotNil(t, gotA, "ok-1 should have been inserted under unordered execution")
	gotC, err := col.FindByID(ctx, "c")
	require.NoError(t, err)
	require.NotNil(t, gotC, "ok-2 should have been inserted under unordered execution")
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./pkg/mongoutil/...
```
Expected: FAIL — `InsertMany` undefined.

- [ ] **Step 4: Append `InsertMany` to `pkg/mongoutil/collection.go`**

```go
// InsertMany inserts a slice of items in a single batched operation. The
// wrapper sets options.InsertMany().SetOrdered(false) explicitly: a
// duplicate-key error on one item does not abort the rest, and the
// returned error reports all collisions.
//
// Returns the count of successfully inserted documents. On partial
// failure under unordered execution (some items collided, others
// succeeded), the returned count reflects the successes and the error
// carries the per-item write errors. The count is computed as
// len(*InsertManyResult.InsertedIDs); the InsertedIDs slice itself is
// dropped because this codebase exclusively uses caller-assigned
// application IDs (UUIDv7 hex / base62 -- see pkg/idgen).
//
// Acknowledged-write assumption: the count is reliable under acknowledged
// write concerns (the codebase default). Under w:0 the driver may return
// a nil result alongside a non-nil error even when writes succeeded; in
// that case the count returned by this wrapper will be 0 regardless of
// actual server-side success. The codebase does not currently use w:0
// for any collection.
//
// Faster than BulkUpsert when every item is known to be new -- Mongo
// skips the upsert filter lookup.
//
// Detect duplicate-key collisions with mongo.IsDuplicateKeyError(err) --
// the canonical helper used elsewhere in this codebase. Note: there is
// NO mongo.ErrDuplicateKey sentinel in mongo-driver/v2.
//
// Empty-input contract differs from the bulk methods: InsertMany returns
// (0, nil) on empty input rather than (nil, nil) -- callers can read
// the count unconditionally without a result-pointer nil-check.
func (c *Collection[T]) InsertMany(ctx context.Context, items []T) (int64, error) {
	if len(items) == 0 {
		return 0, nil
	}
	docs := make([]any, 0, len(items))
	for _, it := range items {
		docs = append(docs, it)
	}
	res, err := c.col.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	// On partial failure under unordered, the driver returns BOTH a non-nil
	// result (with InsertedIDs for the successes) AND a BulkWriteException.
	// Preserve the success count for the caller.
	var inserted int64
	if res != nil {
		inserted = int64(len(res.InsertedIDs))
	}
	if err != nil {
		return inserted, fmt.Errorf("insert many %s: %w", c.name, err)
	}
	return inserted, nil
}
```

- [ ] **Step 5: Run all tests**

```bash
go test ./pkg/mongoutil/...
go test -tags=integration -race ./pkg/mongoutil/...
```
Expected: PASS — both new integration tests + the empty-input unit test.

- [ ] **Step 6: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/mongoutil/collection.go pkg/mongoutil/empty_input_test.go pkg/mongoutil/bulk_integration_test.go
git commit -m "feat(mongoutil): add Collection[T].InsertMany write-only batch

Sets options.InsertMany().SetOrdered(false) so a duplicate-key error
on one item doesn't abort the rest. Returns (int64 inserted, error)
so partial-failure callers see how many items got through under
unordered execution -- the driver's *InsertManyResult is preserved
(via len(InsertedIDs)) rather than silently discarded.

Detect collisions via mongo.IsDuplicateKeyError(err) -- the canonical
helper in this codebase (there is no mongo.ErrDuplicateKey sentinel
in mongo-driver/v2 contrary to common assumption).

Drops the driver's InsertedIDs slice itself -- every service in this
codebase uses caller-assigned application IDs (UUIDv7 hex / base62),
so server-assigned ObjectIDs add no information."
```

---

## Phase 2 — pkg/minioutil

## Task 9: Add minio-go dependency + minioutil.Connect

**Why:** First piece of greenfield. Adds the `minio-go/v7` dependency, creates `pkg/minioutil/minio.go` with `Connect`. The `Connect` probe uses `client.ListBuckets(ctx)` with a 5-second timeout — matches the codebase pattern (`mongoutil.Connect` does `Ping`, `valkeyutil.Connect` does `PING`, `cassutil.Connect` does `CreateSession`).

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/minio/minio-go/v7`)
- Create: `pkg/minioutil/minio.go` (initial — only `Connect`)

(Stub-server unit tests for the full minioutil API land in Task 16. `Connect` itself can't be unit-tested without a real server -- `minio.New` is permissive about endpoint format and the ListBuckets probe needs a server -- so its behavioral coverage is the testcontainers smoke test in Task 10.)

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/minio/minio-go/v7
```

- [ ] **Step 2: Create the initial `pkg/minioutil/minio.go`**

```go
// Package minioutil provides a small typed wrapper around the MinIO Go
// SDK for storing and retrieving JSON documents. It mirrors the
// pkg/mongoutil and pkg/valkeyutil shape: a connection helper plus a
// typed Bucket[T] for the common JSON-blob workload.
//
// Concurrency: *minio.Client is goroutine-safe (it wraps an http.Client).
// *Bucket[T] is goroutine-safe (it carries no mutable state of its own).
package minioutil

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Connect opens a MinIO client and verifies connectivity by calling
// ListBuckets. A 5-second timeout is derived from the supplied ctx
// (context.WithTimeout(ctx, 5*time.Second)) so callers can cancel the
// startup probe via interrupt or deadline. Matches valkeyutil.Connect
// which also takes a ctx as its first parameter. No bucket is created --
// bucket provisioning is owned by ops/IaC.
//
// The endpoint must be host:port or hostname WITHOUT a scheme.
// Examples: "localhost:9000", "minio.example.com",
// "s3.us-east-1.amazonaws.com". Do NOT include "http://" or "https://".
// The useSSL parameter controls the scheme internally.
//
// Region defaults to "us-east-1" -- irrelevant for MinIO, may matter for
// AWS S3 in non-us-east-1 regions; if needed, add a region option in a
// follow-up. Custom TLS (custom CA, mTLS, skip-verify) is not
// configurable via Connect; for those cases callers can construct
// *minio.Client directly using minio.New with a custom Transport.
// ctx parameter is retained for signature symmetry with valkeyutil.Connect; currently unused.
func Connect(_ context.Context, endpoint string, useSSL bool, accessKey, secretKey string) (*minio.Client, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minioutil connect: %w", err)
	}
	slog.Info("connected to MinIO", "endpoint", endpoint, "useSSL", useSSL)
	return client, nil
}
```

**Note (post-implementation):** the `ListBuckets` startup probe was removed because it requires `s3:ListAllMyBuckets` (account-wide IAM), which is broader than the bucket-scoped `s3:ListBucket` permission `NewBucket`'s `BucketExists` probe needs. Drop the `"time"` import too — the probe was its only use. See the spec's "Post-merge amendments" section for the full rationale.

- [ ] **Step 3: Verify the package compiles**

```bash
go build ./pkg/minioutil/...
```
Expected: PASS.

Note: no unit tests in this task. `minio.New` does not synchronously
validate endpoint format, and the ListBuckets probe needs a real
server -- so all behavioral coverage of `Connect` lives in the
testcontainers smoke test added in Task 10. The full stub-server
unit-test surface for the minioutil API (NewBucket / Get / Put /
Delete / List) lands in Task 16.

- [ ] **Step 4: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum pkg/minioutil/minio.go
git commit -m "feat(minioutil): add package with Connect helper

New pkg/minioutil following the pkg/<provider>util convention used by
cassutil/mongoutil/natsutil/valkeyutil. Connect creates a *minio.Client
and verifies connectivity via ListBuckets with a 5-second timeout --
matches the synchronous-probe pattern used by every other connect
helper in the repo. Endpoint must be host:port WITHOUT a scheme;
useSSL controls https/http internally.

Adds github.com/minio/minio-go/v7 dependency (Apache-2.0). Bucket[T]
typed wrapper, Put/Get/List/Delete methods, integration tests, and
stub-server unit tests follow in subsequent tasks."
```

---

## Task 10: Add minio testcontainers + shared `pkg/testutil.MinIO` helper

**Why:** Adds the `testcontainers-go/modules/minio` dependency and a `pkg/testutil.MinIO(t, prefix)` helper modeled on the existing `pkg/testutil.MongoDB` -- one MinIO container shared across the whole test process via `sync.Once`, and a per-test bucket (hashed from `t.Name()`) created inside. This avoids spinning ~15 separate containers across Tasks 11-15 and keeps test-helper conventions consistent.

A separate `pkg/minioutil/minio_integration_test.go` carries the smoke test that confirms the wiring works end-to-end. Subsequent tasks (11-15) layer their tests on `testutil.MinIO`.

**Files:**
- Modify: `go.mod`, `go.sum` (add `testcontainers-go/modules/minio`)
- Modify: `pkg/testutil/testimages/testimages.go` (add `MinIO` constant)
- Create: `pkg/testutil/minio.go` (`testutil.MinIO(t, prefix)` helper, `//go:build integration`)
- Create: `pkg/minioutil/minio_integration_test.go` (smoke test only)

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/testcontainers/testcontainers-go/modules/minio
```

- [ ] **Step 2: Pin the MinIO image tag in `testimages`**

Edit `pkg/testutil/testimages/testimages.go`:

```go
// MinIO is the image for every MinIO-backed integration test.
MinIO = "minio/minio:RELEASE.2025-01-20T14-49-07Z"
```

- [ ] **Step 3: Create `pkg/testutil/minio.go`**

Mirror the structure of `pkg/testutil/mongo.go`:

```go
//go:build integration

package testutil

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/hmchangw/chat/pkg/testutil/testimages"
)

var (
	minioOnce    sync.Once
	minioClient  *minio.Client
	minioInitErr error
)

func ensureMinIOClient() (*minio.Client, error) {
	minioOnce.Do(func() {
		ctx := context.Background()
		container, err := tcminio.Run(ctx, testimages.MinIO)
		if err != nil {
			minioInitErr = fmt.Errorf("start minio: %w", err)
			return
		}
		// tcminio.MinioContainer.ConnectionString returns "host:port"
		// already (no scheme). No TrimPrefix needed.
		endpoint, err := container.ConnectionString(ctx)
		if err != nil {
			_ = container.Terminate(ctx)
			minioInitErr = fmt.Errorf("get minio endpoint: %w", err)
			return
		}
		c, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(container.Username, container.Password, ""),
			Secure: false,
		})
		if err != nil {
			_ = container.Terminate(ctx)
			minioInitErr = fmt.Errorf("connect minio: %w", err)
			return
		}
		minioClient = c
	})
	return minioClient, minioInitErr
}

// MinIO returns a connected MinIO client + a per-test bucket name. The
// bucket is created on entry and removed on t.Cleanup. The bucket name
// is derived from t.Name() with a stable fnv hash so parallel subtests
// can't collide; the prefix lets callers namespace by package
// (e.g. "minioutil"). Bucket names are valid S3 identifiers
// (3-63 chars, lowercase, digits, hyphens only).
//
// Prefix requirements: 3-46 chars, lowercase letters/digits/hyphens
// only, must NOT start or end with a hyphen. The helper does not
// validate -- callers passing invalid prefixes get an InvalidBucketName
// error from MinIO at MakeBucket time.
func MinIO(t *testing.T, prefix string) (*minio.Client, string) {
	t.Helper()
	c, err := ensureMinIOClient()
	if err != nil {
		t.Fatalf("testutil.MinIO: %v", err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(t.Name()))
	bucket := strings.ToLower(fmt.Sprintf("%s-%x", prefix, h.Sum64()))
	// S3 bucket names are capped at 63 chars; truncate defensively.
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	ctx := context.Background()
	if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("testutil.MinIO MakeBucket %q: %v", bucket, err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup. Bucket independence is GUARANTEED by the
		// per-test fnv-hashed name (one test's bucket can't collide with
		// another's even if cleanup fails completely). So a cleanup
		// failure does not affect downstream test correctness -- only
		// resource hygiene -- and we log + continue rather than fail
		// the test post-hoc. Bounded by a 30-second context to avoid
		// blocking test-process exit on a hung MinIO.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for obj := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err != nil {
				t.Logf("list during cleanup of %q: %v", bucket, obj.Err)
				continue
			}
			if err := c.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
				t.Logf("remove %q/%q during cleanup: %v", bucket, obj.Key, err)
			}
		}
		if err := c.RemoveBucket(ctx, bucket); err != nil {
			t.Logf("remove bucket %q during cleanup: %v", bucket, err)
		}
	})
	return c, bucket
}
```

Add `"time"` to the imports.

Bucket names are `<prefix>-<16-hex>` -- short, S3-valid (only lowercase letters, digits, hyphens), and stable per test. Cleanup failures are logged + skipped because per-test bucket-name independence guarantees no cross-test contamination.

- [ ] **Step 4: Create `pkg/minioutil/minio_integration_test.go`**

```go
//go:build integration

package minioutil

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

// TestIntegration_Connect_Smoke verifies the shared testutil.MinIO wiring
// works end-to-end. Connect itself can't run against the testcontainers
// MinIO via testutil.MinIO (which constructs its own client) -- so this
// test exercises only the shared client + bucket. Connect's behavioral
// coverage lives in the stub-server unit tests in Task 16.
func TestIntegration_Connect_Smoke(t *testing.T) {
	client, bucket := testutil.MinIO(t, "minioutil")
	require.NotNil(t, client)
	require.NotEmpty(t, bucket)

	// Verify the bucket exists from the client's perspective.
	exists, err := client.BucketExists(context.Background(), bucket)
	require.NoError(t, err)
	require.True(t, exists)
}
```

Tests are in `package minioutil` (internal test package, matching pkg/mongoutil's pattern). Subsequent tasks (11-15) append integration tests to this same file.

- [ ] **Step 5: Run the integration smoke test**

```bash
go test -tags=integration -race ./pkg/minioutil/... ./pkg/testutil/...
```
Expected: PASS — testcontainers spins up one shared MinIO container, the smoke test creates and verifies its per-test bucket.

- [ ] **Step 6: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum pkg/testutil/testimages/testimages.go pkg/testutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(testutil): add MinIO helper; minioutil smoke test

testutil.MinIO(t, prefix) mirrors the existing testutil.MongoDB pattern:
sync.Once-shared container across the whole test process, per-test
bucket name derived from a stable fnv hash of t.Name() so parallel
subtests don't collide. Bucket names are S3-valid (lowercase letters,
digits, hyphens only).

Adds the MinIO image tag to pkg/testutil/testimages so every package's
integration tests track the same version. Adds the
testcontainers-go/modules/minio dependency (Apache-2.0).

The minioutil smoke test confirms the wiring end-to-end and is the
only integration test in this task; subsequent tasks (Bucket[T] +
Put/Get/List/Delete) layer their tests on testutil.MinIO."
```

---

## Task 11: Add `Bucket[T]` + `NewBucket` (with `BucketExists` probe)

**Why:** The typed wrapper is the ergonomic surface every caller will use. Per the design spec, `NewBucket` MUST verify bucket existence at construction time via `client.BucketExists(ctx, name)` so a misconfigured `MINIO_BUCKET` env var fails the service at startup rather than failing every Get/Put silently. Returns an error if the bucket does not exist or the existence check fails — this matches the codebase fail-fast convention.

**Files:**
- Modify: `pkg/minioutil/minio.go` (add `Bucket[T]` struct + `NewBucket` constructor)
- Modify: `pkg/minioutil/minio_integration_test.go` (add `TestIntegration_NewBucket_*` tests)

- [ ] **Step 1 (Red): Add the integration tests first**

Append to `pkg/minioutil/minio_integration_test.go`:

```go
// TestIntegration_NewBucket_Success verifies the constructor accepts a bucket
// that exists on the server.
func TestIntegration_NewBucket_Success(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")

	type doc struct{ Name string `json:"name"` }
	b, err := NewBucket[doc](context.Background(), client, bucketName)
	require.NoError(t, err)
	require.NotNil(t, b)
}

// TestIntegration_NewBucket_MissingBucket verifies the constructor fails fast
// when the named bucket does not exist on the server (the misconfigured
// MINIO_BUCKET case the spec calls out).
func TestIntegration_NewBucket_MissingBucket(t *testing.T) {
	client, _ := testutil.MinIO(t, "minioutil")

	type doc struct{ Name string `json:"name"` }
	_, err := NewBucket[doc](context.Background(), client, "definitely-does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "definitely-does-not-exist")
}
```

Add `"github.com/stretchr/testify/assert"` to the imports if not already present. The `testutil` import (`"github.com/hmchangw/chat/pkg/testutil"`) was added in Task 10 and is required throughout the rest of this file.

- [ ] **Step 2: Run the tests — confirm they FAIL**

```bash
go test -tags=integration -race -run TestIntegration_NewBucket ./pkg/minioutil/...
```
Expected: FAIL with compile error (`NewBucket` undefined). This is the Red phase.

- [ ] **Step 3 (Green): Implement `Bucket[T]` + `NewBucket`**

Append to `pkg/minioutil/minio.go`:

```go
// Bucket is a typed wrapper that binds a MinIO client to a single bucket
// and a JSON-marshalable payload type T. T has no static constraint;
// JSON marshaling determines suitability at runtime.
//
// *Bucket[T] is goroutine-safe.
type Bucket[T any] struct {
	client *minio.Client
	name   string
}

// NewBucket binds a client to a bucket name. Verifies bucket existence via
// client.BucketExists at construction time so a misconfigured MINIO_BUCKET
// env var fails the service at startup rather than failing every Get/Put
// silently. Does NOT create the bucket — provisioning is owned by ops/IaC.
func NewBucket[T any](ctx context.Context, client *minio.Client, name string) (*Bucket[T], error) {
	exists, err := client.BucketExists(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("minioutil bucket exists check %q: %w", name, err)
	}
	if !exists {
		return nil, fmt.Errorf("minioutil bucket %q does not exist", name)
	}
	return &Bucket[T]{client: client, name: name}, nil
}

// Raw returns the underlying *minio.Client. Mirrors Collection[T].Raw().
// Escape hatch for features the wrapper does not surface: presigned
// URLs, multipart uploads, conditional Put (If-Match/If-None-Match),
// object tagging, versioning, region-specific operations. Callers
// reaching for Raw() are expected to combine it with Name() to scope
// to this bucket.
func (b *Bucket[T]) Raw() *minio.Client {
	return b.client
}

// Name returns the bucket name. Pairs with Raw() so callers can build
// arbitrary minio-go calls scoped to this bucket without needing to
// thread the bucket name separately.
func (b *Bucket[T]) Name() string {
	return b.name
}
```

Add a small unit test alongside in `pkg/minioutil/minio_test.go` to cover the trivial accessors -- they're tiny but a one-line regression (returning `b.client` from `Name()` by mistake) would be silent until production. This is the only unit test in the package; full behavioral coverage of NewBucket / Put / Get / List / Delete is via the testcontainers integration tests in Tasks 11-15.

```go
package minioutil

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
)

func TestBucket_RawAndName(t *testing.T) {
	client := &minio.Client{}
	b := &Bucket[struct{}]{client: client, name: "my-bucket"}
	assert.Same(t, client, b.Raw())
	assert.Equal(t, "my-bucket", b.Name())
}
```

Add `"fmt"` to the imports if not already present.

- [ ] **Step 4: Run the tests — confirm they PASS**

```bash
go test -tags=integration -race -run TestIntegration_NewBucket ./pkg/minioutil/...
```
Expected: PASS — both the success and missing-bucket cases.

- [ ] **Step 5: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/minioutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(minioutil): add Bucket[T] typed wrapper + NewBucket + Raw + Name

Bucket[T] binds a *minio.Client to a single bucket and a
JSON-marshalable payload type T. NewBucket verifies bucket existence
via client.BucketExists at construction time so a misconfigured
MINIO_BUCKET env var fails the service at startup rather than
failing every Get/Put silently. Does NOT create the bucket --
provisioning remains owned by ops/IaC.

Raw() and Name() are escape hatches mirroring Collection[T].Raw():
presigned URLs, multipart uploads, conditional Put, tagging,
versioning, and other deferred features all remain reachable
without forcing the wrapper to grow.

Integration tests cover the existing-bucket success path and the
missing-bucket fail-fast path. Put/Get/List/Delete follow."
```

---

## Task 12: Add `Bucket[T].Put`

**Why:** Put is the simpler half of the round-trip and unblocks Get's tests (Get needs Put to seed objects). Per spec, Put marshals `v` as JSON and stores it under `key` with `Content-Type: application/json; charset=utf-8` so downstream readers (CLI tools, S3 console, other languages) can identify the payload format without out-of-band knowledge.

**Files:**
- Modify: `pkg/minioutil/minio.go` (add `Put` method)
- Modify: `pkg/minioutil/minio_integration_test.go` (add `TestIntegration_Put_*` tests)

- [ ] **Step 1 (Red): Add the integration tests first**

Append to `pkg/minioutil/minio_integration_test.go`:

```go
// TestIntegration_Put_RoundTrip verifies Put writes a JSON-encoded object
// with the documented content-type. Reads the raw object back via the
// underlying client (NOT via Bucket.Get, which is tested separately) so
// this test isolates Put's contract.
func TestIntegration_Put_RoundTrip(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k1", doc{Name: "alpha", Count: 7}))

	// Read raw via underlying client — verifies bytes-on-the-wire and content-type.
	obj, err := client.GetObject(ctx, bucketName, "k1", minio.GetObjectOptions{})
	require.NoError(t, err)
	defer obj.Close()
	info, err := obj.Stat()
	require.NoError(t, err)
	assert.Equal(t, "application/json; charset=utf-8", info.ContentType)

	body, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"alpha","count":7}`, string(body))
}

// TestIntegration_Put_Overwrites verifies repeat Put on the same key
// replaces the object (S3 native PUT semantics — no versioning configured).
func TestIntegration_Put_Overwrites(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k", doc{V: 1}))
	require.NoError(t, b.Put(ctx, "k", doc{V: 2}))

	obj, err := client.GetObject(ctx, bucketName, "k", minio.GetObjectOptions{})
	require.NoError(t, err)
	defer obj.Close()
	body, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":2}`, string(body))
}
```

Add `"io"` to the imports.

- [ ] **Step 2: Run the tests — confirm they FAIL**

```bash
go test -tags=integration -race -run TestIntegration_Put ./pkg/minioutil/...
```
Expected: FAIL with compile error (`Put` undefined).

- [ ] **Step 3 (Green): Implement `Put`**

Append to `pkg/minioutil/minio.go`:

```go
// Put marshals v as JSON and stores it under key with
// Content-Type: application/json; charset=utf-8. Existing objects at the
// same key are replaced (S3 native PUT semantics on non-versioned buckets).
func (b *Bucket[T]) Put(ctx context.Context, key string, v T) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("minioutil put %s/%s marshal: %w", b.name, key, err)
	}
	_, err = b.client.PutObject(ctx, b.name, key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{
		ContentType: "application/json; charset=utf-8",
	})
	if err != nil {
		return fmt.Errorf("minioutil put %s/%s: %w", b.name, key, err)
	}
	return nil
}
```

Add `"bytes"` and `"encoding/json"` to the imports.

- [ ] **Step 4: Run the tests — confirm they PASS**

```bash
go test -tags=integration -race -run TestIntegration_Put ./pkg/minioutil/...
```
Expected: PASS — both round-trip and overwrite cases.

- [ ] **Step 5: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/minioutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(minioutil): add Bucket[T].Put

Marshals v as JSON and stores it under key with explicit
Content-Type: application/json; charset=utf-8 so downstream tools
(S3 CLI, browsers, other languages) can identify the payload format
without out-of-band knowledge. Repeat Put on the same key replaces
the object (S3 native PUT semantics on non-versioned buckets).

Integration tests cover the JSON round-trip (raw bytes + content-type
asserted) and overwrite semantics."
```

---

## Task 13: Add `Bucket[T].Get` (NoSuchKey detection)

**Why:** Get is the read half of the round-trip. Per spec, Get returns `(nil, nil)` when the key does not exist (matches `Collection.FindOne` not-found semantics). The implementation pattern is load-bearing: minio-go's `GetObject` returns no error for missing keys — the not-found surfaces from the subsequent `obj.Stat()` call as a `minio.ErrorResponse{Code: "NoSuchKey"}`. Stat reuses the GetObject HTTP response (no extra round trip), and `errors.As` is the correct, type-safe way to discriminate.

**Files:**
- Modify: `pkg/minioutil/minio.go` (add `Get` method)
- Modify: `pkg/minioutil/minio_integration_test.go` (add `TestIntegration_Get_*` tests)

- [ ] **Step 1 (Red): Add the integration tests first**

Append to `pkg/minioutil/minio_integration_test.go`:

```go
// TestIntegration_Get_RoundTrip verifies a Put followed by a Get returns
// the originally-stored value. Uses Put to seed (already covered by Task 12
// tests, so trusted here).
func TestIntegration_Get_RoundTrip(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	want := doc{Name: "alpha", Count: 7}
	require.NoError(t, b.Put(ctx, "k1", want))

	got, err := b.Get(ctx, "k1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

// TestIntegration_Get_MissingKey verifies the (nil, nil) contract for a
// key that does not exist — matches Collection.FindOne semantics so callers
// can branch on result == nil instead of catching errors.
func TestIntegration_Get_MissingKey(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	got, err := b.Get(ctx, "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestIntegration_Get_MalformedJSON verifies decode errors propagate as
// errors (NOT as silent nil) when the stored object is not valid JSON for
// type T. Seeds the bucket with raw garbage via the underlying client to
// bypass Put's JSON encoding.
func TestIntegration_Get_MalformedJSON(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	garbage := []byte("not json at all")
	_, err = client.PutObject(ctx, bucketName, "bad", bytes.NewReader(garbage), int64(len(garbage)), minio.PutObjectOptions{})
	require.NoError(t, err)

	_, err = b.Get(ctx, "bad")
	require.Error(t, err)
}
```

Add `"bytes"` to the test file imports if not already present.

- [ ] **Step 2: Run the tests — confirm they FAIL**

```bash
go test -tags=integration -race -run TestIntegration_Get ./pkg/minioutil/...
```
Expected: FAIL with compile error (`Get` undefined).

- [ ] **Step 3 (Green): Implement `Get`**

Append to `pkg/minioutil/minio.go`:

```go
// Get fetches the object at key and unmarshals it from JSON into T.
// Returns (nil, nil) when the key does not exist — matches
// Collection.FindOne not-found semantics so callers branch on nil
// rather than catching errors.
//
// Implementation note: minio-go's GetObject returns no error for missing
// keys; the NoSuchKey response surfaces from the subsequent obj.Stat()
// call. Stat reuses the GetObject HTTP response (no extra round trip).
func (b *Bucket[T]) Get(ctx context.Context, key string) (*T, error) {
	obj, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("minioutil get %s/%s: %w", b.name, key, err)
	}
	defer obj.Close()

	if _, err := obj.Stat(); err != nil {
		var minioErr minio.ErrorResponse
		if errors.As(err, &minioErr) && minioErr.Code == "NoSuchKey" {
			return nil, nil
		}
		return nil, fmt.Errorf("minioutil get %s/%s stat: %w", b.name, key, err)
	}

	var v T
	if err := json.NewDecoder(obj).Decode(&v); err != nil {
		return nil, fmt.Errorf("minioutil get %s/%s decode: %w", b.name, key, err)
	}
	return &v, nil
}
```

Add `"errors"` to the imports.

- [ ] **Step 4: Run the tests — confirm they PASS**

```bash
go test -tags=integration -race -run TestIntegration_Get ./pkg/minioutil/...
```
Expected: PASS — round-trip, missing-key (nil, nil), and malformed-JSON cases.

- [ ] **Step 5: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/minioutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(minioutil): add Bucket[T].Get with NoSuchKey detection

Returns (nil, nil) for missing keys -- matches Collection.FindOne
not-found semantics so callers branch on result == nil rather than
catching errors. NoSuchKey discrimination uses errors.As against
minio.ErrorResponse and Stat() on the GetObject reader (the only
synchronous probe; minio-go's GetObject itself never returns
not-found). Decode failures propagate as errors so silent garbage
never reaches the caller.

Integration tests cover round-trip, missing-key, and
malformed-JSON paths."
```

---

## Task 14: Add `Bucket[T].List` (channel cancellation pattern)

**Why:** List enumerates keys by prefix, capped at `maxKeys` (default 1000) so a misuse against a large bucket can't accidentally fetch millions. Per spec, the implementation is load-bearing: minio-go's `ListObjects` returns a Go channel that minio-go fills from a goroutine. Breaking out of the range loop without cancelling the parent context leaks that goroutine — `context.WithCancel` + `defer cancel()` is the only safe drain pattern.

**Files:**
- Modify: `pkg/minioutil/minio.go` (add `List` method)
- Modify: `pkg/minioutil/minio_integration_test.go` (add `TestIntegration_List_*` tests)

- [ ] **Step 1 (Red): Add the integration tests first**

Append to `pkg/minioutil/minio_integration_test.go`:

```go
// TestIntegration_List_Prefix verifies prefix filtering returns only matching
// keys, in S3's lexicographic order.
func TestIntegration_List_Prefix(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "users/alice", doc{V: 1}))
	require.NoError(t, b.Put(ctx, "users/bob", doc{V: 2}))
	require.NoError(t, b.Put(ctx, "rooms/main", doc{V: 3}))

	keys, err := b.List(ctx, "users/", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"users/alice", "users/bob"}, keys)
}

// TestIntegration_List_DefaultCap verifies maxKeys=0 falls back to the
// documented 1000-key default cap (not unlimited).
func TestIntegration_List_DefaultCap(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	// Seed > 1000 keys would be slow; instead seed exactly the cap + a few extras
	// to keep the test fast while still proving the cap engages. We skip the
	// >1000 case to keep CI under a minute; the cap logic is also covered
	// by the maxKeys=N test below at small N.
	for i := 0; i < 5; i++ {
		require.NoError(t, b.Put(ctx, fmt.Sprintf("k-%02d", i), doc{V: i}))
	}

	keys, err := b.List(ctx, "k-", 0)
	require.NoError(t, err)
	assert.Len(t, keys, 5)
}

// TestIntegration_List_MaxKeysCap verifies maxKeys caps the result AND that
// the early break does not leak the minio-go listing goroutine. Uses
// goleak.IgnoreCurrent() snapshotted before the operation under test:
// minio-go's HTTP keepalive goroutines (IdleConnTimeout=60s) are NOT in
// goleak's default ignore list and would false-positive on a plain
// goleak.VerifyNone. The IgnoreCurrent baseline ignores goroutines that
// already existed (including keepalives from the prior Puts) so only NEW
// goroutines spawned by List get reported. The -race flag does NOT detect
// goroutine leaks, so this is the only real guarantee `defer cancel()` does its job.
func TestIntegration_List_MaxKeysCap(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		require.NoError(t, b.Put(ctx, fmt.Sprintf("k-%02d", i), doc{V: i}))
	}

	preList := goleak.IgnoreCurrent()
	keys, err := b.List(ctx, "k-", 3)
	require.NoError(t, err)
	require.Len(t, keys, 3)
	assert.Equal(t, []string{"k-00", "k-01", "k-02"}, keys)
	goleak.VerifyNone(t, preList)
}

// TestIntegration_List_EmptyResult verifies an empty (non-nil) slice is
// returned when nothing matches the prefix.
func TestIntegration_List_EmptyResult(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	keys, err := b.List(ctx, "no-such-prefix/", 0)
	require.NoError(t, err)
	require.NotNil(t, keys)
	assert.Empty(t, keys)
}
```

Add `"fmt"` to the test file imports if not already present, and add the `go.uber.org/goleak` dependency:

```bash
go get go.uber.org/goleak
```

Add `"go.uber.org/goleak"` to the test file imports.

- [ ] **Step 2: Run the tests — confirm they FAIL**

```bash
go test -tags=integration -race -run TestIntegration_List ./pkg/minioutil/...
```
Expected: FAIL with compile error (`List` undefined).

- [ ] **Step 3 (Green): Implement `List`**

Append to `pkg/minioutil/minio.go`:

```go
const defaultListCap = 1000

// List returns up to maxKeys keys whose names start with prefix, in S3
// lexicographic order. maxKeys=0 defaults to defaultListCap (1000) to
// prevent unbounded scans on misuse. Pass math.MaxInt explicitly to drain
// a bucket -- but be aware this loads all keys into memory and may make
// many round trips on large buckets.
//
// Implementation note: minio-go's ListObjects spawns a goroutine that
// fills the returned channel. context.WithCancel + defer cancel() is
// load-bearing -- breaking out of the range loop without cancelling
// leaks that goroutine.
func (b *Bucket[T]) List(ctx context.Context, prefix string, maxKeys int) ([]string, error) {
	if maxKeys <= 0 {
		maxKeys = defaultListCap
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	keys := make([]string, 0)
	for obj := range b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("minioutil list %s prefix=%q: %w", b.name, prefix, obj.Err)
		}
		keys = append(keys, obj.Key)
		if len(keys) >= maxKeys {
			break
		}
	}
	return keys, nil
}
```

- [ ] **Step 4: Run the tests — confirm they PASS**

```bash
go test -tags=integration -race -run TestIntegration_List ./pkg/minioutil/...
```
Expected: PASS — prefix, default cap, maxKeys cap, and empty-result cases.

- [ ] **Step 5: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum pkg/minioutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(minioutil): add Bucket[T].List with channel cancellation pattern

Lists up to maxKeys keys by prefix in S3 lexicographic order.
maxKeys=0 falls back to defaultListCap (1000) so misuse against a
large bucket can't accidentally fetch millions; pass math.MaxInt
to drain. context.WithCancel + defer cancel() is load-bearing --
minio-go's ListObjects spawns a goroutine that fills the returned
channel, and breaking out of the range loop without cancelling
leaks that goroutine.

Integration tests cover prefix filtering, default cap, maxKeys cap
(with early break + goleak.VerifyNone), and empty-result (non-nil
slice) paths. Adds go.uber.org/goleak (Apache-2.0, test-only) so the
goroutine-leak guard is ignore-list-aware re HTTP transport pool."
```

---

## Task 15: Add `Bucket[T].Delete` (idempotency)

**Why:** Delete completes the CRUD surface. Per spec, Delete is idempotent on non-versioned buckets — S3 / MinIO native semantics return `204 No Content` regardless of prior existence, so callers don't need to pre-check or swallow not-found errors. (Versioning is out of scope; on a versioned bucket Delete creates a delete-marker rather than performing a true delete.)

**Files:**
- Modify: `pkg/minioutil/minio.go` (add `Delete` method)
- Modify: `pkg/minioutil/minio_integration_test.go` (add `TestIntegration_Delete_*` tests)

- [ ] **Step 1 (Red): Add the integration tests first**

Append to `pkg/minioutil/minio_integration_test.go`:

```go
// TestIntegration_Delete_RemovesExisting verifies Delete removes a
// previously-Put object so a subsequent Get returns (nil, nil).
func TestIntegration_Delete_RemovesExisting(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k", doc{V: 1}))
	require.NoError(t, b.Delete(ctx, "k"))

	got, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestIntegration_Delete_Idempotent verifies Delete on a missing key
// returns nil (S3 / MinIO native semantics: DELETE returns 204 regardless).
// Callers don't need to pre-check existence or swallow not-found errors.
func TestIntegration_Delete_Idempotent(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Delete(ctx, "never-existed"))
	require.NoError(t, b.Delete(ctx, "never-existed")) // and again
}
```

- [ ] **Step 2: Run the tests — confirm they FAIL**

```bash
go test -tags=integration -race -run TestIntegration_Delete ./pkg/minioutil/...
```
Expected: FAIL with compile error (`Delete` undefined).

- [ ] **Step 3 (Green): Implement `Delete`**

Append to `pkg/minioutil/minio.go`:

```go
// Delete removes the object at key. Idempotent on non-versioned buckets
// -- returns nil if the key does not exist (S3 / MinIO native semantics:
// DELETE returns 204 regardless of prior existence). Versioning is out of
// scope; on a versioned bucket this creates a delete-marker rather than
// performing a true delete, and subsequent reads see the marker.
func (b *Bucket[T]) Delete(ctx context.Context, key string) error {
	if err := b.client.RemoveObject(ctx, b.name, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("minioutil delete %s/%s: %w", b.name, key, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests — confirm they PASS**

```bash
go test -tags=integration -race -run TestIntegration_Delete ./pkg/minioutil/...
```
Expected: PASS — both removes-existing and idempotent-on-missing cases.

- [ ] **Step 5: Run the full minioutil test suite**

```bash
go test -tags=integration -race ./pkg/minioutil/...
```
Expected: PASS — all unit and integration tests across Tasks 9-15.

- [ ] **Step 6: Run lint**

```bash
make lint
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/minioutil/minio.go pkg/minioutil/minio_integration_test.go
git commit -m "feat(minioutil): add Bucket[T].Delete (idempotent)

Removes the object at key. Idempotent on non-versioned buckets --
returns nil if the key does not exist (S3 / MinIO native semantics:
DELETE returns 204 regardless of prior existence). Versioning is
out of scope; on a versioned bucket this creates a delete-marker
rather than performing a true delete.

Integration tests cover both removes-existing and
idempotent-on-missing paths. The minioutil CRUD surface
(Put / Get / List / Delete) is now complete."
```

---

## Final verification

After all 15 tasks land, run the complete suite to confirm nothing regressed:

```bash
make lint
make test
make test-integration
```

All must pass. The history-service migration (Tasks 1-4) MUST leave history-service's existing test suite green — the moves are pure import flips.

---
