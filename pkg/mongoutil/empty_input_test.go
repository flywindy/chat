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

func TestBulkUpsert_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	res, err := c.BulkUpsert(context.Background(), nil, func(testEmptyDoc) any { return nil })
	require.NoError(t, err)
	assert.Nil(t, res)
}

func TestBulkUpsertByID_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	res, err := c.BulkUpsertByID(context.Background(), nil, func(testEmptyDoc) string { return "" })
	require.NoError(t, err)
	assert.Nil(t, res)
}

func TestInsertMany_EmptyInputShortCircuits(t *testing.T) {
	c := &Collection[testEmptyDoc]{col: nil, name: "test"}
	n, err := c.InsertMany(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

type testEmptyDoc struct {
	ID string `bson:"_id"`
}
