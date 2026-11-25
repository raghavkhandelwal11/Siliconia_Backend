// Package shipping defines the shipping provider (LLD 12) and the free
// sim-ship adapter used in this build. Paid aggregators are planned in
// paid-later/ only.
package shipping

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

// Shipment kinds.
const (
	Forward = "FORWARD"
	Return  = "RETURN"
)

// Serviceability is what couriers offer for a PIN code.
type Serviceability struct {
	Deliverable bool
	COD         bool
	ETADays     int
}

// Address is a pickup or delivery address.
type Address struct {
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Line1   string `json:"line1"`
	Line2   string `json:"line2,omitempty"`
	City    string `json:"city"`
	State   string `json:"state"`
	Pincode string `json:"pincode"`
}

// Parcel is one box.
type Parcel struct {
	WeightGrams int    `json:"weightGrams"`
	Description string `json:"description"`
}

// ShipmentRequest books a forward shipment or a return pickup.
type ShipmentRequest struct {
	Ref       string   `json:"ref"`
	Kind      string   `json:"kind"`
	From      Address  `json:"from"`
	To        Address  `json:"to"`
	Parcels   []Parcel `json:"parcels"`
	CODAmount int64    `json:"codAmount"`
}

// Shipment is a booked consignment.
type Shipment struct {
	AWB    string
	Status string
	ETA    time.Time
}

// Event is a verified tracking webhook.
type Event struct {
	ID       string
	AWB      string
	Ref      string
	Kind     string
	Status   string
	Location string
	At       time.Time
	ETA      time.Time
	Raw      json.RawMessage
}

// Provider ships parcels.
type Provider interface {
	Name() string
	Serviceability(ctx context.Context, pincode string) (Serviceability, error)
	CreateShipment(ctx context.Context, req ShipmentRequest) (Shipment, error)
	Label(ctx context.Context, awb string) ([]byte, error)
	Cancel(ctx context.Context, awb string) error
	VerifyWebhook(r *http.Request) (Event, error)
}

var (
	// ErrRejected means the provider refused the request (bad state or input).
	ErrRejected = errors.New("shipping: rejected by provider")
	// ErrNotServiceable means no courier serves the PIN (or not with COD).
	ErrNotServiceable = errors.New("shipping: not serviceable")
	// ErrMalformed means a webhook body could not be read.
	ErrMalformed = errors.New("shipping: malformed event")
)

// Sim talks to sim-ship.
type Sim struct {
	BaseURL string
	APIKey  string
	Secret  []byte
	Client  *http.Client
	Now     func() time.Time
}

// NewSim creates the sim-ship adapter.
func NewSim(baseURL, apiKey string, secret []byte) *Sim {
	return &Sim{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Secret: secret, Client: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

// Name implements Provider.
func (s *Sim) Name() string { return "sim-ship" }

func (s *Sim) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shipping: %w", err)
	}
	switch {
	case res.StatusCode == http.StatusUnprocessableEntity:
		res.Body.Close()
		return nil, ErrNotServiceable
	case res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound:
		res.Body.Close()
		return nil, ErrRejected
	case res.StatusCode >= 300:
		res.Body.Close()
		return nil, fmt.Errorf("shipping: %s returned %d", path, res.StatusCode)
	}
	return res, nil
}

func (s *Sim) call(ctx context.Context, method, path string, in, out any) error {
	res, err := s.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(out)
}

// Serviceability implements Provider.
func (s *Sim) Serviceability(ctx context.Context, pincode string) (Serviceability, error) {
	var out struct {
		Deliverable bool `json:"deliverable"`
		COD         bool `json:"cod"`
		ETADays     int  `json:"etaDays"`
	}
	err := s.call(ctx, http.MethodGet, "/v1/serviceability/"+pincode, nil, &out)
	return Serviceability{Deliverable: out.Deliverable, COD: out.COD, ETADays: out.ETADays}, err
}

type simShipment struct {
	AWB    string    `json:"awb"`
	Status string    `json:"status"`
	ETA    time.Time `json:"eta"`
}

// CreateShipment implements Provider.
func (s *Sim) CreateShipment(ctx context.Context, req ShipmentRequest) (Shipment, error) {
	var out simShipment
	err := s.call(ctx, http.MethodPost, "/v1/shipments", req, &out)
	return Shipment(out), err
}

// Label implements Provider: the label PDF.
func (s *Sim) Label(ctx context.Context, awb string) ([]byte, error) {
	res, err := s.do(ctx, http.MethodGet, "/v1/shipments/"+awb+"/label", nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 2<<20))
}

// Cancel implements Provider.
func (s *Sim) Cancel(ctx context.Context, awb string) error {
	var out simShipment
	return s.call(ctx, http.MethodPost, "/v1/shipments/"+awb+"/cancel", nil, &out)
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
	var e struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			AWB      string `json:"awb"`
			Ref      string `json:"ref"`
			Kind     string `json:"kind"`
			Status   string `json:"status"`
			Location string `json:"location"`
			At       string `json:"at"`
			ETA      string `json:"eta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || e.Type != "shipment.status" || e.Data.AWB == "" || e.Data.Status == "" {
		return Event{}, ErrMalformed
	}
	at, err := time.Parse(time.RFC3339, e.Data.At)
	if err != nil {
		return Event{}, ErrMalformed
	}
	eta, _ := time.Parse(time.RFC3339, e.Data.ETA)
	return Event{ID: e.ID, AWB: e.Data.AWB, Ref: e.Data.Ref, Kind: e.Data.Kind, Status: e.Data.Status, Location: e.Data.Location, At: at, ETA: eta, Raw: body}, nil
}
