// Command notify runs the StoneSuite notifications service: a small,
// standalone backend that persists in-app notifications and serves them to
// the frontend bell. It trusts sessions minted by StoneSuite-Backend (same
// JWT secret) rather than handling login itself, and accepts notification
// creation from other StoneSuite services over an internal-secret endpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"stonesuite-notify/config"
	"stonesuite-notify/controllers"
	"stonesuite-notify/database"
	"stonesuite-notify/middleware"
	"stonesuite-notify/notifications"
	"stonesuite-notify/pushsubs"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("CRITICAL: config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("CRITICAL: database: %v", err)
	}
	defer pool.Close()

	if err := database.ApplySchema(ctx, pool); err != nil {
		log.Fatalf("CRITICAL: schema: %v", err)
	}

	if !cfg.EmailConfigured() {
		log.Println("notice: no email provider configured (RESEND_API_KEY or SMTP_HOST) — email channel will be skipped")
	}
	if !cfg.PushConfigured() {
		log.Println("notice: no VAPID keys configured (VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY) — push channel will be skipped")
	}

	store := notifications.NewPGStore(pool)
	pushStore := pushsubs.NewPGStore(pool)
	handler := controllers.NewHandler(store, pushStore, cfg)
	pushHandler := controllers.NewPushHandler(pushStore, cfg.VAPIDPublicKey)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":false,"message":"database unreachable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	requireAuth := middleware.RequireAuth(cfg.JWTSecret)
	requireInternal := middleware.RequireInternalSecret(cfg.InternalServiceSecret)

	mux.Handle("GET /api/notifications/summary", requireAuth(http.HandlerFunc(handler.Summary)))
	mux.Handle("GET /api/notifications", requireAuth(http.HandlerFunc(handler.List)))
	mux.Handle("POST /api/notifications/read-all", requireAuth(http.HandlerFunc(handler.MarkAllRead)))
	mux.Handle("POST /api/notifications/{id}/read", requireAuth(http.HandlerFunc(handler.MarkRead)))
	mux.Handle("POST /api/notifications/internal", requireInternal(http.HandlerFunc(handler.Create)))

	mux.HandleFunc("GET /api/push/vapid-public-key", pushHandler.VAPIDKey)
	mux.Handle("POST /api/push/subscribe", requireAuth(http.HandlerFunc(pushHandler.Subscribe)))
	mux.Handle("DELETE /api/push/subscribe", requireAuth(http.HandlerFunc(pushHandler.Unsubscribe)))

	globalHandler := withLogging(withCORS(mux, cfg.CorsOrigins))

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           globalHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	fmt.Println("===============================================")
	fmt.Println("  StoneSuite Notify service is running!        ")
	fmt.Printf("  Local Endpoint: http://localhost:%s\n", cfg.Port)
	fmt.Println("===============================================")

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("CRITICAL: server: %v", err)
	}
}

// withCORS echoes the request Origin back only if it is in the configured
// allowlist, mirroring StoneSuite-Backend's CORS handling so browser
// requests from the same frontend origins are accepted here too.
func withCORS(next http.Handler, allowedOrigins []string) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Internal-Secret")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLogging logs one line per request: method, path, status, duration.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
