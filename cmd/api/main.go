// Command api serves the Siliconics REST API (/api/v1).
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/redisx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
)

func main() {
	log := logx.New("api")
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	rdb, err := redisx.Open(ctx, cfg.RedisURL)
	if err != nil {
		log.Error("redis", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()

	handler := server.New(server.Deps{
		Log:            log,
		AllowedOrigins: cfg.AllowedOrigins,
		Limiter:        ratelimit.New(rdb),
		GuestPerMinute: cfg.GuestRateLimitPerMinute,
		Health: &health.Handler{
			Version: cfg.Version,
			Mode:    string(cfg.Mode),
			Checks: map[string]health.Pinger{
				"postgres": health.PingFunc(pool.Ping),
				"redis":    health.PingFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
			},
		},
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: SSE streams stay open; handlers set their own deadlines.
	}

	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "mode", cfg.Mode, "version", cfg.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	log.Info("stopped")
}
