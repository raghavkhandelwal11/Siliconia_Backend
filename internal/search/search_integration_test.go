package search_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/search"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	prod  *product.Service
	srv   *httptest.Server
	ctx   context.Context
	cover []byte
}

func setup(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, seller_profiles, uploads, products, categories CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := store.New(pool)
	prod := &product.Service{Pool: pool, Q: q, Store: storage.NewMemory(), Log: log}
	srv := httptest.NewServer(server.New(server.Deps{Log: log, Health: &health.Handler{}, Search: &search.Handler{Svc: &search.Service{Pool: pool, Q: q}}}))
	t.Cleanup(srv.Close)
	img := image.NewNRGBA(image.Rect(0, 0, 500, 500))
	img.Set(1, 1, color.NRGBA{R: 255, A: 255})
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return &fixture{t: t, pool: pool, prod: prod, srv: srv, ctx: ctx, cover: b.Bytes()}
}

func (f *fixture) seller(phone string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO accounts (id, role, phone_e164, name, approval) VALUES ($1, 'SELLER', $2, 'S', 'APPROVED')`, id, "+91"+phone); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO seller_profiles (account_id, legal_name, store_name) VALUES ($1, 'L', $2)`, id, "Store "+phone); err != nil {
		f.t.Fatal(err)
	}
	return id
}

type item struct {
	cat, name, brand string
	local            bool
	specs            map[string]any
	price, stock     int64
	publish          bool
}

func (f *fixture) add(seller uuid.UUID, it item) uuid.UUID {
	f.t.Helper()
	var catID uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM categories WHERE slug = $1`, it.cat).Scan(&catID); err != nil {
		f.t.Fatal(err)
	}
	v, err := f.prod.Create(f.ctx, seller, product.CreateInput{CategoryID: catID, Name: it.name, Brand: it.brand})
	if err != nil {
		f.t.Fatal(err)
	}
	if v, err = f.prod.Update(f.ctx, seller, v.ID, product.UpdateInput{
		CategoryID: catID, Name: it.name, Brand: it.brand, Description: strings.Repeat("Great product. ", 4), Warranty: "1 year",
		OriginCountry: "IN", IsLocal: it.local, Specs: it.specs, Version: v.Version,
	}); err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.prod.SaveVariants(f.ctx, seller, v.ID, []product.VariantInput{{SKU: "SKU-1", MrpPaise: it.price * 125 / 100, PricePaise: it.price, Stock: int32(it.stock)}}); err != nil {
		f.t.Fatal(err)
	}
	up, err := f.prod.UploadImage(f.ctx, seller, f.cover)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.prod.SaveImages(f.ctx, seller, v.ID, []product.ImageInput{{UploadID: up.ID}}); err != nil {
		f.t.Fatal(err)
	}
	if it.publish {
		if _, err := f.prod.SetStatus(f.ctx, seller, v.ID, "ACTIVE"); err != nil {
			got, _ := f.prod.Get(f.ctx, seller, v.ID)
			f.t.Fatalf("%s: %v; missing %v; spec problems %v", it.name, err, got.Missing, got.SpecProblems)
		}
	}
	return v.ID
}

type result struct {
	Items []struct {
		Name        string `json:"name"`
		PricePaise  int64  `json:"pricePaise"`
		DiscountPct int    `json:"discountPct"`
		InStock     bool   `json:"inStock"`
		CoverURL    string `json:"coverUrl"`
	} `json:"items"`
	Total  int64 `json:"total"`
	Facets struct {
		Categories []search.Count     `json:"categories"`
		Brands     []search.Count     `json:"brands"`
		Price      *[2]int64          `json:"price"`
		Specs      []search.SpecFacet `json:"specs"`
	} `json:"facets"`
	Ignored []string `json:"ignoredFilters"`
	Code    string   `json:"code"`
}

func (f *fixture) get(query string) (int, result) {
	f.t.Helper()
	res, err := http.Get(f.srv.URL + "/api/v1/products?" + query)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	var r result
	_ = json.NewDecoder(res.Body).Decode(&r)
	return res.StatusCode, r
}

func names(r result) string {
	out := make([]string, len(r.Items))
	for i, it := range r.Items {
		out[i] = it.Name
	}
	return strings.Join(out, ",")
}

func counts(cs []search.Count) map[string]int64 {
	m := map[string]int64{}
	for _, c := range cs {
		m[c.Value] = c.Count
	}
	return m
}

func TestSearchFiltersFacetsAndSorting(t *testing.T) {
	f := setup(t)
	a, b := f.seller("9811111111"), f.seller("9822222222")
	phone := func(os string, ram, storage float64, g5 bool) map[string]any {
		return map[string]any{"os": os, "ram_gb": ram, "storage_gb": storage, "display_in": 6.5, "battery_mah": 5000.0, "has_5g": g5}
	}
	f.add(a, item{"smartphones", "Nova 5G", "Nova", true, phone("Android", 8, 128, true), 1999900, 10, true})
	f.add(b, item{"smartphones", "Zen Pro", "Zen", false, phone("iOS", 6, 256, true), 5999900, 0, true})
	lite := f.add(a, item{"smartphones", "Nova Lite", "Nova", false, phone("Android", 4, 64, false), 999900, 5, true})
	f.add(a, item{"laptops", "Aero 14", "Aero", false, map[string]any{"processor": "X1", "ram_gb": 16.0, "storage_gb": 512.0, "display_in": 14.0}, 6999900, 3, true})
	f.add(a, item{"smartphones", "Nova Draft", "Nova", false, phone("Android", 8, 128, true), 100000, 1, false})

	st, r := f.get("category=smartphones&sort=price_asc")
	if st != 200 || names(r) != "Nova Lite,Nova 5G,Zen Pro" || r.Total != 3 {
		t.Fatalf("category listing: %d %s %+v", st, names(r), r)
	}
	if r.Items[0].DiscountPct != 20 || r.Items[0].CoverURL == "" || r.Items[2].InStock {
		t.Fatalf("card fields: %+v", r.Items)
	}
	if b := counts(r.Facets.Brands); b["Nova"] != 2 || b["Zen"] != 1 {
		t.Fatalf("brands: %v", r.Facets.Brands)
	}
	if r.Facets.Price == nil || r.Facets.Price[0] != 999900 || r.Facets.Price[1] != 5999900 {
		t.Fatalf("price facet: %v", r.Facets.Price)
	}
	var osFacet, ramFacet *search.SpecFacet
	for i := range r.Facets.Specs {
		switch r.Facets.Specs[i].Key {
		case "os":
			osFacet = &r.Facets.Specs[i]
		case "ram_gb":
			ramFacet = &r.Facets.Specs[i]
		}
	}
	if osFacet == nil || counts(osFacet.Options)["Android"] != 2 || ramFacet == nil || *ramFacet.Min != 4 || *ramFacet.Max != 8 {
		t.Fatalf("spec facets: %+v", r.Facets.Specs)
	}

	// Category names match too: "laptop" finds Aero 14 though its name lacks the word.
	if _, r = f.get("q=laptop"); names(r) != "Aero 14" {
		t.Fatalf("category-name search: %s", names(r))
	}
	if _, r = f.get("q=phone&sort=price_asc"); names(r) != "Nova Lite,Nova 5G,Zen Pro" {
		t.Fatalf("parent-category search: %s", names(r))
	}

	// Choosing iOS narrows results and brands, but OS still offers Android.
	_, r = f.get("category=smartphones&spec.os=iOS")
	if names(r) != "Zen Pro" || len(r.Facets.Brands) != 1 {
		t.Fatalf("os=iOS: %s %v", names(r), r.Facets.Brands)
	}
	for _, sf := range r.Facets.Specs {
		if sf.Key == "os" && counts(sf.Options)["Android"] != 2 {
			t.Fatalf("os facet should ignore its own filter: %+v", sf)
		}
	}
	checks := map[string]string{
		"category=smartphones&brand=nova&spec.ram_gb=6..8":  "Nova 5G",
		"category=smartphones&spec.has_5g=false":            "Nova Lite",
		"category=smartphones&inStock=true&sort=price_desc": "Nova 5G,Nova Lite",
		"local=true": "Nova 5G",
		"category=smartphones&priceMax=2000000&sort=price_asc": "Nova Lite,Nova 5G",
		"q=nova&sort=price_asc":                                "Nova Lite,Nova 5G",
		"q=lite":                                               "Nova Lite",
		"q=aero":                                               "Aero 14",
		"category=smartphones&sort=price_asc&pageSize=2&page=2": "Zen Pro",
	}
	for query, want := range checks {
		if st, r := f.get(query); st != 200 || names(r) != want {
			t.Errorf("%s: %d got %q want %q", query, st, names(r), want)
		}
	}
	_, r = f.get("category=mobiles-tablets&spec.os=iOS")
	if r.Total != 3 || len(r.Ignored) != 1 || counts(r.Facets.Categories)["smartphones"] != 3 {
		t.Fatalf("top-level category: %+v", r)
	}
	for query, want := range map[string]int{"sort=cheapest": 400, "category=smartphones&spec.ram_gb=8": 400, "category=nope": 404, "page=0": 400} {
		if st, _ := f.get(query); st != want {
			t.Errorf("%s: %d want %d", query, st, want)
		}
	}

	// Paused products and sellers who are no longer approved drop out.
	if _, err := f.prod.SetStatus(f.ctx, a, lite, "PAUSED"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE accounts SET approval = 'SUSPENDED' WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}
	if _, r := f.get("category=smartphones"); names(r) != "Nova 5G" {
		t.Fatalf("after pause and suspension: %s", names(r))
	}

	res, err := http.Get(f.srv.URL + "/api/v1/products/suggest?q=nov")
	if err != nil {
		t.Fatal(err)
	}
	var sg struct {
		Suggestions []search.Suggestion `json:"suggestions"`
	}
	_ = json.NewDecoder(res.Body).Decode(&sg)
	res.Body.Close()
	kinds := map[string]string{}
	for _, s := range sg.Suggestions {
		kinds[s.Kind] += s.Text + ";"
	}
	if !strings.Contains(kinds["product"], "Nova 5G") || !strings.Contains(kinds["brand"], "Nova") || strings.Contains(kinds["product"], "Draft") {
		t.Fatalf("suggest: %+v", sg.Suggestions)
	}
}
