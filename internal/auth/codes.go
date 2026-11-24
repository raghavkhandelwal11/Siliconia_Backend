package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1 for authenticator-app compatibility
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// NormalizeIndianPhone accepts common Indian mobile formats and returns
// +91XXXXXXXXXX (TRS A-03: Indian numbers only).
func NormalizeIndianPhone(raw string) (string, bool) {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		d = d[2:]
	case len(d) == 11 && strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	if len(d) != 10 || d[0] < '6' { // Indian mobile numbers start with 6–9
		return "", false
	}
	return "+91" + d, true
}

// MaskPhone hides all but the last 4 digits for logs and messages (LLD 15).
func MaskPhone(e164 string) string {
	if len(e164) < 4 {
		return "****"
	}
	return "+91******" + e164[len(e164)-4:]
}

// NewOTP returns a random 6-digit code.
func NewOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashOTP binds a code to its phone, role and purpose; only the hash is stored.
func HashOTP(pepper []byte, phone, role, purpose, code string) []byte {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(phone + "|" + role + "|" + purpose + "|" + code))
	return m.Sum(nil)
}

// NewTOTPSecret returns a random base32 secret for an authenticator app.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// TOTPURI is the otpauth:// link an authenticator app scans.
func TOTPURI(secret, account string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", "Siliconics")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("Siliconics:"+account) + "?" + v.Encode()
}

// TOTPCode computes the RFC 6238 code for a time.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", fmt.Errorf("auth: bad TOTP secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t.Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

// VerifyTOTP accepts the current code or one step either side (clock drift).
func VerifyTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	for _, step := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
		want, err := TOTPCode(secret, now.Add(step))
		if err == nil && hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}
