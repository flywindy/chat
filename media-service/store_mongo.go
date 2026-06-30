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
	users         *mongo.Collection
	subscriptions *mongo.Collection
	avatars       *mongo.Collection
}

func newMongoStore(db *mongo.Database) *mongoStore {
	return &mongoStore{
		users:         db.Collection("users"),
		subscriptions: db.Collection("subscriptions"),
		avatars:       db.Collection("avatars"),
	}
}

func (s *mongoStore) EmployeeID(ctx context.Context, account string) (string, bool, error) {
	var u model.User
	err := s.users.FindOne(ctx, bson.M{"account": account},
		options.FindOne().SetProjection(bson.M{"employeeId": 1})).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find employeeId: %w", err)
	}
	if u.EmployeeID == "" {
		return "", false, nil
	}
	return u.EmployeeID, true, nil
}

func (s *mongoStore) BotSite(ctx context.Context, account string) (string, bool, error) {
	var u model.User
	err := s.users.FindOne(ctx, bson.M{"account": account},
		options.FindOne().SetProjection(bson.M{"siteId": 1})).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find bot site: %w", err)
	}
	if u.SiteID == "" {
		return "", false, nil
	}
	return u.SiteID, true, nil
}

func (s *mongoStore) RoomSite(ctx context.Context, roomID string) (string, model.RoomType, string, bool, error) {
	var sub model.Subscription
	err := s.subscriptions.FindOne(ctx, bson.M{"roomId": roomID},
		options.FindOne().SetProjection(bson.M{"siteId": 1, "roomType": 1, "name": 1})).Decode(&sub)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("find room subscription: %w", err)
	}
	return sub.SiteID, sub.RoomType, sub.Name, true, nil
}

func (s *mongoStore) Avatar(ctx context.Context, subjectType model.AvatarSubjectType, subjectID string) (*model.Avatar, bool, error) {
	id := string(subjectType) + ":" + subjectID
	var av model.Avatar
	err := s.avatars.FindOne(ctx, bson.M{"_id": id}).Decode(&av)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find avatar: %w", err)
	}
	return &av, true, nil
}

func (s *mongoStore) SetBotAvatar(ctx context.Context, av *model.Avatar) error {
	_, err := s.avatars.ReplaceOne(ctx, bson.M{"_id": av.ID}, av, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert bot avatar: %w", err)
	}
	return nil
}
