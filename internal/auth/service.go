package auth

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Timings from TRS FR-AUTH-02, FR-AUTH-05, FR-AUTH-06.
const (
	OTPTTL           = 5 * time.Minute
	OTPMaxAttempts   = 5
	OTPResendAfter   = 30 * time.Second
	AccessTTL        = 15 * time.Minute
	RefreshTTL       = 30 * 24 * time.Hour
	AdminIdleTimeout = 30 * time.Minute
)

const (
	PurposeSignIn      = "SIGN_IN"
	PurposeVerifyPhone = "VERIFY_PHONE"
)

// Errors specific to sign-in (LLD 4.6 catalogue).
var (
	ErrOTPInvalid      = httpx.NewError(http.StatusUnauthorized, "AUTH_OTP_INVALID", "That code is not right")
	ErrOTPExpired      = httpx.NewError(http.StatusUnauthorized, "AUTH_OTP_EXPIRED", "That code has expired. Request a new one")
	ErrPhoneInvalid    = httpx.NewError(http.StatusBadRequest, "VALIDATION_FAILED", "Enter a 10-digit Indian mobile number")
	ErrSuspended       = httpx.NewError(http.StatusForbidden, "ACCOUNT_SUSPENDED", "This account is suspended")
	ErrAdminSignUp     = httpx.NewError(http.StatusForbidden, "ROLE_NOT_ALLOWED", "Admin accounts are created by an existing admin")
	ErrPhoneInUse      = httpx.NewError(http.StatusConflict, "PHONE_IN_USE", "This number already belongs to another account of this type")
	ErrAdminLimit      = httpx.NewError(http.StatusUnprocessableEntity, "ADMIN_LIMIT_REACHED", "There can be at most 3 admin accounts")
	ErrSessionExpired  = httpx.NewError(http.StatusUnauthorized, "AUTH_REQUIRED", "Your session has ended. Sign in again")
	ErrMFARequired     = httpx.NewError(http.StatusForbidden, "MFA_REQUIRED", "Enter your authenticator code to continue")
	ErrTOTPInvalid     = httpx.NewError(http.StatusUnauthorized, "AUTH_TOTP_INVALID", "That authenticator code is not right")
	ErrTOTPEnrolled    = httpx.NewError(http.StatusConflict, "TOTP_ALREADY_SET_UP", "The authenticator is already set up")
	ErrTOTPNotEnrolled = httpx.NewError(http.StatusConflict, "TOTP_NOT_SET_UP", "Set up the authenticator first")
)

// Meta describes the client making a request.
type Meta struct {
	UserAgent string
	IP        string
}

// Tokens are what the handler turns into cookies.
type Tokens struct {
	Access       string
	AccessClaims Claims
	Refresh      string
}

// Service holds the sign-in logic. Handlers stay thin.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Redis    *redis.Client
	Limiter  *ratelimit.Limiter
	Messages messaging.Messenger
	Signer   *Signer
	Box      *crypto.Box
	Pepper   []byte
	Log      *slog.Logger
	Now      func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SendOTP creates a challenge and delivers the code by SMS (to the test inbox
// in this build). accountID is set when an existing account verifies a phone.
func (s *Service) SendOTP(ctx context.Context, rawPhone string, role Role, purpose string, accountID *uuid.UUID, m Meta) (uuid.UUID, error) {
	phone, ok := NormalizeIndianPhone(rawPhone)
	if !ok {
		return uuid.Nil, ErrPhoneInvalid
	}
	// Rate limits (TRS 8.5): 3 sends per phone per 15 min, 10 per IP per hour, 30 s cooldown.
	if s.Limiter != nil {
		for _, l := range []struct {
			group, subject string
			limit          int
			window         time.Duration
		}{
			{"otp-send-phone", phone, 3, 15 * time.Minute},
			{"otp-send-ip", m.IP, 10, time.Hour},
		} {
			res, err := s.Limiter.Allow(ctx, l.group, l.subject, l.limit, l.window)
			if err == nil && !res.Allowed {
				return uuid.Nil, httpx.ErrRateLimited
			}
		}
	}
	if s.Redis != nil {
		ok, err := s.Redis.SetNX(ctx, "otp:cool:"+purpose+":"+phone, 1, OTPResendAfter).Result()
		if err == nil && !ok {
			return uuid.Nil, httpx.ErrRateLimited.WithDetail("Wait 30 seconds before asking for another code")
		}
	}

	code, err := NewOTP()
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	roleCol := store.NullAccountRole{AccountRole: store.AccountRole(role), Valid: role != ""}
	_, err = s.Q.CreateOTPChallenge(ctx, store.CreateOTPChallengeParams{
		ID: id, PhoneE164: phone, Role: roleCol, Purpose: purpose, AccountID: accountID,
		CodeHash: HashOTP(s.Pepper, phone, string(role), purpose, code), ExpiresAt: s.now().Add(OTPTTL),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: create challenge: %w", err)
	}
	_, err = s.Messages.Send(ctx, messaging.Message{
		Channel:  messaging.SMS,
		To:       phone,
		Template: "otp",
		Vars:     map[string]string{"code": code},
		Body:     fmt.Sprintf("Your Siliconics code is %s. It expires in 5 minutes. Do not share it with anyone.", code),
	})
	if err != nil {
		s.Log.Error("otp delivery failed", "phone", MaskPhone(phone), "err", err)
		return uuid.Nil, httpx.ErrUnavailable.WithDetail("We could not send the code. Try again in a moment")
	}
	return id, nil
}

// SignInResult is returned after a successful sign-in.
type SignInResult struct {
	Account store.Account
	IsNew   bool
	Tokens  Tokens
}

// VerifySignInOTP checks the code, finds or creates the account for
// (phone, role) and starts a session.
func (s *Service) VerifySignInOTP(ctx context.Context, challengeID uuid.UUID, code string, m Meta) (SignInResult, error) {
	var out SignInResult
	var fail error
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		ch, err := s.checkChallenge(ctx, q, challengeID, code, PurposeSignIn)
		if err != nil {
			fail = err
			return nil // commit the attempt counter
		}
		role := Role(ch.Role.AccountRole)
		acc, err := q.GetAccountByPhoneRole(ctx, store.GetAccountByPhoneRoleParams{PhoneE164: &ch.PhoneE164, Role: store.AccountRole(role)})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if role == RoleAdmin {
				fail = ErrAdminSignUp
				return nil
			}
			id, _ := uuid.NewV7()
			now := s.now()
			approval := store.ApprovalStatusDRAFT
			if role == RoleBuyer {
				approval = store.ApprovalStatusAPPROVED
			}
			acc, err = q.CreateAccount(ctx, store.CreateAccountParams{
				ID: id, Role: store.AccountRole(role), PhoneE164: &ch.PhoneE164, PhoneVerifiedAt: &now, Approval: approval,
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
		if err := q.ConsumeOTPChallenge(ctx, ch.ID); err != nil {
			return err
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

// VerifyPhone attaches a verified phone to an existing (for example Google) account (FR-AUTH-04).
func (s *Service) VerifyPhone(ctx context.Context, accountID, challengeID uuid.UUID, code string) (store.Account, error) {
	var acc store.Account
	var fail error
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		ch, err := s.checkChallenge(ctx, q, challengeID, code, PurposeVerifyPhone)
		if err != nil {
			fail = err
			return nil
		}
		if ch.AccountID == nil || *ch.AccountID != accountID {
			fail = ErrOTPInvalid
			return nil
		}
		acc, err = q.SetPhoneVerified(ctx, store.SetPhoneVerifiedParams{ID: accountID, PhoneE164: &ch.PhoneE164})
		if isUniqueViolation(err) {
			fail = ErrPhoneInUse
			return nil
		}
		if err != nil {
			return err
		}
		return q.ConsumeOTPChallenge(ctx, ch.ID)
	})
	if err != nil {
		return store.Account{}, err
	}
	return acc, fail
}

// checkChallenge locks the challenge row and validates expiry, attempts and code.
func (s *Service) checkChallenge(ctx context.Context, q *store.Queries, id uuid.UUID, code, purpose string) (store.OtpChallenge, error) {
	ch, err := q.GetOTPChallengeForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ch.Purpose != purpose) {
		return ch, ErrOTPExpired
	}
	if err != nil {
		return ch, err
	}
	if ch.ConsumedAt != nil || s.now().After(ch.ExpiresAt) || ch.Attempts >= OTPMaxAttempts {
		return ch, ErrOTPExpired
	}
	want := HashOTP(s.Pepper, ch.PhoneE164, string(ch.Role.AccountRole), purpose, code)
	if !hmac.Equal(want, ch.CodeHash) {
		if err := q.IncrementOTPAttempts(ctx, ch.ID); err != nil {
			return ch, err
		}
		left := OTPMaxAttempts - int(ch.Attempts) - 1
		if left <= 0 {
			return ch, ErrOTPExpired.WithDetail("Too many wrong codes. Request a new one")
		}
		return ch, ErrOTPInvalid.WithDetail(fmt.Sprintf("%d attempts left", left))
	}
	return ch, nil
}

// startSession creates a session in a new refresh-token family.
func (s *Service) startSession(ctx context.Context, q *store.Queries, acc store.Account, m Meta) (Tokens, error) {
	refresh, hash, err := NewRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	id, _ := uuid.NewV7()
	family, _ := uuid.NewV7()
	sess, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID: id, AccountID: acc.ID, FamilyID: family, RefreshHash: hash,
		UserAgent: truncate(m.UserAgent, 200), Ip: parseIP(m.IP), ExpiresAt: s.now().Add(RefreshTTL),
	})
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(acc, sess, refresh)
}

func (s *Service) issue(acc store.Account, sess store.Session, refresh string) (Tokens, error) {
	c := Claims{Subject: acc.ID, Role: Role(acc.Role), SessionID: sess.ID}
	if acc.Role == store.AccountRoleADMIN {
		c.MFA = sess.MfaAt != nil
		if sess.ActingRole.Valid {
			c.Acting = Role(sess.ActingRole.AccountRole)
		}
	}
	access, claims, err := s.Signer.Issue(c)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, AccessClaims: claims, Refresh: refresh}, nil
}

// Refresh rotates the refresh token. Presenting an already-rotated token is
// treated as theft: the whole session family is revoked (FR-AUTH-05).
func (s *Service) Refresh(ctx context.Context, refresh string, m Meta) (store.Account, Tokens, error) {
	hash := HashRefresh(refresh)
	sess, err := s.Q.GetSessionByRefreshHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		if old, err2 := s.Q.GetSessionByPrevHash(ctx, hash); err2 == nil {
			ids, _ := s.Q.RevokeFamily(ctx, old.FamilyID)
			s.denySessions(ctx, ids)
			s.Log.Warn("refresh token reuse detected; session family revoked", "family", old.FamilyID)
		}
		return store.Account{}, Tokens{}, ErrSessionExpired
	}
	if err != nil {
		return store.Account{}, Tokens{}, err
	}
	if sess.RevokedAt != nil || s.now().After(sess.ExpiresAt) {
		return store.Account{}, Tokens{}, ErrSessionExpired
	}
	acc, err := s.Q.GetAccount(ctx, sess.AccountID)
	if err != nil {
		return store.Account{}, Tokens{}, err
	}
	if acc.Status != "ACTIVE" {
		return store.Account{}, Tokens{}, ErrSuspended
	}
	if acc.Role == store.AccountRoleADMIN && s.now().Sub(sess.LastUsedAt) > AdminIdleTimeout {
		_ = s.Q.RevokeSession(ctx, store.RevokeSessionParams{ID: sess.ID, AccountID: acc.ID})
		return store.Account{}, Tokens{}, ErrSessionExpired.WithDetail("Admin sessions end after 30 minutes without activity")
	}
	next, nextHash, err := NewRefreshToken()
	if err != nil {
		return store.Account{}, Tokens{}, err
	}
	sess, err = s.Q.RotateSession(ctx, store.RotateSessionParams{ID: sess.ID, RefreshHash: hash, RefreshHash_2: nextHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Account{}, Tokens{}, ErrSessionExpired // lost a race with another refresh
	}
	if err != nil {
		return store.Account{}, Tokens{}, err
	}
	tokens, err := s.issue(acc, sess, next)
	return acc, tokens, err
}

// Logout revokes the session and blocks its access token until it expires.
func (s *Service) Logout(ctx context.Context, c Claims) error {
	if err := s.Q.RevokeSession(ctx, store.RevokeSessionParams{ID: c.SessionID, AccountID: c.Subject}); err != nil {
		return err
	}
	s.denySessions(ctx, []uuid.UUID{c.SessionID})
	return nil
}

// RevokeOwnSession signs one of the user's devices out.
func (s *Service) RevokeOwnSession(ctx context.Context, accountID, sessionID uuid.UUID) error {
	if err := s.Q.RevokeSession(ctx, store.RevokeSessionParams{ID: sessionID, AccountID: accountID}); err != nil {
		return err
	}
	s.denySessions(ctx, []uuid.UUID{sessionID})
	return nil
}

func (s *Service) denySessions(ctx context.Context, ids []uuid.UUID) {
	if s.Redis == nil {
		return
	}
	for _, id := range ids {
		_ = s.Redis.Set(ctx, "sess:deny:"+id.String(), 1, AccessTTL).Err()
	}
}

// IsDenied reports whether a session was revoked after its access token was issued.
func (s *Service) IsDenied(ctx context.Context, sessionID uuid.UUID) bool {
	if s.Redis == nil {
		return false
	}
	n, err := s.Redis.Exists(ctx, "sess:deny:"+sessionID.String()).Result()
	return err == nil && n > 0
}

// --- Admin TOTP (FR-AUTH-06) and role switch (FR-ROLE-07) ---

// TOTPStatus reports whether the admin has a confirmed authenticator.
func (s *Service) TOTPStatus(ctx context.Context, accountID uuid.UUID) (bool, error) {
	acc, err := s.Q.GetAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	return acc.TotpConfirmedAt != nil, nil
}

// TOTPSetup stores a new encrypted secret and returns the otpauth URI.
func (s *Service) TOTPSetup(ctx context.Context, accountID uuid.UUID) (secret, uri string, err error) {
	acc, err := s.Q.GetAccount(ctx, accountID)
	if err != nil {
		return "", "", err
	}
	if acc.TotpConfirmedAt != nil {
		return "", "", ErrTOTPEnrolled
	}
	secret, err = NewTOTPSecret()
	if err != nil {
		return "", "", err
	}
	enc, err := s.Box.Seal([]byte(secret), acc.ID[:])
	if err != nil {
		return "", "", err
	}
	if err := s.Q.SetTOTPSecret(ctx, store.SetTOTPSecretParams{ID: acc.ID, TotpSecretEnc: enc}); err != nil {
		return "", "", err
	}
	label := acc.Name
	if label == "" && acc.PhoneE164 != nil {
		label = MaskPhone(*acc.PhoneE164)
	}
	return secret, TOTPURI(secret, label), nil
}

// TOTPVerify checks a code, confirms enrollment if needed, marks the session
// as MFA-verified and issues a new access token.
func (s *Service) TOTPVerify(ctx context.Context, c Claims, code string) (Tokens, error) {
	acc, err := s.Q.GetAccount(ctx, c.Subject)
	if err != nil {
		return Tokens{}, err
	}
	if len(acc.TotpSecretEnc) == 0 {
		return Tokens{}, ErrTOTPNotEnrolled
	}
	if s.Limiter != nil {
		if res, err := s.Limiter.Allow(ctx, "totp", acc.ID.String(), 5, 15*time.Minute); err == nil && !res.Allowed {
			return Tokens{}, httpx.ErrRateLimited
		}
	}
	secret, err := s.Box.Open(acc.TotpSecretEnc, acc.ID[:])
	if err != nil {
		return Tokens{}, err
	}
	if !VerifyTOTP(string(secret), code, s.now()) {
		return Tokens{}, ErrTOTPInvalid
	}
	if acc.TotpConfirmedAt == nil {
		if err := s.Q.ConfirmTOTP(ctx, acc.ID); err != nil {
			return Tokens{}, err
		}
	}
	if err := s.Q.SetSessionMFA(ctx, c.SessionID); err != nil {
		return Tokens{}, err
	}
	return s.reissue(ctx, acc, c.SessionID)
}

// SwitchRole lets an MFA-verified admin view the platform as another role
// (nil returns to admin). Every switch is audit-logged.
func (s *Service) SwitchRole(ctx context.Context, c Claims, to *Role, m Meta) (Tokens, error) {
	acc, err := s.Q.GetAccount(ctx, c.Subject)
	if err != nil {
		return Tokens{}, err
	}
	col := store.NullAccountRole{}
	if to != nil && *to != RoleAdmin {
		col = store.NullAccountRole{AccountRole: store.AccountRole(*to), Valid: true}
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := q.SetSessionActingRole(ctx, store.SetSessionActingRoleParams{ID: c.SessionID, ActingRole: col}); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"actingRole": col.AccountRole, "active": col.Valid})
		before, _ := json.Marshal(map[string]any{"actingRole": c.Acting})
		auditID, _ := uuid.NewV7()
		actor := acc.ID
		return q.InsertAudit(ctx, store.InsertAuditParams{
			ID: auditID, ActorID: &actor, Action: "admin.role_switch", TargetType: "session",
			TargetID: c.SessionID.String(), Before: before, After: after, Ip: parseIP(m.IP),
		})
	})
	if err != nil {
		return Tokens{}, err
	}
	return s.reissue(ctx, acc, c.SessionID)
}

func (s *Service) reissue(ctx context.Context, acc store.Account, sessionID uuid.UUID) (Tokens, error) {
	sess, err := s.Q.GetSession(ctx, sessionID)
	if err != nil {
		return Tokens{}, err
	}
	if sess.RevokedAt != nil {
		return Tokens{}, ErrSessionExpired
	}
	_ = s.Q.TouchSession(ctx, sessionID)
	return s.issue(acc, sess, "")
}

// CreateAdmin is used by the admin CLI; the database enforces the 3-admin limit.
func (s *Service) CreateAdmin(ctx context.Context, rawPhone, name string) (store.Account, error) {
	phone, ok := NormalizeIndianPhone(rawPhone)
	if !ok {
		return store.Account{}, ErrPhoneInvalid
	}
	id, _ := uuid.NewV7()
	now := s.now()
	acc, err := s.Q.CreateAccount(ctx, store.CreateAccountParams{
		ID: id, Role: store.AccountRoleADMIN, PhoneE164: &phone, PhoneVerifiedAt: &now, Name: name, Approval: store.ApprovalStatusAPPROVED,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Message == "ADMIN_LIMIT_REACHED" {
		return store.Account{}, ErrAdminLimit
	}
	if isUniqueViolation(err) {
		return store.Account{}, ErrPhoneInUse
	}
	return acc, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func parseIP(s string) *netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &a
}

// truncate keeps at most n characters without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// DecodeKey decodes a base64 key from configuration.
func DecodeKey(b64 string) ([]byte, error) { return base64.StdEncoding.DecodeString(b64) }
