// Package server builds the HTTP router with the middleware order from LLD 4.1.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
)

// Deps are the shared dependencies modules need. Modules add their routes in
// Mount as they are built.
type Deps struct {
	Log            *slog.Logger
	AllowedOrigins []string
	Limiter        *ratelimit.Limiter
	GuestPerMinute int
	Health         *health.Handler
	// Auth is optional so probe-only tests can build a router without it.
	AuthService *auth.Service
	Auth        *auth.Handler
}

// New returns the root handler.
func New(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recover(d.Log), httpx.Logger(d.Log), httpx.SecurityHeaders, httpx.CORS(d.AllowedOrigins))

	// Probes are outside the rate limiter so health checks never get 429.
	r.Get("/healthz", d.Health.Healthz)
	r.Get("/readyz", d.Health.Readyz)

	r.Route("/api/v1", func(api chi.Router) {
		if d.Limiter != nil {
			api.Use(d.Limiter.PerIP("guest", d.GuestPerMinute, time.Minute))
		}
		api.Get("/healthz", d.Health.Healthz)
		api.Get("/readyz", d.Health.Readyz)
		if d.Auth != nil {
			api.Group(func(api chi.Router) {
				api.Use(auth.Authenticate(d.AuthService), auth.CSRF)
				d.Auth.Routes(api)
			})
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, httpx.ErrNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.NewError(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed"))
	})
	return r
}
