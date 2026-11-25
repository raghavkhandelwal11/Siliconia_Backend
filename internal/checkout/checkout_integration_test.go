package checkout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/cart"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/checkout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	shipprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/shipping"
	simpay "github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/pay"
	simship "github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/ship"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

type inbox struct {
	mu   sync.Mutex
	last map[string]string
}

func (b *inbox) Send(_ context.Context, m messaging.Message) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last[m.To] = regexp.MustCompile(`\b\d{6}\b`).FindString(m.Body)
	return "t", nil
}

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	srv    *httptest.Server
	prods  *product.Service
	inbox  *inbox
	mr     *miniredis.Miniredis
	co     *checkout.Service
	sim    *simpay.Sim
	simURL string
	ship   *shipping.Service
	ships  *simship.Sim
}

func setup(t *testing.T) *env {
	t.Helper()
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
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, seller_profiles, uploads, products, categories, carts, wishlist_items, payments, refunds, webhook_events, outbox, idempotency_keys, shipments CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	signer, _ := auth.NewSigner(config.DevKey("auth-signing"), auth.AccessTTL)
	box, _ := crypto.NewBox(config.DevKey("encryption-master"))
	in := &inbox{last: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := store.New(pool)
	authSvc := &auth.Service{Pool: pool, Q: q, Redis: rdb, Messages: in, Signer: signer, Box: box, Pepper: []byte("p"), Log: log}
	secret := []byte("whsec-test")
	pays := &payment.Service{Pool: pool, Q: q, Log: log}
	shipSecret := []byte("ship-secret")
	ships := &shipping.Service{Pool: pool, Q: q, Log: log}
	co := &checkout.Service{Pool: pool, Q: q, Payments: pays, PublicURL: "http://shop.test", Log: log, Shipping: ships}
	pays.Hooks = map[string]payment.Hook{"ORDER": co.OnPayment}
	srv := httptest.NewServer(server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: authSvc, Auth: auth.NewHandler(authSvc, nil, false),
		Cart:    &cart.Handler{Svc: &cart.Service{Pool: pool, Q: q}},
		Payment: &payment.Handler{Svc: pays}, Checkout: &checkout.Handler{Svc: co}, Shipping: &shipping.Handler{Svc: ships},
	}))
	t.Cleanup(srv.Close)
	sim := simpay.New(simpay.Config{PublicURL: "http://shop.test", APIKey: "k", WebhookURL: srv.URL + "/api/v1/webhooks/payment",
		WebhookSecret: secret, CaptureDelay: 5 * time.Millisecond, PendingDelay: time.Second, RetryBase: 5 * time.Millisecond, MaxAttempts: 5, Log: log})
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	pays.Provider = provider.NewSim(simSrv.URL, "k", secret)
	shipSim := simship.New(simship.Config{APIKey: "sk", WebhookURL: srv.URL + "/api/v1/webhooks/shipping", WebhookSecret: shipSecret,
		Step: 10 * time.Millisecond, RetryBase: 5 * time.Millisecond, MaxAttempts: 5, Log: log})
	shipSrv := httptest.NewServer(shipSim.Handler())
	t.Cleanup(shipSrv.Close)
	ships.Provider = shipprovider.NewSim(shipSrv.URL, "sk", shipSecret)
	return &env{t: t, ctx: ctx, pool: pool, srv: srv, prods: &product.Service{Pool: pool, Q: q, Store: storage.NewMemory(), Log: log}, inbox: in, mr: mr,
		co: co, sim: sim, simURL: simSrv.URL, ship: ships, ships: shipSim}
}

// listed creates a live product with one variant and returns its variant ID.
func (e *env) listed(store string, name string, price int64, stock int32) (uuid.UUID, uuid.UUID, uuid.UUID) {
	e.t.Helper()
	seller := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO accounts (id, role, phone_e164, approval) VALUES ($1, 'SELLER', $2, 'APPROVED')`, seller, "+9180"+seller.String()[:8]); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO seller_profiles (account_id, legal_name, store_name) VALUES ($1, 'L', $2)`, seller, store); err != nil {
		e.t.Fatal(err)
	}
	var cat uuid.UUID
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM categories WHERE slug = 'power-banks'`).Scan(&cat)
	v, err := e.prods.Create(e.ctx, seller, product.CreateInput{CategoryID: cat, Name: name, Brand: "Volt"})
	if err != nil {
		e.t.Fatal(err)
	}
	if v, err = e.prods.Update(e.ctx, seller, v.ID, product.UpdateInput{CategoryID: cat, Name: name, Brand: "Volt", Description: strings.Repeat("Charges fast. ", 4),
		Warranty: "6 months", OriginCountry: "IN", Specs: map[string]any{"capacity_mah": 10000.0}, Version: v.Version}); err != nil {
		e.t.Fatal(err)
	}
	if v, err = e.prods.SaveVariants(e.ctx, seller, v.ID, []product.VariantInput{{SKU: "V1", MrpPaise: price * 2, PricePaise: price, Stock: stock}}); err != nil {
		e.t.Fatal(err)
	}
	var img bytes.Buffer
	_ = png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 500, 500)))
	up, _ := e.prods.UploadImage(e.ctx, seller, img.Bytes())
	if _, err = e.prods.SaveImages(e.ctx, seller, v.ID, []product.ImageInput{{UploadID: up.ID}}); err != nil {
		e.t.Fatal(err)
	}
	if v, err = e.prods.SetStatus(e.ctx, seller, v.ID, "ACTIVE"); err != nil {
		e.t.Fatal(err)
	}
	return v.Variants[0].ID, v.ID, seller
}

type client struct {
	e   *env
	jar *cookiejar.Jar
}

func (e *env) browser() *client { j, _ := cookiejar.New(nil); return &client{e: e, jar: j} }

func (c *client) cookie(name string) string {
	u, _ := url.Parse(c.e.srv.URL + "/")
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) do(method, path string, body any, csrf bool) (int, map[string]any) {
	c.e.t.Helper()
	return c.doKey(method, path, body, csrf, "")
}

func (c *client) doKey(method, path string, body any, csrf bool, key string) (int, map[string]any) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if csrf {
		req.Header.Set(auth.HeaderCSRF, c.cookie(auth.CookieCSRF))
	}
	res, err := (&http.Client{Jar: c.jar}).Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (c *client) signIn(phone, role string) {
	c.e.t.Helper()
	c.e.mr.FastForward(time.Minute)
	_, out := c.do("POST", "/auth/otp/send", map[string]string{"phone": phone, "role": role}, false)
	if st, o := c.do("POST", "/auth/otp/verify", map[string]string{"challengeId": out["challengeId"].(string), "code": c.e.inbox.last["+91"+phone]}, false); st != 200 {
		c.e.t.Fatalf("sign-in: %d %v", st, o)
	}
}

// lot returns (reserved, sold) for a variant's seller lot.
func (e *env) lot(variant uuid.UUID) (int, int) {
	var r, s int
	if err := e.pool.QueryRow(e.ctx, `SELECT reserved, sold FROM inventory_lots WHERE variant_id = $1`, variant).Scan(&r, &s); err != nil {
		e.t.Fatal(err)
	}
	return r, s
}

func (e *env) outbox(topic string) int {
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM outbox WHERE topic = $1`, topic).Scan(&n)
	return n
}

// buyer signs in and saves a default address.
func (e *env) buyer(phone string) (*client, string) { return e.buyerAt(phone, "560001") }

func (e *env) buyerAt(phone, pin string) (*client, string) {
	b := e.browser()
	b.signIn(phone, "BUYER")
	st, a := b.do("POST", "/me/addresses", map[string]any{"name": "Asha Rao", "phone": "+91 98765 00000", "line1": "12 MG Road",
		"city": "Bengaluru", "state": "Karnataka", "pincode": pin}, true)
	if st != 201 || a["isDefault"] != true {
		e.t.Fatalf("address: %d %v", st, a)
	}
	return b, a["id"].(string)
}

func (c *client) quote(addr, method string) map[string]any {
	c.e.t.Helper()
	body := map[string]any{"payMethod": method}
	if addr != "" {
		body["addressId"] = addr
	}
	st, q := c.do("POST", "/checkout/quote", body, true)
	if st != 200 {
		c.e.t.Fatalf("quote: %d %v", st, q)
	}
	return q
}

// pay completes the hosted sim-pay page for an order and waits for webhooks.
func (e *env) pay(checkoutURL string) {
	e.t.Helper()
	id := checkoutURL[strings.LastIndex(checkoutURL, "/")+1:]
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.PostForm(e.simURL+"/pay/checkout/"+id, url.Values{"method": {"CARD"}, "card": {"4111 1111 1111 1111"}})
	if err != nil || res.StatusCode != 303 {
		e.t.Fatalf("pay: %v %v", res, err)
	}
	e.sim.Wait()
}

func TestAddressBook(t *testing.T) {
	e := setup(t)
	b, first := e.buyer("9876522222")
	if st, o := b.do("POST", "/me/addresses", map[string]any{"name": "A", "phone": "123", "line1": "x", "city": "y", "state": "z", "pincode": "012345"}, true); st != 400 {
		t.Fatalf("invalid address: %d %v", st, o)
	}
	st, second := b.do("POST", "/me/addresses", map[string]any{"name": "Office", "phone": "9876500001", "line1": "1 Tech Park",
		"city": "Pune", "state": "Maharashtra", "pincode": "411001", "isDefault": true}, true)
	if st != 201 || second["isDefault"] != true {
		t.Fatalf("second: %d %v", st, second)
	}
	_, list := b.do("GET", "/me/addresses", nil, false)
	items := list["items"].([]any)
	defaults := 0
	for _, it := range items {
		if it.(map[string]any)["isDefault"] == true {
			defaults++
		}
	}
	if len(items) != 2 || defaults != 1 {
		t.Fatalf("one default only: %v", list)
	}
	if st, _ := b.do("DELETE", "/me/addresses/"+first, nil, true); st != 204 {
		t.Fatalf("delete: %d", st)
	}
	other, _ := e.buyer("9876533333")
	if st, _ := other.do("DELETE", "/me/addresses/"+second["id"].(string), nil, true); st != 404 {
		t.Fatalf("someone else's address: %d", st)
	}
	seller := e.browser()
	seller.signIn("9876544444", "SELLER")
	if st, o := seller.do("GET", "/me/addresses", nil, false); st != 403 {
		t.Fatalf("sellers have no address book: %d %v", st, o)
	}
}

func TestCashOnDeliveryAndOnlineOrders(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 5)
	slim, _, _ := e.listed("Orbit Store", "Orbit Slim", 109900, 50)
	b, addr := e.buyer("9876511111")
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 2}, true)
	b.do("PUT", "/cart/items/"+slim.String(), map[string]int{"qty": 1}, true)

	if q := b.quote("", "COD"); q["canPlace"] != false {
		t.Fatalf("no address, no order: %v", q)
	}
	q := b.quote(addr, "COD")
	if q["canPlace"] != true || q["codEligible"] != true || q["payablePaise"].(float64) != 669700 || len(q["groups"].([]any)) != 2 || q["electronsNonMember"].(float64) != 34 {
		t.Fatalf("COD quote: %v", q)
	}
	body := map[string]any{"addressId": addr, "payMethod": "COD", "quoteHash": q["quoteHash"]}
	if st, o := b.do("POST", "/orders", body, true); st != 400 {
		t.Fatalf("missing Idempotency-Key: %d %v", st, o)
	}
	if st, o := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "COD", "quoteHash": "stale"}, true, "key-stale-1"); st != 409 || o["code"] != "QUOTE_CHANGED" {
		t.Fatalf("stale quote: %d %v", st, o)
	}
	st, o := b.doKey("POST", "/orders", body, true, "key-cod-0001")
	if st != 201 || o["status"] != "CONFIRMED" || len(o["subOrders"].([]any)) != 2 || o["payablePaise"].(float64) != 669700 {
		t.Fatalf("COD order: %d %v", st, o)
	}
	if r, s := e.lot(bank); r != 0 || s != 2 {
		t.Fatalf("COD stock moves straight to sold: reserved=%d sold=%d", r, s)
	}
	if _, c := b.do("GET", "/cart", nil, false); c["itemCount"].(float64) != 0 {
		t.Fatalf("cart cleared: %v", c)
	}
	if st, again := b.doKey("POST", "/orders", body, true, "key-cod-0001"); st != 201 || again["id"] != o["id"] {
		t.Fatalf("replay returns the same order: %d %v", st, again)
	}
	if st, x := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": "x"}, true, "key-cod-0001"); st != 422 || x["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("key reuse with another body: %d %v", st, x)
	}

	// Above the COD limit: online only.
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 3}, true)
	if q := b.quote(addr, "COD"); q["codEligible"] != false || q["canPlace"] != false {
		t.Fatalf("COD over limit: %v", q)
	}
	q = b.quote(addr, "ONLINE")
	st, o = b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"]}, true, "key-online-01")
	if st != 201 || o["status"] != "PENDING_PAYMENT" {
		t.Fatalf("online order: %d %v", st, o)
	}
	pay, _ := o["payment"].(map[string]any)
	if pay == nil || !strings.Contains(pay["checkoutUrl"].(string), "/pay/checkout/") {
		t.Fatalf("checkout URL: %v", o)
	}
	if r, s := e.lot(bank); r != 3 || s != 2 {
		t.Fatalf("online stock is reserved: reserved=%d sold=%d", r, s)
	}
	e.pay(pay["checkoutUrl"].(string))
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if o["status"] != "CONFIRMED" || o["subOrders"].([]any)[0].(map[string]any)["status"] != "CONFIRMED" {
		t.Fatalf("captured payment confirms: %v", o)
	}
	if r, s := e.lot(bank); r != 0 || s != 5 {
		t.Fatalf("captured stock is sold: reserved=%d sold=%d", r, s)
	}
	if e.outbox("order.confirmed") != 2 || e.outbox("order.placed") != 2 {
		t.Fatalf("events: confirmed=%d placed=%d", e.outbox("order.confirmed"), e.outbox("order.placed"))
	}
	if st, _ := b.do("POST", "/orders/"+o["id"].(string)+"/pay", nil, true); st != 409 {
		t.Fatalf("paying a confirmed order again: %d", st)
	}
	_, list := b.do("GET", "/orders", nil, false)
	if items := list["items"].([]any); len(items) != 2 || items[0].(map[string]any)["number"] != o["number"] {
		t.Fatalf("order list, newest first: %v", list)
	}
	other, _ := e.buyer("9876533333")
	if st, _ := other.do("GET", "/orders/"+o["id"].(string), nil, false); st != 404 {
		t.Fatalf("someone else's order: %d", st)
	}
}

func TestLastUnitGoesToOneBuyer(t *testing.T) {
	e := setup(t)
	last, _, _ := e.listed("Volt Store", "Volt Last", 99900, 1)
	type buyer struct {
		c    *client
		addr string
		hash string
	}
	var bs []buyer
	for _, p := range []string{"9876511112", "9876511113"} {
		c, addr := e.buyer(p)
		c.do("PUT", "/cart/items/"+last.String(), map[string]int{"qty": 1}, true)
		bs = append(bs, buyer{c, addr, c.quote(addr, "COD")["quoteHash"].(string)})
	}
	codes := make([]int, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = b.c.doKey("POST", "/orders", map[string]any{"addressId": b.addr, "payMethod": "COD", "quoteHash": b.hash}, true, "race-key-"+b.addr[:8])
		}()
	}
	wg.Wait()
	if !((codes[0] == 201 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 201)) {
		t.Fatalf("exactly one order: %v", codes)
	}
	if r, s := e.lot(last); r != 0 || s != 1 {
		t.Fatalf("never oversold: reserved=%d sold=%d", r, s)
	}
}

func TestUnpaidOrderExpiresAndLatePaymentIsRefunded(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 5)
	b, addr := e.buyer("9876511111")
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 2}, true)
	q := b.quote(addr, "ONLINE")
	_, o := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"]}, true, "key-expire-1")
	checkoutURL := o["payment"].(map[string]any)["checkoutUrl"].(string)

	if n, err := e.co.Sweep(e.ctx); err != nil || n != 0 {
		t.Fatalf("nothing due yet: %d %v", n, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE orders SET expires_at = now() - interval '1 second' WHERE id = $1`, o["id"]); err != nil {
		t.Fatal(err)
	}
	if n, err := e.co.Sweep(e.ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if o["status"] != "EXPIRED" || o["subOrders"].([]any)[0].(map[string]any)["status"] != "EXPIRED" {
		t.Fatalf("expired: %v", o)
	}
	if r, s := e.lot(bank); r != 0 || s != 0 {
		t.Fatalf("stock released: reserved=%d sold=%d", r, s)
	}
	if st, _ := b.do("POST", "/orders/"+o["id"].(string)+"/pay", nil, true); st != 409 {
		t.Fatalf("no payment for an expired order: %d", st)
	}

	// The shopper still had the payment page open and pays: refunded in full.
	e.pay(checkoutURL)
	var ref string
	var amount int64
	if err := e.pool.QueryRow(e.ctx, `SELECT ref, amount_paise FROM refunds`).Scan(&ref, &amount); err != nil {
		t.Fatalf("late payment refund: %v", err)
	}
	if ref != o["number"].(string)+"-LATE" || amount != 559800 {
		t.Fatalf("refund: %s %d", ref, amount)
	}
	if _, o = b.do("GET", "/orders/"+o["id"].(string), nil, false); o["status"] != "EXPIRED" {
		t.Fatalf("stays expired: %v", o)
	}
}

// pickup gives a seller a pickup address.
func (e *env) pickup(seller uuid.UUID) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE seller_profiles SET pickup_address = '{"line1":"Plot 4, Sector 18","city":"Noida","state":"Uttar Pradesh","pincode":"201301","phone":"9876500009"}' WHERE account_id = $1`, seller); err != nil {
		e.t.Fatal(err)
	}
}

// placeCOD places a COD order for one unit and returns the order view.
func (e *env) placeCOD(b *client, addr string, variant uuid.UUID, key string) map[string]any {
	e.t.Helper()
	b.do("PUT", "/cart/items/"+variant.String(), map[string]int{"qty": 1}, true)
	q := b.quote(addr, "COD")
	st, o := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "COD", "quoteHash": q["quoteHash"]}, true, key)
	if st != 201 {
		e.t.Fatalf("place: %d %v", st, o)
	}
	return o
}

func subOrder(o map[string]any) map[string]any { return o["subOrders"].([]any)[0].(map[string]any) }

func TestShipTrackAndDeliver(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	b, addr := e.buyer("9876511111")
	o := e.placeCOD(b, addr, bank, "key-ship-0001")
	subID := uuid.MustParse(subOrder(o)["id"].(string))

	if _, err := e.ship.Ship(e.ctx, seller, subID, 400); err != shipping.ErrNoPickup {
		t.Fatalf("no pickup address: %v", err)
	}
	e.pickup(seller)
	if _, err := e.ship.Ship(e.ctx, uuid.New(), subID, 400); err == nil {
		t.Fatal("another seller cannot ship it")
	}
	sh, err := e.ship.Ship(e.ctx, seller, subID, 400)
	if err != nil || sh.AWB == "" || sh.CODAmountPaise != 279900 {
		t.Fatalf("ship: %+v %v", sh, err)
	}
	if again, _ := e.ship.Ship(e.ctx, seller, subID, 400); again.AWB != sh.AWB {
		t.Fatal("shipping twice returns the same AWB")
	}
	pdf, awb, err := e.ship.Label(e.ctx, seller, subID)
	if err != nil || awb != sh.AWB || !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatalf("label: %v %s", err, awb)
	}
	if st, _ := b.do("POST", "/seller/sub-orders/"+subID.String()+"/ship", nil, true); st != 403 {
		t.Fatalf("buyers cannot ship: %d", st)
	}

	e.ships.Wait()
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	so := subOrder(o)
	track, _ := so["shipment"].(map[string]any)
	if so["status"] != "DELIVERED" || so["deliveredAt"] == nil || so["returnWindowEndsAt"] == nil || track == nil || track["status"] != "DELIVERED" {
		t.Fatalf("delivered: %v", so)
	}
	if n := len(track["events"].([]any)); n != 6 {
		t.Fatalf("tracking scans: %d %v", n, track["events"])
	}
	var cod int64
	var window time.Duration
	_ = e.pool.QueryRow(e.ctx, `SELECT cod_collected_paise, return_window_ends_at - delivered_at FROM sub_orders WHERE id = $1`, subID).Scan(&cod, &window)
	if cod != 279900 || window != 7*24*time.Hour {
		t.Fatalf("COD collected %d, return window %v", cod, window)
	}
	if e.outbox("suborder.status_changed") < 3 {
		t.Fatalf("status events: %d", e.outbox("suborder.status_changed"))
	}
}

func TestUndeliveredParcelReturnsToOrigin(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyerAt("9876511111", simship.PinRTO)
	o := e.placeCOD(b, addr, bank, "key-rto-00001")
	if _, err := e.ship.Ship(e.ctx, seller, uuid.MustParse(subOrder(o)["id"].(string)), 0); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if so := subOrder(o); so["status"] != "RTO" || so["shipment"].(map[string]any)["status"] != "RTO_DELIVERED" || so["deliveredAt"] != nil {
		t.Fatalf("RTO: %v", so)
	}
}

func TestPinServiceability(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 5)
	b := e.browser()
	b.signIn("9876511111", "BUYER")
	if st, o := b.do("POST", "/me/addresses", map[string]any{"name": "Asha Rao", "phone": "9876500000", "line1": "1 Road",
		"city": "Nowhere", "state": "Delhi", "pincode": simship.PinNotServiceable}, true); st != 422 || o["code"] != "PIN_NOT_SERVICEABLE" {
		t.Fatalf("unserviceable PIN: %d %v", st, o)
	}
	_, a := b.do("POST", "/me/addresses", map[string]any{"name": "Asha Rao", "phone": "9876500000", "line1": "1 Hill Road",
		"city": "Itanagar", "state": "Arunachal Pradesh", "pincode": "791111"}, true)
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 1}, true)
	q := b.quote(a["id"].(string), "COD")
	d, _ := q["delivery"].(map[string]any)
	if q["codEligible"] != false || q["codReason"] != "PIN_NO_COD" || q["canPlace"] != false || d == nil || d["etaDays"].(float64) != 6 || d["estimatedBy"] == nil {
		t.Fatalf("prepaid-only PIN: %v", q)
	}
	if q = b.quote(a["id"].(string), "ONLINE"); q["canPlace"] != true {
		t.Fatalf("online still works: %v", q)
	}
	res, _ := http.Get(e.srv.URL + "/api/v1/serviceability/560001")
	var sv map[string]any
	_ = json.NewDecoder(res.Body).Decode(&sv)
	if res.StatusCode != 200 || sv["deliverable"] != true || sv["cod"] != true || sv["etaDays"].(float64) != 2 {
		t.Fatalf("public PIN check: %d %v", res.StatusCode, sv)
	}
	if res, _ := http.Get(e.srv.URL + "/api/v1/serviceability/12"); res.StatusCode != 400 {
		t.Fatalf("bad PIN: %d", res.StatusCode)
	}
}
