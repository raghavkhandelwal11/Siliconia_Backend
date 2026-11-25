package checkout_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/admin"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	paymentprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func (e *env) m5() *admin.Service {
	box, _ := crypto.NewBox(config.DevKey("encryption-master"))
	s := &admin.Service{Pool: e.pool, Q: store.New(e.pool), Box: box, Payout: e.prog.Payout, Log: e.prog.Log}
	e.prog.SellerPayout = s.OnPayoutEvent
	return s
}

func (e *env) admin() uuid.UUID {
	id := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO accounts (id, role, phone_e164, approval) VALUES ($1, 'ADMIN', $2, 'APPROVED')`, id, "+9170"+id.String()[:8]); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) bank(s *admin.Service, seller uuid.UUID, account string) {
	sealed, _ := s.Box.Seal([]byte(account), []byte(seller.String()+":bank"))
	if _, err := e.pool.Exec(e.ctx, `UPDATE seller_profiles SET bank_enc = $2, ifsc = 'HDFC0001234' WHERE account_id = $1`, seller, sealed); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) payoutOf(seller uuid.UUID) store.Payout {
	e.t.Helper()
	var p store.Payout
	for i := 0; i < 200; i++ {
		ps, err := store.New(e.pool).SellerPayouts(e.ctx, seller)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(ps) > 0 && ps[0].Status != "PROCESSING" {
			return ps[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("payout still processing: %+v", p)
	return p
}

func TestSellerPayoutBatchSettlesAndRetriesFailures(t *testing.T) {
	e := setup(t)
	m5 := e.m5()
	good, _, sellerA := e.listed("Volt Store", "Volt Cell", 100000, 5)
	bad, _, sellerB := e.listed("Bad Bank Store", "Bad Cell", 50000, 5)
	e.pickup(sellerA)
	e.pickup(sellerB)
	e.bank(m5, sellerA, "123456789012")
	e.bank(m5, sellerB, "000000000000") // the simulator rejects this account
	b, addr := e.buyer("9876500051")
	e.delivered(b, addr, good, sellerA, "COD", "key-po-a")
	e.delivered(b, addr, bad, sellerB, "COD", "key-po-b")

	// Still inside the return window: nothing is due.
	if _, err := m5.CreateBatch(e.ctx, nil); !errors.Is(err, admin.ErrNothingDue) {
		t.Fatalf("batch inside return window: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE sub_orders SET return_window_ends_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	// Two batch runs at once: each sub-order lands in exactly one batch.
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); ids[i], errs[i] = m5.CreateBatch(e.ctx, nil) }()
	}
	wg.Wait()
	var batch uuid.UUID
	ok := 0
	for i := range 2 {
		if errs[i] == nil {
			ok++
			batch = ids[i]
		}
	}
	var claims int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM payout_claims`).Scan(&claims)
	if ok != 1 || claims != 2 {
		t.Fatalf("concurrent batches: ok=%d claims=%d errs=%v", ok, claims, errs)
	}

	approver, other := e.admin(), e.admin()
	if err := m5.Release(e.ctx, approver, batch); err == nil {
		t.Fatal("released before approval")
	}
	if err := m5.Approve(e.ctx, approver, batch); err != nil {
		t.Fatal(err)
	}
	if err := m5.Release(e.ctx, other, batch); err != nil {
		t.Fatal(err)
	}

	// COD orders: commission at the category rate, TDS 0.1% of gross.
	pa := e.payoutOf(sellerA)
	if pa.Status != "PAID" || pa.GrossPaise != 100000 || pa.TdsPaise != 100 || pa.NetPaise != pa.GrossPaise-pa.CommissionPaise-pa.TdsPaise || pa.BankRef == nil {
		t.Fatalf("seller A payout: %+v", pa)
	}
	var settled int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM sub_orders WHERE seller_id = $1 AND settled_at IS NOT NULL`, sellerA).Scan(&settled)
	if settled != 1 {
		t.Fatalf("seller A sub-order not settled")
	}
	pb := e.payoutOf(sellerB)
	if pb.Status != "FAILED" {
		t.Fatalf("seller B payout: %+v", pb)
	}
	if e.outbox("payout.finished") != 2 {
		t.Fatalf("payout events: %d", e.outbox("payout.finished"))
	}

	// The failed seller's sub-order goes into the next batch; the paid one does not.
	next, err := m5.CreateBatch(e.ctx, &approver)
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := m5.Q.BatchPayouts(e.ctx, next)
	if len(ps) != 1 || ps[0].SellerID != sellerB {
		t.Fatalf("retry batch: %+v", ps)
	}
	if err := m5.Cancel(e.ctx, approver, next); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM payout_claims c JOIN payouts p ON p.id = c.payout_id WHERE p.batch_id = $1`, next).Scan(&claims)
	if claims != 0 {
		t.Fatalf("cancel kept claims: %d", claims)
	}

	// Statement: the seller sees only their own payout.
	if _, err := m5.Statement(e.ctx, sellerB, pa.ID, false); err == nil {
		t.Fatal("seller read another seller's statement")
	}
	if csv, err := m5.Statement(e.ctx, sellerA, pa.ID, false); err != nil || len(csv) == 0 {
		t.Fatalf("statement: %v", err)
	}
	var audits int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'payout_batch.%' AND actor_id IN ($1, $2)`, approver, other).Scan(&audits)
	if audits != 4 { // approve, release, create, cancel
		t.Fatalf("audit rows: %d", audits)
	}
}

func TestLedgerAdjustmentNeedsSecondAdmin(t *testing.T) {
	e := setup(t)
	m5 := e.m5()
	e.buyer("9876500052")
	acc := e.accountID("9876500052")
	a, b := e.admin(), e.admin()

	r, err := m5.RequestAdjustment(e.ctx, a, acc, 50, "Goodwill credit")
	if err != nil {
		t.Fatal(err)
	}
	if err := m5.DecideAdjustment(e.ctx, a, r.ID, true); err == nil {
		t.Fatal("requester approved their own adjustment")
	}
	if err := m5.DecideAdjustment(e.ctx, b, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := m5.DecideAdjustment(e.ctx, b, r.ID, true); err == nil {
		t.Fatal("applied twice")
	}
	d, _ := m5.RequestAdjustment(e.ctx, a, acc, -20, "Correct duplicate")
	if err := m5.DecideAdjustment(e.ctx, b, d.ID, true); err != nil {
		t.Fatal(err)
	}
	over, _ := m5.RequestAdjustment(e.ctx, a, acc, -100, "Too much")
	if err := m5.DecideAdjustment(e.ctx, b, over.ID, true); err == nil {
		t.Fatal("debited more than the balance")
	}
	if avail, _, inv := e.available(acc); avail != 30 || inv != 30 {
		t.Fatalf("balance %d invariant %d", avail, inv)
	}
	if n, err := m5.CheckLedger(e.ctx); err != nil || n != 0 {
		t.Fatalf("ledger check: %d %v", n, err)
	}
}

type fakeSettler []paymentprovider.Settled

func (f fakeSettler) Settlement(context.Context, string) ([]paymentprovider.Settled, error) {
	return f, nil
}

func TestHealthIncidentsConfigAndReconciliation(t *testing.T) {
	e := setup(t)
	m5 := e.m5()
	admin := e.admin()
	msg := "HTTP 503"

	// One failure is noise; two in a row open one incident and alert once.
	for i := 0; i < 3; i++ {
		if err := m5.Record(e.ctx, "search", false, 12, &msg); err != nil {
			t.Fatal(err)
		}
	}
	var open int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM incidents WHERE resolved_at IS NULL`).Scan(&open)
	if open != 1 || e.outbox("health.incident_opened") != 1 {
		t.Fatalf("incidents %d alerts %d", open, e.outbox("health.incident_opened"))
	}
	if err := m5.Record(e.ctx, "search", true, 8, nil); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM incidents WHERE resolved_at IS NULL`).Scan(&open)
	if open != 0 {
		t.Fatal("incident not resolved")
	}

	// Config: optimistic version check and history.
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO config (key, value) VALUES ('payouts.tds_bps', '10')`); err != nil {
		t.Fatal(err)
	}
	if err := m5.SetConfig(e.ctx, admin, "payouts.tds_bps", []byte("20"), 7); err == nil {
		t.Fatal("stale version accepted")
	}
	c, _ := m5.Q.GetConfig(e.ctx, "payouts.tds_bps")
	if err := m5.SetConfig(e.ctx, admin, "payouts.tds_bps", []byte("20"), c.Version); err != nil {
		t.Fatal(err)
	}
	if h, _ := m5.Q.ConfigHistory(e.ctx, "payouts.tds_bps"); len(h) != 1 {
		t.Fatalf("history: %+v", h)
	}

	// Reconciliation: one amount mismatch and one payment the gateway lacks.
	day := time.Now()
	for _, p := range []struct {
		ref string
		amt int64
	}{{"ORD-A", 1000}, {"ORD-B", 2000}, {"ORD-C", 3000}} {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO payments (id, purpose, ref, provider, amount_paise, status, captured_at) VALUES ($1, 'ORDER', $2, 'sim-pay', $3, 'CAPTURED', $4)`,
			uuid.New(), p.ref, p.amt, day); err != nil {
			t.Fatal(err)
		}
	}
	n, err := m5.Reconcile(e.ctx, fakeSettler{{Ref: "ORD-A", Amount: 1000}, {Ref: "ORD-B", Amount: 2500}}, day)
	if err != nil || n != 2 {
		t.Fatalf("recon: %d %v", n, err)
	}

	// Suspension revokes sessions and is audited; terms need re-acceptance.
	e.buyer("9876500053")
	acc := e.accountID("9876500053")
	if err := m5.SetAccountStatus(e.ctx, admin, acc, true, "Fraud review"); err != nil {
		t.Fatal(err)
	}
	var live int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM sessions WHERE account_id = $1 AND revoked_at IS NULL`, acc).Scan(&live)
	if live != 0 {
		t.Fatalf("sessions still live: %d", live)
	}
	for _, k := range []string{"TERMS", "PRIVACY"} { // the test reset empties the seeded documents
		if _, err := m5.PublishLegal(e.ctx, admin, k, k, "Text."); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := m5.Q.PendingLegal(e.ctx, acc)
	if len(pending) != 2 {
		t.Fatalf("pending legal: %d", len(pending))
	}
	if err := m5.AcceptLegal(e.ctx, acc, []uuid.UUID{pending[0].ID, pending[1].ID}, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if _, err := m5.PublishLegal(e.ctx, admin, "TERMS", "Terms of use", "Updated terms."); err != nil {
		t.Fatal(err)
	}
	if pending, _ = m5.Q.PendingLegal(e.ctx, acc); len(pending) != 1 || pending[0].Version != 2 {
		t.Fatalf("re-acceptance: %+v", pending)
	}

	// Admin endpoints stay closed to buyers.
	b, _ := e.buyer("9876500054")
	if st, _ := b.do("GET", "/admin/overview", nil, false); st != 403 {
		t.Fatalf("buyer reached admin overview: %d", st)
	}
}
