// Command api serves the Siliconics REST API (/api/v1).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/cart"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/checkout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/issues"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/orders"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/redisx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	paymentprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	shippingprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/returns"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/search"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/seller"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
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

	limiter := ratelimit.New(rdb)
	authSvc, authHandler, err := buildAuth(ctx, cfg, pool, rdb, limiter, log)
	if err != nil {
		log.Error("auth", "err", err)
		os.Exit(1)
	}
	if cfg.ProviderKYC != "sim" {
		log.Error("kyc", "err", "only PROVIDER_KYC=sim exists in this build (paid adapters: paid-later/)")
		os.Exit(1)
	}
	media, err := storage.NewS3(ctx, storage.S3Config{
		Endpoint: cfg.StorageEndpoint, AccessKey: cfg.StorageAccessKey, SecretKey: cfg.StorageSecretKey,
		Bucket: cfg.StorageBucketMedia, Region: cfg.StorageRegion, UseSSL: cfg.StorageUseSSL,
	})
	if err != nil {
		log.Error("storage", "err", err)
		os.Exit(1)
	}
	searchSvc := &search.Service{Pool: pool, Q: authSvc.Q}
	carts := &cart.Service{Pool: pool, Q: authSvc.Q}
	if cfg.ProviderPayment != "sim" {
		log.Error("payment", "err", "only PROVIDER_PAYMENT=sim exists in this build (paid gateways: paid-later/)")
		os.Exit(1)
	}
	payments := &payment.Service{Pool: pool, Q: authSvc.Q, Log: log,
		Provider: paymentprovider.NewSim(cfg.SimPayURL, cfg.SimPayAPIKey, []byte(cfg.SimPayWebhookSecret))}
	if cfg.ProviderShipping != "sim" {
		log.Error("shipping", "err", "only PROVIDER_SHIPPING=sim exists in this build (paid aggregators: paid-later/)")
		os.Exit(1)
	}
	shipments := &shipping.Service{Pool: pool, Q: authSvc.Q, Log: log,
		Provider: shippingprovider.NewSim(cfg.SimShipURL, cfg.SimShipAPIKey, []byte(cfg.SimShipWebhookSecret))}
	returnSvc := &returns.Service{Pool: pool, Q: authSvc.Q, Courier: shipments.Provider, Payments: payments, Log: log}
	shipments.Returns = returnSvc.OnTracking
	checkouts := &checkout.Service{Shipping: shipments, Pool: pool, Q: authSvc.Q, Payments: payments, PublicURL: cfg.PublicBaseURL, Log: log}
	payments.Hooks = map[string]payment.Hook{"ORDER": checkouts.OnPayment}
	products := &product.Service{Pool: pool, Q: authSvc.Q, Store: media, Limiter: limiter, Log: log}
	sellers := &seller.Service{
		Pool: pool, Q: authSvc.Q, Box: authSvc.Box, KYC: kyc.NewSim(cfg.SimKYCURL),
		Limiter: limiter, Messages: authSvc.Messages, Log: log,
	}

	handler := server.New(server.Deps{
		Log:            log,
		AllowedOrigins: cfg.AllowedOrigins,
		Limiter:        limiter,
		AuthService:    authSvc,
		Auth:           authHandler,
		Catalogue:      &catalogue.Handler{Q: store.New(pool)},
		Seller:         &seller.Handler{Svc: sellers},
		Product:        &product.Handler{Svc: products},
		Search:         &search.Handler{Svc: searchSvc},
		PDP:            &pdp.Handler{Svc: &pdp.Service{Q: authSvc.Q, Search: searchSvc, Saved: carts.Saved}},
		Cart:           &cart.Handler{Svc: carts, Limiter: limiter, Secure: cfg.CookieSecure},
		Payment:        &payment.Handler{Svc: payments},
		Checkout:       &checkout.Handler{Svc: checkouts},
		Shipping:       &shipping.Handler{Svc: shipments},
		Returns:        &returns.Handler{Svc: returnSvc},
		Issues:         &issues.Handler{Svc: &issues.Service{Pool: pool, Q: authSvc.Q, Log: log}},
		Orders:         &orders.Handler{Svc: &orders.Service{Pool: pool, Q: authSvc.Q, Payments: payments, Log: log}},
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

// buildAuth assembles sign-in: messaging adapters, token signer, envelope
// encryption for TOTP secrets and, when configured, Google sign-in.
func buildAuth(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, rdb *redis.Client, limiter *ratelimit.Limiter, log *slog.Logger) (*auth.Service, *auth.Handler, error) {
	msgs, err := messaging.NewRouter(cfg.ProviderSMS, cfg.ProviderWhatsApp, cfg.ProviderEmail, cfg.SimNotifyURL, log)
	if err != nil {
		return nil, nil, err
	}
	signer, err := auth.NewSigner(cfg.AuthSigningKey, auth.AccessTTL)
	if err != nil {
		return nil, nil, err
	}
	box, err := crypto.NewBox(cfg.EncryptionMasterKey)
	if err != nil {
		return nil, nil, err
	}
	pepper, err := auth.DecodeKey(cfg.OTPPepper)
	if err != nil {
		return nil, nil, fmt.Errorf("OTP_PEPPER: %w", err)
	}
	svc := &auth.Service{
		Pool: pool, Q: store.New(pool), Redis: rdb, Limiter: limiter, Messages: msgs,
		Signer: signer, Box: box, Pepper: pepper, Log: log,
	}
	var google *auth.Google
	if cfg.GoogleEnabled() {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if google, err = auth.NewGoogle(dctx, cfg.GoogleIssuer, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.PublicBaseURL); err != nil {
			// Phone sign-in keeps working; Google shows as unavailable.
			log.Warn("google sign-in disabled", "err", err)
			google = nil
		}
	}
	return svc, auth.NewHandler(svc, google, cfg.CookieSecure), nil
}
