// Command sim-notify runs the free test inbox for simulated SMS and WhatsApp.
package main

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/notify"
)

func main() {
	log := logx.New("sim-notify")
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":9005"
	}
	password := os.Getenv("TEST_INBOX_PASSWORD")
	if password == "" {
		log.Error("TEST_INBOX_PASSWORD is required")
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           (&notify.Server{Store: notify.NewStore(24*time.Hour, 5000), Password: password}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
