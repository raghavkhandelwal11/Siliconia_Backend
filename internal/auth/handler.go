package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Handler exposes the sign-in and account endpoints (LLD 7.1).
type Handler struct {
	Svc     *Service
	Google  *Google // nil when Google sign-in is not configured
	Cookies cookieWriter
}

// NewHandler creates the handler; secure sets the Secure flag on cookies.
func NewHandler(svc *Service, google *Google, secure bool) *Handler {
	return &Handler{Svc: svc, Google: google, Cookies: cookieWriter{secure: secure}}
}

// AccountView is the public shape of an account (GET /me).
type AccountView struct {
	ID            uuid.UUID `json:"id"`
	Role          Role      `json:"role"`
	ActingRole    *Role     `json:"actingRole,omitempty"`
	Name          string    `json:"name"`
	Email         *string   `json:"email,omitempty"`
	Phone         *string   `json:"phone,omitempty"`
	PhoneVerified bool      `json:"phoneVerified"`
	Approval      string    `json:"approval"`
	MFAVerified   bool      `json:"mfaVerified"`
	CreatedAt     time.Time `json:"createdAt"`
}

func view(a store.Account, c *Claims) AccountView {
	v := AccountView{
		ID: a.ID, Role: Role(a.Role), Name: a.Name, Email: a.Email, Phone: a.PhoneE164,
		PhoneVerified: a.PhoneVerifiedAt != nil, Approval: string(a.Approval), CreatedAt: a.CreatedAt,
	}
	if c != nil {
		v.MFAVerified = c.MFA
		if c.Acting != "" {
			acting := c.Acting
			v.ActingRole = &acting
		}
	}
	return v
}

// Routes mounts the handlers under /api/v1.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Post("/otp/send", h.sendOTP)
		r.Post("/otp/verify", h.verifyOTP)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
		r.Get("/google/status", h.googleStatus)
		r.Get("/google/start", h.googleStart)
		r.Get("/google/callback", h.googleCallback)
		r.Group(func(r chi.Router) {
			r.Use(RequireAdmin(false))
			r.Get("/totp/status", h.totpStatus)
			r.Post("/totp/setup", h.totpSetup)
			r.Post("/totp/verify", h.totpVerify)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(RequireAuth, RequireAdminMFAWhenAdmin)
		r.Get("/me", h.me)
		r.Patch("/me", h.updateMe)
		r.Post("/me/phone/otp", h.phoneOTP)
		r.Post("/me/phone/verify", h.phoneVerify)
		r.Get("/me/sessions", h.sessions)
		r.Delete("/me/sessions/{id}", h.revokeSession)
	})
	r.With(RequireAdmin(true)).Post("/admin/role-switch", h.roleSwitch)
}

func meta(r *http.Request) Meta { return Meta{UserAgent: r.UserAgent(), IP: httpx.ClientIP(r)} }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func (h *Handler) sendOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	role, ok := ParseRole(body.Role)
	if !ok {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Choose an account type"))
		return
	}
	id, err := h.Svc.SendOTP(r.Context(), body.Phone, role, PurposeSignIn, nil, meta(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"challengeId": id, "resendAfterSec": int(OTPResendAfter.Seconds()), "expiresInSec": int(OTPTTL.Seconds())})
}

func (h *Handler) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID uuid.UUID `json:"challengeId"`
		Code        string    `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := h.Svc.VerifySignInOTP(r.Context(), body.ChallengeID, strings.TrimSpace(body.Code), meta(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.Cookies.set(w, res.Tokens)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"account": view(res.Account, &res.Tokens.AccessClaims), "isNew": res.IsNew})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	ck, err := r.Cookie(CookieRefresh)
	if err != nil || ck.Value == "" {
		httpx.WriteError(w, r, ErrSessionExpired)
		return
	}
	acc, tokens, err := h.Svc.Refresh(r.Context(), ck.Value, meta(r))
	if err != nil {
		h.Cookies.clear(w)
		writeErr(w, r, err)
		return
	}
	h.Cookies.set(w, tokens)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"account": view(acc, &tokens.AccessClaims)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, ok := ClaimsFrom(r.Context()); ok {
		if err := h.Svc.Logout(r.Context(), c); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	h.Cookies.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	acc, err := h.Svc.Q.GetAccount(r.Context(), c.Subject)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(acc, &c))
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var body struct {
		Name  string  `json:"name"`
		Email *string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) > 100 {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Name must be at most 100 characters"))
		return
	}
	if body.Email != nil {
		e := strings.TrimSpace(*body.Email)
		if e == "" {
			body.Email = nil
		} else if !strings.Contains(e, "@") || len(e) > 254 {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Enter a valid email address"))
			return
		} else {
			body.Email = &e
		}
	}
	acc, err := h.Svc.Q.UpdateProfile(r.Context(), store.UpdateProfileParams{ID: c.Subject, Name: body.Name, Email: body.Email})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(acc, &c))
}

func (h *Handler) phoneOTP(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var body struct {
		Phone string `json:"phone"`
	}
	if !decode(w, r, &body) {
		return
	}
	sub := c.Subject
	id, err := h.Svc.SendOTP(r.Context(), body.Phone, c.Role, PurposeVerifyPhone, &sub, meta(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"challengeId": id, "resendAfterSec": int(OTPResendAfter.Seconds())})
}

func (h *Handler) phoneVerify(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var body struct {
		ChallengeID uuid.UUID `json:"challengeId"`
		Code        string    `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	acc, err := h.Svc.VerifyPhone(r.Context(), c.Subject, body.ChallengeID, strings.TrimSpace(body.Code))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(acc, &c))
}

type sessionView struct {
	ID         uuid.UUID `json:"id"`
	UserAgent  string    `json:"userAgent"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	Current    bool      `json:"current"`
}

func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	rows, err := h.Svc.Q.ListActiveSessions(r.Context(), c.Subject)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]sessionView, 0, len(rows))
	for _, s := range rows {
		out = append(out, sessionView{ID: s.ID, UserAgent: s.UserAgent, CreatedAt: s.CreatedAt, LastUsedAt: s.LastUsedAt, Current: s.ID == c.SessionID})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	if err := h.Svc.RevokeOwnSession(r.Context(), c.Subject, id); err != nil {
		writeErr(w, r, err)
		return
	}
	if id == c.SessionID {
		h.Cookies.clear(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) totpStatus(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	enrolled, err := h.Svc.TOTPStatus(r.Context(), c.Subject)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enrolled": enrolled, "verified": c.MFA})
}

func (h *Handler) totpSetup(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	secret, uri, err := h.Svc.TOTPSetup(r.Context(), c.Subject)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauthUri": uri})
}

func (h *Handler) totpVerify(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	tokens, err := h.Svc.TOTPVerify(r.Context(), c, body.Code)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.Cookies.set(w, tokens)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) roleSwitch(w http.ResponseWriter, r *http.Request) {
	c, _ := ClaimsFrom(r.Context())
	var body struct {
		Role *string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	var to *Role
	if body.Role != nil {
		role, ok := ParseRole(*body.Role)
		if !ok {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Unknown role"))
			return
		}
		to = &role
	}
	tokens, err := h.Svc.SwitchRole(r.Context(), c, to, meta(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.Cookies.set(w, tokens)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actingRole": tokens.AccessClaims.Acting})
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	httpx.WriteError(w, r, err)
}
