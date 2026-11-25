// Package pay is sim-pay, the free payment gateway simulator (LLD 12): a
// hosted checkout page, signed webhooks, refunds, mandates and a daily
// settlement report. Outcomes follow fixed test fixtures so testers can pick
// success, failure, pending or timeout. State lives in memory; the API
// reconciles through GetPayment and expires unpaid orders on its own.
package pay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

// Statuses.
const (
	Created  = "CREATED"
	Pending  = "PENDING"
	Captured = "CAPTURED"
	Failed   = "FAILED"
	Refunded = "REFUNDED"
)

// Order is a checkout the API created.
type Order struct {
	ID          string     `json:"id"`
	Ref         string     `json:"ref"`
	Amount      int64      `json:"amount"`
	Mandate     bool       `json:"mandate"`
	ReturnURL   string     `json:"returnUrl"`
	Status      string     `json:"status"`
	PaymentID   string     `json:"paymentId,omitempty"`
	Method      string     `json:"method,omitempty"`
	FailureCode string     `json:"failureCode,omitempty"`
	MandateID   string     `json:"mandateId,omitempty"`
	CheckoutURL string     `json:"checkoutUrl"`
	CreatedAt   time.Time  `json:"createdAt"`
	CapturedAt  *time.Time `json:"capturedAt,omitempty"`
	refunded    int64
}

// Refund is money sent back for a captured payment.
type Refund struct {
	ID        string     `json:"id"`
	Ref       string     `json:"ref"`
	PaymentID string     `json:"paymentId"`
	Amount    int64      `json:"amount"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	DoneAt    *time.Time `json:"processedAt,omitempty"`
}

// Mandate is a standing instruction for renewals (UPI AutoPay / card SI).
type Mandate struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // ACTIVE, FAILED, REVOKED
	Method    string `json:"method"`
	MaxAmount int64  `json:"maxAmount"`
}

// Event is a webhook body.
type Event struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Created int64          `json:"created"`
	Data    map[string]any `json:"data"`
}

// Config tunes the simulator (shorter delays in tests).
type Config struct {
	PublicURL     string // where browsers reach /pay, e.g. http://localhost:8080
	APIKey        string // the API's bearer key for /v1
	WebhookURL    string // the API's /webhooks/payment
	WebhookSecret []byte
	CaptureDelay  time.Duration // success@sim (2 s)
	PendingDelay  time.Duration // pending@sim (60 s)
	DuplicateRate float64       // share of webhooks sent twice (~5% outside production)
	RetryBase     time.Duration // first webhook retry (1 s)
	MaxAttempts   int           // webhook attempts (10)
	Client        *http.Client
	Log           *slog.Logger
	Now           func() time.Time
}

// Sim is the simulator state.
type Sim struct {
	cfg      Config
	mu       sync.Mutex
	orders   map[string]*Order
	byRef    map[string]string
	payments map[string]string // payment id → order id
	refunds  map[string]*Refund
	refByRef map[string]string
	mandates map[string]*Mandate
	wg       sync.WaitGroup
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
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Sim{cfg: cfg, orders: map[string]*Order{}, byRef: map[string]string{}, payments: map[string]string{},
		refunds: map[string]*Refund{}, refByRef: map[string]string{}, mandates: map[string]*Mandate{}}
}

// Wait blocks until scheduled work and webhook deliveries finish (tests).
func (s *Sim) Wait() { s.wg.Wait() }

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrBadInput = errors.New("bad input")
)

// CreateOrder starts a checkout; the same ref returns the same order.
func (s *Sim) CreateOrder(ref string, amount int64, mandate bool, returnURL string) (Order, error) {
	if ref == "" || len(ref) > 64 || amount <= 0 || amount > 1e12 {
		return Order{}, ErrBadInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byRef[ref]; ok {
		return *s.orders[id], nil
	}
	o := &Order{ID: newID("order_sim"), Ref: ref, Amount: amount, Mandate: mandate, ReturnURL: returnURL, Status: Created, CreatedAt: s.cfg.Now()}
	o.CheckoutURL = strings.TrimRight(s.cfg.PublicURL, "/") + "/pay/checkout/" + o.ID
	s.orders[o.ID], s.byRef[ref] = o, o.ID
	return *o, nil
}

// GetOrder returns an order by id.
func (s *Sim) GetOrder(id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	return *o, nil
}

// Outcome of a test credential.
type outcome int

const (
	succeed outcome = iota
	fail
	slow    // pending@sim: captures after PendingDelay
	never   // timeout: stays pending
	invalid // not a test credential
)

// Fixtures (LLD 12).
func upiOutcome(vpa string) (outcome, string) {
	switch strings.ToLower(strings.TrimSpace(vpa)) {
	case "success@sim", "mandate-fail@sim":
		return succeed, ""
	case "failure@sim":
		return fail, "UPI_DECLINED"
	case "pending@sim":
		return slow, ""
	case "timeout@sim":
		return never, ""
	}
	return invalid, ""
}

func cardOutcome(number string) (outcome, string) {
	switch strings.NewReplacer(" ", "", "-", "").Replace(number) {
	case "4111111111111111":
		return succeed, ""
	case "4000000000000002":
		return fail, "CARD_DECLINED"
	case "4000000000009995":
		return never, ""
	}
	return invalid, ""
}

// Banks lists the test net-banking banks and their outcomes.
var Banks = []struct{ Code, Name string }{
	{"SIMB_OK", "SIM Bank (succeeds)"},
	{"SIMB_FAIL", "SIM Bank (declines)"},
	{"SIMB_SLOW", "SIM Bank (slow, about a minute)"},
}

func bankOutcome(code string) (outcome, string) {
	switch code {
	case "SIMB_OK":
		return succeed, ""
	case "SIMB_FAIL":
		return fail, "BANK_DECLINED"
	case "SIMB_SLOW":
		return slow, ""
	}
	return invalid, ""
}

// Pay handles a submitted checkout form. It returns the status to show
// (PENDING or FAILED); captures happen later and are announced by webhook.
func (s *Sim) Pay(orderID, method, credential string) (Order, error) {
	var out outcome
	var code string
	switch method {
	case "UPI":
		out, code = upiOutcome(credential)
	case "CARD":
		out, code = cardOutcome(credential)
	case "NETBANKING":
		out, code = bankOutcome(credential)
	default:
		return Order{}, ErrBadInput
	}
	if out == invalid {
		return Order{}, ErrBadInput
	}
	s.mu.Lock()
	o, ok := s.orders[orderID]
	if !ok {
		s.mu.Unlock()
		return Order{}, ErrNotFound
	}
	if o.Status != Created && o.Status != Failed { // a failed attempt may be retried
		s.mu.Unlock()
		return *o, ErrConflict
	}
	o.Method, o.FailureCode = method, ""
	if o.PaymentID == "" {
		o.PaymentID = newID("pay_sim")
		s.payments[o.PaymentID] = o.ID
	}
	mandateFails := strings.EqualFold(strings.TrimSpace(credential), "mandate-fail@sim")
	switch out {
	case fail:
		o.Status, o.FailureCode = Failed, code
		snap := *o
		s.mu.Unlock()
		s.emit("payment.failed", paymentData(snap))
		return snap, nil
	default:
		o.Status = Pending
	}
	snap := *o
	s.mu.Unlock()
	switch out {
	case succeed:
		s.after(s.cfg.CaptureDelay, func() { s.capture(orderID, mandateFails) })
	case slow:
		s.after(s.cfg.PendingDelay, func() { s.capture(orderID, false) })
	}
	return snap, nil
}

func (s *Sim) after(d time.Duration, fn func()) {
	s.wg.Add(1)
	time.AfterFunc(d, func() {
		defer s.wg.Done()
		fn()
	})
}

func (s *Sim) capture(orderID string, mandateFails bool) {
	s.mu.Lock()
	o := s.orders[orderID]
	if o == nil || o.Status != Pending {
		s.mu.Unlock()
		return
	}
	now := s.cfg.Now()
	o.Status, o.CapturedAt = Captured, &now
	var m *Mandate
	if o.Mandate {
		m = &Mandate{ID: newID("mandate_sim"), Status: "ACTIVE", Method: o.Method, MaxAmount: o.Amount}
		if mandateFails {
			m.Status = "FAILED"
		}
		s.mandates[m.ID] = m
		o.MandateID = m.ID
	}
	snap := *o
	s.mu.Unlock()
	s.emit("payment.captured", paymentData(snap))
	if m != nil {
		typ := "mandate.active"
		if m.Status == "FAILED" {
			typ = "mandate.failed"
		}
		s.emit(typ, map[string]any{"mandateId": m.ID, "orderId": snap.ID, "ref": snap.Ref, "status": m.Status, "method": m.Method})
	}
}

func paymentData(o Order) map[string]any {
	d := map[string]any{"orderId": o.ID, "ref": o.Ref, "paymentId": o.PaymentID, "amount": o.Amount, "method": o.Method, "status": o.Status}
	if o.FailureCode != "" {
		d["failureCode"] = o.FailureCode
	}
	if o.MandateID != "" {
		d["mandateId"] = o.MandateID
	}
	return d
}

// CreateRefund refunds part or all of a captured payment; the same ref
// returns the same refund. Processing finishes after CaptureDelay.
func (s *Sim) CreateRefund(paymentID string, amount int64, ref string) (Refund, error) {
	if ref == "" || amount <= 0 {
		return Refund{}, ErrBadInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.refByRef[ref]; ok {
		return *s.refunds[id], nil
	}
	o := s.orders[s.payments[paymentID]]
	if o == nil {
		return Refund{}, ErrNotFound
	}
	if o.Status != Captured || o.refunded+amount > o.Amount {
		return Refund{}, ErrConflict
	}
	o.refunded += amount
	r := &Refund{ID: newID("rfnd_sim"), Ref: ref, PaymentID: paymentID, Amount: amount, Status: "PROCESSING", CreatedAt: s.cfg.Now()}
	s.refunds[r.ID], s.refByRef[ref] = r, r.ID
	s.after(s.cfg.CaptureDelay, func() {
		s.mu.Lock()
		now := s.cfg.Now()
		r.Status, r.DoneAt = Refunded, &now
		data := map[string]any{"refundId": r.ID, "ref": r.Ref, "paymentId": r.PaymentID, "amount": r.Amount, "status": r.Status, "orderRef": o.Ref}
		s.mu.Unlock()
		s.emit("refund.processed", data)
	})
	return *r, nil
}

// ChargeMandate takes a renewal payment from an active mandate.
func (s *Sim) ChargeMandate(mandateID string, amount int64, ref string) (Order, error) {
	s.mu.Lock()
	m := s.mandates[mandateID]
	if m == nil {
		s.mu.Unlock()
		return Order{}, ErrNotFound
	}
	if m.Status != "ACTIVE" || amount > m.MaxAmount*2 {
		s.mu.Unlock()
		return Order{}, ErrConflict
	}
	method := m.Method
	s.mu.Unlock()
	o, err := s.CreateOrder(ref, amount, false, "")
	if err != nil {
		return Order{}, err
	}
	s.mu.Lock()
	oo := s.orders[o.ID]
	if oo.Status == Created {
		oo.Status, oo.Method, oo.MandateID = Pending, method, mandateID
		oo.PaymentID = newID("pay_sim")
		s.payments[oo.PaymentID] = oo.ID
	}
	snap := *oo
	s.mu.Unlock()
	s.after(s.cfg.CaptureDelay, func() { s.capture(snap.ID, false) })
	return snap, nil
}

// RevokeMandate stops future charges.
func (s *Sim) RevokeMandate(mandateID string) error {
	s.mu.Lock()
	m := s.mandates[mandateID]
	if m == nil {
		s.mu.Unlock()
		return ErrNotFound
	}
	m.Status = "REVOKED"
	s.mu.Unlock()
	s.emit("mandate.revoked", map[string]any{"mandateId": mandateID, "status": "REVOKED"})
	return nil
}

// Settlement is the daily report of money captured and refunded (IST day).
type Settlement struct {
	Date     string   `json:"date"`
	Payments []Order  `json:"payments"`
	Refunds  []Refund `json:"refunds"`
	Gross    int64    `json:"gross"`
	Refunded int64    `json:"refunded"`
	Fees     int64    `json:"fees"`
	Net      int64    `json:"net"`
}

var ist = time.FixedZone("IST", 5*3600+1800)

// Settle reports captures and processed refunds for an IST date (YYYY-MM-DD).
func (s *Sim) Settle(date string) (Settlement, error) {
	day, err := time.ParseInLocation("2006-01-02", date, ist)
	if err != nil {
		return Settlement{}, ErrBadInput
	}
	end := day.Add(24 * time.Hour)
	in := func(t *time.Time) bool { return t != nil && !t.Before(day) && t.Before(end) }
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Settlement{Date: date, Payments: []Order{}, Refunds: []Refund{}}
	for _, o := range s.orders {
		if in(o.CapturedAt) {
			out.Payments = append(out.Payments, *o)
			out.Gross += o.Amount
		}
	}
	for _, r := range s.refunds {
		if in(r.DoneAt) {
			out.Refunds = append(out.Refunds, *r)
			out.Refunded += r.Amount
		}
	}
	sort.Slice(out.Payments, func(i, j int) bool { return out.Payments[i].CapturedAt.Before(*out.Payments[j].CapturedAt) })
	sort.Slice(out.Refunds, func(i, j int) bool { return out.Refunds[i].DoneAt.Before(*out.Refunds[j].DoneAt) })
	out.Net = out.Gross - out.Refunded - out.Fees // the simulator charges no fees
	return out, nil
}

// emit delivers a signed webhook with retries (exponential backoff, capped at
// an hour) and, sometimes, a deliberate duplicate.
func (s *Sim) emit(typ string, data map[string]any) {
	if s.cfg.WebhookURL == "" {
		return
	}
	evt := Event{ID: newID("evt_sim"), Type: typ, Created: s.cfg.Now().Unix(), Data: data}
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
		s.cfg.Log.Warn("webhook retry", "event", evt.ID, "type", evt.Type, "attempt", attempt, "err", err)
		time.Sleep(wait)
	}
	s.cfg.Log.Error("webhook gave up", "event", evt.ID, "type", evt.Type)
}
