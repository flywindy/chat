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

type mongoStore struct {
	subscriptions *mongo.Collection
	rooms         *mongo.Collection
}

// NewMongoStore returns a Store backed by the subscriptions and rooms collections.
func NewMongoStore(db *mongo.Database) *mongoStore {
	return &mongoStore{
		subscriptions: db.Collection("subscriptions"),
		rooms:         db.Collection("rooms"),
	}
}

func (s *mongoStore) IsMember(ctx context.Context, roomID, account string) (bool, error) {
	// Existence check — a projected FindOne is lighter than CountDocuments
	// (which runs an aggregation) and stops at the first index match.
	err := s.subscriptions.FindOne(ctx,
		bson.M{"roomId": roomID, "u.account": account},
		options.FindOne().SetProjection(bson.M{"_id": 1}),
	).Err()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		return false, fmt.Errorf("find subscription for room %s: %w", roomID, err)
	}
	return true, nil
}

func (s *mongoStore) GetRoom(ctx context.Context, roomID string) (*model.Room, error) {
	var room model.Room
	if err := s.rooms.FindOne(ctx, bson.M{"_id": roomID}).Decode(&room); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("get room %s: %w", roomID, ErrRoomNotFound)
		}
		return nil, fmt.Errorf("get room %s: %w", roomID, err)
	}
	return &room, nil
}
