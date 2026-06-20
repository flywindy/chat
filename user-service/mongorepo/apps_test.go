//go:build integration

package mongorepo

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/mongoutil"
)

func seedApps(t *testing.T, db *mongo.Database) {
	t.Helper()
	seed(t, db, "apps",
		bson.M{"_id": "app-helper", "name": "Helper", "assistant": bson.M{"enabled": true, "name": "helper.bot"}},
		bson.M{"_id": "app-other", "name": "Other", "assistant": bson.M{"enabled": true, "name": "other.bot"}},
	)
	// alice is subscribed to helper.bot only.
	seed(t, db, "subscriptions",
		bson.M{"_id": "sub-helper", "u": bson.M{"_id": "u-alice", "account": "alice"}, "name": "helper.bot", "roomId": "r-helper",
			"roomType": "botDM", "siteId": "site-a", "isSubscribed": true},
		bson.M{"_id": "sub-other", "u": bson.M{"_id": "u-alice", "account": "alice"}, "name": "other.bot", "roomId": "r-other",
			"roomType": "botDM", "siteId": "site-a", "isSubscribed": false},
		// Collision: same name as other.bot but channel roomType — lookup must NOT count this toward Other's isSubscribed.
		bson.M{"_id": "sub-collision", "u": bson.M{"_id": "u-alice", "account": "alice"}, "name": "other.bot", "roomId": "r-collision",
			"roomType": "channel", "siteId": "site-a", "isSubscribed": true},
	)
}

func TestGetApp_Integration(t *testing.T) {
	r, db := newTestAppRepo(t)
	ctx := context.Background()
	seedApps(t, db)

	t.Run("found", func(t *testing.T) {
		app, err := r.GetApp(ctx, "app-helper")
		require.NoError(t, err)
		require.NotNil(t, app)
		assert.Equal(t, "Helper", app.Name)
		require.NotNil(t, app.Assistant)
		assert.Equal(t, "helper.bot", app.Assistant.Name)
	})

	t.Run("miss", func(t *testing.T) {
		app, err := r.GetApp(ctx, "nope")
		require.NoError(t, err)
		assert.Nil(t, app)
	})
}

func TestGetAppsByAssistants_Integration(t *testing.T) {
	r, db := newTestAppRepo(t)
	ctx := context.Background()
	seedApps(t, db)

	t.Run("maps bot accounts to full app docs", func(t *testing.T) {
		apps, err := r.GetAppsByAssistants(ctx, []string{"helper.bot", "other.bot"})
		require.NoError(t, err)
		require.Len(t, apps, 2)
		require.NotNil(t, apps["helper.bot"])
		assert.Equal(t, "app-helper", apps["helper.bot"].ID, "AppID must come from the doc _id")
		assert.Equal(t, "Helper", apps["helper.bot"].Name)
		require.NotNil(t, apps["helper.bot"].Assistant)
		assert.Equal(t, "helper.bot", apps["helper.bot"].Assistant.Name)
		require.NotNil(t, apps["other.bot"])
		assert.Equal(t, "Other", apps["other.bot"].Name)
	})

	t.Run("unknown bot omitted", func(t *testing.T) {
		apps, err := r.GetAppsByAssistants(ctx, []string{"helper.bot", "ghost.bot"})
		require.NoError(t, err)
		require.Len(t, apps, 1)
		require.NotNil(t, apps["helper.bot"])
		assert.Equal(t, "Helper", apps["helper.bot"].Name)
	})

	t.Run("empty input yields empty map", func(t *testing.T) {
		apps, err := r.GetAppsByAssistants(ctx, []string{})
		require.NoError(t, err)
		assert.Empty(t, apps)
	})
}

func TestListApps_Integration(t *testing.T) {
	r, db := newTestAppRepo(t)
	ctx := context.Background()
	seedApps(t, db)

	page, err := r.ListApps(ctx, "alice", mongoutil.NewOffsetPageRequest(0, 0))
	require.NoError(t, err)
	require.Len(t, page.Data, 2)
	assert.Equal(t, int64(2), page.Total)
	// Sorted by name: Helper, Other.
	assert.Equal(t, "Helper", page.Data[0].Name)
	assert.True(t, page.Data[0].IsSubscribed, "helper.bot is subscribed")
	assert.Equal(t, "Other", page.Data[1].Name)
	assert.False(t, page.Data[1].IsSubscribed,
		"other.bot has no subscribed botDM; a same-name channel sub must NOT flip isSubscribed")
}

func TestListApps_Pagination_Integration(t *testing.T) {
	r, db := newTestAppRepo(t)
	ctx := context.Background()
	seedApps(t, db)
	// Three extras on top of Helper + Other; names sort after "Helper" and
	// before/after "Other" deterministically: Helper, Other, Zeta1, Zeta2, Zeta3.
	for i := 1; i <= 3; i++ {
		seed(t, db, "apps", bson.M{
			"_id":  fmt.Sprintf("app-zeta-%d", i),
			"name": fmt.Sprintf("Zeta%d", i),
		})
	}

	t.Run("slice keeps full catalog total", func(t *testing.T) {
		page, err := r.ListApps(ctx, "alice", mongoutil.NewOffsetPageRequest(1, 2))
		require.NoError(t, err)
		require.Len(t, page.Data, 2)
		assert.Equal(t, int64(5), page.Total, "total is the full catalog count, not the page size")
		assert.Equal(t, "Other", page.Data[0].Name)
		assert.Equal(t, "Zeta1", page.Data[1].Name)
	})

	t.Run("last partial page", func(t *testing.T) {
		page, err := r.ListApps(ctx, "alice", mongoutil.NewOffsetPageRequest(4, 2))
		require.NoError(t, err)
		require.Len(t, page.Data, 1)
		assert.Equal(t, "Zeta3", page.Data[0].Name)
		assert.Equal(t, int64(5), page.Total)
	})

	t.Run("offset beyond catalog", func(t *testing.T) {
		page, err := r.ListApps(ctx, "alice", mongoutil.NewOffsetPageRequest(10, 2))
		require.NoError(t, err)
		require.NotNil(t, page.Data, "Data must be non-nil so JSON marshals to []")
		assert.Empty(t, page.Data)
		assert.Equal(t, int64(5), page.Total)
	})
}

func TestListApps_FieldPathAccountTreatedAsLiteral_Integration(t *testing.T) {
	r, db := newTestAppRepo(t)
	ctx := context.Background()
	seedApps(t, db)

	// Without $literal, "$u.account" is a field path and $eq:["$u.account","$u.account"] holds for every doc, flipping isSubscribed incorrectly.
	page, err := r.ListApps(ctx, "$u.account", mongoutil.NewOffsetPageRequest(0, 0))
	require.NoError(t, err)
	require.Len(t, page.Data, 2)
	for _, app := range page.Data {
		assert.False(t, app.IsSubscribed, "field-path-shaped account must match no subscription (app %s)", app.Name)
	}
}
