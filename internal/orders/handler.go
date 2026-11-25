package orders

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves seller fulfilment, buyer cancellation and invoices.
type Handler struct{ Svc *Service }

// Routes mounts the routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Get("/seller/orders", h.queue)
		r.Get("/seller/orders/{id}", h.detail)
		r.Post("/seller/sub-orders/{id}/pack", h.pack)
		r.Post("/seller/sub-orders/{id}/cancel", h.sellerCancel)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleBuyer))
		r.Post("/sub-orders/{id}/cancel", h.buyerCancel)
	})
	r.Get("/invoices/{id}", h.invoice)
}

func id(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	v, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
	}
	return v, err == nil
}

func subject(r *http.Request) uuid.UUID {
	c, _ := auth.ClaimsFrom(r.Context())
	return c.Subject
}

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	list, counts, err := h.Svc.Queue(r.Context(), subject(r), r.URL.Query().Get("status"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list, "counts": counts})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	sid, ok := id(w, r)
	if !ok {
		return
	}
	d, err := h.Svc.SellerDetail(r.Context(), subject(r), sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (h *Handler) pack(w http.ResponseWriter, r *http.Request) {
	sid, ok := id(w, r)
	if !ok {
		return
	}
	if err := h.Svc.Pack(r.Context(), subject(r), sid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, role string) {
	sid, ok := id(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		httpx.WriteError(w, r, ErrBadReason)
		return
	}
	if err := h.Svc.Cancel(r.Context(), Actor{ID: subject(r), Role: role}, sid, in.Reason); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sellerCancel(w http.ResponseWriter, r *http.Request) { h.cancel(w, r, "SELLER") }
func (h *Handler) buyerCancel(w http.ResponseWriter, r *http.Request)  { h.cancel(w, r, "BUYER") }

func (h *Handler) invoice(w http.ResponseWriter, r *http.Request) {
	sid, ok := id(w, r)
	if !ok {
		return
	}
	c, _ := auth.ClaimsFrom(r.Context())
	pdf, no, err := h.Svc.Invoice(r.Context(), Viewer{ID: c.Subject, Admin: c.Role == auth.RoleAdmin}, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="invoice-`+sanitize(no)+`.pdf"`)
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(pdf)
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b)
}
