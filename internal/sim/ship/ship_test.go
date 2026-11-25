package ship

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

type inbox struct {
	mu     sync.Mutex
	byAWB  map[string][]string
	ids    map[string]int
	secret []byte
}

func (b *inbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if whsig.Verify(b.secret, r.Header.Get(whsig.Header), body, time.Now()) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var e Event
	_ = json.Unmarshal(body, &e)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ids[e.ID]++
	if b.ids[e.ID] == 1 {
		awb := e.Data["awb"].(string)
		b.byAWB[awb] = append(b.byAWB[awb], e.Data["status"].(string))
	}
}

func newSim(t *testing.T) (*Sim, *inbox) {
	t.Helper()
	in := &inbox{byAWB: map[string][]string{}, ids: map[string]int{}, secret: []byte("s")}
	srv := httptest.NewServer(in)
	t.Cleanup(srv.Close)
	return New(Config{APIKey: "k", WebhookURL: srv.URL, WebhookSecret: in.secret, Step: 5 * time.Millisecond, DuplicateRate: 0.5,
		RetryBase: time.Millisecond, MaxAttempts: 3, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}), in
}

var (
	seller = Address{Name: "Volt Store", Phone: "9876500001", Line1: "Plot 4, Sector 18", City: "Noida", State: "Uttar Pradesh", Pincode: "201301"}
	buyer  = func(pin string) Address {
		return Address{Name: "Asha Rao", Phone: "9876500000", Line1: "12 MG Road", City: "Bengaluru", State: "Karnataka", Pincode: pin}
	}
)

func TestServiceability(t *testing.T) {
	cases := map[string]Serviceability{
		"560001": {Pincode: "560001", Deliverable: true, COD: true, ETADays: 2, Region: "Karnataka"},
		"302001": {Pincode: "302001", Deliverable: true, COD: true, ETADays: 4, Region: "Rajasthan"},
		"791001": {Pincode: "791001", Deliverable: true, COD: false, ETADays: 6, Region: "North East"},
		"999996": {Pincode: "999996", Deliverable: true, COD: false, ETADays: 3, Region: "Test"},
		"999997": {Pincode: "999997"},
		"560":    {Pincode: "560"},
		"290001": {Pincode: "290001"},
		"900001": {Pincode: "900001"},
	}
	for pin, want := range cases {
		if got := Check(pin); got != want {
			t.Errorf("%s: got %+v want %+v", pin, got, want)
		}
	}
}

func TestForwardShipmentIsTrackedToDelivery(t *testing.T) {
	s, in := newSim(t)
	sh, err := s.CreateShipment(ShipmentRequest{Ref: "SX-1-A", From: seller, To: buyer("560001"), CODAmount: 64900})
	if err != nil || !strings.HasPrefix(sh.AWB, "SX") || len(sh.AWB) != 14 || sh.Status != Manifested {
		t.Fatalf("create: %+v %v", sh, err)
	}
	if again, _ := s.CreateShipment(ShipmentRequest{Ref: "SX-1-A", From: seller, To: buyer("560001"), CODAmount: 64900}); again.AWB != sh.AWB {
		t.Fatal("same ref should return the same shipment")
	}
	s.Wait()
	got, _ := s.Get(sh.AWB)
	if got.Status != Delivered || len(got.Events) != 6 {
		t.Fatalf("final: %+v", got)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if n := len(in.byAWB[sh.AWB]); n != 6 {
		t.Fatalf("webhooks (deduplicated): %v", in.byAWB[sh.AWB])
	}
}

func TestTestPinsPickTheOutcome(t *testing.T) {
	s, _ := newSim(t)
	rto, _ := s.CreateShipment(ShipmentRequest{Ref: "rto", From: seller, To: buyer(PinRTO)})
	retry, _ := s.CreateShipment(ShipmentRequest{Ref: "retry", From: seller, To: buyer(PinFailedThenOK)})
	if _, err := s.CreateShipment(ShipmentRequest{Ref: "nope", From: seller, To: buyer(PinNotServiceable)}); err != ErrNotServiceable {
		t.Fatalf("unserviceable PIN: %v", err)
	}
	if _, err := s.CreateShipment(ShipmentRequest{Ref: "cod", From: seller, To: buyer(PinPrepaidOnly), CODAmount: 100}); err != ErrNotServiceable {
		t.Fatalf("COD to a prepaid-only PIN: %v", err)
	}
	s.Wait()
	if got, _ := s.Get(rto.AWB); got.Status != RTODelivered {
		t.Fatalf("RTO: %+v", got.Events)
	}
	got, _ := s.Get(retry.AWB)
	statuses := []string{}
	for _, e := range got.Events {
		statuses = append(statuses, e.Status)
	}
	if got.Status != Delivered || !strings.Contains(strings.Join(statuses, ","), FailedAttempt) {
		t.Fatalf("failed attempt then delivered: %v", statuses)
	}
}

func TestReturnPickupCancelAndAdvance(t *testing.T) {
	s, _ := newSim(t)
	s.cfg.Step = time.Hour // nothing moves on its own
	pickup, err := s.CreateShipment(ShipmentRequest{Ref: "RET-1", Kind: Return, From: buyer("560001"), To: seller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateShipment(ShipmentRequest{Ref: "RET-2", Kind: Return, From: buyer("560001"), To: seller, CODAmount: 5}); err != ErrBadInput {
		t.Fatalf("returns carry no COD: %v", err)
	}
	if sh, err := s.Advance(pickup.AWB); err != nil || sh.Status != PickedUp {
		t.Fatalf("advance: %+v %v", sh, err)
	}
	if _, err := s.Cancel(pickup.AWB); err != ErrConflict {
		t.Fatalf("cannot cancel after pickup: %v", err)
	}
	fwd, _ := s.CreateShipment(ShipmentRequest{Ref: "SX-2-A", From: seller, To: buyer("560001")})
	if sh, err := s.Cancel(fwd.AWB); err != nil || sh.Status != Cancelled {
		t.Fatalf("cancel: %+v %v", sh, err)
	}
	if _, err := s.Advance(fwd.AWB); err != ErrConflict {
		t.Fatalf("cancelled shipments stay put: %v", err)
	}
}

func TestHTTPAndLabel(t *testing.T) {
	s, _ := newSim(t)
	s.cfg.Step = time.Hour
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(method, path string, body any) *http.Response {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, r)
		req.Header.Set("Authorization", "Bearer k")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res, _ := http.Get(srv.URL + "/v1/serviceability/560001"); res.StatusCode != 401 {
		t.Fatalf("no key: %d", res.StatusCode)
	}
	res := call("POST", "/v1/shipments", ShipmentRequest{Ref: "SX-3-A", From: seller, To: buyer("560001"), CODAmount: 1899900})
	var sh Shipment
	_ = json.NewDecoder(res.Body).Decode(&sh)
	if res.StatusCode != 200 || sh.AWB == "" {
		t.Fatalf("create: %d %+v", res.StatusCode, sh)
	}
	if res := call("POST", "/v1/shipments", ShipmentRequest{Ref: "x", From: seller, To: buyer("999997")}); res.StatusCode != 422 {
		t.Fatalf("unserviceable: %d", res.StatusCode)
	}
	res = call("GET", "/v1/shipments/"+sh.AWB+"/label", nil)
	pdf, _ := io.ReadAll(res.Body)
	if res.Header.Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) ||
		!bytes.Contains(pdf, []byte(sh.AWB)) || !bytes.Contains(pdf, []byte("Collect Rs 18,999")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatalf("label: %s", pdf)
	}
}
