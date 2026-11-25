package checkout

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

var (
	errSignIn   = httpx.NewError(http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in to check out")
	errNotBuyer = httpx.NewError(http.StatusForbidden, "OTHER_ROLE", "Checkout is for shoppers. Sign in with a buyer account to buy")
)

// Handler serves addresses, checkout and the buyer's orders.
type Handler struct{ Svc *Service }

// Routes mounts the routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/me/addresses", h.addresses)
	r.Post("/me/addresses", h.createAddress)
	r.Put("/me/addresses/{id}", h.updateAddress)
	r.Delete("/me/addresses/{id}", h.deleteAddress)
	r.Post("/checkout/quote", h.quote)
	r.Post("/orders", h.place)
	r.Get("/orders", h.orders)
	r.Get("/orders/{id}", h.order)
	r.Post("/orders/{id}/pay", h.pay)
}

func buyer(w http.ResponseWriter, r *http.Request) (Buyer, bool) {
	c, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, errSignIn)
		return Buyer{}, false
	}
	if c.EffectiveRole() != auth.RoleBuyer {
		httpx.WriteError(w, r, errNotBuyer)
		return Buyer{}, false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return Buyer{ID: c.Subject, Synthetic: c.Role == auth.RoleAdmin}, true
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) addresses(w http.ResponseWriter, r *http.Request) {
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	list, err := h.Svc.Addresses(r.Context(), b.ID)
	reply(w, r, http.StatusOK, map[string]any{"items": list, "max": MaxAddresses}, err)
}

func (h *Handler) createAddress(w http.ResponseWriter, r *http.Request) {
	var in AddressInput
	b, ok := buyer(w, r)
	if !ok || !decode(w, r, &in) {
		return
	}
	a, err := h.Svc.SaveAddress(r.Context(), b.ID, nil, in)
	reply(w, r, http.StatusCreated, a, err)
}

func (h *Handler) updateAddress(w http.ResponseWriter, r *http.Request) {
	var in AddressInput
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok || !decode(w, r, &in) {
		return
	}
	a, err := h.Svc.SaveAddress(r.Context(), b.ID, &id, in)
	reply(w, r, http.StatusOK, a, err)
}

func (h *Handler) deleteAddress(w http.ResponseWriter, r *http.Request) {
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.DeleteAddress(r.Context(), b.ID, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) quote(w http.ResponseWriter, r *http.Request) {
	var in QuoteInput
	b, ok := buyer(w, r)
	if !ok || !decode(w, r, &in) {
		return
	}
	q, err := h.Svc.Quote(r.Context(), b.ID, in)
	reply(w, r, http.StatusOK, q, err)
}

func (h *Handler) place(w http.ResponseWriter, r *http.Request) {
	var in PlaceInput
	b, ok := buyer(w, r)
	if !ok || !decode(w, r, &in) {
		return
	}
	o, err := h.Svc.Place(r.Context(), b, r.Header.Get("Idempotency-Key"), in)
	if err == nil {
		w.Header().Set("Location", "/api/v1/orders/"+o.ID.String())
	}
	reply(w, r, http.StatusCreated, o, err)
}

func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	list, err := h.Svc.Orders(r.Context(), b.ID)
	reply(w, r, http.StatusOK, map[string]any{"items": list}, err)
}

func (h *Handler) order(w http.ResponseWriter, r *http.Request) {
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := h.Svc.Order(r.Context(), b.ID, id)
	reply(w, r, http.StatusOK, o, err)
}

func (h *Handler) pay(w http.ResponseWriter, r *http.Request) {
	b, ok := buyer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	url, err := h.Svc.StartPayment(r.Context(), b.ID, id)
	reply(w, r, http.StatusOK, map[string]string{"checkoutUrl": url}, err)
}
