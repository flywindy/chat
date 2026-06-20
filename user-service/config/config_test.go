package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	t.Setenv("MONGO_URI", "mongodb://x")
	t.Setenv("NATS_URL", "nats://x")
	t.Setenv("SITE_ID", "site-a")
	t.Setenv("ALL_SITE_IDS", "site-a,site-b")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "site-a", cfg.SiteID)
	require.Equal(t, []string{"site-a", "site-b"}, cfg.AllSiteIDs)
	require.Equal(t, "chat", cfg.Mongo.DB)
	require.Equal(t, 1000, cfg.MaxSubscriptionLimit)
}

func TestLoad_EmptyAllSiteIDs(t *testing.T) {
	t.Setenv("MONGO_URI", "mongodb://x")
	t.Setenv("NATS_URL", "nats://x")
	t.Setenv("SITE_ID", "site-a")
	t.Setenv("ALL_SITE_IDS", "")
	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.AllSiteIDs) // empty env ⇒ no stray "" site that would later be published to
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("MONGO_URI", "mongodb://x")
	t.Setenv("NATS_URL", "nats://x")
	t.Setenv("SITE_ID", "site-a")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, 1000, cfg.MaxSubscriptionLimit)
	require.Equal(t, 15*time.Second, cfg.HandlerTimeout)
}

func TestLoad_MissingRequired(t *testing.T) {
	// notEmpty fires when a required value is unset OR set-but-empty;
	// each required field is exercised independently.
	cases := []struct {
		name                      string
		mongoURI, natsURL, siteID string
	}{
		{"mongo uri empty", "", "nats://x", "site-a"},
		{"nats url empty", "mongodb://x", "", "site-a"},
		{"site id empty", "mongodb://x", "nats://x", ""},
		{"all empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MONGO_URI", tc.mongoURI)
			t.Setenv("NATS_URL", tc.natsURL)
			t.Setenv("SITE_ID", tc.siteID)
			_, err := Load()
			require.Error(t, err)
		})
	}
}

func TestLoad_InvalidMaxSubscriptionLimit(t *testing.T) {
	// A non-positive limit must fail at startup, not produce a $limit:0/negative
	// stage that errors at query time.
	for _, v := range []string{"0", "-1"} {
		t.Run("limit="+v, func(t *testing.T) {
			t.Setenv("MONGO_URI", "mongodb://x")
			t.Setenv("NATS_URL", "nats://x")
			t.Setenv("SITE_ID", "site-a")
			t.Setenv("MAX_SUBSCRIPTION_LIMIT", v)
			_, err := Load()
			require.Error(t, err)
		})
	}
}
