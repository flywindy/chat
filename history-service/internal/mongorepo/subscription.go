package mongorepo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/model"
)

const subscriptionsCollection = "subscriptions"

type SubscriptionRepo struct {
	subscriptions *Collection[model.Subscription]
}

func NewSubscriptionRepo(db *mongo.Database) *SubscriptionRepo {
	return &SubscriptionRepo{
		subscriptions: NewCollection[model.Subscription](db.Collection(subscriptionsCollection)),
	}
}

// GetSubscription returns the full subscription for a user in a room, or (nil, nil) when not subscribed.
func (r *SubscriptionRepo) GetSubscription(ctx context.Context, account, roomID string) (*model.Subscription, error) {
	return r.subscriptions.FindOne(ctx, bson.M{"u.account": account, "roomId": roomID})
}

// GetHistorySharedSince returns (nil, true, nil) = full access; (&t, true, nil) = restricted; (nil, false, nil) = not subscribed.
func (r *SubscriptionRepo) GetHistorySharedSince(ctx context.Context, account, roomID string) (*time.Time, bool, error) {
	sub, err := r.subscriptions.FindOne(ctx,
		bson.M{"u.account": account, "roomId": roomID},
		WithProjection(bson.M{"historySharedSince": 1, "_id": 0}),
	)
	if err != nil {
		return nil, false, err
	}
	if sub == nil {
		return nil, false, nil
	}
	return sub.HistorySharedSince, true, nil
}
