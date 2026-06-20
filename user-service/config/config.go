package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// MongoConfig holds MongoDB connection settings (env prefix: MONGO_).
type MongoConfig struct {
	URI      string `env:"URI,notEmpty"`
	DB       string `env:"DB"       envDefault:"chat"`
	Username string `env:"USERNAME" envDefault:""`
	Password string `env:"PASSWORD" envDefault:""`
}

// NATSConfig holds NATS connection settings (env prefix: NATS_).
type NATSConfig struct {
	URL       string `env:"URL,notEmpty"`
	CredsFile string `env:"CREDS_FILE" envDefault:""`
}

// Config is the top-level configuration for user-service.
type Config struct {
	// SiteID is required: baked into subscription subjects and outbox routing; missing it would silently federate under a wrong ID.
	SiteID               string        `env:"SITE_ID,notEmpty"`
	AllSiteIDs           []string      `env:"ALL_SITE_IDS"           envDefault:"" envSeparator:","`
	MaxSubscriptionLimit int           `env:"MAX_SUBSCRIPTION_LIMIT" envDefault:"1000"`
	MaxAccountNames      int           `env:"MAX_ACCOUNT_NAMES"      envDefault:"100"`
	HandlerTimeout       time.Duration `env:"HANDLER_TIMEOUT"        envDefault:"15s"`
	Mongo                MongoConfig   `envPrefix:"MONGO_"`
	NATS                 NATSConfig    `envPrefix:"NATS_"`
}

// Load parses environment variables into Config; rejects MAX_SUBSCRIPTION_LIMIT < 1 because $limit:0 errors at query time.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse user-service config: %w", err)
	}
	if cfg.MaxSubscriptionLimit < 1 {
		return Config{}, fmt.Errorf("MAX_SUBSCRIPTION_LIMIT must be >= 1, got %d", cfg.MaxSubscriptionLimit)
	}
	return cfg, nil
}
