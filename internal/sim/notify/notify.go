// Package notify is the sim-notify test inbox (FR-NOT-05, LLD 12): it receives
// simulated SMS, WhatsApp and local email from the API and shows them to
// testers by phone number. It is free and never sends anything for real.
package notify

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Message is one captured message.
type Message struct {
	ID        string            `json:"id"`
	Channel   string            `json:"channel"`
	To        string            `json:"to"`
	Template  string            `json:"template"`
	Vars      map[string]string `json:"vars,omitempty"`
	Body      string            `json:"body"`
	CreatedAt time.Time         `json:"createdAt"`
}

// Store keeps messages in memory for a limited time and count.
type Store struct {
	mu       sync.Mutex
	messages []Message
	ttl      time.Duration
	max      int
	now      func() time.Time
}

// NewStore creates a store keeping at most max messages for ttl.
func NewStore(ttl time.Duration, max int) *Store {
	return &Store{ttl: ttl, max: max, now: time.Now}
}

// Add stores a message and returns it with ID and time set.
func (s *Store) Add(m Message) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = uuid.NewString()
	m.CreatedAt = s.now()
	s.messages = append(s.messages, m)
	s.pruneLocked()
	return m
}

// List returns messages, newest first, optionally filtered by recipient.
func (s *Store) List(to string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	to = normalise(to)
	out := make([]Message, 0, len(s.messages))
	for i := len(s.messages) - 1; i >= 0; i-- {
		m := s.messages[i]
		if to == "" || normalise(m.To) == to || strings.HasSuffix(normalise(m.To), to) {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) pruneLocked() {
	cutoff := s.now().Add(-s.ttl)
	i := 0
	for i < len(s.messages) && s.messages[i].CreatedAt.Before(cutoff) {
		i++
	}
	s.messages = s.messages[i:]
	if over := len(s.messages) - s.max; over > 0 {
		s.messages = s.messages[over:]
	}
}

func normalise(phone string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '@' || r == '.' || (r >= 'a' && r <= 'z') {
			return r
		}
		return -1
	}, strings.ToLower(strings.TrimSpace(phone)))
}

//go:embed inbox.html
var pages embed.FS

var inboxTmpl = template.Must(template.ParseFS(pages, "inbox.html"))

// Server serves the internal ingest endpoint and the protected tester UI.
type Server struct {
	Store    *Store
	Password string
}

// Handler returns the HTTP routes.
//
//	POST /internal/messages          from the API (internal network only; Caddy never routes /internal)
//	GET  /test-inbox                 tester page (password)
//	GET  /test-inbox/api/messages    JSON list (password)
//	GET  /healthz
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/messages", s.ingest)
	mux.HandleFunc("GET /test-inbox", s.auth(s.page))
	mux.HandleFunc("GET /test-inbox/api/messages", s.auth(s.list))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	var m Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&m); err != nil || m.To == "" || m.Channel == "" {
		http.Error(w, `{"code":"VALIDATION_FAILED"}`, http.StatusBadRequest)
		return
	}
	saved := s.Store.Add(m)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": saved.ID})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": s.Store.List(r.URL.Query().Get("to"))})
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("to")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	_ = inboxTmpl.Execute(w, map[string]any{"Query": q, "Messages": s.Store.List(q)})
}

// auth protects tester pages with HTTP basic auth (any user name, shared password).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if s.Password == "" || !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Siliconics test inbox"`)
			http.Error(w, "Test inbox password required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
