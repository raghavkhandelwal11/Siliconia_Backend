package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorProblemJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	WriteError(w, r, ErrNotFound.WithDetail("no such product"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type = %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "NOT_FOUND" || p.Type != "https://siliconics.in/errors/not-found" || p.Detail != "no such product" {
		t.Fatalf("unexpected problem: %+v", p)
	}
}

func TestWriteErrorHidesUnknownErrors(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("db password is hunter2"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if body := w.Body.String(); contains(body, "hunter2") {
		t.Fatalf("internal error leaked: %s", body)
	}
}

func TestRecoverAndRequestID(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := RequestID(Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	h := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for origin, want := range map[string]string{"http://localhost:3000": "http://localhost:3000", "https://evil.example": ""} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Fatalf("origin %s: got %q want %q", origin, got, want)
		}
	}
}

func TestClientIPTrustsProxyOnly(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:41000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 172.18.0.5")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("got %s", got)
	}
	r.RemoteAddr = "198.51.100.7:5000"
	if got := ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("public caller spoofed XFF: got %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
