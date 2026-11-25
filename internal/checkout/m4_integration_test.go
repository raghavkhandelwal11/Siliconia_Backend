package checkout_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/programmes"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func (e *env) productOf(variant uuid.UUID) uuid.UUID {
	var id uuid.UUID
	_ = e.pool.QueryRow(e.ctx, `SELECT product_id FROM variants WHERE id = $1`, variant).Scan(&id)
	return id
}

func TestBuyerReviewsVotesAndModeration(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 10)
	e.pickup(seller)
	product := e.productOf(bank)
	b, addr := e.buyer("9876511111")

	pending := e.placeCOD(b, addr, bank, "key-rev-pend1")
	pendingItem := subOrder(pending)["items"].([]any)[0].(map[string]any)["id"].(string)
	if st, _ := b.do("POST", "/order-items/"+pendingItem+"/review", map[string]any{"rating": 5}, true); st != 403 {
		t.Fatalf("review before delivery: %d", st)
	}
	_, item := e.delivered(b, addr, bank, seller, "COD", "key-rev-del01")

	var img bytes.Buffer
	_ = png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 400, 400)))
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/uploads/review-images", bytes.NewReader(img.Bytes()))
	req.Header.Set("X-CSRF-Token", b.cookie("sx_csrf"))
	res, err := (&http.Client{Jar: b.jar}).Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("photo upload: %v %v", res, err)
	}
	var up struct{ ID string }
	_ = jsonDecode(res, &up)

	st, r := b.do("POST", "/order-items/"+item+"/review", map[string]any{"rating": 4, "title": "Solid", "body": "Charges fast.", "photoIds": []string{up.ID}}, true)
	if st != 201 {
		t.Fatalf("review: %d %v", st, r)
	}
	if st, _ := b.do("POST", "/order-items/"+item+"/review", map[string]any{"rating": 5}, true); st != 409 {
		t.Fatalf("second review: %d", st)
	}
	_, list := b.do("GET", "/products/"+product.String()+"/reviews", nil, false)
	got := list["items"].([]any)[0].(map[string]any)
	sum := list["summary"].(map[string]any)
	if sum["count"].(float64) != 1 || sum["average"].(float64) != 4 || len(got["photos"].([]any)) != 1 || got["author"] != "Buyer" {
		t.Fatalf("list: %v", list)
	}
	rid := got["id"].(string)
	b.do("POST", "/reviews/"+rid+"/helpful", nil, true) // own review: not counted
	others := []*client{}
	for _, p := range []string{"9876511112", "9876511113", "9876511114"} {
		o, _ := e.buyer(p)
		others = append(others, o)
	}
	others[0].do("POST", "/reviews/"+rid+"/helpful", nil, true)
	others[0].do("POST", "/reviews/"+rid+"/helpful", nil, true)
	_, list = b.do("GET", "/products/"+product.String()+"/reviews", nil, false)
	if list["items"].([]any)[0].(map[string]any)["helpful"].(float64) != 1 {
		t.Fatalf("helpful counted once: %v", list)
	}
	for _, o := range others {
		if st, _ := o.do("POST", "/reviews/"+rid+"/report", map[string]string{"reason": "Off topic"}, true); st != 204 {
			t.Fatalf("report: %d", st)
		}
	}
	var rating float64
	var count int
	_ = e.pool.QueryRow(e.ctx, `SELECT rating_avg, rating_count FROM products WHERE id = $1`, product).Scan(&rating, &count)
	if _, l := b.do("GET", "/products/"+product.String()+"/reviews", nil, false); len(l["items"].([]any)) != 0 || count != 0 {
		t.Fatalf("three reports hide it: %v count %d", l, count)
	}
	if err := e.rev.Moderate(e.ctx, uuid.MustParse(rid), true); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT rating_avg, rating_count FROM products WHERE id = $1`, product).Scan(&rating, &count)
	if rating != 4 || count != 1 {
		t.Fatalf("republished: %v %d", rating, count)
	}

	// Feedback to the manufacturer needs a delivered purchase; the seller replies.
	if st, _ := others[0].do("POST", "/products/"+product.String()+"/feedback", map[string]string{"category": "FEATURE", "body": "Add a USB-C cable"}, true); st != 403 {
		t.Fatalf("feedback without purchase: %d", st)
	}
	if st, _ := b.do("POST", "/products/"+product.String()+"/feedback", map[string]string{"category": "FEATURE", "body": "Add a USB-C cable"}, true); st != 204 {
		t.Fatalf("feedback: %d", st)
	}
	fb, _ := e.rev.Q.SellerFeedback(e.ctx, seller)
	if len(fb) != 1 || e.rev.Reply(e.ctx, seller, fb[0].ID, "Coming next batch") != nil || e.rev.Reply(e.ctx, uuid.New(), fb[0].ID, "Nope") == nil {
		t.Fatalf("feedback reply: %+v", fb)
	}

	// Discovery: this week's products and bought-together.
	if err := e.rev.ComputeWeek(e.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, w := b.do("GET", "/home/products-of-week", nil, false); len(w["items"].([]any)) == 0 {
		t.Fatalf("products of the week: %v", w)
	}
}

func TestCreatorJourneyAndCashout(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 10)
	e.pickup(seller)
	product := e.productOf(bank)

	c := e.browser()
	c.signIn("9876522221", "CREATOR")
	var creator uuid.UUID
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM accounts WHERE phone_e164 = '+919876522221' AND role = 'CREATOR'`).Scan(&creator)
	app := map[string]any{"niche": "Gadgets", "bio": "Honest reviews", "socials": []map[string]any{{"platform": "YOUTUBE", "url": "https://youtube.com/@x", "followers": 400}}}
	c.do("PUT", "/creator/application", app, true)
	if st, o := c.do("POST", "/creator/application/submit", map[string]bool{"acceptTerms": true}, true); st != 422 || o["code"] != "CRITERIA_NOT_MET" {
		t.Fatalf("criteria: %d %v", st, o)
	}
	app["socials"] = []map[string]any{{"platform": "YOUTUBE", "url": "https://youtube.com/@x", "followers": 800}, {"platform": "INSTAGRAM", "url": "https://instagram.com/x", "followers": 300}}
	c.do("PUT", "/creator/application", app, true)
	if _, o := c.do("POST", "/creator/application/submit", map[string]bool{"acceptTerms": true}, true); o["approval"] != "SUBMITTED" {
		t.Fatalf("submitted: %v", o)
	}
	if st, _ := c.do("POST", "/creator/demo-requests", map[string]any{"productId": product}, true); st != 403 {
		t.Fatalf("demo before approval: %d", st)
	}
	if q, _ := e.prog.Queue(e.ctx, "CREATOR"); len(q) != 1 {
		t.Fatalf("queue: %v", q)
	}
	if err := e.prog.Decide(e.ctx, creator, true, ""); err != nil {
		t.Fatal(err)
	}

	// Demo: requested → seller approves (courier to the creator's address) → delivered.
	_, _ = e.pool.Exec(e.ctx, `INSERT INTO addresses (id, account_id, name, phone, line1, city, state, pincode, is_default)
		VALUES (gen_random_uuid(), $1, 'Kiran', '9876522221', '5 Film Nagar', 'Hyderabad', 'Telangana', '500033', true)`, creator)
	if st, o := c.do("POST", "/creator/reviews", map[string]any{"productId": product, "rating": 5, "body": "Twenty characters at least here."}, true); st != 409 {
		t.Fatalf("review before demo: %d %v", st, o)
	}
	_, d := c.do("POST", "/creator/demo-requests", map[string]any{"productId": product, "note": "For a review video"}, true)
	if err := e.prog.DecideDemo(e.ctx, seller, uuid.MustParse(d["id"].(string)), true, ""); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	_, demos := c.do("GET", "/creator/demo-requests", nil, false)
	if demos["items"].([]any)[0].(map[string]any)["status"] != "DELIVERED" {
		t.Fatalf("demo delivered: %v", demos)
	}
	st, cr := c.do("POST", "/creator/reviews", map[string]any{"productId": product, "rating": 5, "body": "Great battery, honest numbers.", "videos": []string{"https://youtu.be/abc"}}, true)
	if st != 201 || cr["rank"].(float64) != 1 {
		t.Fatalf("creator review: %d %v", st, cr)
	}

	// Votes: only buyers with a delivered purchase count toward the pool.
	verified, addr := e.buyer("9876511111")
	e.delivered(verified, addr, bank, seller, "COD", "key-vote-0001")
	casual, _ := e.buyer("9876511112")
	verified.do("POST", "/creator-reviews/"+cr["id"].(string)+"/vote", map[string]int{"value": 1}, true)
	casual.do("POST", "/creator-reviews/"+cr["id"].(string)+"/vote", map[string]int{"value": 1}, true)
	_, crs := verified.do("GET", "/products/"+product.String()+"/creator-reviews", nil, false)
	if row := crs["items"].([]any)[0].(map[string]any); row["agrees"].(float64) != 2 || row["myVote"].(float64) != 1 {
		t.Fatalf("votes: %v", crs)
	}

	period := time.Now().In(time.FixedZone("IST", 19800)).Format("2006-01")
	if _, err := e.prog.SetPool(e.ctx, product, period, 1000); err != nil {
		t.Fatal(err)
	}
	if n, err := e.prog.Settle(e.ctx, period); err != nil || n != 1 {
		t.Fatalf("settle: %d %v", n, err)
	}
	if _, err := e.prog.SetPool(e.ctx, product, period, 5); err == nil {
		t.Fatal("a settled pool cannot change")
	}
	if n, err := e.led.Mature(e.ctx); err != nil || n < 1 {
		t.Fatalf("pool lot matures: %d %v", n, err)
	}
	if avail, _, inv := e.available(creator); avail != 1000 || inv != 1000 {
		t.Fatalf("creator Electrons: %d invariant %d", avail, inv)
	}

	// Cash-out: KYC first, at least 250, paid by sim-payout; a bad account gives them back.
	if st, o := c.do("POST", "/creator/cashouts", map[string]int{"electrons": 300}, true); st != 409 || o["code"] != "KYC_REQUIRED" {
		t.Fatalf("KYC first: %d %v", st, o)
	}
	if st, o := c.do("POST", "/creator/kyc", map[string]string{"pan": "ABCDE1234F", "account": "123456789012", "ifsc": "HDFC0000001"}, true); st != 200 || o["kycVerified"] != true || o["bankLast4"] != "9012" {
		t.Fatalf("KYC: %d %v", st, o)
	}
	if st, _ := c.do("POST", "/creator/cashouts", map[string]int{"electrons": 100}, true); st != 400 {
		t.Fatalf("minimum 250: %d", st)
	}
	if st, _ := c.do("POST", "/creator/cashouts", map[string]int{"electrons": 600}, true); st != 201 {
		t.Fatalf("cash-out: %d", st)
	}
	if avail, _, _ := e.available(creator); avail != 400 {
		t.Fatalf("held: %d", avail)
	}
	if n, err := e.prog.Batch(e.ctx); err != nil || n != 1 {
		t.Fatalf("batch: %d %v", n, err)
	}
	e.paysim.Wait()
	_, cos := c.do("GET", "/creator/cashouts", nil, false)
	if co := cos["items"].([]any)[0].(map[string]any); co["Status"] != "PAID" && co["status"] != "PAID" {
		t.Fatalf("paid: %v", cos)
	}
	// Change the bank to the failing test account and cash out the rest.
	c.do("POST", "/creator/kyc", map[string]string{"pan": "ABCDE1234F", "account": "000000000000", "ifsc": "HDFC0000001"}, true)
	c.do("POST", "/creator/cashouts", map[string]int{"electrons": 400}, true)
	_, _ = e.prog.Batch(e.ctx)
	e.paysim.Wait()
	if avail, _, inv := e.available(creator); avail != 400 || inv != 400 {
		t.Fatalf("failed transfer restores: %d invariant %d", avail, inv)
	}
}

func TestPromoterEarnsOnDeliveredSales(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 10)
	e.pickup(seller)
	product := e.productOf(bank)
	p := e.browser()
	p.signIn("9876533331", "PROMOTER")
	var promoter uuid.UUID
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM accounts WHERE phone_e164 = '+919876533331' AND role = 'PROMOTER'`).Scan(&promoter)
	if st, _ := p.do("PUT", "/promoter/application", map[string]string{"type": "INDIVIDUAL", "displayName": "Angel Ravi"}, true); st != 204 {
		t.Fatalf("apply: %d", st)
	}
	if err := e.prog.Decide(e.ctx, promoter, true, ""); err != nil {
		t.Fatal(err)
	}
	p.do("POST", "/promoter/investments", map[string]any{"sellerId": seller, "proof": "Seed round, March 2026"}, true)
	if st, _ := p.do("PUT", "/promoter/products/"+product.String()+"/promotion", nil, true); st != 403 {
		t.Fatalf("promote before confirmation: %d", st)
	}
	inv, _ := e.prog.Q.ListInvestments(e.ctx, storeInv(nil, &seller))
	if err := e.prog.DecideInvestment(e.ctx, seller, inv[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.do("PUT", "/promoter/products/"+product.String()+"/promotion", nil, true); st != 204 {
		t.Fatalf("promote: %d", st)
	}
	if err := e.prog.SetRewardRule(e.ctx, seller, product, 10); err != nil {
		t.Fatal(err)
	}
	_, list := p.do("GET", "/products/"+product.String()+"/promoters", nil, false)
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("promoted by: %v", list)
	}
	b, addr := e.buyer("9876511111")
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 1}, true)
	e.delivered(b, addr, bank, seller, "COD", "key-promo-001")
	if _, pending, _ := e.available(promoter); pending != 10 {
		t.Fatalf("promoter pending: %d", pending)
	}
	_, _ = e.pool.Exec(e.ctx, `UPDATE electron_lots SET available_at = now() - interval '1 second' WHERE account_id = $1`, promoter)
	_, _ = e.led.Mature(e.ctx)
	if avail, _, _ := e.available(promoter); avail != 10 {
		t.Fatalf("promoter available: %d", avail)
	}
}

var _ = programmes.TermsVersion

func storeInv(promoter, seller *uuid.UUID) store.ListInvestmentsParams {
	return store.ListInvestmentsParams{PromoterID: promoter, SellerID: seller}
}

func jsonDecode(res *http.Response, v any) error {
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(v)
}
