// Package whsig signs and verifies simulator webhooks (LLD 12):
// header X-Sim-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, t + "." + body)>,
// rejected when the timestamp is more than 5 minutes away from now.
package whsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header carries the signature.
const Header = "X-Sim-Signature"

// Tolerance is how far the signed time may be from now.
const Tolerance = 5 * time.Minute

var ErrBadSignature = errors.New("whsig: bad signature")

// Sign returns the header value for body at time t.
func Sign(secret []byte, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

// Verify checks a header value against body at time now.
func Verify(secret []byte, header string, body []byte, now time.Time) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return fmt.Errorf("%w: malformed header", ErrBadSignature)
	}
	if d := now.Sub(time.Unix(unix, 0)); d > Tolerance || d < -Tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
	}
	want, _ := hex.DecodeString(mac(secret, ts, body))
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(want, got) {
		return ErrBadSignature
	}
	return nil
}

func mac(secret []byte, ts string, body []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
