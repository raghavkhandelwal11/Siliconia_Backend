package product_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
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

var codeRE = regexp.MustCompile(`\b\d{6}\b`)

func (b *inbox) Send(_ context.Context, m messaging.Message) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last[m.To] = codeRE.FindString(m.Body)
	return "t", nil
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	pool  *pgxpool.Pool
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
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, seller_profiles, uploads, products, categories CASCADE`); err != nil {
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
	limiter := ratelimit.New(rdb)
	authSvc := &auth.Service{Pool: pool, Q: store.New(pool), Redis: rdb, Limiter: limiter, Messages: in, Signer: signer, Box: box, Pepper: []byte("p"), Log: log}
	products := &product.Service{Pool: pool, Q: authSvc.Q, Store: storage.NewMemory(), Limiter: limiter, Log: log}
	srv := httptest.NewServer(server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: authSvc, Auth: auth.NewHandler(authSvc, nil, false),
		Product: &product.Handler{Svc: products},
	}))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, pool: pool, inbox: in, mr: mr}
}

type client struct {
	e   *env
	jar *cookiejar.Jar
}

// seller signs in a seller account and, unless pending, approves it.
func (e *env) seller(phone string, approved bool) *client {
	e.t.Helper()
	j, _ := cookiejar.New(nil)
	c := &client{e: e, jar: j}
	e.mr.FastForward(time.Minute)
	_, out := c.json("POST", "/auth/otp/send", map[string]string{"phone": phone, "role": "SELLER"})
	c.must("POST", "/auth/otp/verify", map[string]string{"challengeId": out["challengeId"].(string), "code": e.inbox.last["+91"+phone]}, 200)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO seller_profiles (account_id, legal_name, store_name)
		SELECT id, 'Legal '||$1, 'Store '||$1 FROM accounts WHERE phone_e164 = '+91'||$1 AND role = 'SELLER'`, phone); err != nil {
		e.t.Fatal(err)
	}
	if approved {
		if _, err := e.pool.Exec(ctx, `UPDATE accounts SET approval = 'APPROVED' WHERE phone_e164 = '+91'||$1`, phone); err != nil {
			e.t.Fatal(err)
		}
	}
	return c
}

func (c *client) do(req *http.Request) (int, map[string]any, []byte) {
	c.e.t.Helper()
	u, _ := url.Parse(c.e.srv.URL + "/")
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == auth.CookieCSRF {
			req.Header.Set(auth.HeaderCSRF, ck.Value)
		}
	}
	res, err := (&http.Client{Jar: c.jar}).Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, raw
}

func (c *client) json(method, path string, body any) (int, map[string]any) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	st, out, _ := c.do(req)
	return st, out
}

func (c *client) must(method, path string, body any, want int) map[string]any {
	c.e.t.Helper()
	st, out := c.json(method, path, body)
	if st != want {
		c.e.t.Fatalf("%s %s: got %d want %d: %v", method, path, st, want, out)
	}
	return out
}

func (c *client) upload(data []byte) (int, map[string]any) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	f, _ := w.CreateFormFile("file", "photo.png")
	_, _ = f.Write(data)
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.e.srv.URL+"/api/v1/seller/uploads/images", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	st, out, _ := c.do(req)
	return st, out
}

func pngImage(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.NRGBA{B: 200, A: 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func (e *env) categoryID(slug string) string {
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM categories WHERE slug = $1`, slug).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestProductLifecycle(t *testing.T) {
	e := setup(t)
	s := e.seller("9876500051", true)
	phones := e.categoryID("smartphones")

	if st, o := s.json("POST", "/seller/products", map[string]any{"categoryId": e.categoryID("mobiles-tablets"), "name": "Nova 5G"}); st != 400 {
		t.Fatalf("top-level category accepted: %d %v", st, o)
	}
	p := s.must("POST", "/seller/products", map[string]any{"categoryId": phones, "name": "Nova 5G (8 GB)", "brand": "Nova"}, 200)
	id := p["id"].(string)
	if p["status"] != "DRAFT" || !strings.HasPrefix(p["slug"].(string), "nova-5g-8-gb-") {
		t.Fatalf("draft: %v", p)
	}

	details := map[string]any{
		"categoryId": phones, "name": "Nova 5G (8 GB)", "brand": "Nova", "model": "N5", "warranty": "1 year manufacturer warranty",
		"description": "A bright 120 Hz display, a big battery and fast charging for everyday use.", "originCountry": "in",
		"isLocal": true, "specs": map[string]any{"ram_gb": "eight"}, "version": p["version"],
	}
	if st, o := s.json("PUT", "/seller/products/"+id, details); st != 422 || o["code"] != "SPECS_INVALID" {
		t.Fatalf("bad spec type: %d %v", st, o)
	}
	details["specs"] = map[string]any{"ram_gb": 8, "os": "Android"} // a partial draft is fine
	p = s.must("PUT", "/seller/products/"+id, details, 200)
	if !contains(p["missing"], "specs") || !contains(p["missing"], "variants") || !contains(p["missing"], "images") {
		t.Fatalf("missing after details: %v", p["missing"])
	}
	if st, o := s.json("PUT", "/seller/products/"+id, details); st != 409 || o["code"] != "VERSION_CONFLICT" {
		t.Fatalf("stale version: %d %v", st, o)
	}
	details["version"] = p["version"]
	details["specs"] = map[string]any{"os": "Android", "ram_gb": 8, "storage_gb": 128, "display_in": 6.7, "battery_mah": 5000, "has_5g": true}
	p = s.must("PUT", "/seller/products/"+id, details, 200)
	if contains(p["missing"], "specs") || p["originCountry"] != "IN" {
		t.Fatalf("specs complete: %v", p)
	}

	variants := []map[string]any{
		{"sku": "NOVA-8-128-BLK", "attrs": map[string]string{"Colour": "Black", "Storage": "128 GB"}, "mrpPaise": 2499900, "pricePaise": 1999900, "stock": 25},
		{"sku": "nova-8-128-blk", "attrs": map[string]string{"Colour": "Blue"}, "mrpPaise": 2499900, "pricePaise": 1999900, "stock": 5},
	}
	if st, o := s.json("PUT", "/seller/products/"+id+"/variants", map[string]any{"variants": variants}); st != 409 || o["code"] != "DUPLICATE_SKU" {
		t.Fatalf("duplicate sku: %d %v", st, o)
	}
	variants[1]["sku"] = "NOVA-8-128-BLU"
	variants[1]["pricePaise"] = 2599900 // above MRP
	if st, _ := s.json("PUT", "/seller/products/"+id+"/variants", map[string]any{"variants": variants}); st != 400 {
		t.Fatalf("price above MRP: %d", st)
	}
	variants[1]["pricePaise"] = 1949900
	p = s.must("PUT", "/seller/products/"+id+"/variants", map[string]any{"variants": variants}, 200)
	vs := p["variants"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["stock"].(float64) != 25 {
		t.Fatalf("variants: %v", vs)
	}

	if st, o := s.upload(pngImage(200, 200)); st != 422 || o["code"] != "IMAGE_REJECTED" {
		t.Fatalf("small image: %d %v", st, o)
	}
	if st, o := s.upload([]byte("<html>not an image</html>")); st != 422 {
		t.Fatalf("fake image: %d %v", st, o)
	}
	st, up := s.upload(pngImage(1200, 900))
	if st != 200 {
		t.Fatalf("upload: %d %v", st, up)
	}
	if st, o := s.json("POST", "/seller/products/"+id+"/status", map[string]string{"status": "ACTIVE"}); st != 422 || !strings.Contains(o["detail"].(string), "images") {
		t.Fatalf("publish without images: %d %v", st, o)
	}
	p = s.must("PUT", "/seller/products/"+id+"/images", map[string]any{"images": []map[string]any{{"uploadId": up["id"], "alt": "Front in black"}}}, 200)
	img := p["images"].([]any)[0].(map[string]any)
	if img["alt"] != "Front in black" || img["width"].(float64) != 1200 {
		t.Fatalf("images: %v", p["images"])
	}

	// The stored image is a JPEG served with long caching.
	req, _ := http.NewRequest("GET", e.srv.URL+img["url"].(string), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/jpeg" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("media: %v %v", res, err)
	}
	res.Body.Close()

	p = s.must("POST", "/seller/products/"+id+"/status", map[string]string{"status": "ACTIVE"}, 200)
	if p["status"] != "ACTIVE" || p["publishedAt"] == nil {
		t.Fatalf("publish: %v", p)
	}
	// The category is fixed once published.
	details["version"] = p["version"]
	details["categoryId"] = e.categoryID("tablets")
	details["specs"] = map[string]any{}
	if st, o := s.json("PUT", "/seller/products/"+id, details); st != 409 || o["code"] != "CATEGORY_LOCKED" {
		t.Fatalf("category change after publish: %d %v", st, o)
	}

	list := s.must("GET", "/seller/products", nil, 200)["products"].([]any)
	row := list[0].(map[string]any)
	if row["minPricePaise"].(float64) != 1949900 || row["maxPricePaise"].(float64) != 1999900 || row["available"].(float64) != 30 || row["coverUrl"] == "" {
		t.Fatalf("list row: %v", row)
	}
	s.must("POST", "/seller/products/"+id+"/status", map[string]string{"status": "PAUSED"}, 200)
	if st, _ := s.json("POST", "/seller/products/"+id+"/status", map[string]string{"status": "DRAFT"}); st != 409 {
		t.Fatalf("paused to draft: %d", st)
	}

	// Units reserved by an order protect the variant and its stock.
	v0 := vs[0].(map[string]any)
	if _, err := e.pool.Exec(context.Background(), `UPDATE inventory_lots SET reserved = 3, sold = 2 WHERE variant_id = $1`, v0["id"]); err != nil {
		t.Fatal(err)
	}
	v0copy := map[string]any{"id": v0["id"], "sku": v0["sku"], "attrs": v0["attrs"], "mrpPaise": v0["mrpPaise"], "pricePaise": v0["pricePaise"], "stock": 4}
	if st, o := s.json("PUT", "/seller/products/"+id+"/variants", map[string]any{"variants": []any{v0copy}}); st != 409 || o["code"] != "STOCK_BELOW_COMMITTED" {
		t.Fatalf("stock below committed: %d %v", st, o)
	}
	other := vs[1].(map[string]any)
	otherOnly := map[string]any{"id": other["id"], "sku": other["sku"], "attrs": other["attrs"], "mrpPaise": other["mrpPaise"], "pricePaise": other["pricePaise"], "stock": 5}
	if st, o := s.json("PUT", "/seller/products/"+id+"/variants", map[string]any{"variants": []any{otherOnly}}); st != 409 || o["code"] != "VARIANT_IN_USE" {
		t.Fatalf("remove variant with orders: %d %v", st, o)
	}
}

func TestProductAccessRules(t *testing.T) {
	e := setup(t)
	pending := e.seller("9876500061", false)
	if st, o := pending.json("POST", "/seller/products", map[string]any{"categoryId": e.categoryID("smartphones"), "name": "Phone X"}); st != 403 || o["code"] != "SELLER_NOT_APPROVED" {
		t.Fatalf("pending seller: %d %v", st, o)
	}
	a := e.seller("9876500062", true)
	b := e.seller("9876500063", true)
	p := a.must("POST", "/seller/products", map[string]any{"categoryId": e.categoryID("laptops"), "name": "Aero 14"}, 200)
	if st, _ := b.json("GET", "/seller/products/"+p["id"].(string), nil); st != 404 {
		t.Fatalf("other seller reads product: %d", st)
	}
	_, up := a.upload(pngImage(800, 800))
	pb := b.must("POST", "/seller/products", map[string]any{"categoryId": e.categoryID("laptops"), "name": "Bolt 15"}, 200)
	if st, _ := b.json("PUT", "/seller/products/"+pb["id"].(string)+"/images", map[string]any{"images": []map[string]any{{"uploadId": up["id"]}}}); st != 400 {
		t.Fatalf("attach another seller's upload: %d", st)
	}
	if list := b.must("GET", "/seller/products", nil, 200)["products"].([]any); len(list) != 1 {
		t.Fatalf("list leaks: %v", list)
	}
}

func contains(list any, s string) bool {
	for _, x := range list.([]any) {
		if x == s {
			return true
		}
	}
	return false
}
