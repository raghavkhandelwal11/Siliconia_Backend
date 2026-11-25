// Command sim-payout runs the free bank-transfer simulator.
package main

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/payout"
)

func main() {
	log := logx.New("sim-payout")
	key, secret := os.Getenv("SIM_PAYOUT_API_KEY"), os.Getenv("SIM_PAYOUT_WEBHOOK_SECRET")
	if key == "" || secret == "" {
		log.Error("SIM_PAYOUT_API_KEY and SIM_PAYOUT_WEBHOOK_SECRET are required")
		os.Exit(1)
	}
	delay, err := time.ParseDuration(env("SIM_PAYOUT_DELAY", "30s"))
	if err != nil {
		log.Error("SIM_PAYOUT_DELAY must be a duration")
		os.Exit(1)
	}
	sim := payout.New(payout.Config{APIKey: key, WebhookURL: env("SIM_PAYOUT_WEBHOOK_URL", "http://api:8080/api/v1/webhooks/payout"),
		WebhookSecret: []byte(secret), Delay: delay, Log: log})
	srv := &http.Server{Addr: env("HTTP_ADDR", ":9003"), Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", srv.Addr)
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
