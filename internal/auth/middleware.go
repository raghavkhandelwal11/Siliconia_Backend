package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"slices"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
)

// Cookie names (LLD 4.2).
const (
	CookieAccess  = "sx_at"
	CookieRefresh = "sx_rt"
	CookieCSRF    = "sx_csrf"
	HeaderCSRF    = "X-CSRF-Token"
	refreshPath   = "/api/v1/auth"
)

type ctxKey int

const claimsKey ctxKey = iota

// ClaimsFrom returns the signed-in user's claims, if any.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey).(Claims)
	return c, ok
}

// WithClaims stores claims in a context (used by tests and the middleware).
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}

// Authenticate reads the access cookie. Missing, expired or revoked tokens
// leave the request anonymous; routes that need a user return 401, and the web
// app then calls /auth/refresh.
func Authenticate(s *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ck, err := r.Cookie(CookieAccess); err == nil && ck.Value != "" {
				if c, err := s.Signer.Verify(ck.Value); err == nil && !s.IsDenied(r.Context(), c.SessionID) {
					r = r.WithContext(WithClaims(r.Context(), c))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRF requires the double-submit token on state-changing requests that carry
// session cookies (LLD 4.2). Requests without cookies (sign-in, webhooks) pass.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		_, errAT := r.Cookie(CookieAccess)
		_, errRT := r.Cookie(CookieRefresh)
		if errAT != nil && errRT != nil { // neither session cookie present
			next.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(CookieCSRF)
		header := r.Header.Get(HeaderCSRF)
		if err != nil || ck.Value == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(header)) != 1 {
			httpx.WriteError(w, r, httpx.NewError(http.StatusForbidden, "CSRF_FAILED", "Refresh the page and try again"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAuth returns 401 for anonymous requests.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ClaimsFrom(r.Context()); !ok {
			httpx.WriteError(w, r, httpx.ErrAuthRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole allows only the given effective roles (switched admins count as
// the role they switched to).
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.ErrAuthRequired)
				return
			}
			if !slices.Contains(roles, c.EffectiveRole()) {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdmin allows admin accounts (switched or not). Without verifyMFA it
// is used for the TOTP endpoints themselves.
func RequireAdmin(verifyMFA bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.ErrAuthRequired)
				return
			}
			if c.Role != RoleAdmin {
				httpx.WriteError(w, r, httpx.ErrForbidden)
				return
			}
			if verifyMFA && !c.MFA {
				httpx.WriteError(w, r, ErrMFARequired)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdminMFAWhenAdmin blocks admins who have not passed TOTP from every
// route except sign-in and TOTP itself.
func RequireAdminMFAWhenAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := ClaimsFrom(r.Context()); ok && c.Role == RoleAdmin && !c.MFA {
			httpx.WriteError(w, r, ErrMFARequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cookieWriter sets and clears the session cookies.
type cookieWriter struct{ secure bool }

func (cw cookieWriter) set(w http.ResponseWriter, t Tokens) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieAccess, Value: t.Access, Path: "/", HttpOnly: true, Secure: cw.secure,
		SameSite: http.SameSiteLaxMode, Expires: time.Unix(t.AccessClaims.ExpiresAt, 0), MaxAge: int(AccessTTL.Seconds()),
	})
	if t.Refresh != "" {
		http.SetCookie(w, &http.Cookie{
			Name: CookieRefresh, Value: t.Refresh, Path: refreshPath, HttpOnly: true, Secure: cw.secure,
			SameSite: http.SameSiteLaxMode, MaxAge: int(RefreshTTL.Seconds()),
		})
		http.SetCookie(w, &http.Cookie{
			Name: CookieCSRF, Value: newCSRF(), Path: "/", Secure: cw.secure,
			SameSite: http.SameSiteLaxMode, MaxAge: int(RefreshTTL.Seconds()),
		})
	}
}

func (cw cookieWriter) clear(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{{CookieAccess, "/"}, {CookieRefresh, refreshPath}, {CookieCSRF, "/"}} {
		http.SetCookie(w, &http.Cookie{Name: c.name, Value: "", Path: c.path, MaxAge: -1, HttpOnly: c.name != CookieCSRF, Secure: cw.secure, SameSite: http.SameSiteLaxMode})
	}
}

func newCSRF() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
