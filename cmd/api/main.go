// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Ref: oga9-engineering/sections/08-job-queue-asynq.md
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/config"
	"github.com/jedafe/oga9/internal/handler"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/middleware"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.GlitchTipDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{Dsn: cfg.GlitchTipDSN, Environment: cfg.Env}); err != nil {
			log.Printf("glitchtip init failed (continuing without): %v", err)
		}
		defer sentry.Flush(2 * time.Second)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()

	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{cfg.ValkeyAddr}})
	if err != nil {
		log.Fatalf("valkey: %v", err)
	}
	defer vk.Close()
	_ = vk

	// Asynq server + scheduler (3-tier: critical/default/low — §08)
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.ValkeyAddr},
		asynq.Config{Queues: map[string]int{"critical": 6, "default": 3, "low": 1}},
	)
	mux := asynq.NewServeMux()
	go func() {
		if err := srv.Run(mux); err != nil {
			log.Printf("asynq server: %v", err)
		}
	}()

	scheduler := asynq.NewScheduler(
		asynq.RedisClientOpt{Addr: cfg.ValkeyAddr},
		&asynq.SchedulerOpts{},
	)
	go func() {
		if err := scheduler.Run(); err != nil {
			log.Printf("asynq scheduler: %v", err)
		}
	}()

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	// §02 identity: auth service + handlers
	mail := mailer.New(cfg.SMTPHost, cfg.SMTPPort)
	authSvc := auth.NewService(pool, cfg.JWTSecret, 24*time.Hour)
	authH := handler.NewAuthHandler(authSvc, mail, cfg.JWTSecret)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":{"service":"oga9","version":"0.0.1"}}`))
		})

		// public auth
		r.Post("/auth/register", authH.Register)
		r.Post("/auth/login", authH.Login)
		r.Post("/auth/verify/{token}", authH.VerifyEmail)
		r.Post("/auth/password/forgot", authH.ForgotPassword)
		r.Post("/auth/password/reset", authH.ResetPassword)

		// authed
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(authSvc, cfg.JWTSecret))
			r.Post("/auth/logout", authH.Logout)
			r.Get("/me", authH.Me)
			r.Get("/me/sessions", authH.Sessions)
			r.Delete("/me/sessions/{session_id}", authH.RevokeSession)
		})
	})

	httpSrv := &http.Server{Addr: ":" + itoa(cfg.APIPort), Handler: r}
	go func() {
		log.Printf("api listening on :%d (env=%s)", cfg.APIPort, cfg.Env)
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	srv.Shutdown()
	scheduler.Shutdown()
	httpSrv.Shutdown(shutdownCtx)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
