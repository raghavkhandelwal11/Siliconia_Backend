// Package health serves liveness and readiness probes (LLD 7.8).
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Pinger is anything that can be checked for readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingFunc adapts a function to Pinger.
type PingFunc func(ctx context.Context) error

// Ping implements Pinger.
func (f PingFunc) Ping(ctx context.Context) error { return f(ctx) }

// Handler serves /healthz and /readyz.
type Handler struct {
	Version string
	Mode    string
	Checks  map[string]Pinger
}

// Healthz reports that the process is up.
func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.Version, "mode": h.Mode})
}

// Readyz checks every dependency and returns 503 if any fails.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result := map[string]string{}
	status := http.StatusOK
	for name, c := range h.Checks {
		if err := c.Ping(ctx); err != nil {
			result[name] = "down"
			status = http.StatusServiceUnavailable
		} else {
			result[name] = "up"
		}
	}
	overall := "ok"
	if status != http.StatusOK {
		overall = "degraded"
	}
	httpx.WriteJSON(w, status, map[string]any{"status": overall, "checks": result})
}
