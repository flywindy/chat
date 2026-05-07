package mongoutil

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BulkResult mirrors mongo.BulkWriteResult; bulk methods return (nil, nil) on empty input.
type BulkResult struct {
	Matched      int64
	Modified     int64
	Upserted     int64
	Inserted     int64
	Deleted      int64
	UpsertedIDs  map[int64]any // ordinal -> _id; non-contiguous under unordered partial failures
	Acknowledged bool
}

func UpsertModel(filter, update any) *mongo.UpdateOneModel {
	return mongo.NewUpdateOneModel().SetFilter(filter).SetUpdate(update).SetUpsert(true)
}

func DeleteModel(filter any) *mongo.DeleteOneModel {
	return mongo.NewDeleteOneModel().SetFilter(filter)
}

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

// bsonSetWithoutID strips _id from the marshaled $set payload — MongoDB rejects updates that touch immutable _id.
func bsonSetWithoutID(item any) (bson.M, error) {
	raw, err := bson.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("bson marshal: %w", err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bson unmarshal: %w", err)
	}
	delete(m, "_id")
	return m, nil
}
