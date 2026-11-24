package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newLimiter(t *testing.T) (*Limiter, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	l := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestAllowUpToLimitThenBlock(t *testing.T) {
	l, _ := newLimiter(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := l.Allow(ctx, "otp-send", "+919999999999", 3, 15*time.Minute)
		if err != nil || !res.Allowed {
			t.Fatalf("hit %d: allowed=%v err=%v", i+1, res.Allowed, err)
		}
	}
	res, err := l.Allow(ctx, "otp-send", "+919999999999", 3, 15*time.Minute)
	if err != nil || res.Allowed || res.RetryAfter <= 0 {
		t.Fatalf("4th hit should be blocked with retry-after: %+v err=%v", res, err)
	}
}

func TestSubjectsAreIndependent(t *testing.T) {
	l, _ := newLimiter(t)
	ctx := context.Background()
	if res, _ := l.Allow(ctx, "g", "a", 1, time.Minute); !res.Allowed {
		t.Fatal("a first hit blocked")
	}
	if res, _ := l.Allow(ctx, "g", "b", 1, time.Minute); !res.Allowed {
		t.Fatal("b blocked by a's hit")
	}
}

func TestPreviousWindowIsWeighted(t *testing.T) {
	l, now := newLimiter(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = l.Allow(ctx, "g", "ip", 10, time.Minute)
	}
	// Just after the window rolls over, almost the whole previous window still counts.
	*now = now.Add(61 * time.Second)
	if res, _ := l.Allow(ctx, "g", "ip", 10, time.Minute); res.Allowed {
		t.Fatal("should still be limited right after the window boundary")
	}
	// Two windows later the old hits no longer count.
	*now = now.Add(2 * time.Minute)
	if res, _ := l.Allow(ctx, "g", "ip", 10, time.Minute); !res.Allowed {
		t.Fatal("should be allowed after the previous window expires")
	}
}

func TestPerIPMiddlewareReturns429(t *testing.T) {
	l, _ := newLimiter(t)
	h := l.PerIP("guest", 1, time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for i, want := range []int{204, 429} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "198.51.100.1:1234"
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("request %d: status %d want %d", i+1, w.Code, want)
		}
		if want == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}
}
