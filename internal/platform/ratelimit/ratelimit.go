// Package ratelimit implements a Redis sliding-window rate limiter (LLD 9.5,
// TRS NFR-RATE-02) and HTTP middleware that answers 429 with Retry-After.
package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// slidingWindow counts the current fixed window plus a weighted share of the
// previous one, then increments if under the limit. Returns {allowed, retryAfterMs}.
var slidingWindow = redis.NewScript(`
local cur = tonumber(redis.call("GET", KEYS[1]) or "0")
local prev = tonumber(redis.call("GET", KEYS[2]) or "0")
local limit = tonumber(ARGV[1])
local windowMs = tonumber(ARGV[2])
local elapsedMs = tonumber(ARGV[3])
local weighted = prev * ((windowMs - elapsedMs) / windowMs) + cur
if weighted + 1 > limit then
  return {0, windowMs - elapsedMs}
end
redis.call("INCR", KEYS[1])
redis.call("PEXPIRE", KEYS[1], windowMs * 2)
return {1, 0}`)

// Limiter checks limits for a named group, e.g. "otp-send" or "guest".
type Limiter struct {
	client *redis.Client
	now    func() time.Time
}

// New creates a limiter.
func New(c *redis.Client) *Limiter { return &Limiter{client: c, now: time.Now} }

// Result is the outcome of one check.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Allow records one hit for subject in group if it is under limit per window.
func (l *Limiter) Allow(ctx context.Context, group, subject string, limit int, window time.Duration) (Result, error) {
	now := l.now()
	winMs := window.Milliseconds()
	idx := now.UnixMilli() / winMs
	elapsed := now.UnixMilli() % winMs
	cur := fmt.Sprintf("rl:%s:%s:%d", group, subject, idx)
	prev := fmt.Sprintf("rl:%s:%s:%d", group, subject, idx-1)
	vals, err := slidingWindow.Run(ctx, l.client, []string{cur, prev}, limit, winMs, elapsed).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: %w", err)
	}
	return Result{Allowed: vals[0] == 1, RetryAfter: time.Duration(vals[1]) * time.Millisecond}, nil
}

// PerIP limits every request by client IP. If Redis is down it fails open and
// logs nothing here; readiness checks report the outage instead.
func (l *Limiter) PerIP(group string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res, err := l.Allow(r.Context(), group, httpx.ClientIP(r), limit, window)
			if err == nil && !res.Allowed {
				secs := int(res.RetryAfter.Seconds())
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				httpx.WriteError(w, r, httpx.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
