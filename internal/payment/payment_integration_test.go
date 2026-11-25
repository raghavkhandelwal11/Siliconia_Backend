package payment_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	simpay "github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/pay"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func TestPaymentLoopWithSimPay(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE payments, refunds, webhook_events, outbox CASCADE`); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := []byte("whsec-test")

	// The API, then sim-pay pointed at the API's webhook (every event sent twice).
	svc := &payment.Service{Pool: pool, Q: store.New(pool), Log: log}
	api := httptest.NewServer(server.New(server.Deps{Log: log, Health: &health.Handler{}, Payment: &payment.Handler{Svc: svc}}))
	defer api.Close()
	sim := simpay.New(simpay.Config{PublicURL: "http://shop.test", APIKey: "k", WebhookURL: api.URL + "/api/v1/webhooks/payment",
		WebhookSecret: secret, CaptureDelay: 10 * time.Millisecond, PendingDelay: time.Second, DuplicateRate: 1,
		RetryBase: 5 * time.Millisecond, MaxAttempts: 5, Log: log})
	simSrv := httptest.NewServer(sim.Handler())
	defer simSrv.Close()
	svc.Provider = provider.NewSim(simSrv.URL, "k", secret)

	co, err := svc.Start(ctx, "ORDER", "SX-1001", nil, 1899900, false, "http://shop.test/checkout/result")
	if err != nil || !strings.Contains(co.CheckoutURL, "/pay/checkout/") {
		t.Fatalf("start: %+v %v", co, err)
	}
	if again, _ := svc.Start(ctx, "ORDER", "SX-1001", nil, 1899900, false, ""); again.PaymentID != co.PaymentID {
		t.Fatal("same ref should reuse the payment")
	}

	// The shopper pays on the hosted page.
	id := co.CheckoutURL[strings.LastIndex(co.CheckoutURL, "/")+1:]
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if res, err := noRedirect.PostForm(simSrv.URL+"/pay/checkout/"+id, url.Values{"method": {"CARD"}, "card": {"4111 1111 1111 1111"}}); err != nil || res.StatusCode != 303 {
		t.Fatalf("pay: %v %v", res, err)
	}
	sim.Wait()

	p, err := svc.Q.GetPayment(ctx, co.PaymentID)
	if err != nil || p.Status != "CAPTURED" || p.ProviderPaymentID == nil || !p.Method.Valid || p.Method.PayMethod != "CARD" {
		t.Fatalf("payment: %+v %v", p, err)
	}
	var events, captured int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic = 'payment.captured'`).Scan(&captured)
	if events != 1 || captured != 1 {
		t.Fatalf("duplicate deliveries must apply once: events=%d captured=%d", events, captured)
	}

	// A forged webhook is refused.
	body := `{"id":"evt_forged","type":"payment.captured","data":{"orderId":"x"}}`
	req, _ := http.NewRequest("POST", api.URL+"/api/v1/webhooks/payment", strings.NewReader(body))
	req.Header.Set(whsig.Header, whsig.Sign([]byte("wrong"), []byte(body), time.Now()))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 400 {
		t.Fatalf("forged webhook: %d", res.StatusCode)
	}

	// Refund part of it; the webhook completes it.
	r, err := svc.Refund(ctx, co.PaymentID, 500000, 0, "SX-1001-R1", "return")
	if err != nil || r.Status != "PROCESSING" {
		t.Fatalf("refund: %+v %v", r, err)
	}
	if _, err := svc.Refund(ctx, co.PaymentID, 1500000, 0, "SX-1001-R2", "too much"); err == nil {
		t.Fatal("refund above the remaining amount must fail")
	}
	sim.Wait()
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM refunds WHERE ref = 'SX-1001-R1'`).Scan(&status)
	var refundedEvents int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic = 'refund.completed'`).Scan(&refundedEvents)
	if status != "REFUNDED" || refundedEvents != 1 {
		t.Fatalf("refund after webhook: %s events=%d", status, refundedEvents)
	}
}
