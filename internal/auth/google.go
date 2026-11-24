package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Google implements "Continue with Google" with OpenID Connect + PKCE
// (FR-AUTH-04). Google's OAuth client is free; it is enabled only when
// GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET are set.
type Google struct {
	OAuth    oauth2.Config
	Verifier *oidc.IDTokenVerifier
	BaseURL  string // public base URL for redirects back into the web app
}

const (
	cookieOAuth  = "sx_oauth"
	oauthTTL     = 10 * time.Minute
	googleScopes = "openid email profile"
)

var errGoogleOff = httpx.NewError(http.StatusServiceUnavailable, "GOOGLE_NOT_CONFIGURED", "Google sign-in is not set up yet. Use your mobile number")

// NewGoogle discovers the issuer's endpoints and keys.
func NewGoogle(ctx context.Context, issuer, clientID, clientSecret, baseURL string) (*Google, error) {
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: google discovery: %w", err)
	}
	return &Google{
		OAuth: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, Endpoint: p.Endpoint(),
			RedirectURL: strings.TrimRight(baseURL, "/") + "/api/v1/auth/google/callback",
			Scopes:      strings.Fields(googleScopes),
		},
		Verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
		BaseURL:  strings.TrimRight(baseURL, "/"),
	}, nil
}

type oauthState struct {
	Role     Role   `json:"role"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

func (h *Handler) googleStatus(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enabled": h.Google != nil})
}

func (h *Handler) googleStart(w http.ResponseWriter, r *http.Request) {
	if h.Google == nil {
		httpx.WriteError(w, r, errGoogleOff)
		return
	}
	role, ok := ParseRole(r.URL.Query().Get("role"))
	if !ok || role == RoleAdmin {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Choose an account type"))
		return
	}
	st := oauthState{Role: role, Nonce: randToken(), Verifier: oauth2.GenerateVerifier(), Next: safeNext(r.URL.Query().Get("next"))}
	state := randToken()
	raw, _ := json.Marshal(st)
	if err := h.Svc.Redis.Set(r.Context(), "oauth:state:"+state, raw, oauthTTL).Err(); err != nil {
		writeErr(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieOAuth, Value: state, Path: "/api/v1/auth/google", HttpOnly: true,
		Secure: h.Cookies.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(oauthTTL.Seconds())})
	u := h.Google.OAuth.AuthCodeURL(state, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, u, http.StatusFound)
}

func (h *Handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	if h.Google == nil {
		httpx.WriteError(w, r, errGoogleOff)
		return
	}
	fail := func(reason string) {
		http.Redirect(w, r, h.Google.BaseURL+"/auth/sign-in?error="+url.QueryEscape(reason), http.StatusFound)
	}
	state := r.URL.Query().Get("state")
	ck, err := r.Cookie(cookieOAuth)
	if err != nil || state == "" || ck.Value != state {
		fail("state")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieOAuth, Value: "", Path: "/api/v1/auth/google", MaxAge: -1})
	raw, err := h.Svc.Redis.GetDel(r.Context(), "oauth:state:"+state).Bytes()
	if err != nil {
		fail("expired")
		return
	}
	var st oauthState
	if json.Unmarshal(raw, &st) != nil {
		fail("state")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tok, err := h.Google.OAuth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail("exchange")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := h.Google.Verifier.Verify(ctx, rawID)
	if err != nil || idt.Nonce != st.Nonce {
		fail("token")
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil || !claims.EmailVerified {
		fail("email")
		return
	}
	res, err := h.Svc.SignInGoogle(r.Context(), idt.Subject, claims.Email, claims.Name, st.Role, meta(r))
	if err != nil {
		var appErr *httpx.Error
		if errors.As(err, &appErr) {
			fail(strings.ToLower(appErr.Code))
			return
		}
		fail("server")
		return
	}
	h.Cookies.set(w, res.Tokens)
	next := st.Next
	if res.Account.PhoneVerifiedAt == nil {
		next = "/auth/verify-phone?next=" + url.QueryEscape(st.Next)
	}
	http.Redirect(w, r, h.Google.BaseURL+next, http.StatusFound)
}

// SignInGoogle finds or creates the account for (Google subject, role).
func (s *Service) SignInGoogle(ctx context.Context, subject, email, name string, role Role, m Meta) (SignInResult, error) {
	var out SignInResult
	var fail error
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		acc, err := q.GetAccountByGoogleRole(ctx, store.GetAccountByGoogleRoleParams{GoogleSub: &subject, Role: store.AccountRole(role)})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			id, _ := uuid.NewV7()
			approval := store.ApprovalStatusDRAFT
			if role == RoleBuyer {
				approval = store.ApprovalStatusAPPROVED
			}
			var emailPtr *string
			if email != "" {
				emailPtr = &email
			}
			acc, err = q.CreateAccount(ctx, store.CreateAccountParams{
				ID: id, Role: store.AccountRole(role), GoogleSub: &subject, Email: emailPtr, Name: truncate(name, 100), Approval: approval,
			})
			if err != nil {
				return err
			}
			out.IsNew = true
		case err != nil:
			return err
		case acc.Status != "ACTIVE":
			fail = ErrSuspended
			return nil
		}
		tokens, err := s.startSession(ctx, q, acc, m)
		if err != nil {
			return err
		}
		out.Account, out.Tokens = acc, tokens
		return nil
	})
	if err != nil {
		return SignInResult{}, err
	}
	return out, fail
}

// safeNext only allows same-site relative paths, preventing open redirects.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func randToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
