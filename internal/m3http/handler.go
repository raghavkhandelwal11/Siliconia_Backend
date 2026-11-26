// Package m3http serves the M3 endpoints: Electrons balances, Electra
// Membership and launch deals (LLD 7.5).
package m3http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/launch"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/membership"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves M3.
type Handler struct {
	Electrons  *electrons.Service
	Membership *membership.Service
	Launches   *launch.Service
	Redis      *redis.Client
}

// PublicRoutes need no session (the auth middleware still reads one if present).
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/launches", h.listLaunches(false))
	r.Get("/launches/{id}/stream", h.stream)
}

// ViewerRoutes read the session when there is one (mounted with Authenticate only).
func (h *Handler) ViewerRoutes(r chi.Router) {
	r.Get("/launches/{id}", h.getLaunch)
}

// Routes need a session and CSRF.
func (h *Handler) Routes(r chi.Router) {
	r.With(auth.RequireAuth).Get("/electrons", h.balance)
	r.With(auth.RequireAuth).Get("/electrons/ledger", h.ledger)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleBuyer))
		r.Get("/membership", h.getMembership)
		r.Post("/membership/purchase", h.purchase)
		r.Patch("/membership", h.patchMembership)
		r.Post("/launches/{id}/reserve", h.reserve)
		r.Post("/launches/{id}/queue", h.joinQueue)
		r.Get("/launches/{id}/queue", h.queueStatus)
		r.Delete("/launch-claims/{id}", h.releaseClaim)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(true))
		r.Get("/admin/launches", h.listLaunches(true))
		r.Post("/admin/launches/preview", h.preview)
		r.Post("/admin/launches", h.createLaunch)
		r.Post("/admin/launches/{id}/schedule", h.schedule)
		r.Post("/admin/launches/{id}/cancel", h.cancelLaunch)
	})
}

func subject(r *http.Request) uuid.UUID {
	c, _ := auth.ClaimsFrom(r.Context())
	return c.Subject
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
	}
	return id, err == nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
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
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, status, v)
}

// ---- Electrons -----------------------------------------------------------------

func (h *Handler) balance(w http.ResponseWriter, r *http.Request) {
	b, err := h.Electrons.GetBalance(r.Context(), subject(r))
	reply(w, r, http.StatusOK, b, err)
}

func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	var before *time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("before must be an RFC 3339 time"))
			return
		}
		before = &t
	}
	list, err := h.Electrons.History(r.Context(), subject(r), before)
	reply(w, r, http.StatusOK, map[string]any{"items": list}, err)
}

// ---- Membership ----------------------------------------------------------------

func (h *Handler) getMembership(w http.ResponseWriter, r *http.Request) {
	v, err := h.Membership.Get(r.Context(), subject(r))
	reply(w, r, http.StatusOK, v, err)
}

func (h *Handler) purchase(w http.ResponseWriter, r *http.Request) {
	in := struct {
		Electrons int32 `json:"electrons"`
		AutoRenew *bool `json:"autoRenew"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	auto := in.AutoRenew == nil || *in.AutoRenew // on by default (LLD 8.8)
	co, err := h.Membership.Purchase(r.Context(), subject(r), in.Electrons, auto)
	reply(w, r, http.StatusOK, co, err)
}

func (h *Handler) patchMembership(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AutoRenew bool `json:"autoRenew"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := h.Membership.SetAutoRenew(r.Context(), subject(r), in.AutoRenew)
	reply(w, r, http.StatusOK, v, err)
}

// ---- Launches ------------------------------------------------------------------

func (h *Handler) listLaunches(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := h.Launches.List(r.Context(), r.URL.Query().Get("status"), admin)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if !admin {
			w.Header().Set("Cache-Control", "public, max-age=5")
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
	}
}

func (h *Handler) getLaunch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var viewer *uuid.UUID
	if c, ok := auth.ClaimsFrom(r.Context()); ok {
		viewer = &c.Subject
	}
	v, err := h.Launches.Get(r.Context(), id, viewer)
	reply(w, r, http.StatusOK, v, err)
}

// stream pushes {status, dealRemaining, held, normalAvailable} whenever it
// changes, plus a heartbeat (LLD 7.5).
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := h.Launches.Get(r.Context(), id, nil)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sub := h.Redis.Subscribe(r.Context(), "launch:"+id.String()+":updates")
	defer sub.Close()
	if _, err := sub.Receive(r.Context()); err != nil {
		httpx.WriteError(w, r, httpx.ErrUnavailable)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	first, _ := json.Marshal(launch.Live{Status: v.Status, DealRemaining: v.DealRemaining, Held: v.Held, NormalAvailable: v.NormalAvailable})
	fmt.Fprintf(w, "retry: 3000\nevent: live\ndata: %s\n\n", first)
	_ = rc.Flush()
	beat := time.NewTicker(20 * time.Second)
	defer beat.Stop()
	done := time.After(15 * time.Minute)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-beat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case m, ok := <-sub.Channel():
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: live\ndata: %s\n\n", m.Payload)
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func (h *Handler) reserve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Qty            int32  `json:"qty"`
		AdmissionToken string `json:"admissionToken"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := h.Launches.Reserve(r.Context(), subject(r), id, in.Qty, in.AdmissionToken)
	reply(w, r, http.StatusCreated, c, err)
}

func (h *Handler) joinQueue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := h.Launches.Join(r.Context(), subject(r), id)
	reply(w, r, http.StatusOK, st, err)
}

func (h *Handler) queueStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := h.Launches.QueueStatus(r.Context(), subject(r), id)
	reply(w, r, http.StatusOK, st, err)
}

func (h *Handler) releaseClaim(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Launches.Release(r.Context(), subject(r), id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	var d launch.Draft
	if !decode(w, r, &d) {
		return
	}
	p, err := h.Launches.Calculate(r.Context(), d)
	reply(w, r, http.StatusOK, p, err)
}

func (h *Handler) createLaunch(w http.ResponseWriter, r *http.Request) {
	var d launch.Draft
	if !decode(w, r, &d) {
		return
	}
	l, err := h.Launches.Create(r.Context(), subject(r), d)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	reply(w, r, http.StatusCreated, map[string]any{"id": l.ID, "dealUnits": l.DealUnits, "dealPricePaise": l.DealPricePaise, "status": l.Status}, nil)
}

func (h *Handler) schedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Launches.Schedule(r.Context(), id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) cancelLaunch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Launches.Cancel(r.Context(), id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
