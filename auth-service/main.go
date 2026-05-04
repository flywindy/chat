package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nkeys"

	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
	"github.com/hmchangw/chat/pkg/shutdown"
)

type config struct {
	Port           string        `env:"PORT"                  envDefault:"8080"`
	DevMode        bool          `env:"DEV_MODE"              envDefault:"false"`
	AuthSigningKey string        `env:"AUTH_SIGNING_KEY,required"`
	NATSJWTExpiry  time.Duration `env:"NATS_JWT_EXPIRY"       envDefault:"2h"`

	// OIDC settings — required when DEV_MODE is false.
	OIDCIssuerURL string `env:"OIDC_ISSUER_URL"`
	OIDCAudience  string `env:"OIDC_AUDIENCE"`
	TLSSkipVerify bool   `env:"TLS_SKIP_VERIFY"           envDefault:"false"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

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

	signingKP, err := nkeys.FromSeed([]byte(cfg.AuthSigningKey))
	if err != nil {
		return fmt.Errorf("parse signing key: %w", err)
	}

	ctx := context.Background()

	var handler *AuthHandler

	if cfg.DevMode {
		slog.Info("dev mode enabled — OIDC validation disabled")
		handler = NewAuthHandler(nil, signingKP, cfg.NATSJWTExpiry, true)
	} else {
		if cfg.OIDCIssuerURL == "" || cfg.OIDCAudience == "" {
			return fmt.Errorf("OIDC_ISSUER_URL and OIDC_AUDIENCE are required when DEV_MODE is false")
		}

		// Initialize OIDC validator — connects to issuer and fetches JWKS keys.
		oidcValidator, err := pkgoidc.NewValidator(ctx, pkgoidc.Config{
			IssuerURL:     cfg.OIDCIssuerURL,
			Audience:      cfg.OIDCAudience,
			TLSSkipVerify: cfg.TLSSkipVerify,
		})
		if err != nil {
			return fmt.Errorf("create oidc validator: %w", err)
		}
		slog.Info("oidc validator initialized", "issuer", cfg.OIDCIssuerURL)
		handler = NewAuthHandler(oidcValidator, signingKP, cfg.NATSJWTExpiry, false)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware())
	r.Use(accessLogMiddleware())
	r.Use(corsMiddleware())
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
		slog.Info("auth service starting", "addr", addr)
		srvErr <- srv.ListenAndServe()
	}()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		shutdown.Wait(ctx, 25*time.Second, func(ctx context.Context) error {
			slog.Info("shutting down auth service")
			return srv.Shutdown(ctx)
		})
	}()

	err = <-srvErr
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen auth server: %w", err)
	}
	<-shutdownDone

	return nil
}
