package seller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves /seller/application and /admin/approvals/sellers.
type Handler struct{ Svc *Service }

// Routes mounts the routes (the caller has applied Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Get("/seller/application", h.get)
		r.Group(func(r chi.Router) {
			r.Use(ownerOnly)
			r.Put("/seller/application", h.save)
			r.Post("/seller/application/pan", h.pan)
			r.Post("/seller/application/bank", h.bank)
			r.Post("/seller/application/submit", h.submit)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(true))
		r.Get("/admin/approvals/sellers", h.queue)
		r.Get("/admin/approvals/sellers/{accountId}", h.detail)
		r.Post("/admin/approvals/sellers/{accountId}/decision", h.decide)
	})
}

// ownerOnly blocks admins who are viewing as a seller from changing or
// submitting an application (LLD 4.2 deny-list: KYC submission).
func ownerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, _ := auth.ClaimsFrom(r.Context()); c.Role != auth.RoleSeller {
			httpx.WriteError(w, r, ErrActingDenied.WithDetail("Admins viewing as a seller cannot change applications"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	if c.Role != auth.RoleSeller {
		// An admin viewing as a seller has no store of their own: show an empty,
		// read-only portal so the seller pages can be previewed (FR-ROLE-07).
		// Nothing is written; every change is still refused by ownerOnly.
		httpx.WriteJSON(w, http.StatusOK, Application{AccountID: c.Subject, Approval: "APPROVED", PhoneVerified: true, Missing: []string{}})
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Get(r.Context(), c.Subject) })
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	var in BusinessInput
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.SaveBusiness(r.Context(), c.Subject, in) })
}

func (h *Handler) pan(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	var in struct {
		PAN string `json:"pan"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.CheckPAN(r.Context(), c.Subject, in.PAN) })
}

func (h *Handler) bank(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	var in struct {
		Account string `json:"account"`
		IFSC    string `json:"ifsc"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.CheckBank(r.Context(), c.Subject, in.Account, in.IFSC) })
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.ClaimsFrom(r.Context())
	var in struct {
		AcceptTerms bool `json:"acceptTerms"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Submit(r.Context(), c.Subject, in.AcceptTerms) })
}

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	states := []string{"SUBMITTED", "UNDER_REVIEW"}
	switch strings.ToLower(r.URL.Query().Get("status")) {
	case "", "pending":
	case "approved":
		states = []string{"APPROVED"}
	case "rejected":
		states = []string{"REJECTED"}
	case "draft":
		states = []string{"DRAFT"}
	default:
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("status is pending, approved, rejected or draft"))
		return
	}
	reply(w, r, func() (any, error) {
		items, err := h.Svc.Queue(r.Context(), states)
		return map[string]any{"applications": items}, err
	})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Detail(r.Context(), id) })
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	c, _ := auth.ClaimsFrom(r.Context())
	var in struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Decision != "APPROVE" && in.Decision != "REJECT" {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("decision is APPROVE or REJECT"))
		return
	}
	actor := Actor{ID: c.Subject, Acting: string(c.Acting), IP: httpx.ClientIP(r)}
	reply(w, r, func() (any, error) {
		return h.Svc.Decide(r.Context(), actor, id, Decision{Approve: in.Decision == "APPROVE", Reason: in.Reason})
	})
}

func accountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "accountId"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, r *http.Request, fn func() (any, error)) {
	v, err := fn()
	if errors.Is(err, pgx.ErrNoRows) {
		err = httpx.ErrNotFound
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}
