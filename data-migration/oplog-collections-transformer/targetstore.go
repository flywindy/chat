package main

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/model"
)

// mongoTargetStore is the new-stack per-site Mongo access the transformer needs:
// user insert-if-absent plus thread_room / user FK resolution for thread-sub mapping.
type mongoTargetStore struct {
	users       *mongo.Collection // TargetDB.users
	threadRooms *mongo.Collection // TargetDB.thread_rooms
}

// Compile-time assertion that *mongoTargetStore satisfies targetStore.
var _ targetStore = (*mongoTargetStore)(nil)

// NewMongoTargetStore binds the users and thread_rooms collections on the target DB.
func NewMongoTargetStore(db *mongo.Database) *mongoTargetStore {
	return &mongoTargetStore{
		users:       db.Collection("users"),
		threadRooms: db.Collection("thread_rooms"),
	}
}

// EnsureIndexes creates the unique index on users.account — the insert-if-absent dedup key.
// thread_rooms indexes are owned by message-worker and intentionally not touched here.
func (s *mongoTargetStore) EnsureIndexes(ctx context.Context) error {
	if _, err := s.users.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "account", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return fmt.Errorf("ensure users account index: %w", err)
	}
	return nil
}

// UpsertUserIfAbsent inserts u keyed by account only when absent, leaving an existing doc
// (owned by the company-wide sync) untouched. inserted reports whether a new doc was created.
//
//nolint:gocritic // model.User passed by value: one per migrated user record, off the hot path.
func (s *mongoTargetStore) UpsertUserIfAbsent(ctx context.Context, u model.User) (bool, error) {
	res, err := s.users.UpdateOne(ctx,
		bson.M{"account": u.Account},
		bson.M{"$setOnInsert": u},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return false, fmt.Errorf("upsert user if absent: %w", err)
	}
	return res.UpsertedCount > 0, nil
}

// FindThreadRoom resolves the thread room for parentMessageID, returning room id, thread room id,
// and its home site. found=false (no error) when no thread room exists yet for that parent message.
func (s *mongoTargetStore) FindThreadRoom(ctx context.Context, parentMessageID string) (string, string, string, bool, error) {
	var tr model.ThreadRoom
	err := s.threadRooms.FindOne(ctx, bson.M{"parentMessageId": parentMessageID},
		options.FindOne().SetProjection(bson.M{"roomId": 1, "siteId": 1}),
	).Decode(&tr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("find thread room by parent message: %w", err)
	}
	return tr.RoomID, tr.ID, tr.SiteID, true, nil
}

// FindUserID resolves the new-stack user _id for the given account. found=false (no error)
// when the account has not been seeded yet.
func (s *mongoTargetStore) FindUserID(ctx context.Context, account string) (string, bool, error) {
	var u model.User
	err := s.users.FindOne(ctx, bson.M{"account": account},
		options.FindOne().SetProjection(bson.M{"_id": 1}),
	).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find user id by account: %w", err)
	}
	return u.ID, true, nil
}
