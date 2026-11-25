// Package server builds the HTTP router with the middleware order from LLD 4.1.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/cart"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/checkout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/issues"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/orders"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/returns"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/search"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/seller"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/shipping"
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
	Catalogue   *catalogue.Handler
	Seller      *seller.Handler
	Product     *product.Handler
	Search      *search.Handler
	PDP         *pdp.Handler
	Cart        *cart.Handler
	Payment     *payment.Handler
	Checkout    *checkout.Handler
	Shipping    *shipping.Handler
	Orders      *orders.Handler
	Returns     *returns.Handler
	Issues      *issues.Handler
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
		if d.Catalogue != nil {
			d.Catalogue.Routes(api)
		}
		if d.Product != nil {
			d.Product.PublicRoutes(api)
		}
		if d.Search != nil {
			d.Search.Routes(api)
		}
		if d.Payment != nil {
			d.Payment.Routes(api) // signed webhooks: no session, no CSRF
		}
		if d.Shipping != nil {
			d.Shipping.PublicRoutes(api)
		}
		if d.PDP != nil {
			d.PDP.PublicRoutes(api)
			// The live block works for everyone and reads the session when there is one.
			api.Group(func(g chi.Router) {
				if d.AuthService != nil {
					g.Use(auth.Authenticate(d.AuthService))
				}
				d.PDP.ViewerRoutes(g)
			})
		}
		if d.Auth != nil {
			api.Group(func(api chi.Router) {
				api.Use(auth.Authenticate(d.AuthService), auth.CSRF)
				d.Auth.Routes(api)
				if d.Seller != nil {
					d.Seller.Routes(api)
				}
				if d.Product != nil {
					d.Product.Routes(api)
				}
				if d.Cart != nil {
					d.Cart.Routes(api)
				}
				if d.Checkout != nil {
					d.Checkout.Routes(api)
				}
				if d.Shipping != nil {
					d.Shipping.Routes(api)
				}
				if d.Orders != nil {
					d.Orders.Routes(api)
				}
				if d.Returns != nil {
					d.Returns.Routes(api)
				}
				if d.Issues != nil {
					d.Issues.Routes(api)
				}

			})
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, httpx.ErrNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.NewError(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed"))
	})
	return r
}
