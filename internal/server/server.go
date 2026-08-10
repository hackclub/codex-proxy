package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/hackclub/codex-proxy/internal/db"
	"github.com/hackclub/codex-proxy/internal/ratelimit"
)

type App struct {
	Handler http.Handler
	store   *db.Store
}

func New(store *db.Store, proxyHandler, adminHandler http.Handler) *App {
	mux := http.NewServeMux()
	app := &App{store: store}
	mux.Handle("POST /v1/responses", proxyHandler)
	mux.Handle("/admin", adminHandler)
	mux.Handle("/admin/", adminHandler)
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("GET /{$}", app.index)
	app.Handler = requestLogger(mux)
	return app
}

func (a *App) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name": "codex-proxy",
		"endpoints": map[string]string{
			"responses": "POST /v1/responses",
			"health":    "GET /health",
			"admin":     "GET /admin",
		},
	})
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		writeHealth(w, http.StatusServiceUnavailable, false, 0, "database unavailable")
		return
	}
	count, err := a.store.HealthyTokenCount(ctx)
	if err != nil {
		writeHealth(w, http.StatusServiceUnavailable, false, 0, "token pool unavailable")
		return
	}
	if count == 0 {
		writeHealth(w, http.StatusServiceUnavailable, false, 0, "no healthy tokens")
		return
	}
	writeHealth(w, http.StatusOK, true, count, "")
}

func writeHealth(w http.ResponseWriter, status int, healthy bool, tokenCount int64, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy":        healthy,
		"healthy_tokens": tokenCount,
		"message":        message,
	})
}

func RunJanitors(ctx context.Context, store *db.Store, limiter *ratelimit.Limiter, requestLogTTL time.Duration) {
	requestTicker := time.NewTicker(time.Hour)
	limiterTicker := time.NewTicker(10 * time.Minute)
	defer requestTicker.Stop()
	defer limiterTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-requestTicker.C:
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			removed, err := store.CleanupRequests(cleanupCtx, time.Now().Add(-requestLogTTL))
			cancel()
			if err != nil {
				log.Printf("request log cleanup: %v", err)
			} else if removed > 0 {
				log.Printf("removed %d expired request logs", removed)
			}
		case <-limiterTicker.C:
			limiter.Cleanup(time.Hour)
		}
	}
}

type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *loggingWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *loggingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		writer := &loggingWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, status, time.Since(started).Truncate(time.Millisecond))
	})
}
