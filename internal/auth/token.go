// Package auth implements sign-in (phone OTP and Google), sessions with
// rotating refresh tokens, admin TOTP and role switching (LLD 4.2, 4.3; TRS
// FR-AUTH, FR-ROLE).
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is an account type.
type Role string

const (
	RoleBuyer    Role = "BUYER"
	RoleSeller   Role = "SELLER"
	RolePromoter Role = "PROMOTER"
	RoleCreator  Role = "CREATOR"
	RoleAdmin    Role = "ADMIN"
)

// ParseRole validates a role name (case-insensitive).
func ParseRole(s string) (Role, bool) {
	r := Role(strings.ToUpper(strings.TrimSpace(s)))
	switch r {
	case RoleBuyer, RoleSeller, RolePromoter, RoleCreator, RoleAdmin:
		return r, true
	}
	return "", false
}

// Claims are carried by the short-lived access token (cookie sx_at).
type Claims struct {
	Subject   uuid.UUID `json:"sub"`
	Role      Role      `json:"role"`
	SessionID uuid.UUID `json:"sid"`
	Acting    Role      `json:"act,omitempty"` // admins only, after role switch
	MFA       bool      `json:"mfa,omitempty"` // admins only, after TOTP
	IssuedAt  int64     `json:"iat"`
	ExpiresAt int64     `json:"exp"`
}

// EffectiveRole is the role guards check: the acting role for switched admins.
func (c Claims) EffectiveRole() Role {
	if c.Role == RoleAdmin && c.Acting != "" {
		return c.Acting
	}
	return c.Role
}

// Signer issues and verifies EdDSA-signed JWT access tokens.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	ttl  time.Duration
	now  func() time.Time
}

// NewSigner builds a signer from a base64 32-byte ed25519 seed.
func NewSigner(seedB64 string, ttl time.Duration) (*Signer, error) {
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("auth: signing key must be a base64 32-byte seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey), ttl: ttl, now: time.Now}, nil
}

// PublicKey is shared with the web app's proxy so it can check tokens too.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

var jwtHeader = b64(mustJSON(map[string]string{"alg": "EdDSA", "typ": "JWT"}))

// Issue signs claims, setting iat and exp.
func (s *Signer) Issue(c Claims) (string, Claims, error) {
	now := s.now()
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(s.ttl).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", c, err
	}
	signingInput := jwtHeader + "." + b64(payload)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + b64(sig), c, nil
}

// ErrInvalidToken covers malformed, forged and expired tokens.
var ErrInvalidToken = errors.New("auth: invalid token")

// Verify checks the signature and expiry and returns the claims.
func (s *Signer) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return Claims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(s.pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if s.now().Unix() >= c.ExpiresAt {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

// NewRefreshToken returns a random opaque token and the hash stored in the database.
func NewRefreshToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = b64(b)
	return token, HashRefresh(token), nil
}

// HashRefresh hashes a refresh token for storage and lookup.
func HashRefresh(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("auth: %v", err))
	}
	return b
}
