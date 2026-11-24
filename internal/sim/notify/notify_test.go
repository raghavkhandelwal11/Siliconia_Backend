package notify

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStoreFiltersAndOrders(t *testing.T) {
	s := NewStore(24*time.Hour, 100)
	s.Add(Message{Channel: "sms", To: "+91 98123 45678", Body: "first"})
	s.Add(Message{Channel: "sms", To: "+919000000000", Body: "other"})
	s.Add(Message{Channel: "whatsapp", To: "+919812345678", Body: "second"})

	got := s.List("9812345678")
	if len(got) != 2 || got[0].Body != "second" || got[1].Body != "first" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if all := s.List(""); len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
}

func TestStoreExpiresAndCaps(t *testing.T) {
	s := NewStore(time.Hour, 2)
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.Add(Message{Channel: "sms", To: "1", Body: "old"})
	now = now.Add(2 * time.Hour)
	if n := len(s.List("")); n != 0 {
		t.Fatalf("expired message kept: %d", n)
	}
	for i := 0; i < 3; i++ {
		s.Add(Message{Channel: "sms", To: "1"})
	}
	if n := len(s.List("")); n != 2 {
		t.Fatalf("cap not applied: %d", n)
	}
}

func TestServerRequiresPasswordAndEscapes(t *testing.T) {
	srv := &Server{Store: NewStore(time.Hour, 10), Password: "letmein"}
	h := srv.Handler()

	body, _ := json.Marshal(map[string]string{"channel": "sms", "to": "+919812345678", "template": "otp", "body": "<script>alert(1)</script> code 123456"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/internal/messages", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("ingest status %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test-inbox", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("page without password: %d", w.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/test-inbox?to=9812345678", nil)
	r.SetBasicAuth("tester", "letmein")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("page status %d", w.Code)
	}
	page := w.Body.String()
	if !strings.Contains(page, "code 123456") || strings.Contains(page, "<script>alert(1)") || !strings.Contains(page, "TEST MODE") {
		t.Fatalf("page content not as expected")
	}
}

func TestIngestRejectsInvalid(t *testing.T) {
	h := (&Server{Store: NewStore(time.Hour, 10), Password: "x"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/internal/messages", strings.NewReader(`{"channel":"sms"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}
