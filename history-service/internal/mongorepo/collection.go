package mongorepo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Collection is a type-safe wrapper around *mongo.Collection that normalises
// ErrNoDocuments and wraps errors consistently.
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
