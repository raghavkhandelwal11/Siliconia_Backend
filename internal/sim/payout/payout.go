// Package payout is sim-payout, the free bank-transfer simulator (LLD 12):
// account 000000000000 fails as invalid, every other transfer succeeds after
// a delay with a bank reference. Results arrive as signed webhooks.
package payout

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

// Config tunes the simulator.
type Config struct {
	APIKey        string
	WebhookURL    string
	WebhookSecret []byte
	Delay         time.Duration // 30 s in dev
	Log           *slog.Logger
}

// Transfer is a payout to a bank account.
type Transfer struct {
	ID      string `json:"id"`
	Ref     string `json:"ref"`
	Amount  int64  `json:"amount"`
	Status  string `json:"status"` // PROCESSING, PAID, FAILED
	BankRef string `json:"bankRef,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Sim is the simulator.
type Sim struct {
	cfg   Config
	mu    sync.Mutex
	byRef map[string]*Transfer
	wg    sync.WaitGroup
}

// New creates the simulator.
func New(cfg Config) *Sim {
	if cfg.Delay == 0 {
		cfg.Delay = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Sim{cfg: cfg, byRef: map[string]*Transfer{}}
}

// Wait blocks until scheduled results are delivered (tests).
func (s *Sim) Wait() { s.wg.Wait() }

func id(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// Handler serves POST /v1/transfers (bearer key).
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/transfers", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.cfg.APIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var in struct {
			Ref     string `json:"ref"`
			Amount  int64  `json:"amount"`
			Account string `json:"account"`
			IFSC    string `json:"ifsc"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil || in.Ref == "" || in.Amount <= 0 || in.Account == "" {
			http.Error(w, `{"error":"bad_input"}`, http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		t, ok := s.byRef[in.Ref]
		if !ok { // the same ref is the same transfer
			t = &Transfer{ID: id("tr_sim"), Ref: in.Ref, Amount: in.Amount, Status: "PROCESSING"}
			s.byRef[in.Ref] = t
			fail := in.Account == "000000000000"
			s.wg.Add(1)
			time.AfterFunc(s.cfg.Delay, func() {
				defer s.wg.Done()
				s.finish(t, fail)
			})
		}
		out := *t
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

func (s *Sim) finish(t *Transfer, fail bool) {
	s.mu.Lock()
	if fail {
		t.Status, t.Reason = "FAILED", "INVALID_ACCOUNT"
	} else {
		t.Status, t.BankRef = "PAID", "UTR"+strings.ToUpper(id("")[1:13])
	}
	snap := *t
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"id": id("evt_payout"), "type": "transfer." + strings.ToLower(snap.Status), "data": snap})
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(whsig.Header, whsig.Sign(s.cfg.WebhookSecret, body, time.Now()))
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	s.cfg.Log.Error("payout webhook gave up", "ref", snap.Ref)
}
