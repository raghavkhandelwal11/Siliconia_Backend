package checkout_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/launch"
)

func (e *env) available(account uuid.UUID) (avail, pending int32, invariant int32) {
	e.t.Helper()
	b, err := e.led.GetBalance(e.ctx, account)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT coalesce(sum(amount), 0) FROM ledger_entries WHERE account_id = $1
		AND type NOT IN ('EARN_PURCHASE', 'EARN_CREATOR_POOL', 'EARN_PROMOTER', 'REVERSAL')`, account).Scan(&invariant)
	return b.Available, b.Pending, invariant
}

func (e *env) accountID(phone string) uuid.UUID {
	var id uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM accounts WHERE phone_e164 = $1 AND role = 'BUYER'`, "+91"+phone).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestElectronsEarnMatureSpendRestoreExpire(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 10)
	cable, _, _ := e.listed("Cable Store", "Volt Cable", 4000, 10)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")
	me := e.accountID("9876511111")

	// A member earns 2% (rounded up) on a paid order; it stays pending until the return window closes.
	o, itemID := e.delivered(b, addr, bank, seller, "COD", "key-earn-0001")
	if avail, pending, _ := e.available(me); avail != 0 || pending != 56 {
		t.Fatalf("after delivery: available %d pending %d", avail, pending)
	}
	_ = o
	if _, err := e.pool.Exec(e.ctx, `UPDATE electron_lots SET available_at = now() - interval '1 minute' WHERE source_ref = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.led.Mature(e.ctx); err != nil || n != 1 {
		t.Fatalf("mature: %d %v", n, err)
	}
	if avail, pending, inv := e.available(me); avail != 56 || pending != 0 || inv != 56 {
		t.Fatalf("matured: %d %d invariant %d", avail, pending, inv)
	}

	// Spend 50 on an online order: the payable drops by ₹50.
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 1}, true)
	if st, q := b.do("POST", "/checkout/quote", map[string]any{"addressId": addr, "payMethod": "ONLINE", "electrons": 57}, true); st != 422 {
		t.Fatalf("more than available: %d %v", st, q)
	}
	_, q := b.do("POST", "/checkout/quote", map[string]any{"addressId": addr, "payMethod": "ONLINE", "electrons": 50}, true)
	if q["electronsMax"].(float64) != 56 || q["electronsUsed"].(float64) != 50 || q["payablePaise"].(float64) != 279900-5000 {
		t.Fatalf("quote with Electrons: %v", q)
	}
	_, ord := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"], "electrons": 50}, true, "key-spend-001")
	e.pay(ord["payment"].(map[string]any)["checkoutUrl"].(string))
	if avail, _, inv := e.available(me); avail != 6 || inv != 6 {
		t.Fatalf("after spending: %d invariant %d", avail, inv)
	}
	var paid int64
	_ = e.pool.QueryRow(e.ctx, `SELECT amount_paise FROM payments WHERE ref = $1`, ord["number"]).Scan(&paid)
	if paid != 274900 {
		t.Fatalf("charged %d", paid)
	}
	// Cancelling gives the Electrons back and refunds only the money part.
	if res, _ := b.raw("POST", "/sub-orders/"+subOrder(ord)["id"].(string)+"/cancel", map[string]string{"reason": "Changed my mind"}); res.StatusCode != 204 {
		t.Fatalf("cancel: %d", res.StatusCode)
	}
	e.sim.Wait()
	var refunded int64
	_ = e.pool.QueryRow(e.ctx, `SELECT amount_paise FROM refunds WHERE ref = $1`, subOrder(ord)["id"].(string)+"-CANCEL").Scan(&refunded)
	if avail, _, inv := e.available(me); avail != 56 || inv != 56 || refunded != 274900 {
		t.Fatalf("after cancel: available %d invariant %d refunded %d", avail, inv, refunded)
	}

	// Paid fully with Electrons: confirmed at once, no payment page.
	b.do("PUT", "/cart/items/"+cable.String(), map[string]int{"qty": 1}, true)
	_, q = b.do("POST", "/checkout/quote", map[string]any{"addressId": addr, "payMethod": "ONLINE", "electrons": 40}, true)
	if q["electronsOnly"] != true || q["payablePaise"].(float64) != 0 {
		t.Fatalf("Electrons-only quote: %v", q)
	}
	st, ord := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"], "electrons": 40}, true, "key-eonly-001")
	if st != 201 || ord["status"] != "CONFIRMED" || ord["payMethod"] != "ELECTRONS_ONLY" {
		t.Fatalf("Electrons-only order: %d %v", st, ord)
	}
	if avail, _, _ := e.available(me); avail != 16 {
		t.Fatalf("after Electrons-only: %d", avail)
	}

	// Expiry removes what is left; the ledger still balances.
	if _, err := e.pool.Exec(e.ctx, `UPDATE electron_lots SET expires_at = now() - interval '1 second' WHERE account_id = $1 AND status = 'AVAILABLE'`, me); err != nil {
		t.Fatal(err)
	}
	if n, err := e.led.Expire(e.ctx); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if avail, _, inv := e.available(me); avail != 0 || inv != 0 {
		t.Fatalf("after expiry: %d invariant %d", avail, inv)
	}
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM ledger_entries WHERE account_id = $1`, me); err == nil {
		t.Fatal("the ledger must be append-only")
	}
	_, hist := b.do("GET", "/electrons/ledger", nil, false)
	if len(hist["items"].([]any)) < 6 {
		t.Fatalf("history: %v", hist)
	}
}

func TestMembershipPurchaseRenewAndCOD(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 10)
	b := e.browser()
	b.signIn("9876544441", "BUYER")
	_, a := b.do("POST", "/me/addresses", map[string]any{"name": "Asha Rao", "phone": "9876500000", "line1": "12 MG Road",
		"city": "Bengaluru", "state": "Karnataka", "pincode": "560001"}, true)
	addr := a["id"].(string)
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 1}, true)
	if q := b.quote(addr, "COD"); q["codEligible"] != false || q["codReason"] != "NOT_MEMBER" || q["isMember"] != false {
		t.Fatalf("COD is for members: %v", q)
	}

	_, m := b.do("GET", "/membership", nil, false)
	if m["status"] != "NONE" || m["feePaise"].(float64) != 65000 {
		t.Fatalf("no membership: %v", m)
	}
	st, co := b.do("POST", "/membership/purchase", map[string]any{"electrons": 0, "autoRenew": true}, true)
	if st != 200 || co["status"] != "PENDING_PAYMENT" {
		t.Fatalf("purchase: %d %v", st, co)
	}
	e.pay(co["checkoutUrl"].(string))
	_, m = b.do("GET", "/membership", nil, false)
	if m["status"] != "ACTIVE" || m["isMember"] != true || m["hasMandate"] != true || m["nextChargeAt"] == nil {
		t.Fatalf("active: %v", m)
	}
	if q := b.quote(addr, "COD"); q["codEligible"] != true || q["isMember"] != true {
		t.Fatalf("member COD: %v", q)
	}

	// Renewal day: the mandate is charged and the year extends.
	me := e.accountID("9876544441")
	if _, err := e.pool.Exec(e.ctx, `UPDATE memberships SET expires_at = now() - interval '1 minute' WHERE account_id = $1`, me); err != nil {
		t.Fatal(err)
	}
	if c, l, err := e.mem.Renew(e.ctx); err != nil || c != 1 || l != 0 {
		t.Fatalf("renew: %d %d %v", c, l, err)
	}
	e.sim.Wait()
	var exp time.Time
	_ = e.pool.QueryRow(e.ctx, `SELECT expires_at FROM memberships WHERE account_id = $1`, me).Scan(&exp)
	if time.Until(exp) < 360*24*time.Hour {
		t.Fatalf("not renewed: %v", exp)
	}
	// Auto-renew off revokes the mandate; when it expires, it lapses.
	if st, m := b.do("PATCH", "/membership", map[string]bool{"autoRenew": false}, true); st != 200 || m["autoRenew"] != false || m["hasMandate"] != false {
		t.Fatalf("auto-renew off: %d %v", st, m)
	}
	if st, _ := b.do("PATCH", "/membership", map[string]bool{"autoRenew": true}, true); st != 409 {
		t.Fatalf("auto-renew on needs a new mandate: %d", st)
	}
	_, _ = e.pool.Exec(e.ctx, `UPDATE memberships SET expires_at = now() - interval '1 minute' WHERE account_id = $1`, me)
	if _, l, _ := e.mem.Renew(e.ctx); l != 1 {
		t.Fatalf("lapse: %d", l)
	}
	if _, m := b.do("GET", "/membership", nil, false); m["status"] != "LAPSED" || m["isMember"] != false {
		t.Fatalf("lapsed: %v", m)
	}
}

// launchFor creates a live launch of deal units on a platform-owned product.
func (e *env) launchFor(units, perMember int32, rps *int32) (uuid.UUID, uuid.UUID) {
	e.t.Helper()
	variant, product, seller := e.listed("Siliconics Launch Store", "Nova Launch", 2000000, 5)
	if _, err := e.pool.Exec(e.ctx, `UPDATE seller_profiles SET is_platform = true WHERE account_id = $1`, seller); err != nil {
		e.t.Fatal(err)
	}
	admin := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO accounts (id, role, phone_e164, approval) VALUES ($1, 'ADMIN', $2, 'APPROVED')`, admin, "+9190"+admin.String()[:8]); err != nil {
		e.t.Fatal(err)
	}
	d := launch.Draft{ProductID: product, VariantID: variant, SellerID: seller, UnitsBought: units, UnitCostPaise: 100, NormalPricePaise: 2000000,
		DealDiscountBps: 8000, PerMemberLimit: perMember, StartsAt: time.Now().Add(time.Hour), WaitingRoomRPS: rps}
	if p, err := e.lau.Calculate(e.ctx, d); err != nil || p.DealUnits != int64(units) {
		e.t.Fatalf("preview: %+v %v", p, err)
	}
	l, err := e.lau.Create(e.ctx, admin, d)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.lau.Schedule(e.ctx, l.ID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE launches SET starts_at = now() - interval '1 second' WHERE id = $1`, l.ID); err != nil {
		e.t.Fatal(err)
	}
	if err := e.lau.Tick(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	return l.ID, variant
}

func TestLaunchReserveCheckoutAndRelease(t *testing.T) {
	e := setup(t)
	id, _ := e.launchFor(3, 1, nil)
	b, addr := e.buyer("9876511111")

	other := e.browser()
	other.signIn("9876544442", "BUYER")
	if st, o := other.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true); st != 403 || o["code"] != "MEMBERS_ONLY" {
		t.Fatalf("non-member: %d %v", st, o)
	}
	st, c := b.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true)
	if st != 201 || c["status"] != "HELD" {
		t.Fatalf("reserve: %d %v", st, c)
	}
	if st, o := b.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true); st != 409 || o["code"] != "MEMBER_LIMIT" {
		t.Fatalf("over the member limit: %d %v", st, o)
	}
	_, v := b.do("GET", "/launches/"+id.String(), nil, false)
	if v["status"] != "LIVE" || v["dealRemaining"].(float64) != 2 || v["held"].(float64) != 1 || len(v["myClaims"].([]any)) != 1 {
		t.Fatalf("launch view: %v", v)
	}

	// Checkout with the claim: deal price, no COD.
	claim := c["id"].(string)
	_, q := b.do("POST", "/checkout/quote", map[string]any{"addressId": addr, "payMethod": "COD", "launchClaimId": claim}, true)
	if q["launchDeal"] != true || q["codReason"] != "LAUNCH_DEAL" || q["payablePaise"].(float64) != 400000 {
		t.Fatalf("deal quote: %v", q)
	}
	_, q = b.do("POST", "/checkout/quote", map[string]any{"addressId": addr, "payMethod": "ONLINE", "launchClaimId": claim}, true)
	st, ord := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"], "launchClaimId": claim}, true, "key-launch-01")
	if st != 201 {
		t.Fatalf("deal order: %d %v", st, ord)
	}
	e.pay(ord["payment"].(map[string]any)["checkoutUrl"].(string))
	var status, priceType string
	_ = e.pool.QueryRow(e.ctx, `SELECT c.status, i.price_type FROM launch_claims c JOIN order_items i ON i.launch_claim_id = c.id WHERE c.id = $1`, claim).Scan(&status, &priceType)
	if status != "PAID" || priceType != "LAUNCH_DEAL" {
		t.Fatalf("claim %s price type %s", status, priceType)
	}

	// A hold that runs out goes back on sale.
	b2, _ := e.buyer("9876511112")
	_, c2 := b2.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE launch_claims SET held_until = now() - interval '1 second' WHERE id = $1`, c2["id"]); err != nil {
		t.Fatal(err)
	}
	if n, err := e.lau.Sweep(e.ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	var remaining int32
	_ = e.pool.QueryRow(e.ctx, `SELECT deal_remaining FROM launches WHERE id = $1`, id).Scan(&remaining)
	if redisLeft, _ := e.rdb.Get(e.ctx, "launch:"+id.String()+":remaining").Int(); remaining != 2 || redisLeft != 2 {
		t.Fatalf("released: db %d redis %d", remaining, redisLeft)
	}
	if st, _ := b2.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true); st != 201 {
		t.Fatalf("reserve again after release: %d", st)
	}
	// Releasing early by hand works too.
	b3, _ := e.buyer("9876511113")
	_, c3 := b3.do("POST", "/launches/"+id.String()+"/reserve", map[string]int{"qty": 1}, true)
	if res, _ := b3.raw("DELETE", "/launch-claims/"+c3["id"].(string), nil); res.StatusCode != 204 {
		t.Fatalf("release: %d", res.StatusCode)
	}
}

func TestWaitingRoom(t *testing.T) {
	e := setup(t)
	rps := int32(1)
	id, _ := e.launchFor(5, 1, &rps)
	b1, _ := e.buyer("9876511111")
	b2, _ := e.buyer("9876511112")
	path := "/launches/" + id.String()
	if st, o := b1.do("POST", path+"/reserve", map[string]int{"qty": 1}, true); st != 428 || o["code"] != "LAUNCH_QUEUE_REQUIRED" {
		t.Fatalf("queue first: %d %v", st, o)
	}
	_, q1 := b1.do("POST", path+"/queue", nil, true)
	_, q2 := b2.do("POST", path+"/queue", nil, true)
	if q1["position"].(float64) != 1 || q2["position"].(float64) != 2 {
		t.Fatalf("positions: %v %v", q1, q2)
	}
	if err := e.lau.Admit(e.ctx); err != nil {
		t.Fatal(err)
	}
	_, s1 := b1.do("GET", path+"/queue", nil, false)
	_, s2 := b2.do("GET", path+"/queue", nil, false)
	if s1["admitted"] != true || s1["admissionToken"] == nil || s2["admitted"] == true || s2["position"].(float64) != 1 {
		t.Fatalf("admit one per second: %v %v", s1, s2)
	}
	if st, _ := b2.do("POST", path+"/reserve", map[string]any{"qty": 1, "admissionToken": s1["admissionToken"]}, true); st != 403 {
		t.Fatalf("another member's pass: %d", st)
	}
	if st, o := b1.do("POST", path+"/reserve", map[string]any{"qty": 1, "admissionToken": s1["admissionToken"]}, true); st != 201 {
		t.Fatalf("admitted reserve: %d %v", st, o)
	}
}

// TestNoOversellUnderLoad is T3.7: 5,000 members race for 50 deal units.
func TestNoOversellUnderLoad(t *testing.T) {
	e := setup(t)
	id, _ := e.launchFor(50, 1, nil)
	if _, err := e.pool.Exec(e.ctx, `
		WITH a AS (
			INSERT INTO accounts (id, role, phone_e164, approval)
			SELECT gen_random_uuid(), 'BUYER', '+917' || lpad(g::text, 9, '0'), 'APPROVED' FROM generate_series(1, 5000) g
			RETURNING id)
		INSERT INTO memberships (id, account_id, status, started_at, expires_at)
		SELECT gen_random_uuid(), id, 'ACTIVE', now(), now() + interval '1 year' FROM a`); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(e.ctx, `SELECT id FROM accounts WHERE phone_e164 LIKE '+917%'`)
	if err != nil {
		t.Fatal(err)
	}
	var members []uuid.UUID
	for rows.Next() {
		var m uuid.UUID
		_ = rows.Scan(&m)
		members = append(members, m)
	}
	rows.Close()

	var ok, soldOut, other atomic.Int64
	work := make(chan uuid.UUID)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range work {
				_, err := e.lau.Reserve(e.ctx, m, id, 1, "")
				switch {
				case err == nil:
					ok.Add(1)
				case err == launch.ErrSoldOut:
					soldOut.Add(1)
				default:
					other.Add(1)
					t.Logf("unexpected: %v", err)
				}
			}
		}()
	}
	for _, m := range members {
		work <- m
	}
	close(work)
	wg.Wait()

	var remaining, held int64
	_ = e.pool.QueryRow(e.ctx, `SELECT deal_remaining FROM launches WHERE id = $1`, id).Scan(&remaining)
	_ = e.pool.QueryRow(e.ctx, `SELECT coalesce(sum(qty), 0) FROM launch_claims WHERE launch_id = $1 AND status = 'HELD'`, id).Scan(&held)
	redisLeft, _ := e.rdb.Get(e.ctx, "launch:"+id.String()+":remaining").Int64()
	if len(members) != 5000 || ok.Load() != 50 || held != 50 || remaining != 0 || redisLeft != 0 || soldOut.Load() != 4950 || other.Load() != 0 {
		t.Fatalf("members %d ok %d soldOut %d other %d held %d remaining %d redis %d",
			len(members), ok.Load(), soldOut.Load(), other.Load(), held, remaining, redisLeft)
	}
}
