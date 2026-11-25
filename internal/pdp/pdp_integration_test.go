package pdp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/search"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func TestProductPage(t *testing.T) {
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
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, seller_profiles, uploads, products, categories, config CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := store.New(pool)
	prods := &product.Service{Pool: pool, Q: q, Store: storage.NewMemory(), Log: log}
	svc := &pdp.Service{Q: q, Search: &search.Service{Pool: pool, Q: q}}
	srv := httptest.NewServer(server.New(server.Deps{Log: log, Health: &health.Handler{}, PDP: &pdp.Handler{Svc: svc},
		Search: &search.Handler{Svc: svc.Search}}))
	defer srv.Close()

	seller := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, role, phone_e164, approval) VALUES ($1, 'SELLER', '+919833333333', 'APPROVED')`, seller); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO seller_profiles (account_id, legal_name, store_name, verified_tag) VALUES ($1, 'L', 'Nova Store', true)`, seller); err != nil {
		t.Fatal(err)
	}
	var cat uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM categories WHERE slug = 'smartphones'`).Scan(&cat)
	var cover bytes.Buffer
	_ = png.Encode(&cover, image.NewNRGBA(image.Rect(0, 0, 500, 500)))
	add := func(name string, price int64, stock int32) product.View {
		v, err := prods.Create(ctx, seller, product.CreateInput{CategoryID: cat, Name: name, Brand: "Nova"})
		if err != nil {
			t.Fatal(err)
		}
		v, err = prods.Update(ctx, seller, v.ID, product.UpdateInput{CategoryID: cat, Name: name, Brand: "Nova", Description: strings.Repeat("A phone. ", 5),
			Warranty: "1 year", OriginCountry: "IN", Specs: map[string]any{"battery_mah": 5000.0, "os": "Android", "ram_gb": 8.0, "storage_gb": 128.0, "display_in": 6.7}, Version: v.Version})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = prods.SaveVariants(ctx, seller, v.ID, []product.VariantInput{
			{SKU: "A", Attrs: map[string]string{"Colour": "Black"}, MrpPaise: price * 5 / 4, PricePaise: price, Stock: stock},
			{SKU: "B", Attrs: map[string]string{"Colour": "Blue"}, MrpPaise: price * 5 / 4, PricePaise: price + 100000, Stock: 0},
		}); err != nil {
			t.Fatal(err)
		}
		up, _ := prods.UploadImage(ctx, seller, cover.Bytes())
		if _, err = prods.SaveImages(ctx, seller, v.ID, []product.ImageInput{{UploadID: up.ID, Alt: "Front"}}); err != nil {
			t.Fatal(err)
		}
		if v, err = prods.SetStatus(ctx, seller, v.ID, "ACTIVE"); err != nil {
			t.Fatal(err)
		}
		return v
	}
	main := add("Nova 5G", 1899900, 7)
	add("Nova Lite", 899900, 3)
	add("Nova Ultra", 8999900, 1)

	get := func(path string, out any) int {
		res, err := http.Get(srv.URL + "/api/v1" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			_ = json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}

	var p pdp.Product
	if st := get("/products/"+main.Slug, &p); st != 200 {
		t.Fatalf("product: %d", st)
	}
	if p.Seller.StoreName != "Nova Store" || !p.Seller.Verified || p.Category.ParentSlug != "mobiles-tablets" || len(p.Images) != 1 || len(p.Variants) != 2 {
		t.Fatalf("product: %+v", p)
	}
	if p.Specs[0].Key != "os" || p.Specs[0].Label != "Operating system" { // schema order, labelled
		t.Fatalf("spec order: %+v", p.Specs)
	}

	var live pdp.Live
	if st := get("/products/"+main.ID.String()+"/live", &live); st != 200 {
		t.Fatalf("live: %d", st)
	}
	if live.Viewer != "ANONYMOUS" || live.Variants[0].Available != 7 || live.Variants[0].ElectronsNonMember != 95 || live.Variants[0].ElectronsMember != 380 {
		t.Fatalf("live: %+v", live)
	}
	if live.Actions[0].Key != "buy_now" || live.Actions[0].Reason != "SIGN_IN" {
		t.Fatalf("anonymous actions: %+v", live.Actions)
	}
	for role, want := range map[auth.Role]string{auth.RoleBuyer: "buy_now", auth.RoleCreator: "request_demo", auth.RolePromoter: "promote"} {
		l, err := svc.LiveBlock(ctx, main.ID, &auth.Claims{Subject: uuid.New(), Role: role})
		if err != nil || l.Actions[0].Key != want {
			t.Errorf("%s actions: %+v %v", role, l.Actions, err)
		}
	}
	own, _ := svc.LiveBlock(ctx, main.ID, &auth.Claims{Subject: seller, Role: auth.RoleSeller})
	if own.Actions[0].Key != "edit" || !own.Actions[0].Enabled {
		t.Fatalf("owner actions: %+v", own.Actions)
	}

	var sim struct {
		Items []search.Card `json:"items"`
	}
	if st := get("/products/"+main.ID.String()+"/similar", &sim); st != 200 || len(sim.Items) != 2 || sim.Items[0].Name != "Nova Lite" {
		t.Fatalf("similar: %d %+v", st, sim.Items)
	}
	// Suggestions still route to their own handler next to /products/{slug}.
	if st := get("/products/suggest?q=nova", nil); st != 200 {
		t.Fatalf("suggest: %d", st)
	}

	if _, err := prods.SetStatus(ctx, seller, main.ID, "PAUSED"); err != nil {
		t.Fatal(err)
	}
	if st := get("/products/"+main.Slug, nil); st != 404 {
		t.Fatalf("paused product: %d", st)
	}
	if st := get("/products/"+main.ID.String()+"/live", nil); st != 404 {
		t.Fatalf("paused live: %d", st)
	}
	if st := get("/products/no-such-thing", nil); st != 404 {
		t.Fatalf("unknown: %d", st)
	}
}
