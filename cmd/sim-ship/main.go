// Command sim-ship runs the free shipping aggregator simulator.
package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/ship"
)

func main() {
	log := logx.New("sim-ship")
	addr := env("HTTP_ADDR", ":9002")
	key, secret := os.Getenv("SIM_SHIP_API_KEY"), os.Getenv("SIM_SHIP_WEBHOOK_SECRET")
	if key == "" || secret == "" {
		log.Error("SIM_SHIP_API_KEY and SIM_SHIP_WEBHOOK_SECRET are required")
		os.Exit(1)
	}
	step, err := time.ParseDuration(env("SIM_SHIP_SPEED", "1m"))
	if err != nil || step < time.Second {
		log.Error("SIM_SHIP_SPEED must be a duration of at least 1s, e.g. 1m")
		os.Exit(1)
	}
	dup, _ := strconv.ParseFloat(env("SIM_DUPLICATE_RATE", "0.05"), 64)
	sim := ship.New(ship.Config{
		APIKey: key, WebhookURL: env("SIM_SHIP_WEBHOOK_URL", "http://api:8080/api/v1/webhooks/shipping"),
		WebhookSecret: []byte(secret), Step: step, DuplicateRate: dup, Log: log,
	})
	srv := &http.Server{Addr: addr, Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", addr, "step", step.String())
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
