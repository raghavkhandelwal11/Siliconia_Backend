package cart_test

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
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
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
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	srv   *httptest.Server
	prods *product.Service
	inbox *inbox
	mr    *miniredis.Miniredis
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
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, seller_profiles, uploads, products, categories, carts, wishlist_items CASCADE`); err != nil {
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
	srv := httptest.NewServer(server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: authSvc, Auth: auth.NewHandler(authSvc, nil, false),
		Cart: &cart.Handler{Svc: &cart.Service{Pool: pool, Q: q}},
	}))
	t.Cleanup(srv.Close)
	return &env{t: t, ctx: ctx, pool: pool, srv: srv, prods: &product.Service{Pool: pool, Q: q, Store: storage.NewMemory(), Log: log}, inbox: in, mr: mr}
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
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
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

func lines(c map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, g := range c["groups"].([]any) {
		for _, l := range g.(map[string]any)["lines"].([]any) {
			m := l.(map[string]any)
			out[m["name"].(string)] = m
		}
	}
	return out
}

func TestGuestCartMergesOnSignIn(t *testing.T) {
	e := setup(t)
	bank, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 5)
	slim, slimProduct, slimSeller := e.listed("Orbit Store", "Orbit Slim", 109900, 50)

	g := e.browser()
	if st, c := g.do("GET", "/cart", nil, false); st != 200 || len(c["groups"].([]any)) != 0 || g.cookie(auth.CookieDevice) != "" {
		t.Fatalf("empty guest cart: %d %v", st, c)
	}
	if st, c := g.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 2}, false); st != 200 || c["itemCount"].(float64) != 2 {
		t.Fatalf("first guest add: %d %v", st, c)
	}
	if g.cookie(auth.CookieDevice) == "" || g.cookie(auth.CookieCSRF) == "" {
		t.Fatal("guest cookies not set")
	}
	if st, o := g.do("PUT", "/cart/items/"+slim.String(), map[string]int{"qty": 1}, false); st != 403 || o["code"] != "CSRF_FAILED" {
		t.Fatalf("guest write without CSRF: %d %v", st, o)
	}
	for qty, want := range map[int]int{11: 400, 6: 409} {
		if st, o := g.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": qty}, true); st != want {
			t.Fatalf("qty %d: %d %v", qty, st, o)
		}
	}
	if _, o := g.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 6}, true); o["detail"] != "Only 5 left" {
		t.Fatalf("stock message: %v", o)
	}
	_, c := g.do("PUT", "/cart/items/"+slim.String(), map[string]int{"qty": 3}, true)
	if len(c["groups"].([]any)) != 2 || c["subtotalPaise"].(float64) != 2*279900+3*109900 || c["electronsNonMember"].(float64) != 45 || c["canCheckout"] != true {
		t.Fatalf("guest cart: %v", c)
	}

	// The buyer already has one Volt Cell; signing in on this device merges (2+1=3, within stock).
	b := e.browser()
	b.signIn("9876511111", "BUYER")
	b.do("PUT", "/cart/items/"+bank.String(), map[string]int{"qty": 4}, true)
	u, _ := url.Parse(e.srv.URL + "/")
	g.jar.SetCookies(u, b.jar.Cookies(u)) // same browser, now signed in
	g.jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieDevice, Value: g.cookie(auth.CookieDevice), Path: "/"}})
	_, c = g.do("GET", "/cart", nil, false)
	ls := lines(c)
	merged, _ := c["merged"].(map[string]any)
	if merged == nil || merged["adjusted"] != true || ls["Volt Cell"]["qty"].(float64) != 5 || ls["Orbit Slim"]["qty"].(float64) != 3 {
		t.Fatalf("merge (4+2 capped at stock 5): %v", c)
	}
	if g.cookie(auth.CookieDevice) != "" {
		t.Fatal("device cookie should be cleared after merge")
	}

	// A price change and a paused product show up on the cart.
	if _, err := e.pool.Exec(e.ctx, `UPDATE variants SET price_paise = 99900 WHERE id = $1`, slim); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prods.SetStatus(e.ctx, slimSeller, slimProduct, "PAUSED"); err != nil {
		t.Fatal(err)
	}
	_, c = g.do("GET", "/cart", nil, false)
	ls = lines(c)
	if ls["Orbit Slim"]["problem"] != "UNAVAILABLE" || ls["Orbit Slim"]["priceAtAddPaise"].(float64) != 109900 || c["canCheckout"] != false {
		t.Fatalf("problems: %v", c)
	}
	g.do("DELETE", "/cart/items/"+slim.String(), nil, true)
	if _, c = g.do("GET", "/cart", nil, false); c["canCheckout"] != true || len(lines(c)) != 1 {
		t.Fatalf("after removing: %v", c)
	}
}

func TestWishlistAndRoles(t *testing.T) {
	e := setup(t)
	v, _, _ := e.listed("Volt Store", "Volt Cell", 279900, 0) // out of stock can still be saved

	guest := e.browser()
	if st, _ := guest.do("PUT", "/wishlist/"+v.String(), nil, false); st != 401 {
		t.Fatalf("guest wishlist: %d", st)
	}
	b := e.browser()
	b.signIn("9876522222", "BUYER")
	if st, o := b.do("PUT", "/wishlist/"+v.String(), nil, true); st != 204 {
		t.Fatalf("save: %d %v", st, o)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE variants SET price_paise = 249900 WHERE id = $1`, v); err != nil {
		t.Fatal(err)
	}
	_, w := b.do("GET", "/wishlist", nil, false)
	items := w["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["priceDropped"] != true || items[0].(map[string]any)["inStock"] != false {
		t.Fatalf("wishlist: %v", w)
	}
	b.do("DELETE", "/wishlist/"+v.String(), nil, true)
	if _, w = b.do("GET", "/wishlist", nil, false); len(w["items"].([]any)) != 0 {
		t.Fatalf("after remove: %v", w)
	}
	if st, _ := b.do("PUT", "/cart/items/"+v.String(), map[string]int{"qty": 1}, true); st != 409 {
		t.Fatalf("out of stock add: %d", st)
	}

	s := e.browser()
	s.signIn("9876533333", "SELLER")
	if st, o := s.do("GET", "/cart", nil, false); st != 403 || o["code"] != "OTHER_ROLE" {
		t.Fatalf("seller cart: %d %v", st, o)
	}
}
