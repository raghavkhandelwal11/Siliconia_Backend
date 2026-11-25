// Command sim-pay runs the free payment gateway simulator.
package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/pay"
)

func main() {
	log := logx.New("sim-pay")
	addr := env("HTTP_ADDR", ":9001")
	key, secret := os.Getenv("SIM_PAY_API_KEY"), os.Getenv("SIM_PAY_WEBHOOK_SECRET")
	if key == "" || secret == "" {
		log.Error("SIM_PAY_API_KEY and SIM_PAY_WEBHOOK_SECRET are required")
		os.Exit(1)
	}
	dup, _ := strconv.ParseFloat(env("SIM_DUPLICATE_RATE", "0.05"), 64)
	sim := pay.New(pay.Config{
		PublicURL: env("SIM_PAY_PUBLIC_URL", "http://localhost:8080"), APIKey: key,
		WebhookURL: env("SIM_PAY_WEBHOOK_URL", "http://api:8080/api/v1/webhooks/payment"), WebhookSecret: []byte(secret),
		CaptureDelay: 2 * time.Second, PendingDelay: 60 * time.Second, DuplicateRate: dup, Log: log,
	})
	srv := &http.Server{Addr: addr, Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
