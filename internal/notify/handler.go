package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Handler serves the notification inbox, preferences and the live stream.
type Handler struct {
	Svc   *Service
	Redis *redis.Client
	// StreamFor bounds one stream; the browser reconnects (EventSource).
	StreamFor time.Duration
}

// Routes mounts the routes (after Authenticate and CSRF); any role.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/me/notifications", h.list)
	r.Post("/me/notifications/read", h.read)
	r.Get("/me/notifications/stream", h.stream)
	r.Get("/me/notification-prefs", h.getPrefs)
	r.Put("/me/notification-prefs", h.putPrefs)
}

func subject(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	c, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrAuthRequired)
		return uuid.Nil, false
	}
	return c.Subject, true
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

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	acc, ok := subject(w, r)
	if !ok {
		return
	}
	var before *time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("before must be an RFC 3339 time"))
			return
		}
		before = &t
	}
	items, unread, err := h.Svc.List(r.Context(), acc, before)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	acc, ok := subject(w, r)
	if !ok {
		return
	}
	var in struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.IDs) > 100 {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("At most 100 at a time"))
		return
	}
	unread, err := h.Svc.MarkRead(r.Context(), acc, in.IDs)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"unread": unread})
}

func (h *Handler) getPrefs(w http.ResponseWriter, r *http.Request) {
	acc, ok := subject(w, r)
	if !ok {
		return
	}
	p, err := h.Svc.GetPrefs(r.Context(), acc)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (h *Handler) putPrefs(w http.ResponseWriter, r *http.Request) {
	acc, ok := subject(w, r)
	if !ok {
		return
	}
	var p Prefs
	if !decode(w, r, &p) {
		return
	}
	if err := h.Svc.SavePrefs(r.Context(), acc, p); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// stream sends new notifications as server-sent events (LLD 13): an
// "unread" event first, then "notification" events, with a heartbeat so
// proxies keep the connection open.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	acc, ok := subject(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	sub := h.Redis.Subscribe(r.Context(), Channel(acc))
	defer sub.Close()
	if _, err := sub.Receive(r.Context()); err != nil { // wait for the subscription before reading the count
		httpx.WriteError(w, r, httpx.ErrUnavailable)
		return
	}
	unread, err := h.Svc.Q.CountUnread(r.Context(), acc)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 5000\nevent: unread\ndata: {\"unread\":%d}\n\n", unread)
	_ = rc.Flush()

	limit := h.StreamFor
	if limit == 0 {
		limit = 30 * time.Minute
	}
	done := time.After(limit)
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()
	msgs := sub.Channel()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-beat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case m, ok := <-msgs:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: notification\ndata: %s\n\n", m.Payload)
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
