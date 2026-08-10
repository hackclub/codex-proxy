package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hackclub/codex-proxy/internal/admin"
	"github.com/hackclub/codex-proxy/internal/config"
	"github.com/hackclub/codex-proxy/internal/db"
	"github.com/hackclub/codex-proxy/internal/proxy"
	"github.com/hackclub/codex-proxy/internal/ratelimit"
	"github.com/hackclub/codex-proxy/internal/secretbox"
	"github.com/hackclub/codex-proxy/internal/server"
	"github.com/hackclub/codex-proxy/internal/token"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	box, err := secretbox.New(cfg.TokenEncryptionKey)
	if err != nil {
		log.Fatalf("token encryption: %v", err)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := db.Connect(startupCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		cancelStartup()
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(startupCtx, pool); err != nil {
		cancelStartup()
		log.Fatalf("migrations: %v", err)
	}
	cancelStartup()

	store := db.NewStore(pool, box)
	tokenManager := token.NewManager(store, token.Config{
		OAuthURL:         cfg.OAuthTokenURL,
		ClientID:         cfg.OAuthClientID,
		RefreshBuffer:    cfg.TokenRefreshBuffer,
		FailureThreshold: cfg.TokenFailureThreshold,
	})
	limiter := ratelimit.New()
	proxyHandler := proxy.NewHandler(store, tokenManager, limiter, proxy.HandlerConfig{
		UpstreamURL:         cfg.UpstreamURL,
		DefaultInstructions: cfg.DefaultInstructions,
		MaxRequestBytes:     cfg.RequestBodyLimit,
		HTTPClient:          &http.Client{Timeout: cfg.UpstreamTimeout},
	})
	adminHandler := admin.NewHandler(store, tokenManager, cfg.AdminUser, cfg.AdminPass)
	app := server.New(store, proxyHandler, adminHandler.Routes())

	janitorCtx, stopJanitors := context.WithCancel(context.Background())
	defer stopJanitors()
	go server.RunJanitors(janitorCtx, store, limiter, cfg.RequestLogTTL)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("codex-proxy listening on %s", httpServer.Addr)
		serverErrors <- httpServer.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case signal := <-signals:
		log.Printf("received %s; shutting down", signal)
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server: %v", err)
		}
	}

	stopJanitors()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
}
