package whsig

import (
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret, body, now := []byte("s3cret"), []byte(`{"id":"evt_1"}`), time.Unix(1_800_000_000, 0)
	h := Sign(secret, body, now)
	if err := Verify(secret, h, body, now.Add(time.Minute)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	cases := map[string]error{
		"tampered body":   Verify(secret, h, []byte(`{"id":"evt_2"}`), now),
		"wrong secret":    Verify([]byte("other"), h, body, now),
		"too old":         Verify(secret, h, body, now.Add(6*time.Minute)),
		"from the future": Verify(secret, h, body, now.Add(-6*time.Minute)),
		"malformed":       Verify(secret, "v1=abc", body, now),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
