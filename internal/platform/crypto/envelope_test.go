package crypto

import (
	"bytes"
	"testing"
)

const key = "c2lsaWNvbmljcy1sb2NhbC1kZXYtbWFzdGVyLWtleSE="

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := b.Seal([]byte("JBSWY3DPEHPK3PXP"), []byte("account-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("JBSWY3DPEHPK3PXP")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	pt, err := b.Open(ct, []byte("account-1"))
	if err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("round trip failed: %q %v", pt, err)
	}
}

func TestOpenRejectsWrongContextAndTampering(t *testing.T) {
	b, _ := NewBox(key)
	ct, _ := b.Seal([]byte("secret"), []byte("account-1"))
	if _, err := b.Open(ct, []byte("account-2")); err == nil {
		t.Fatal("opened with another account's context")
	}
	ct[len(ct)-1] ^= 0xff
	if _, err := b.Open(ct, []byte("account-1")); err == nil {
		t.Fatal("opened tampered ciphertext")
	}
}

func TestSealIsRandomised(t *testing.T) {
	b, _ := NewBox(key)
	a, _ := b.Seal([]byte("same"), nil)
	c, _ := b.Seal([]byte("same"), nil)
	if bytes.Equal(a, c) {
		t.Fatal("two seals of the same value are identical")
	}
}

func TestNewBoxRejectsBadKey(t *testing.T) {
	if _, err := NewBox("c2hvcnQ="); err == nil {
		t.Fatal("accepted a short key")
	}
}
