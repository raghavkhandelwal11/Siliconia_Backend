package shipping

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
)

// Handler serves the shipping webhook, PIN checks and seller shipping.
type Handler struct{ Svc *Service }

var pinRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// PublicRoutes mounts routes that need no session.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/webhooks/shipping", h.webhook) // signed: no session, no CSRF
	r.Get("/serviceability/{pincode}", h.serviceability)
}

// Routes mounts seller routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Post("/seller/sub-orders/{id}/ship", h.ship)
		r.Get("/seller/sub-orders/{id}/label", h.label)
	})
}

func (h *Handler) serviceability(w http.ResponseWriter, r *http.Request) {
	pin := chi.URLParam(r, "pincode")
	if !pinRE.MatchString(pin) {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Enter a 6-digit PIN code"))
		return
	}
	sv := h.Svc.Check(r.Context(), pin)
	if !sv.Known {
		httpx.WriteError(w, r, ErrShippingDown)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=600")
	httpx.WriteJSON(w, http.StatusOK, sv)
}

func (h *Handler) ship(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	var in struct {
		WeightGrams int `json:"weightGrams"`
	}
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if dec.Decode(&in) != nil {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
			return
		}
	}
	sh, err := h.Svc.Ship(r.Context(), c.Subject, id, in.WeightGrams)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sh)
}

func (h *Handler) label(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	pdf, awb, err := h.Svc.Label(r.Context(), c.Subject, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="label-`+awb+`.pdf"`)
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(pdf)
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.HandleWebhook(r); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		h.Svc.Log.Warn("shipping webhook rejected", "err", err)
		status := http.StatusInternalServerError // the provider retries
		if errors.Is(err, whsig.ErrBadSignature) || errors.Is(err, provider.ErrMalformed) {
			status = http.StatusBadRequest
		}
		http.Error(w, `{"error":"`+http.StatusText(status)+`"}`, status)
		return
	}
	w.WriteHeader(http.StatusOK)
}
