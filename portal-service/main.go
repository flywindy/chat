package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/gin-gonic/gin"

	o11ygin "github.com/flywindy/o11y/gin"

	"github.com/hmchangw/chat/pkg/ginutil"
	"github.com/hmchangw/chat/pkg/mongoutil"
	"github.com/hmchangw/chat/pkg/obs"
	"github.com/hmchangw/chat/pkg/shutdown"
)

// cacheRetryInterval paces reload attempts after a failed directory load, so
// a Mongo blip at startup does not leave the portal unready for a full
// refresh interval.
const cacheRetryInterval = 30 * time.Second

type config struct {
	Port               string `env:"PORT"                         envDefault:"8081"`
	DevMode            bool   `env:"DEV_MODE"                     envDefault:"false"`
	DevFallbackSiteID  string `env:"PORTAL_DEV_FALLBACK_SITE_ID"  envDefault:"site-local"`
	DevFallbackNatsURL string `env:"PORTAL_DEV_FALLBACK_NATS_URL" envDefault:"ws://localhost:9222"`

	// SiteURLs is the per-site URL registry: a JSON object mapping siteId to
	// {authServiceUrl, baseUrl}. A single template can't express sites on
	// different domains, so each site's URLs are listed explicitly.
	SiteURLs string `env:"PORTAL_SITE_URLS,required"`

	// CacheRefreshInterval drives how often the directory is reloaded (the
	// hr_employee × users intersection via $lookup). Shorter than the daily HR
	// cron so a newly provisioned user appears within a couple of hours.
	CacheRefreshInterval time.Duration `env:"PORTAL_CACHE_REFRESH_INTERVAL" envDefault:"2h"`

	MongoURI      string `env:"MONGO_URI,required"`
	MongoDB       string `env:"MONGO_DB"       envDefault:"portal"`
	MongoUsername string `env:"MONGO_USERNAME" envDefault:""`
	MongoPassword string `env:"MONGO_PASSWORD" envDefault:""`
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	sites, err := parseSiteURLs(cfg.SiteURLs)
	if err != nil {
		return fmt.Errorf("parse site URL registry: %w", err)
	}

	ctx := context.Background()

	sdk, obsShutdown, err := obs.Init(ctx)
	if err != nil {
		return fmt.Errorf("init observability: %w", err)
	}

	mongoClient, err := mongoutil.Connect(ctx, cfg.MongoURI, cfg.MongoUsername, cfg.MongoPassword, mongoutil.WithObservability(sdk))
	if err != nil {
		return fmt.Errorf("connect mongo: %w", err)
	}

	store := newMongoDirectoryStore(mongoClient.Database(cfg.MongoDB))
	if err := store.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure directory indexes: %w", err)
	}

	// Populate the directory cache in the background; /readyz stays
	// unavailable until the first successful load.
	cache := newDirectoryCache()
	refreshCtx, refreshCancel := context.WithCancel(ctx)
	defer refreshCancel()
	var refreshWG sync.WaitGroup
	refreshWG.Go(func() {
		cache.RefreshLoop(refreshCtx, store, cfg.CacheRefreshInterval, cacheRetryInterval)
	})

	slog.Info("directory config", "sites", len(sites), "refreshInterval", cfg.CacheRefreshInterval.String())

	handler := NewPortalHandler(cache, cfg.DevMode,
		cfg.DevFallbackSiteID, cfg.DevFallbackNatsURL, sites)
	if cfg.DevMode {
		slog.Info("dev mode enabled — unknown accounts fall back to the dev site")
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// CORS handles preflight before tracing so OPTIONS noise does not pollute Tempo.
	r.Use(ginutil.CORS())
	// o11y server-span middleware wraps real requests so downstream slog/handlers
	// are trace-correlated.
	r.Use(o11ygin.Middleware("portal-service", sdk.TracerProvider(), sdk.MeterProvider(), sdk.Propagator)...)
	r.Use(gin.Recovery())
	r.Use(ginutil.RequestID())
	r.Use(ginutil.AccessLog())
	registerRoutes(r, handler)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	srvErr := make(chan error, 1)
	go func() {
		slog.Info("portal service starting", "addr", addr)
		srvErr <- srv.ListenAndServe()
	}()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		shutdown.Wait(ctx, 25*time.Second,
			func(ctx context.Context) error {
				slog.Info("shutting down portal service")
				err := srv.Shutdown(ctx)
				refreshCancel()
				refreshWG.Wait()
				mongoutil.Disconnect(ctx, mongoClient)
				return err
			},
			func(ctx context.Context) error { return obsShutdown(ctx) },
		)
	}()

	err = <-srvErr
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen portal server: %w", err)
	}
	<-shutdownDone

	return nil
}
