package pay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
)

type hooks struct {
	mu     sync.Mutex
	events []Event
	fail   int // reject this many deliveries first (tests retries)
}

func (h *hooks) handler(t *testing.T, secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := whsig.Verify(secret, r.Header.Get(whsig.Header), body, time.Now()); err != nil {
			t.Errorf("bad signature: %v", err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail > 0 {
			h.fail--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var e Event
		_ = json.Unmarshal(body, &e)
		h.events = append(h.events, e)
	})
}

func (h *hooks) types() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.events))
	for i, e := range h.events {
		out[i] = e.Type
	}
	return out
}

func setup(t *testing.T, h *hooks) (*Sim, *httptest.Server) {
	secret := []byte("whsec")
	hookSrv := httptest.NewServer(h.handler(t, secret))
	t.Cleanup(hookSrv.Close)
	s := New(Config{PublicURL: "http://shop.test", APIKey: "key", WebhookURL: hookSrv.URL, WebhookSecret: secret,
		CaptureDelay: 5 * time.Millisecond, PendingDelay: 20 * time.Millisecond, RetryBase: time.Millisecond, MaxAttempts: 5})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestUPISuccessCapturesWithSignedWebhook(t *testing.T) {
	h := &hooks{fail: 2} // the first two deliveries fail and are retried
	s, srv := setup(t, h)
	o, err := s.CreateOrder("ORD-1", 1899900, false, "http://shop.test/checkout/result")
	if err != nil || !strings.HasPrefix(o.CheckoutURL, "http://shop.test/pay/checkout/order_sim_") {
		t.Fatalf("create: %+v %v", o, err)
	}
	again, _ := s.CreateOrder("ORD-1", 1899900, false, "")
	if again.ID != o.ID {
		t.Fatal("same ref should return the same order")
	}

	// Submitting the hosted form redirects back to the shop while capture runs.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.PostForm(srv.URL+"/pay/checkout/"+o.ID, url.Values{"method": {"UPI"}, "vpa": {"success@sim"}})
	if err != nil || res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "status=processing") {
		t.Fatalf("submit: %v %v", res, err)
	}
	s.Wait()
	got, _ := s.GetOrder(o.ID)
	if got.Status != Captured || got.CapturedAt == nil {
		t.Fatalf("after capture: %+v", got)
	}
	if ts := h.types(); len(ts) != 1 || ts[0] != "payment.captured" {
		t.Fatalf("webhooks: %v", ts)
	}
}

func TestFixtureOutcomes(t *testing.T) {
	h := &hooks{}
	s, _ := setup(t, h)
	pay := func(ref, method, cred string) Order {
		o, _ := s.CreateOrder(ref, 10000, false, "")
		got, err := s.Pay(o.ID, method, cred)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		return got
	}
	if o := pay("A", "UPI", "failure@sim"); o.Status != Failed || o.FailureCode != "UPI_DECLINED" {
		t.Fatalf("failure@sim: %+v", o)
	}
	if o := pay("B", "CARD", "4000 0000 0000 0002"); o.Status != Failed || o.FailureCode != "CARD_DECLINED" {
		t.Fatalf("declined card: %+v", o)
	}
	timeout := pay("C", "CARD", "4000000000009995")
	slow := pay("D", "NETBANKING", "SIMB_SLOW")
	s.Wait()
	if o, _ := s.GetOrder(timeout.ID); o.Status != Pending {
		t.Fatalf("timeout card should stay pending: %+v", o)
	}
	if o, _ := s.GetOrder(slow.ID); o.Status != Captured {
		t.Fatalf("slow bank should capture later: %+v", o)
	}
	o, _ := s.CreateOrder("E", 10000, false, "")
	if _, err := s.Pay(o.ID, "UPI", "someone@okbank"); err != ErrBadInput {
		t.Fatalf("real-looking VPA must be refused: %v", err)
	}
}

func TestRefundsMandatesAndSettlement(t *testing.T) {
	h := &hooks{}
	s, srv := setup(t, h)
	o, _ := s.CreateOrder("MEM-1", 65000, true, "")
	if _, err := s.Pay(o.ID, "UPI", "success@sim"); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	o, _ = s.GetOrder(o.ID)
	if o.MandateID == "" {
		t.Fatalf("mandate not created: %+v", o)
	}
	if _, err := s.CreateRefund(o.PaymentID, 70000, "R-too-much"); err != ErrConflict {
		t.Fatalf("over-refund: %v", err)
	}
	r, err := s.CreateRefund(o.PaymentID, 20000, "R-1")
	if err != nil || r.Status != "PROCESSING" {
		t.Fatalf("refund: %+v %v", r, err)
	}
	if again, _ := s.CreateRefund(o.PaymentID, 20000, "R-1"); again.ID != r.ID {
		t.Fatal("refund ref should be idempotent")
	}
	renewal, err := s.ChargeMandate(o.MandateID, 65000, "RENEW-1")
	if err != nil || renewal.Status != Pending {
		t.Fatalf("charge: %+v %v", renewal, err)
	}
	s.Wait()
	if err := s.RevokeMandate(o.MandateID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChargeMandate(o.MandateID, 65000, "RENEW-2"); err != ErrConflict {
		t.Fatalf("charge after revoke: %v", err)
	}
	s.Wait()
	want := map[string]int{"payment.captured": 2, "mandate.active": 1, "refund.processed": 1, "mandate.revoked": 1}
	got := map[string]int{}
	for _, ty := range h.types() {
		got[ty]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("webhooks %v, want %v", got, want)
		}
	}

	today := time.Now().In(ist).Format("2006-01-02")
	st, _ := s.Settle(today)
	if st.Gross != 130000 || st.Refunded != 20000 || st.Net != 110000 {
		t.Fatalf("settlement: %+v", st)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/settlements?date="+today+"&format=csv", nil)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("settlement without key: %d", res.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer key")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("settlement csv: %v %v", res, err)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "net,,,110000") {
		t.Fatalf("csv: %s", body)
	}
}

func TestRupees(t *testing.T) {
	for paise, want := range map[int64]string{1899900: "₹18,999", 12345678900: "₹12,34,56,789", 65050: "₹650.50"} {
		if got := rupees(paise); got != want {
			t.Errorf("rupees(%d) = %s, want %s", paise, got, want)
		}
	}
}
