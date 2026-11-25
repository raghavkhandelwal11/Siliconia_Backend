package cart

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
)

// Handler serves /cart and /wishlist.
type Handler struct {
	Svc     *Service
	Limiter *ratelimit.Limiter
	Secure  bool // cookies over HTTPS only
}

// Routes mounts the routes (after Authenticate and CSRF).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/cart", h.get)
	r.Post("/cart/merge", h.get) // explicit merge after sign-in; same result as GET
	r.Put("/cart/items/{variantId}", h.put)
	r.Delete("/cart/items/{variantId}", h.del)
	r.Get("/wishlist", h.wishlist)
	r.Put("/wishlist/{variantId}", h.save)
	r.Delete("/wishlist/{variantId}", h.unsave)
}

const deviceTTL = 60 * 24 * time.Hour

// owner works out whose cart this request is for. Guests get a device
// cookie (and a CSRF token for their next write) the first time they write.
func (h *Handler) owner(w http.ResponseWriter, r *http.Request, create bool) (Owner, string, error) {
	device := ""
	if ck, err := r.Cookie(auth.CookieDevice); err == nil && len(ck.Value) >= 20 && len(ck.Value) <= 64 {
		device = ck.Value
	}
	if c, ok := auth.ClaimsFrom(r.Context()); ok {
		if c.EffectiveRole() != auth.RoleBuyer {
			return Owner{}, "", ErrNotBuyer
		}
		id := c.Subject
		return Owner{AccountID: &id}, device, nil
	}
	if device == "" && create {
		device = NewDeviceID()
		http.SetCookie(w, &http.Cookie{Name: auth.CookieDevice, Value: device, Path: "/", HttpOnly: true, Secure: h.Secure,
			SameSite: http.SameSiteLaxMode, MaxAge: int(deviceTTL.Seconds())})
		if _, err := r.Cookie(auth.CookieCSRF); err != nil {
			http.SetCookie(w, &http.Cookie{Name: auth.CookieCSRF, Value: NewDeviceID(), Path: "/", Secure: h.Secure,
				SameSite: http.SameSiteLaxMode, MaxAge: int(deviceTTL.Seconds())})
		}
	}
	return Owner{DeviceID: device}, "", nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	o, guest, err := h.owner(w, r, false)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	c, clearGuest, err := h.Svc.Get(r.Context(), o, guest)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if clearGuest {
		http.SetCookie(w, &http.Cookie{Name: auth.CookieDevice, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.Secure, SameSite: http.SameSiteLaxMode})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	id, ok := variant(w, r)
	var in struct {
		Qty int32 `json:"qty"`
	}
	if !ok || !decode(w, r, &in) {
		return
	}
	h.write(w, r, id, in.Qty)
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request) {
	if id, ok := variant(w, r); ok {
		h.write(w, r, id, 0)
	}
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, id uuid.UUID, qty int32) {
	o, _, err := h.owner(w, r, true)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if h.Limiter != nil {
		subject := o.DeviceID
		if o.AccountID != nil {
			subject = o.AccountID.String()
		}
		if res, err := h.Limiter.Allow(r.Context(), "cart-write", subject, 60, time.Minute); err == nil && !res.Allowed {
			httpx.WriteError(w, r, httpx.ErrRateLimited)
			return
		}
	}
	c, err := h.Svc.SetQty(r.Context(), o, id, qty)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) buyer(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	c, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, ErrSignInToSave)
		return uuid.Nil, false
	}
	if c.EffectiveRole() != auth.RoleBuyer {
		httpx.WriteError(w, r, ErrNotBuyer)
		return uuid.Nil, false
	}
	return c.Subject, true
}

func (h *Handler) wishlist(w http.ResponseWriter, r *http.Request) {
	id, ok := h.buyer(w, r)
	if !ok {
		return
	}
	items, err := h.Svc.Wishlist(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	acc, ok := h.buyer(w, r)
	v, ok2 := variant(w, r)
	if !ok || !ok2 {
		return
	}
	if err := h.Svc.Save(r.Context(), acc, v); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unsave(w http.ResponseWriter, r *http.Request) {
	acc, ok := h.buyer(w, r)
	v, ok2 := variant(w, r)
	if !ok || !ok2 {
		return
	}
	if err := h.Svc.Unsave(r.Context(), acc, v); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func variant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "variantId"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}
