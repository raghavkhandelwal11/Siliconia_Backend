package auth

import (
	"encoding/base32"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeIndianPhone(t *testing.T) {
	cases := map[string]string{
		"9812345678":       "+919812345678",
		"+91 98123 45678":  "+919812345678",
		"091-9812345678":   "", // 13 digits with leading 0 and 91 is not a valid form
		"09812345678":      "+919812345678",
		"919812345678":     "+919812345678",
		"5812345678":       "", // Indian mobiles start with 6-9
		"98123":            "",
		"+1 415 555 01234": "",
	}
	for in, want := range cases {
		got, ok := NormalizeIndianPhone(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: got %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("+919812345678"); got != "+91******5678" {
		t.Fatal(got)
	}
}

func TestOTPIsSixDigitsAndHashIsBound(t *testing.T) {
	code, err := NewOTP()
	if err != nil || len(code) != 6 {
		t.Fatalf("code %q err %v", code, err)
	}
	p := []byte("pepper")
	a := HashOTP(p, "+919812345678", "BUYER", "SIGN_IN", code)
	if string(a) == string(HashOTP(p, "+919812345678", "SELLER", "SIGN_IN", code)) {
		t.Fatal("hash not bound to role")
	}
	if string(a) != string(HashOTP(p, "+919812345678", "BUYER", "SIGN_IN", code)) {
		t.Fatal("hash not deterministic")
	}
}

// RFC 6238 appendix B test vectors (SHA-1, 8 digits reduced to the last 6).
func TestTOTPRFCVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	vectors := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range vectors {
		got, err := TOTPCode(secret, time.Unix(ts, 0))
		if err != nil || got != want {
			t.Errorf("t=%d got %s want %s (%v)", ts, got, want, err)
		}
	}
	if !VerifyTOTP(secret, "287082", time.Unix(59+20, 0)) {
		t.Error("one step of drift should be accepted")
	}
	if VerifyTOTP(secret, "287082", time.Unix(59+120, 0)) {
		t.Error("old code accepted")
	}
}

func TestSignerIssueVerify(t *testing.T) {
	s, err := NewSigner("c2lsaWNvbmljcy1sb2NhbC1kZXYtc2lnbmluZy1rZXk=", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	id, sid := uuid.New(), uuid.New()
	tok, _, err := s.Issue(Claims{Subject: id, Role: RoleAdmin, SessionID: sid, Acting: RoleSeller, MFA: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil || c.Subject != id || c.EffectiveRole() != RoleSeller || !c.MFA {
		t.Fatalf("verify: %+v %v", c, err)
	}

	// Tampered payload.
	if _, err := s.Verify(tok[:len(tok)-2] + "AA"); err == nil {
		t.Fatal("tampered token accepted")
	}
	// Other key.
	other, _ := NewSigner("b3RoZXItc2lsaWNvbmljcy1kZXYtc2lnbmluZy1rZXk=", time.Minute)
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("token from another key accepted")
	}
	// Expired.
	now = now.Add(16 * time.Minute)
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestEffectiveRoleOnlyForAdmins(t *testing.T) {
	if (Claims{Role: RoleBuyer, Acting: RoleAdmin}).EffectiveRole() != RoleBuyer {
		t.Fatal("non-admin could act as another role")
	}
}
