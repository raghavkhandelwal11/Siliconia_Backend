package checkout_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/issues"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/notify"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/orders"
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
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/returns"
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
	ord    *orders.Service
	ret    *returns.Service
	iss    *issues.Service
	note   *notify.Service
	sent   *recorder
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
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, seller_profiles, uploads, products, categories, carts, wishlist_items, payments, refunds, webhook_events, outbox, idempotency_keys, shipments, issues, notifications, notification_prefs CASCADE`); err != nil {
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
	ord := &orders.Service{Pool: pool, Q: q, Payments: pays, Log: log}
	ret := &returns.Service{Pool: pool, Q: q, Payments: pays, Log: log}
	ships.Returns = ret.OnTracking
	iss := &issues.Service{Pool: pool, Q: q, Log: log}
	sent := &recorder{}
	note := &notify.Service{Pool: pool, Q: q, Messenger: sent, PublicURL: "http://shop.test", Log: log,
		Publish: func(ctx context.Context, account uuid.UUID, it notify.Item) error {
			return rdb.Publish(ctx, notify.Channel(account), notify.MarshalItem(it)).Err()
		}}
	srv := httptest.NewServer(server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: authSvc, Auth: auth.NewHandler(authSvc, nil, false),
		Cart:    &cart.Handler{Svc: &cart.Service{Pool: pool, Q: q}},
		Payment: &payment.Handler{Svc: pays}, Checkout: &checkout.Handler{Svc: co}, Shipping: &shipping.Handler{Svc: ships}, Orders: &orders.Handler{Svc: ord}, Returns: &returns.Handler{Svc: ret}, Issues: &issues.Handler{Svc: iss}, Notify: &notify.Handler{Svc: note, Redis: rdb, StreamFor: 3 * time.Second},
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
	ret.Courier = ships.Provider
	return &env{t: t, ctx: ctx, pool: pool, srv: srv, prods: &product.Service{Pool: pool, Q: q, Store: storage.NewMemory(), Log: log}, inbox: in, mr: mr,
		co: co, sim: sim, simURL: simSrv.URL, ship: ships, ships: shipSim, ord: ord, ret: ret, iss: iss, note: note, sent: sent}
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
	// Shipped and delivered at least; out-for-delivery may be skipped when scans arrive out of order.
	if e.outbox("suborder.status_changed") < 2 {
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

func (c *client) raw(method, path string, body any) (*http.Response, []byte) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderCSRF, c.cookie(auth.CookieCSRF))
	res, err := (&http.Client{Jar: c.jar}).Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestSellerFulfilmentTimelineAndInvoice(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")
	o := e.placeCOD(b, addr, bank, "key-fulfil-01")
	subID := uuid.MustParse(subOrder(o)["id"].(string))

	queue, counts, err := e.ord.Queue(e.ctx, seller, "")
	if err != nil || len(queue) != 1 || queue[0].Status != "CONFIRMED" || queue[0].BuyerName != "Asha Rao" || counts["CONFIRMED"] != 1 {
		t.Fatalf("queue: %+v %v %v", queue, counts, err)
	}
	if _, _, err := e.ord.Queue(e.ctx, seller, "BOGUS"); err == nil {
		t.Fatal("unknown status filter must fail")
	}
	if res, body := b.raw("GET", "/invoices/"+subID.String(), nil); res.StatusCode != 409 || !strings.Contains(string(body), "NO_INVOICE_YET") {
		t.Fatalf("invoice before shipping: %d %s", res.StatusCode, body)
	}
	if err := e.ord.Pack(e.ctx, uuid.New(), subID); err == nil {
		t.Fatal("another seller cannot pack")
	}
	if err := e.ord.Pack(e.ctx, seller, subID); err != nil {
		t.Fatal(err)
	}
	if err := e.ord.Pack(e.ctx, seller, subID); err != orders.ErrCannotPack {
		t.Fatalf("pack twice: %v", err)
	}
	d, err := e.ord.SellerDetail(e.ctx, seller, subID)
	if err != nil || d.Status != "PACKED" || !d.CanShip || d.CanPack || len(d.Items) != 1 || d.Address.Phone == "" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if _, err := e.ship.Ship(e.ctx, seller, subID, 300); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()

	res, pdf := b.raw("GET", "/invoices/"+subID.String(), nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/pdf" || !strings.Contains(string(pdf), "TAX INVOICE") ||
		!strings.Contains(string(pdf), "Not a valid tax invoice") || !strings.Contains(string(pdf), "Rs 2,799.00") {
		t.Fatalf("invoice: %d %.300s", res.StatusCode, pdf)
	}
	inter := strings.Contains(string(pdf), "IGST") // Uttar Pradesh seller, Karnataka buyer
	_, sellerPDF, err := e.ord.Invoice(e.ctx, orders.Viewer{ID: seller}, subID)
	if err != nil || !inter {
		t.Fatalf("seller invoice: %v inter=%v", err, inter)
	}
	var no string
	_ = e.pool.QueryRow(e.ctx, `SELECT invoice_no FROM sub_orders WHERE id = $1`, subID).Scan(&no)
	if sellerPDF != no || !strings.HasPrefix(no, "SX-") || !strings.HasSuffix(no, "/00001") {
		t.Fatalf("one invoice number per sub-order: %q vs %q", sellerPDF, no)
	}
	other, _ := e.buyer("9876533333")
	if res, _ := other.raw("GET", "/invoices/"+subID.String(), nil); res.StatusCode != 404 {
		t.Fatalf("someone else's invoice: %d", res.StatusCode)
	}

	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	so := subOrder(o)
	var steps []string
	for _, ev := range so["events"].([]any) {
		m := ev.(map[string]any)
		steps = append(steps, m["status"].(string)+"/"+m["actor"].(string))
	}
	// Courier scans can arrive out of order; a late "out for delivery" never moves a delivered order back.
	got := strings.Join(steps, ",")
	want := "CONFIRMED/BUYER,PACKED/SELLER,SHIPPED/SELLER,"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "DELIVERED/COURIER") || so["hasInvoice"] != true || so["canCancel"] != false {
		t.Fatalf("timeline: %v (want %s) %v", steps, want, so)
	}
}

func TestBuyerCancelsBeforeShipping(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")

	// Prepaid: cancelling refunds the payment and puts stock back.
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 2}, true)
	q := b.quote(addr, "ONLINE")
	_, o := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": "ONLINE", "quoteHash": q["quoteHash"]}, true, "key-cancel-01")
	e.pay(o["payment"].(map[string]any)["checkoutUrl"].(string))
	subID := subOrder(o)["id"].(string)
	if r, s := e.lot(bank); r != 0 || s != 2 {
		t.Fatalf("sold: %d %d", r, s)
	}
	if res, _ := b.raw("POST", "/sub-orders/"+subID+"/cancel", map[string]string{"reason": "x"}); res.StatusCode != 400 {
		t.Fatalf("reason required: %d", res.StatusCode)
	}
	other, _ := e.buyer("9876533333")
	if res, _ := other.raw("POST", "/sub-orders/"+subID+"/cancel", map[string]string{"reason": "Not mine"}); res.StatusCode != 404 {
		t.Fatalf("someone else's order: %d", res.StatusCode)
	}
	if res, body := b.raw("POST", "/sub-orders/"+subID+"/cancel", map[string]string{"reason": "Ordered by mistake"}); res.StatusCode != 204 {
		t.Fatalf("cancel: %d %s", res.StatusCode, body)
	}
	e.sim.Wait()
	if r, s := e.lot(bank); r != 0 || s != 0 {
		t.Fatalf("stock back on sale: %d %d", r, s)
	}
	var refund int64
	var status string
	if err := e.pool.QueryRow(e.ctx, `SELECT amount_paise, status FROM refunds WHERE ref = $1`, subID+"-CANCEL").Scan(&refund, &status); err != nil || refund != 559800 || status != "REFUNDED" {
		t.Fatalf("refund: %d %s %v", refund, status, err)
	}
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if so := subOrder(o); so["status"] != "CANCELLED" || so["cancelReason"] != "Ordered by mistake" || so["items"].([]any)[0].(map[string]any)["status"] != "CANCELLED" {
		t.Fatalf("cancelled: %v", so)
	}
	if res, _ := b.raw("POST", "/sub-orders/"+subID+"/cancel", map[string]string{"reason": "Again please"}); res.StatusCode != 409 {
		t.Fatalf("cancel twice: %d", res.StatusCode)
	}

	// After shipping there is no cancelling.
	o = e.placeCOD(b, addr, bank, "key-cancel-02")
	shipped := uuid.MustParse(subOrder(o)["id"].(string))
	if _, err := e.ship.Ship(e.ctx, seller, shipped, 0); err != nil {
		t.Fatal(err)
	}
	if res, _ := b.raw("POST", "/sub-orders/"+shipped.String()+"/cancel", map[string]string{"reason": "Changed my mind"}); res.StatusCode != 409 {
		t.Fatalf("cancel after shipping: %d", res.StatusCode)
	}
	e.ships.Wait()

	// The seller can cancel a new order too (no refund for COD).
	o = e.placeCOD(b, addr, bank, "key-cancel-03")
	if err := e.ord.Cancel(e.ctx, orders.Actor{ID: seller, Role: "SELLER"}, uuid.MustParse(subOrder(o)["id"].(string)), "Out of stock at the warehouse"); err != nil {
		t.Fatal(err)
	}
}

// delivered places an order (COD or paid online), ships it and waits until it is delivered.
func (e *env) delivered(b *client, addr string, variant uuid.UUID, seller uuid.UUID, method, key string) (order map[string]any, itemID string) {
	e.t.Helper()
	b.do("PUT", "/cart/items/"+variant.String(), map[string]int{"qty": 1}, true)
	q := b.quote(addr, method)
	st, o := b.doKey("POST", "/orders", map[string]any{"addressId": addr, "payMethod": method, "quoteHash": q["quoteHash"]}, true, key)
	if st != 201 {
		e.t.Fatalf("place: %d %v", st, o)
	}
	if method == "ONLINE" {
		e.pay(o["payment"].(map[string]any)["checkoutUrl"].(string))
	}
	if _, err := e.ship.Ship(e.ctx, seller, uuid.MustParse(subOrder(o)["id"].(string)), 0); err != nil {
		e.t.Fatal(err)
	}
	e.ships.Wait()
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if subOrder(o)["status"] != "DELIVERED" {
		e.t.Fatalf("not delivered: %v", subOrder(o))
	}
	return o, subOrder(o)["items"].([]any)[0].(map[string]any)["id"].(string)
}

func item(o map[string]any, i int) map[string]any {
	return o["subOrders"].([]any)[i].(map[string]any)["items"].([]any)[0].(map[string]any)
}

func TestReturnIsCollectedAndRefunded(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")

	// Not delivered yet: no return.
	pending := e.placeCOD(b, addr, bank, "key-ret-pend1")
	pendingItem := subOrder(pending)["items"].([]any)[0].(map[string]any)["id"].(string)
	if st, o := b.do("POST", "/order-items/"+pendingItem+"/returns", map[string]string{"type": "RETURN", "reason": "DAMAGED"}, true); st != 409 || o["code"] != "NOT_DELIVERED" {
		t.Fatalf("before delivery: %d %v", st, o)
	}

	o, itemID := e.delivered(b, addr, bank, seller, "ONLINE", "key-ret-paid1")
	if item(o, 0)["canReturn"] != true {
		t.Fatalf("returnable: %v", item(o, 0))
	}
	if st, _ := b.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "SWAP", "reason": "DAMAGED"}, true); st != 400 {
		t.Fatalf("bad type: %d", st)
	}
	st, r := b.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "RETURN", "reason": "NO_LONGER_NEEDED", "note": "Gift duplicate"}, true)
	if st != 201 || r["status"] != "REQUESTED" || !strings.HasPrefix(r["number"].(string), "RT-") {
		t.Fatalf("request: %d %v", st, r)
	}
	if st, o := b.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "RETURN", "reason": "DAMAGED"}, true); st != 409 || o["code"] != "RETURN_EXISTS" {
		t.Fatalf("second request: %d %v", st, o)
	}
	other, _ := e.buyer("9876533333")
	if st, _ := other.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "RETURN", "reason": "DAMAGED"}, true); st != 404 {
		t.Fatalf("someone else's item: %d", st)
	}
	list, _ := e.ret.SellerList(e.ctx, seller, "REQUESTED")
	if len(list) != 1 || list[0].Note != "Gift duplicate" {
		t.Fatalf("seller queue: %+v", list)
	}
	rid := uuid.MustParse(r["id"].(string))
	if err := e.ret.Decide(e.ctx, uuid.New(), rid, true, ""); err == nil {
		t.Fatal("another seller cannot decide")
	}
	if err := e.ret.Decide(e.ctx, seller, rid, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.ret.Decide(e.ctx, seller, rid, false, "Too late"); err != returns.ErrDecided {
		t.Fatalf("decide twice: %v", err)
	}
	e.ships.Wait()
	e.sim.Wait()

	_, mine := b.do("GET", "/returns", nil, false)
	got := mine["items"].([]any)[0].(map[string]any)
	if got["status"] != "REFUNDED" || got["refundAmountPaise"].(float64) != 279900 || got["pickupAwb"] == "" {
		t.Fatalf("refunded: %v", got)
	}
	var refundStatus string
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM refunds WHERE ref = $1`, rid.String()+"-REFUND").Scan(&refundStatus)
	if refundStatus != "REFUNDED" {
		t.Fatalf("payment refund: %q", refundStatus)
	}
	if _, s := e.lot(bank); s != 1 { // the COD order still holds one; the returned unit is back on sale
		t.Fatalf("restocked: sold=%d", s)
	}
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	if it := item(o, 0); it["status"] != "RETURNED" || it["canReturn"] != false || it["return"].(map[string]any)["status"] != "REFUNDED" {
		t.Fatalf("item after return: %v", it)
	}

	// The window closes 7 days after delivery.
	o2, item2 := e.delivered(b, addr, bank, seller, "COD", "key-ret-cod01")
	if _, err := e.pool.Exec(e.ctx, `UPDATE sub_orders SET return_window_ends_at = now() - interval '1 minute' WHERE id = $1`, subOrder(o2)["id"]); err != nil {
		t.Fatal(err)
	}
	if st, o := b.do("POST", "/order-items/"+item2+"/returns", map[string]string{"type": "RETURN", "reason": "DAMAGED"}, true); st != 409 || o["code"] != "RETURN_WINDOW_CLOSED" {
		t.Fatalf("window closed: %d %v", st, o)
	}
}

func TestReplacementAndRejection(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")
	o, itemID := e.delivered(b, addr, bank, seller, "COD", "key-rep-0001")

	// A rejected request can be followed by another.
	_, r := b.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "REPLACE", "reason": "DEFECTIVE"}, true)
	if err := e.ret.Decide(e.ctx, seller, uuid.MustParse(r["id"].(string)), false, "x"); err == nil {
		t.Fatal("a rejection needs a reason")
	}
	if err := e.ret.Decide(e.ctx, seller, uuid.MustParse(r["id"].(string)), false, "Please share a photo of the fault"); err != nil {
		t.Fatal(err)
	}
	st, r := b.do("POST", "/order-items/"+itemID+"/returns", map[string]string{"type": "REPLACE", "reason": "DEFECTIVE", "note": "Does not charge"}, true)
	if st != 201 {
		t.Fatalf("second request after rejection: %d %v", st, r)
	}
	if err := e.ret.Decide(e.ctx, seller, uuid.MustParse(r["id"].(string)), true, ""); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	_, o = b.do("GET", "/orders/"+o["id"].(string), nil, false)
	subs := o["subOrders"].([]any)
	if len(subs) != 2 {
		t.Fatalf("replacement sub-order: %v", subs)
	}
	repl := subs[1].(map[string]any)
	if repl["isReplacement"] != true || repl["status"] != "CONFIRMED" || repl["subtotalPaise"].(float64) != 0 || repl["hasInvoice"] != false ||
		item(o, 0)["status"] != "REPLACED" || item(o, 0)["return"].(map[string]any)["status"] != "RECEIVED" {
		t.Fatalf("after pickup: %v", o)
	}
	if _, s := e.lot(bank); s != 2 { // the faulty unit is not resold; the replacement is a new sale
		t.Fatalf("stock: sold=%d", s)
	}
	if _, err := e.ship.Ship(e.ctx, seller, uuid.MustParse(repl["id"].(string)), 0); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	_, mine := b.do("GET", "/returns", nil, false)
	if got := mine["items"].([]any)[0].(map[string]any); got["status"] != "REPLACEMENT_SHIPPED" {
		t.Fatalf("replacement shipped: %v", got)
	}
	var cod int64
	_ = e.pool.QueryRow(e.ctx, `SELECT cod_amount_paise FROM shipments WHERE sub_order_id = $1`, repl["id"]).Scan(&cod)
	if cod != 0 {
		t.Fatalf("no cash to collect on a replacement: %d", cod)
	}

	// A cash-paid return is refunded to a bank account later.
	o2, item2 := e.delivered(b, addr, bank, seller, "COD", "key-rep-0002")
	_, r2 := b.do("POST", "/order-items/"+item2+"/returns", map[string]string{"type": "RETURN", "reason": "DAMAGED"}, true)
	_ = o2
	if err := e.ret.Decide(e.ctx, seller, uuid.MustParse(r2["id"].(string)), true, ""); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	_, mine = b.do("GET", "/returns", nil, false)
	if got := mine["items"].([]any)[0].(map[string]any); got["status"] != "REFUND_PENDING" || got["number"] != r2["number"] {
		t.Fatalf("COD return: %v", got)
	}
}

func TestIssueConversation(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 5)
	b, addr := e.buyer("9876511111")
	o := e.placeCOD(b, addr, bank, "key-issue-001")
	itemID := subOrder(o)["items"].([]any)[0].(map[string]any)["id"].(string)

	if st, _ := b.do("POST", "/issues", map[string]any{"orderId": o["id"], "type": "DAMAGED", "body": " "}, true); st != 400 {
		t.Fatalf("empty body: %d", st)
	}
	other, _ := e.buyer("9876533333")
	if st, _ := other.do("POST", "/issues", map[string]any{"orderId": o["id"], "type": "DAMAGED", "body": "Box was crushed"}, true); st != 400 {
		t.Fatalf("someone else's order: %d", st)
	}
	st, is := b.do("POST", "/issues", map[string]any{"orderId": o["id"], "orderItemId": itemID, "type": "DAMAGED", "body": "Box was crushed"}, true)
	if st != 201 || is["status"] != "OPEN" || len(is["messages"].([]any)) != 1 || !strings.HasPrefix(is["number"].(string), "IS-") {
		t.Fatalf("raise: %d %v", st, is)
	}
	id := uuid.MustParse(is["id"].(string))
	if st, _ := other.do("GET", "/issues/"+id.String(), nil, false); st != 404 {
		t.Fatalf("another buyer: %d", st)
	}
	if st, _ := b.do("POST", "/issues/"+id.String()+"/messages", map[string]string{"body": "x", "status": "RESOLVED"}, true); st != 400 {
		t.Fatalf("buyers cannot set status: %d", st)
	}
	if st, _ := b.do("GET", "/admin/issues", nil, false); st != 403 {
		t.Fatalf("buyers cannot see the support queue: %d", st)
	}

	agent := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO accounts (id, role, phone_e164, approval) VALUES ($1, 'ADMIN', '+919000000001', 'APPROVED')`, agent); err != nil {
		t.Fatal(err)
	}
	support := issues.Viewer{ID: agent, Support: true}
	if _, err := e.iss.Reply(e.ctx, support, id, "Sorry! Can you share a photo?", "WAITING_CUSTOMER"); err != nil {
		t.Fatal(err)
	}
	_, is = b.do("POST", "/issues/"+id.String()+"/messages", map[string]string{"body": "Photo attached by courier"}, true)
	if is["status"] != "IN_PROGRESS" || len(is["messages"].([]any)) != 3 || is["firstResponseAt"] == nil {
		t.Fatalf("buyer reply moves it back to support: %v", is)
	}
	if _, err := e.iss.Reply(e.ctx, support, id, "Replacement arranged.", "RESOLVED"); err != nil {
		t.Fatal(err)
	}
	if st, o := b.do("POST", "/issues/"+id.String()+"/messages", map[string]string{"body": "One more thing"}, true); st != 409 || o["code"] != "ISSUE_RESOLVED" {
		t.Fatalf("reply after resolve: %d %v", st, o)
	}
	if _, is = b.do("POST", "/issues/"+id.String()+"/reopen", nil, true); is["status"] != "IN_PROGRESS" {
		t.Fatalf("reopen: %v", is)
	}
	if _, err := e.iss.Reply(e.ctx, support, id, "Done.", "RESOLVED"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE issues SET resolved_at = now() - interval '8 days' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := e.iss.CloseResolved(e.ctx); err != nil || n != 1 {
		t.Fatalf("auto-close: %d %v", n, err)
	}
	if st, o := b.do("POST", "/issues/"+id.String()+"/reopen", nil, true); st != 409 {
		t.Fatalf("closed issues stay closed: %d %v", st, o)
	}
	queue, _ := e.iss.List(e.ctx, support, "CLOSED")
	if len(queue) != 1 || e.outbox("issue.updated") != 6 {
		t.Fatalf("queue %d, events %d", len(queue), e.outbox("issue.updated"))
	}
}

// recorder is a Messenger that keeps what was sent and can be told to fail.
type recorder struct {
	mu   sync.Mutex
	msgs []messaging.Message
	fail map[messaging.Channel]bool
}

func (r *recorder) Send(_ context.Context, m messaging.Message) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail[m.Channel] {
		return "", errors.New("provider down")
	}
	r.msgs = append(r.msgs, m)
	return "ref", nil
}

func (r *recorder) count(ch messaging.Channel, template string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if m.Channel == ch && m.Template == template {
			n++
		}
	}
	return n
}

// drain runs every outbox event through the notifier, as the worker does.
func (e *env) drain() error {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT id, topic, payload FROM outbox ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	type ev struct {
		id      uuid.UUID
		topic   string
		payload []byte
	}
	var evs []ev
	for rows.Next() {
		var x ev
		_ = rows.Scan(&x.id, &x.topic, &x.payload)
		evs = append(evs, x)
	}
	rows.Close()
	var errs []error
	for _, x := range evs {
		errs = append(errs, e.note.Handle(e.ctx, x.topic, x.id.String(), x.payload))
	}
	return errors.Join(errs...)
}

func TestNotificationsAreRoutedOnceAndStreamed(t *testing.T) {
	e := setup(t)
	bank, _, seller := e.listed("Volt Store", "Volt Cell", 279900, 5)
	e.pickup(seller)
	b, addr := e.buyer("9876511111")

	// Live stream: the unread count first, then each new notification.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/me/notifications/stream", nil)
	res, err := (&http.Client{Jar: b.jar}).Do(req)
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %v %v", res, err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(prefix string) string {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q on the stream", prefix)
			}
		}
	}
	if l := waitFor("data:"); !strings.Contains(l, `"unread":0`) {
		t.Fatalf("first event: %s", l)
	}

	o := e.placeCOD(b, addr, bank, "key-notify-01")
	e.sent.fail = map[messaging.Channel]bool{messaging.WhatsApp: true}
	if err := e.drain(); err == nil {
		t.Fatal("a failed channel must be reported so the queue retries")
	}
	if l := waitFor("event: notification"); l == "" {
		t.Fatal("no live notification")
	}
	if l := waitFor("data:"); !strings.Contains(l, "confirmed") {
		t.Fatalf("live payload: %s", l)
	}
	if e.sent.count(messaging.SMS, "order_confirmed") != 1 || e.sent.count(messaging.WhatsApp, "order_confirmed") != 0 {
		t.Fatalf("first attempt: %+v", e.sent.msgs)
	}
	// The retry sends only what failed; nothing is duplicated.
	e.sent.fail = nil
	if err := e.drain(); err != nil {
		t.Fatal(err)
	}
	if e.sent.count(messaging.SMS, "order_confirmed") != 1 || e.sent.count(messaging.WhatsApp, "order_confirmed") != 1 {
		t.Fatalf("retry: %+v", e.sent.msgs)
	}
	var skipped string
	_ = e.pool.QueryRow(e.ctx, `SELECT d.status FROM notification_deliveries d JOIN notifications n ON n.id = d.notification_id
		WHERE n.template = 'order_confirmed' AND d.channel = 'email'`).Scan(&skipped)
	if skipped != "SKIPPED" { // the buyer has no email address
		t.Fatalf("email without an address: %q", skipped)
	}
	var sellerNotes int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM notifications WHERE account_id = $1 AND template = 'seller_new_order'`, seller).Scan(&sellerNotes)
	if sellerNotes != 1 {
		t.Fatalf("seller told about the order: %d", sellerNotes)
	}

	// Preferences: WhatsApp off means no WhatsApp for the next updates.
	if st, p := b.do("PUT", "/me/notification-prefs", map[string]bool{"whatsapp": false, "email": true, "marketing": false}, true); st != 200 || p["whatsapp"] != false {
		t.Fatalf("prefs: %d %v", st, p)
	}
	if _, err := e.ship.Ship(e.ctx, seller, uuid.MustParse(subOrder(o)["id"].(string)), 0); err != nil {
		t.Fatal(err)
	}
	e.ships.Wait()
	if err := e.drain(); err != nil {
		t.Fatal(err)
	}
	if e.sent.count(messaging.SMS, "order_shipped") != 1 || e.sent.count(messaging.WhatsApp, "order_shipped") != 0 || e.sent.count(messaging.SMS, "order_delivered") != 1 {
		t.Fatalf("after shipping: %+v", e.sent.msgs)
	}

	_, list := b.do("GET", "/me/notifications", nil, false)
	items := list["items"].([]any)
	if len(items) < 3 || list["unread"].(float64) != float64(len(items)) || items[0].(map[string]any)["template"] != "order_delivered" {
		t.Fatalf("inbox: %v", list)
	}
	first := items[0].(map[string]any)["id"].(string)
	if _, r := b.do("POST", "/me/notifications/read", map[string]any{"ids": []string{first}}, true); r["unread"].(float64) != float64(len(items)-1) {
		t.Fatalf("read one: %v", r)
	}
	if _, r := b.do("POST", "/me/notifications/read", map[string]any{"ids": []string{}}, true); r["unread"].(float64) != 0 {
		t.Fatalf("read all: %v", r)
	}
	other, _ := e.buyer("9876533333")
	if _, l := other.do("GET", "/me/notifications", nil, false); len(l["items"].([]any)) != 0 {
		t.Fatalf("someone else's inbox: %v", l)
	}
}
