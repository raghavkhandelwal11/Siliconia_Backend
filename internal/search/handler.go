package search

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves GET /products and GET /products/suggest.
type Handler struct{ Svc *Service }

// Routes mounts the public listing routes.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/products", h.list)
	r.Get("/products/suggest", h.suggest)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, err := ParseParams(r.URL.Query())
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail(err.Error()))
		return
	}
	res, err := h.Svc.Search(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len([]rune(q)) > 60 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"suggestions": []Suggestion{}})
		return
	}
	s, err := h.Svc.Suggest(r.Context(), q)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"suggestions": s})
}
