package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/otelutil"
	"github.com/hmchangw/chat/pkg/searchengine"
	"github.com/hmchangw/chat/pkg/shutdown"
	"github.com/hmchangw/chat/pkg/valkeyutil"
)

// ESConfig bundles the search backend knobs. BACKEND is the key
// `pkg/searchengine.New` reads to choose between elasticsearch/opensearch.
type ESConfig struct {
	URL     string `env:"URL,required"`
	Backend string `env:"BACKEND" envDefault:"elasticsearch"`
}

type ValkeyConfig struct {
	Addr     string `env:"ADDR,required"`
	Password string `env:"PASSWORD" envDefault:""`
}

type NATSConfig struct {
	URL       string `env:"URL,required"`
	CredsFile string `env:"CREDS_FILE" envDefault:""`
}

// SearchConfig groups the request-shape knobs — size caps, cache TTL, and
// the recent-window filter bound. All optional with sane defaults so a
// minimal environment only needs URL + NATS_URL + VALKEY_ADDR.
type SearchConfig struct {
	DocCounts               int           `env:"DOC_COUNTS"                 envDefault:"25"`
	MaxDocCounts            int           `env:"MAX_DOC_COUNTS"             envDefault:"100"`
	RestrictedRoomsCacheTTL time.Duration `env:"RESTRICTED_ROOMS_CACHE_TTL" envDefault:"5m"`
	RecentWindow            time.Duration `env:"RECENT_WINDOW"              envDefault:"8760h"`
	RequestTimeout          time.Duration `env:"REQUEST_TIMEOUT"            envDefault:"10s"`
	UserRoomIndex           string        `env:"USER_ROOM_INDEX"            envDefault:""`
	MetricsAddr             string        `env:"METRICS_ADDR"               envDefault:":9090"`
}

// Config is the root service config. Note that ES and Search share the
// `SEARCH_` env prefix — the fields on the two structs (URL/BACKEND vs
// DOC_COUNTS/MAX_DOC_COUNTS/RECENT_WINDOW/REQUEST_TIMEOUT/…) don't
// collide today, but any new field added to either must be checked
// against the other or moved to a distinct prefix to avoid silent env
// shadowing.
type Config struct {
	SiteID string       `env:"SITE_ID" envDefault:"site-local"`
	ES     ESConfig     `envPrefix:"SEARCH_"`
	Valkey ValkeyConfig `envPrefix:"VALKEY_"`
	NATS   NATSConfig   `envPrefix:"NATS_"`
	Search SearchConfig `envPrefix:"SEARCH_"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		slog.Error("parse config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	tracerShutdown, err := otelutil.InitTracer(ctx, "search-service")
	if err != nil {
		slog.Error("init tracer failed", "error", err)
		os.Exit(1)
	}

	engine, err := searchengine.New(ctx, cfg.ES.Backend, cfg.ES.URL)
	if err != nil {
		slog.Error("search engine connect failed", "error", err)
		os.Exit(1)
	}

	valkey, err := valkeyutil.Connect(ctx, cfg.Valkey.Addr, cfg.Valkey.Password)
	if err != nil {
		slog.Error("valkey connect failed", "error", err)
		os.Exit(1)
	}

	nc, err := natsutil.Connect(cfg.NATS.URL, cfg.NATS.CredsFile)
	if err != nil {
		slog.Error("nats connect failed", "error", err)
		os.Exit(1)
	}

	store := newESStore(engine, cfg.Search.UserRoomIndex)
	cache := newValkeyCache(valkey)
	handler := newHandler(store, cache, handlerConfig{
		DocCounts:               cfg.Search.DocCounts,
		MaxDocCounts:            cfg.Search.MaxDocCounts,
		RestrictedRoomsCacheTTL: cfg.Search.RestrictedRoomsCacheTTL,
		RecentWindow:            cfg.Search.RecentWindow,
		RequestTimeout:          cfg.Search.RequestTimeout,
		UserRoomIndex:           cfg.Search.UserRoomIndex,
	})

	router := natsrouter.New(nc, "search-service")
	router.Use(natsrouter.RequestID())
	router.Use(natsrouter.Recovery())
	router.Use(natsrouter.Logging())
	handler.Register(router)

	// /metrics-only listener. All four timeouts guard against hung
	// scrapers tying up a goroutine indefinitely on an operator-exposed
	// port.
	//
	// Bind synchronously so a port conflict fails startup loudly —
	// otherwise ListenAndServe's error would surface in a goroutine and
	// the service would run happily with no /metrics, silently losing
	// observability. Serve(listener) takes ownership of the listener
	// from here on; Shutdown() closes it.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metricsHandler())
	metricsServer := &http.Server{
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsListener, err := net.Listen("tcp", cfg.Search.MetricsAddr)
	if err != nil {
		slog.Error("metrics server listen failed", "addr", cfg.Search.MetricsAddr, "error", err)
		os.Exit(1)
	}
	go func() {
		slog.Info("metrics server listening", "addr", cfg.Search.MetricsAddr)
		if err := metricsServer.Serve(metricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	slog.Info("search-service running",
		"site", cfg.SiteID,
		"backend", cfg.ES.Backend,
		"valkey", cfg.Valkey.Addr,
	)

	shutdown.Wait(ctx, 25*time.Second,
		// Drain NATS first so no new requests are accepted while metrics
		// keep recording the in-flight ones.
		func(ctx context.Context) error { return nc.Drain() },
		// tracerShutdown + valkey disconnect: internal plumbing, any order.
		func(ctx context.Context) error { return tracerShutdown(ctx) },
		func(_ context.Context) error { valkeyutil.Disconnect(valkey); return nil },
		// Keep /metrics open LAST so Prometheus can scrape the final
		// drain-window observations before the listener closes.
		func(ctx context.Context) error { return metricsServer.Shutdown(ctx) },
	)
}
