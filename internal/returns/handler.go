package returns

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves buyer returns and the seller's return queue.
type Handler struct{ Svc *Service }

// Routes mounts the routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleBuyer))
		r.Post("/order-items/{id}/returns", h.create)
		r.Get("/returns", h.buyerList)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Get("/seller/returns", h.sellerList)
		r.Post("/seller/returns/{id}/decision", h.decide)
	})
}

func subject(r *http.Request) uuid.UUID {
	c, _ := auth.ClaimsFrom(r.Context())
	return c.Subject
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	var in Request
	if !decode(w, r, &in) {
		return
	}
	v, err := h.Svc.Create(r.Context(), subject(r), id, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

func (h *Handler) buyerList(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.BuyerList(r.Context(), subject(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) sellerList(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.SellerList(r.Context(), subject(r), r.URL.Query().Get("status"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	var in struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := h.Svc.Decide(r.Context(), subject(r), id, in.Approve, in.Reason); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
