// Command sim-kyc runs the free KYC simulator (PAN check and penny-drop).
package main

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/kyc"
)

func main() {
	log := logx.New("sim-kyc")
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":9004"
	}
	srv := &http.Server{Addr: addr, Handler: kyc.Server{}.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
