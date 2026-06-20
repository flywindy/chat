package mongorepo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/mongoutil"
	"github.com/hmchangw/chat/user-service/models"
)

const appsCollection = "apps"

// AppRepo is the Mongo implementation of service.AppRepository.
type AppRepo struct {
	apps *mongoutil.Collection[model.App]
	// items views the same collection decoded as AppListItem ($addFields isSubscribed).
	items *mongoutil.Collection[models.AppListItem]
}

// NewAppRepo builds an AppRepo over db.
func NewAppRepo(db *mongo.Database) *AppRepo {
	col := db.Collection(appsCollection)
	return &AppRepo{
		apps:  mongoutil.NewCollection[model.App](col),
		items: mongoutil.NewCollection[models.AppListItem](col),
	}
}

// EnsureIndexes creates the apps index that backs assistant.name lookups
// (GetAppsByAssistants and the bot-DM $lookup), removing the COLLSCAN. The name
// matches room-service's so the two services' CreateOne calls agree instead of
// colliding with IndexOptionsConflict — each service declares the index it needs.
func (r *AppRepo) EnsureIndexes(ctx context.Context) error {
	appsIndex := mongo.IndexModel{
		Keys:    bson.D{{Key: "assistant.name", Value: 1}},
		Options: options.Index().SetName("assistant_name_idx"),
	}
	if _, err := r.apps.Raw().Indexes().CreateOne(ctx, appsIndex); err != nil {
		return fmt.Errorf("ensure apps index: %w", err)
	}
	return nil
}

// GetApp returns the app by id, or (nil, nil) when none matches.
func (r *AppRepo) GetApp(ctx context.Context, appID string) (*model.App, error) {
	return r.apps.FindByID(ctx, appID)
}

// ListApps returns a name-sorted page of apps with isSubscribed per user; Total is the full catalog count.
func (r *AppRepo) ListApps(ctx context.Context, account string, page mongoutil.OffsetPageRequest) (mongoutil.OffsetPage[models.AppListItem], error) {
	pipeline := bson.A{
		bson.M{"$lookup": bson.M{
			"from": subscriptionsCollection,
			"let":  bson.M{"botName": "$assistant.name"},
			"pipeline": bson.A{bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
				// $literal so a $-prefixed account isn't read as a field path.
				bson.M{"$eq": bson.A{"$u.account", bson.M{"$literal": account}}},
				bson.M{"$eq": bson.A{"$name", "$$botName"}},
				bson.M{"$eq": bson.A{"$roomType", "botDM"}},
				bson.M{"$eq": bson.A{"$isSubscribed", true}},
			}}}}},
			"as": "sub",
		}},
		bson.M{"$addFields": bson.M{"isSubscribed": bson.M{"$gt": bson.A{bson.M{"$size": "$sub"}, 0}}}},
		bson.M{"$project": bson.M{"sub": 0}},
		bson.M{"$sort": bson.M{"name": 1}},
	}
	out, err := r.items.AggregatePaged(ctx, pipeline, page)
	if err != nil {
		return mongoutil.OffsetPage[models.AppListItem]{}, fmt.Errorf("aggregate apps page: %w", err)
	}
	return out, nil
}

// GetAppsByAssistants maps bot account (assistant.name) → the full app document for the given accounts.
func (r *AppRepo) GetAppsByAssistants(ctx context.Context, botAccounts []string) (map[string]*model.App, error) {
	apps, err := r.apps.FindMany(ctx, bson.M{"assistant.name": bson.M{"$in": botAccounts}})
	if err != nil {
		return nil, fmt.Errorf("find apps by assistant names: %w", err)
	}
	out := make(map[string]*model.App, len(apps))
	for i := range apps {
		if apps[i].Assistant != nil {
			app := apps[i]
			out[app.Assistant.Name] = &app
		}
	}
	return out, nil
}
