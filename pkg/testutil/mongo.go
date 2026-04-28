//go:build integration

package testutil

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/testutil/testimages"
)

var (
	mongoOnce    sync.Once
	mongoClient  *mongo.Client
	mongoInitErr error
)

func ensureMongoClient() (*mongo.Client, error) {
	mongoOnce.Do(func() {
		ctx := context.Background()
		container, err := mongodb.Run(ctx, testimages.Mongo)
		if err != nil {
			mongoInitErr = fmt.Errorf("start mongo: %w", err)
			return
		}
		uri, err := container.ConnectionString(ctx)
		if err != nil {
			_ = container.Terminate(ctx)
			mongoInitErr = fmt.Errorf("get mongo uri: %w", err)
			return
		}
		c, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			_ = container.Terminate(ctx)
			mongoInitErr = fmt.Errorf("connect mongo: %w", err)
			return
		}
		mongoClient = c
	})
	return mongoClient, mongoInitErr
}

// MongoDB returns an isolated Mongo database for the current test; dropped on t.Cleanup.
func MongoDB(t *testing.T, prefix string) *mongo.Database {
	t.Helper()
	c, err := ensureMongoClient()
	if err != nil {
		t.Fatalf("testutil.MongoDB: %v", err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(t.Name())) // hash.Hash.Write never returns an error.
	db := c.Database(fmt.Sprintf("%s_%x", prefix, h.Sum64()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = db.Drop(ctx)
	})
	return db
}
