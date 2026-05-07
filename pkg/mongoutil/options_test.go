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
