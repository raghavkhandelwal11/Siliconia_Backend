// Package payment defines the payment provider (LLD 12) and the free sim-pay
// adapter used in this build. Paid gateways are planned in paid-later/ only.
package payment

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

// Order is a checkout created at the provider.
type Order struct {
	ProviderOrderID string
	CheckoutURL     string
	Status          string
}

// Status is the provider's view of a payment.
type Status struct {
	ProviderOrderID   string
	ProviderPaymentID string
	Status            string // CREATED, PENDING, CAPTURED, FAILED
	Method            string
	FailureCode       string
	MandateID         string
}

// Refund is a refund at the provider.
type Refund struct {
	ProviderRefundID string
	Status           string
}

// Event is a verified webhook.
type Event struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Str reads a string field from the event data.
func (e Event) Str(key string) string {
	s, _ := e.Data[key].(string)
	return s
}

// Provider takes payments.
type Provider interface {
	Name() string
	CreateOrder(ctx context.Context, ref string, amountPaise int64, mandate bool, returnURL string) (Order, error)
	GetPayment(ctx context.Context, providerOrderID string) (Status, error)
	Refund(ctx context.Context, providerPaymentID string, amountPaise int64, ref string) (Refund, error)
	ChargeMandate(ctx context.Context, mandateID string, amountPaise int64, ref string) (Status, error)
	RevokeMandate(ctx context.Context, mandateID string) error
	VerifyWebhook(r *http.Request) (Event, error)
}

// ErrRejected means the provider refused the request (bad state or input).
var ErrRejected = errors.New("payment: rejected by provider")

// Sim talks to sim-pay.
type Sim struct {
	BaseURL string
	APIKey  string
	Secret  []byte
	Client  *http.Client
	Now     func() time.Time
}

// NewSim creates the sim-pay adapter.
func NewSim(baseURL, apiKey string, secret []byte) *Sim {
	return &Sim{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Secret: secret, Client: &http.Client{Timeout: 3 * time.Second}, Now: time.Now}
}

// Name implements Provider.
func (s *Sim) Name() string { return "sim-pay" }

type simOrder struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PaymentID   string `json:"paymentId"`
	Method      string `json:"method"`
	FailureCode string `json:"failureCode"`
	MandateID   string `json:"mandateId"`
	CheckoutURL string `json:"checkoutUrl"`
}

func (s *Sim) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound:
		return ErrRejected
	case res.StatusCode >= 300:
		return fmt.Errorf("payment: %s returned %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// CreateOrder implements Provider.
func (s *Sim) CreateOrder(ctx context.Context, ref string, amount int64, mandate bool, returnURL string) (Order, error) {
	var o simOrder
	err := s.call(ctx, http.MethodPost, "/v1/orders", map[string]any{"ref": ref, "amount": amount, "mandate": mandate, "returnUrl": returnURL}, &o)
	return Order{ProviderOrderID: o.ID, CheckoutURL: o.CheckoutURL, Status: o.Status}, err
}

// GetPayment implements Provider.
func (s *Sim) GetPayment(ctx context.Context, id string) (Status, error) {
	var o simOrder
	err := s.call(ctx, http.MethodGet, "/v1/orders/"+id, nil, &o)
	return Status{ProviderOrderID: o.ID, ProviderPaymentID: o.PaymentID, Status: o.Status, Method: o.Method, FailureCode: o.FailureCode, MandateID: o.MandateID}, err
}

// Refund implements Provider.
func (s *Sim) Refund(ctx context.Context, paymentID string, amount int64, ref string) (Refund, error) {
	var r struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	err := s.call(ctx, http.MethodPost, "/v1/refunds", map[string]any{"paymentId": paymentID, "amount": amount, "ref": ref}, &r)
	return Refund{ProviderRefundID: r.ID, Status: r.Status}, err
}

// ChargeMandate implements Provider.
func (s *Sim) ChargeMandate(ctx context.Context, mandateID string, amount int64, ref string) (Status, error) {
	var o simOrder
	err := s.call(ctx, http.MethodPost, "/v1/mandates/"+mandateID+"/charges", map[string]any{"amount": amount, "ref": ref}, &o)
	return Status{ProviderOrderID: o.ID, ProviderPaymentID: o.PaymentID, Status: o.Status, Method: o.Method, MandateID: mandateID}, err
}

// RevokeMandate implements Provider.
func (s *Sim) RevokeMandate(ctx context.Context, mandateID string) error {
	var out map[string]string
	return s.call(ctx, http.MethodPost, "/v1/mandates/"+mandateID+"/revoke", nil, &out)
}

// VerifyWebhook implements Provider: checks the signature and decodes the body.
func (s *Sim) VerifyWebhook(r *http.Request) (Event, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 64<<10))
	if err != nil {
		return Event{}, err
	}
	if err := whsig.Verify(s.Secret, r.Header.Get(whsig.Header), body, s.Now()); err != nil {
		return Event{}, err
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || e.Type == "" {
		return Event{}, errors.New("payment: malformed event")
	}
	return e, nil
}
