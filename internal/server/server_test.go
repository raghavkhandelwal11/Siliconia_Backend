package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
)

func newTestServer(dbErr error) http.Handler {
	return New(Deps{
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowedOrigins: []string{"http://localhost:3000"},
		Health: &health.Handler{Version: "test", Mode: "ci", Checks: map[string]health.Pinger{
			"postgres": health.PingFunc(func(context.Context) error { return dbErr }),
			"redis":    health.PingFunc(func(context.Context) error { return nil }),
		}},
	})
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ok" || body["version"] != "test" {
		t.Fatalf("body %v", body)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}

func TestReadyzReportsDownDependency(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(errors.New("down")).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Status != "degraded" || body.Checks["postgres"] != "down" || body.Checks["redis"] != "up" {
		t.Fatalf("body %+v", body)
	}
}

func TestUnknownRouteIsProblemJSON(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if w.Code != 404 || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d type %s", w.Code, w.Header().Get("Content-Type"))
	}
}
