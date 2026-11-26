package checkout_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
)

// TestNoCrossAccountAccess has a second buyer and a second seller try every
// ID-based endpoint on the first buyer's and seller's records.
func TestNoCrossAccountAccess(t *testing.T) {
	e := setup(t)
	anon := e.browser()
	for _, path := range []string{"/electrons", "/electrons/ledger", "/me/legal/pending"} {
		if st, _ := anon.do("GET", path, nil, false); st != 401 {
			t.Errorf("anonymous GET %s: %d", path, st)
		}
	}
	variant, product, sellerA := e.listed("Volt Store", "Volt Cell", 99900, 10)
	e.pickup(sellerA)
	alice, addrA := e.buyer("9876500061")
	open := e.placeCOD(alice, addrA, variant, "key-sec-open")
	openSub := subOrder(open)["id"].(string)
	done, itemA := e.delivered(alice, addrA, variant, sellerA, "COD", "key-sec-done")
	doneSub := subOrder(done)["id"].(string)
	st, ret := alice.do("POST", "/order-items/"+itemA+"/returns", map[string]any{"type": "RETURN", "reason": "DAMAGED"}, true)
	if st != 201 {
		t.Fatalf("alice return: %d %v", st, ret)
	}
	st, iss := alice.do("POST", "/issues", map[string]any{"orderId": done["id"], "type": "DAMAGED", "body": "The box was crushed on arrival."}, true)
	if st != 201 {
		t.Fatalf("alice issue: %d %v", st, iss)
	}

	bob, _ := e.buyer("9876500062")
	notMine := func(c *client, method, path string, body any) {
		t.Helper()
		st, out := c.do(method, path, body, method != "GET")
		if st < 400 || st == 500 {
			t.Errorf("%s %s reached another account's data: %d %v", method, path, st, out)
		}
	}
	notMine(bob, "GET", "/orders/"+open["id"].(string), nil)
	notMine(bob, "GET", "/invoices/"+doneSub, nil)
	notMine(bob, "POST", "/sub-orders/"+openSub+"/cancel", map[string]string{"reason": "x"})
	notMine(bob, "POST", "/orders/"+open["id"].(string)+"/pay", map[string]any{})
	notMine(bob, "POST", "/order-items/"+itemA+"/returns", map[string]any{"type": "RETURN", "reason": "DAMAGED"})
	notMine(bob, "POST", "/order-items/"+itemA+"/review", map[string]any{"rating": 1, "title": "", "body": "bad", "photoIds": []string{}})
	notMine(bob, "GET", "/issues/"+iss["id"].(string), nil)
	notMine(bob, "POST", "/issues/"+iss["id"].(string)+"/messages", map[string]string{"body": "hello"})
	notMine(bob, "POST", "/issues/"+iss["id"].(string)+"/reopen", map[string]string{})
	notMine(bob, "PUT", "/me/addresses/"+addrA, map[string]any{"name": "Bob", "phone": "+91 98765 00000", "line1": "1 Road", "city": "Bengaluru", "state": "Karnataka", "pincode": "560001"})
	notMine(bob, "DELETE", "/me/addresses/"+addrA, nil)
	notMine(bob, "POST", "/issues", map[string]any{"orderId": done["id"], "type": "DAMAGED", "body": "Not my order"})
	notMine(bob, "POST", "/checkout/quote", map[string]any{"payMethod": "COD", "addressId": addrA})
	var sessA string
	_ = e.pool.QueryRow(e.ctx, `SELECT s.id FROM sessions s JOIN accounts a ON a.id = s.account_id WHERE a.phone_e164 = '+919876500061' LIMIT 1`).Scan(&sessA)
	bob.do("DELETE", "/me/sessions/"+sessA, nil, true) // answers 204 either way (no account probing)
	var revoked bool
	_ = e.pool.QueryRow(e.ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE id = $1`, sessA).Scan(&revoked)
	if revoked {
		t.Error("bob revoked alice's session")
	}

	// A second, approved seller.
	sellerB := e.browser()
	sellerB.signIn("9876500063", "SELLER")
	var bID uuid.UUID
	_ = e.pool.QueryRow(e.ctx, `UPDATE accounts SET approval = 'APPROVED' WHERE phone_e164 = '+919876500063' AND role = 'SELLER' RETURNING id`).Scan(&bID)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO seller_profiles (account_id, legal_name, store_name) VALUES ($1, 'B', 'Other Store') ON CONFLICT (account_id) DO UPDATE SET store_name = 'Other Store'`, bID); err != nil {
		t.Fatal(err)
	}
	notMine(sellerB, "GET", "/seller/orders/"+openSub, nil)
	notMine(sellerB, "POST", "/seller/sub-orders/"+openSub+"/pack", map[string]any{})
	notMine(sellerB, "POST", "/seller/sub-orders/"+openSub+"/ship", map[string]any{})
	notMine(sellerB, "GET", "/seller/sub-orders/"+doneSub+"/label", nil)
	notMine(sellerB, "POST", "/seller/sub-orders/"+openSub+"/cancel", map[string]string{"reason": "x"})
	notMine(sellerB, "POST", "/seller/returns/"+ret["id"].(string)+"/decision", map[string]any{"approve": true})
	notMine(sellerB, "GET", "/seller/products/"+product.String(), nil)
	notMine(sellerB, "PUT", "/seller/products/"+product.String()+"/variants", []any{})
	notMine(sellerB, "POST", "/seller/products/"+product.String()+"/status", map[string]string{"status": "ARCHIVED"})
	if _, list := sellerB.do("GET", "/seller/orders", nil, false); len(list["items"].([]any)) != 0 {
		t.Errorf("seller B sees seller A's orders: %v", list)
	}

	// A buyer cannot use seller endpoints, and an unapproved seller cannot sell.
	notMine(bob, "GET", "/seller/orders", nil)
	draft := e.browser()
	draft.signIn("9876500064", "SELLER")
	st, p := draft.do("POST", "/seller/products", map[string]any{"categoryId": uuid.Nil, "name": "Sneaky", "brand": "X"}, true)
	if st < 400 {
		if st2, out := draft.do("POST", "/seller/products/"+p["id"].(string)+"/status", map[string]string{"status": "ACTIVE"}, true); st2 < 400 {
			t.Errorf("unapproved seller published a product: %d %v", st2, out)
		}
	}
}

// TestSignInAfterSignOutWithGuestCart: the guest-cart device cookie outlives
// the session, so the CSRF token must survive sign-out (and be re-issued on
// any read if it is missing), or every later sign-in fails with CSRF_FAILED.
func TestSignInAfterSignOutWithGuestCart(t *testing.T) {
	e := setup(t)
	variant, _, _ := e.listed("Volt Store", "Volt Cell", 99900, 5)
	c := e.browser()
	if st, out := c.do("PUT", "/cart/items/"+variant.String(), map[string]int{"qty": 1}, false); st != 200 {
		t.Fatalf("guest cart: %d %v", st, out)
	}
	if c.cookie(auth.CookieDevice) == "" {
		t.Fatal("no device cookie")
	}
	sendOTP := func() int {
		e.mr.FastForward(time.Minute)
		st, _ := c.do("POST", "/auth/otp/send", map[string]string{"phone": "9876500071", "role": "BUYER"}, true)
		return st
	}

	// Sign in (with the token, as the web app sends it) and out: the token is replaced, not deleted.
	e.mr.FastForward(time.Minute)
	_, ch := c.do("POST", "/auth/otp/send", map[string]string{"phone": "9876500071", "role": "BUYER"}, true)
	if st, o := c.do("POST", "/auth/otp/verify", map[string]string{"challengeId": ch["challengeId"].(string), "code": e.inbox.last["+919876500071"]}, true); st != 200 {
		t.Fatalf("sign-in: %d %v", st, o)
	}
	if st, _ := c.do("POST", "/auth/logout", nil, true); st != 204 {
		t.Fatalf("logout: %d", st)
	}
	if c.cookie(auth.CookieCSRF) == "" {
		t.Fatal("sign-out left no CSRF token")
	}
	if st := sendOTP(); st != 200 {
		t.Fatalf("sign-in after sign-out: %d", st)
	}

	// A browser already stuck (device cookie, no token) recovers on its next page load.
	u, _ := url.Parse(e.srv.URL)
	c.jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieCSRF, Value: "", Path: "/", MaxAge: -1}})
	if st := sendOTP(); st != 403 {
		t.Fatalf("stuck state not reproduced: %d", st)
	}
	c.do("GET", "/me", nil, false) // any read, as the sign-in page makes
	if st := sendOTP(); st != 200 {
		t.Fatalf("sign-in after reload: %d", st)
	}
}
