package product

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/imaging"
)

// Handler serves /seller/products, /seller/uploads and /media.
type Handler struct{ Svc *Service }

// PublicRoutes mounts routes that need no session (media).
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/media/*", func(w http.ResponseWriter, r *http.Request) { h.Svc.ServeMedia(w, r, chi.URLParam(r, "*")) })
}

// Routes mounts the seller routes (the caller applied Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller), sellerOnly)
		r.Get("/seller/products", h.list)
		r.Post("/seller/products", h.create)
		r.Get("/seller/products/{id}", h.get)
		r.Put("/seller/products/{id}", h.update)
		r.Put("/seller/products/{id}/variants", h.variants)
		r.Put("/seller/products/{id}/images", h.images)
		r.Post("/seller/products/{id}/status", h.status)
		r.Post("/seller/uploads/images", h.upload)
	})
}

// sellerOnly keeps admins viewing as a seller out: they have no store.
func sellerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admins viewing as a seller may look (their own, empty, catalogue) but not change anything.
		if c, _ := auth.ClaimsFrom(r.Context()); c.Role != auth.RoleSeller && r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.WriteError(w, r, httpx.NewError(http.StatusForbidden, "SWITCH_BACK_REQUIRED", "Admins viewing as a seller have no products of their own"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func seller(r *http.Request) uuid.UUID { c, _ := auth.ClaimsFrom(r.Context()); return c.Subject }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	reply(w, r, func() (any, error) {
		items, err := h.Svc.List(r.Context(), seller(r))
		return map[string]any{"products": items}, err
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Create(r.Context(), seller(r), in) })
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := productID(w, r)
	if !ok {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Get(r.Context(), seller(r), id) })
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := productID(w, r)
	var in UpdateInput
	if !ok || !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.Update(r.Context(), seller(r), id, in) })
}

func (h *Handler) variants(w http.ResponseWriter, r *http.Request) {
	id, ok := productID(w, r)
	var in struct {
		Variants []VariantInput `json:"variants"`
	}
	if !ok || !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.SaveVariants(r.Context(), seller(r), id, in.Variants) })
}

func (h *Handler) images(w http.ResponseWriter, r *http.Request) {
	id, ok := productID(w, r)
	var in struct {
		Images []ImageInput `json:"images"`
	}
	if !ok || !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.SaveImages(r.Context(), seller(r), id, in.Images) })
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	id, ok := productID(w, r)
	var in struct {
		Status string `json:"status"`
	}
	if !ok || !decode(w, r, &in) {
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.SetStatus(r.Context(), seller(r), id, in.Status) })
}

// upload takes one image as multipart field "file".
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, imaging.MaxBytes+64<<10)
	file, _, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", "The image is larger than 8 MB"))
			return
		}
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Send the image as form field \"file\""))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, imaging.MaxBytes+1))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The upload could not be read"))
		return
	}
	reply(w, r, func() (any, error) { return h.Svc.UploadImage(r.Context(), seller(r), data) })
}

func productID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, r *http.Request, fn func() (any, error)) {
	v, err := fn()
	var specErr *SpecError
	switch {
	case errors.As(err, &specErr):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"type": "https://siliconics.in/errors/specs-invalid", "title": "Some specifications are not valid",
			"status": http.StatusUnprocessableEntity, "code": "SPECS_INVALID", "fields": specErr.Fields,
			"requestId": httpx.RequestIDFrom(r.Context()),
		})
		return
	case errors.Is(err, pgx.ErrNoRows):
		err = httpx.ErrNotFound
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}
