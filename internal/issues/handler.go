package issues

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves buyer issues and the support queue.
type Handler struct{ Svc *Service }

// Routes mounts the routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleBuyer))
		r.Post("/issues", h.create)
		r.Get("/issues", h.list(false))
		r.Get("/issues/{id}", h.get(false))
		r.Post("/issues/{id}/messages", h.reply(false))
		r.Post("/issues/{id}/reopen", h.reopen)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(true)) // support works as an admin with the authenticator step
		r.Get("/admin/issues", h.list(true))
		r.Get("/admin/issues/{id}", h.get(true))
		r.Post("/admin/issues/{id}/messages", h.reply(true))
	})
}

func viewer(r *http.Request, support bool) Viewer {
	c, _ := auth.ClaimsFrom(r.Context())
	return Viewer{ID: c.Subject, Support: support}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
	}
	return id, err == nil
}

func reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in Raise
	if !decode(w, r, &in) {
		return
	}
	is, err := h.Svc.Create(r.Context(), viewer(r, false).ID, in)
	reply(w, r, http.StatusCreated, is, err)
}

func (h *Handler) list(support bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := h.Svc.List(r.Context(), viewer(r, support), r.URL.Query().Get("status"))
		reply(w, r, http.StatusOK, map[string]any{"items": list}, err)
	}
}

func (h *Handler) get(support bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		is, err := h.Svc.Get(r.Context(), viewer(r, support), id)
		reply(w, r, http.StatusOK, is, err)
	}
}

func (h *Handler) reply(support bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Body   string `json:"body"`
			Status string `json:"status"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !support && in.Status != "" {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Buyers cannot set the status"))
			return
		}
		is, err := h.Svc.Reply(r.Context(), viewer(r, support), id, in.Body, in.Status)
		reply(w, r, http.StatusOK, is, err)
	}
}

func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	is, err := h.Svc.Reopen(r.Context(), viewer(r, false).ID, id)
	reply(w, r, http.StatusOK, is, err)
}
