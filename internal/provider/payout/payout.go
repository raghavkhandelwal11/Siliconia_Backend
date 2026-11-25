// Package payout defines the bank payout provider and the free sim-payout
// adapter (LLD 12). Paid payout APIs are planned in paid-later/ only.
package payout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

// Event is a verified transfer result.
type Event struct {
	ID      string
	Ref     string
	Status  string // PAID or FAILED
	BankRef string
	Reason  string
	Raw     []byte
}

// ErrMalformed means the webhook body could not be read.
var ErrMalformed = errors.New("payout: malformed event")

// Sim talks to sim-payout.
type Sim struct {
	BaseURL, APIKey string
	Secret          []byte
	Client          *http.Client
}

// NewSim creates the adapter.
func NewSim(baseURL, key string, secret []byte) *Sim {
	return &Sim{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: key, Secret: secret, Client: &http.Client{Timeout: 5 * time.Second}}
}

// Name of the provider.
func (s *Sim) Name() string { return "sim-payout" }

// Transfer pays amount paise to a bank account; ref makes it idempotent.
func (s *Sim) Transfer(ctx context.Context, ref string, amount int64, account, ifsc string) error {
	b, _ := json.Marshal(map[string]any{"ref": ref, "amount": amount, "account": account, "ifsc": ifsc})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/v1/transfers", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("payout: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("payout: status %d", res.StatusCode)
	}
	return nil
}

// VerifyWebhook checks the signature and decodes the event.
func (s *Sim) VerifyWebhook(r *http.Request) (Event, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 16<<10))
	if err != nil {
		return Event{}, err
	}
	if err := whsig.Verify(s.Secret, r.Header.Get(whsig.Header), body, time.Now()); err != nil {
		return Event{}, err
	}
	var e struct {
		ID   string `json:"id"`
		Data struct {
			Ref, Status, BankRef, Reason string
		} `json:"data"`
	}
	if json.Unmarshal(body, &e) != nil || e.ID == "" || e.Data.Ref == "" {
		return Event{}, ErrMalformed
	}
	return Event{ID: e.ID, Ref: e.Data.Ref, Status: e.Data.Status, BankRef: e.Data.BankRef, Reason: e.Data.Reason, Raw: body}, nil
}
