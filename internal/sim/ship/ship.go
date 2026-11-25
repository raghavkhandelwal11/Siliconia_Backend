// Package ship is sim-ship, the free shipping aggregator simulator (LLD 8.5,
// 12): PIN serviceability with COD and delivery estimates, shipments with an
// AWB and a label PDF, tracking updates sent as signed webhooks one step at a
// time, returns to origin, return pickups and cancellation. Test PIN codes
// pick the outcome. State lives in memory.
package ship

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

// Shipment kinds.
const (
	Forward = "FORWARD" // seller → buyer
	Return  = "RETURN"  // buyer → seller (return pickup)
)

// Tracking statuses.
const (
	Manifested     = "MANIFESTED"
	PickedUp       = "PICKED_UP"
	InTransit      = "IN_TRANSIT"
	OutForDelivery = "OUT_FOR_DELIVERY"
	FailedAttempt  = "FAILED_ATTEMPT"
	Delivered      = "DELIVERED"
	RTOInitiated   = "RTO_INITIATED"
	RTODelivered   = "RTO_DELIVERED"
	Cancelled      = "CANCELLED"
)

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

// ShipmentRequest books a shipment.
type ShipmentRequest struct {
	Ref       string   `json:"ref"` // the shop's reference (sub-order or return); idempotent
	Kind      string   `json:"kind"`
	From      Address  `json:"from"`
	To        Address  `json:"to"`
	Parcels   []Parcel `json:"parcels"`
	CODAmount int64    `json:"codAmount"` // paise to collect on delivery (forward only)
}

// TrackingEvent is one scan.
type TrackingEvent struct {
	Status   string    `json:"status"`
	Location string    `json:"location"`
	At       time.Time `json:"at"`
}

// Shipment is a booked consignment.
type Shipment struct {
	AWB       string          `json:"awb"`
	Ref       string          `json:"ref"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	From      Address         `json:"from"`
	To        Address         `json:"to"`
	Parcels   []Parcel        `json:"parcels"`
	CODAmount int64           `json:"codAmount"`
	ETA       time.Time       `json:"eta"`
	CreatedAt time.Time       `json:"createdAt"`
	Events    []TrackingEvent `json:"events"`
	plan      []TrackingEvent // steps still to come, in order
	gen       int             // bumped on every step; a timer only applies its own generation
}

// Event is a webhook body.
type Event struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Created int64          `json:"created"`
	Data    map[string]any `json:"data"`
}

// Config tunes the simulator.
type Config struct {
	APIKey        string // the API's bearer key for /v1
	WebhookURL    string // the API's /webhooks/shipping
	WebhookSecret []byte
	Step          time.Duration // time between tracking steps (SIM_SHIP_SPEED, 1 min in dev)
	DuplicateRate float64       // share of webhooks sent twice (~5% outside production)
	RetryBase     time.Duration
	MaxAttempts   int
	Client        *http.Client
	Log           *slog.Logger
	Now           func() time.Time
}

// Sim is the simulator state.
type Sim struct {
	cfg       Config
	mu        sync.Mutex
	shipments map[string]*Shipment
	byRef     map[string]string // kind:ref → awb
	wg        sync.WaitGroup
}

// New creates a simulator.
func New(cfg Config) *Sim {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 10
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.Step == 0 {
		cfg.Step = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Sim{cfg: cfg, shipments: map[string]*Shipment{}, byRef: map[string]string{}}
}

// Wait blocks until every scheduled step and webhook delivery has finished (tests).
func (s *Sim) Wait() { s.wg.Wait() }

var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrBadInput       = errors.New("bad input")
	ErrNotServiceable = errors.New("not serviceable")
)

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// newAWB is a 12-digit air waybill number, like the aggregators issue.
func newAWB() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(9e11))
	return fmt.Sprintf("SX%012d", n.Int64()+1e11)
}

func validAddress(a Address) bool {
	return strings.TrimSpace(a.Name) != "" && strings.TrimSpace(a.Line1) != "" && strings.TrimSpace(a.City) != "" && pinRE.MatchString(a.Pincode)
}

// CreateShipment books a shipment; the same kind and ref return the same one.
func (s *Sim) CreateShipment(req ShipmentRequest) (Shipment, error) {
	if req.Kind == "" {
		req.Kind = Forward
	}
	if req.Ref == "" || len(req.Ref) > 80 || (req.Kind != Forward && req.Kind != Return) || !validAddress(req.From) || !validAddress(req.To) ||
		req.CODAmount < 0 || (req.Kind == Return && req.CODAmount > 0) {
		return Shipment{}, ErrBadInput
	}
	if len(req.Parcels) == 0 {
		req.Parcels = []Parcel{{WeightGrams: 500, Description: "Electronics"}}
	}
	// A forward shipment needs the buyer's PIN served; a pickup needs the buyer's PIN too.
	buyerPin := req.To.Pincode
	if req.Kind == Return {
		buyerPin = req.From.Pincode
	}
	sv := Check(buyerPin)
	if !sv.Deliverable || (req.CODAmount > 0 && !sv.COD) {
		return Shipment{}, ErrNotServiceable
	}

	s.mu.Lock()
	key := req.Kind + ":" + req.Ref
	if awb, ok := s.byRef[key]; ok {
		out := snapshot(s.shipments[awb])
		s.mu.Unlock()
		return out, nil
	}
	now := s.cfg.Now()
	sh := &Shipment{AWB: newAWB(), Ref: req.Ref, Kind: req.Kind, Status: Manifested, From: req.From, To: req.To, Parcels: req.Parcels,
		CODAmount: req.CODAmount, ETA: now.Add(time.Duration(sv.ETADays) * 24 * time.Hour), CreatedAt: now}
	sh.Events = []TrackingEvent{{Status: Manifested, Location: req.From.City, At: now}}
	sh.plan = plan(req, buyerPin)
	s.shipments[sh.AWB], s.byRef[key] = sh, sh.AWB
	out := snapshot(sh)
	s.mu.Unlock()

	s.emit(out, out.Events[0])
	s.schedule(out.AWB, 0)
	return out, nil
}

// plan lays out the tracking steps for a shipment. Test PINs pick the ending.
func plan(req ShipmentRequest, buyerPin string) []TrackingEvent {
	from, to := req.From.City, req.To.City
	ev := func(status, where string) TrackingEvent { return TrackingEvent{Status: status, Location: where} }
	steps := []TrackingEvent{
		ev(PickedUp, from),
		ev(InTransit, from+" hub"),
		ev(InTransit, to+" hub"),
		ev(OutForDelivery, to),
	}
	if req.Kind == Forward {
		switch buyerPin {
		case PinRTO:
			return append(steps, ev(FailedAttempt, to), ev(OutForDelivery, to), ev(FailedAttempt, to),
				ev(RTOInitiated, to+" hub"), ev(InTransit, from+" hub"), ev(RTODelivered, from))
		case PinFailedThenOK:
			return append(steps, ev(FailedAttempt, to), ev(OutForDelivery, to), ev(Delivered, to))
		}
	}
	return append(steps, ev(Delivered, to))
}

// schedule sets the single timer for a shipment's next step. Steps run one
// after another, so a slow machine can delay them but never reorder them.
func (s *Sim) schedule(awb string, gen int) {
	s.after(s.cfg.Step, func() { s.step(awb, gen) })
}

// step applies the next planned step unless the shipment moved on since the
// timer was set (Advance) or was cancelled.
func (s *Sim) step(awb string, gen int) {
	s.mu.Lock()
	sh := s.shipments[awb]
	if sh == nil || sh.Status == Cancelled || sh.gen != gen || len(sh.plan) == 0 {
		s.mu.Unlock()
		return
	}
	e := s.next(sh)
	snap, more, next := snapshot(sh), len(sh.plan) > 0, sh.gen
	s.mu.Unlock()
	s.emit(snap, e)
	if more {
		s.schedule(awb, next)
	}
}

// next moves the shipment one step on. Callers hold the lock.
func (s *Sim) next(sh *Shipment) TrackingEvent {
	e := sh.plan[0]
	e.At = s.cfg.Now()
	sh.plan = sh.plan[1:]
	sh.Events = append(sh.Events, e)
	sh.Status = e.Status
	sh.gen++
	return e
}

// Advance applies the next tracking step now (testers and demos).
func (s *Sim) Advance(awb string) (Shipment, error) {
	s.mu.Lock()
	sh := s.shipments[awb]
	if sh == nil {
		s.mu.Unlock()
		return Shipment{}, ErrNotFound
	}
	if sh.Status == Cancelled || len(sh.plan) == 0 {
		s.mu.Unlock()
		return Shipment{}, ErrConflict
	}
	e := s.next(sh)
	out, more, next := snapshot(sh), len(sh.plan) > 0, sh.gen
	s.mu.Unlock()
	s.emit(out, e)
	if more {
		s.schedule(awb, next) // restart the clock from this step
	}
	return out, nil
}

// Cancel stops a shipment that has not been picked up yet.
func (s *Sim) Cancel(awb string) (Shipment, error) {
	s.mu.Lock()
	sh := s.shipments[awb]
	if sh == nil {
		s.mu.Unlock()
		return Shipment{}, ErrNotFound
	}
	if sh.Status != Manifested {
		s.mu.Unlock()
		return Shipment{}, ErrConflict
	}
	e := TrackingEvent{Status: Cancelled, Location: sh.From.City, At: s.cfg.Now()}
	sh.Status, sh.plan = Cancelled, nil
	sh.Events = append(sh.Events, e)
	out := snapshot(sh)
	s.mu.Unlock()
	s.emit(out, e)
	return out, nil
}

// Get returns a shipment by AWB.
func (s *Sim) Get(awb string) (Shipment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shipments[awb]
	if sh == nil {
		return Shipment{}, ErrNotFound
	}
	return snapshot(sh), nil
}

func snapshot(sh *Shipment) Shipment {
	out := *sh
	out.Events = append([]TrackingEvent(nil), sh.Events...)
	out.plan = nil
	return out
}

func (s *Sim) after(d time.Duration, fn func()) {
	s.wg.Add(1)
	time.AfterFunc(d, func() {
		defer s.wg.Done()
		fn()
	})
}

// emit sends a signed shipment.status webhook with retries and, sometimes, a
// deliberate duplicate.
func (s *Sim) emit(sh Shipment, e TrackingEvent) {
	if s.cfg.WebhookURL == "" {
		return
	}
	evt := Event{ID: newID("evt_ship"), Type: "shipment.status", Created: s.cfg.Now().Unix(), Data: map[string]any{
		"awb": sh.AWB, "ref": sh.Ref, "kind": sh.Kind, "status": e.Status, "location": e.Location, "at": e.At.UTC().Format(time.RFC3339Nano),
		"eta": sh.ETA.UTC().Format(time.RFC3339), "codAmount": sh.CODAmount,
	}}
	body, _ := json.Marshal(evt)
	copies := 1
	if s.cfg.DuplicateRate > 0 && mrand.Float64() < s.cfg.DuplicateRate {
		copies = 2
	}
	for i := 0; i < copies; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.deliver(evt, body)
		}()
	}
}

func (s *Sim) deliver(evt Event, body []byte) {
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(whsig.Header, whsig.Sign(s.cfg.WebhookSecret, body, s.cfg.Now()))
		res, err := s.cfg.Client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		wait := time.Duration(math.Min(float64(s.cfg.RetryBase)*math.Pow(2, float64(attempt-1)), float64(time.Hour)))
		s.cfg.Log.Warn("webhook retry", "event", evt.ID, "attempt", attempt, "err", err)
		time.Sleep(wait)
	}
	s.cfg.Log.Error("webhook gave up", "event", evt.ID)
}
