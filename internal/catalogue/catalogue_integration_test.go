package catalogue_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func TestSeedAndCategoryAPI(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE categories CASCADE`); err != nil {
		t.Fatal(err)
	}

	n, err := catalogue.Seed(ctx, pool)
	if err != nil || n != 65 {
		t.Fatalf("first seed: %d rows, %v (want 12 + 53)", n, err)
	}
	// Admin edits survive a second seed.
	if _, err := pool.Exec(ctx, `UPDATE categories SET name = 'Phones & Tablets' WHERE slug = 'mobiles-tablets'`); err != nil {
		t.Fatal(err)
	}
	if n, err := catalogue.Seed(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second seed: %d rows, %v", n, err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(server.New(server.Deps{Log: log, Health: &health.Handler{}, Catalogue: &catalogue.Handler{Q: store.New(pool)}}))
	defer srv.Close()

	var tree struct {
		Categories []catalogue.Node `json:"categories"`
	}
	res := get(t, srv.URL+"/api/v1/categories", "", &tree)
	if res.StatusCode != 200 || len(tree.Categories) != 12 || tree.Categories[0].Name != "Phones & Tablets" || len(tree.Categories[0].Children) != 5 {
		t.Fatalf("tree: %d %+v", res.StatusCode, tree.Categories[:1])
	}
	if again := get(t, srv.URL+"/api/v1/categories", res.Header.Get("ETag"), nil); again.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match should give 304, got %d", again.StatusCode)
	}

	var d catalogue.Detail
	if res := get(t, srv.URL+"/api/v1/categories/smartphones", "", &d); res.StatusCode != 200 {
		t.Fatalf("detail: %d", res.StatusCode)
	}
	if d.Parent == nil || d.Parent.Slug != "mobiles-tablets" || len(d.SpecSchema.Fields) == 0 || d.SpecSchema.Fields[0].Key != "os" {
		t.Fatalf("detail: %+v", d)
	}

	// Retiring a category hides it and its sub-categories.
	if _, err := pool.Exec(ctx, `UPDATE categories SET status = 'RETIRED' WHERE slug = 'gaming'`); err != nil {
		t.Fatal(err)
	}
	get(t, srv.URL+"/api/v1/categories", "", &tree)
	if len(tree.Categories) != 11 {
		t.Fatalf("retired category still listed: %d", len(tree.Categories))
	}
	if res := get(t, srv.URL+"/api/v1/categories/consoles", "", nil); res.StatusCode != 404 {
		t.Fatalf("child of retired category: %d", res.StatusCode)
	}
	if res := get(t, srv.URL+"/api/v1/categories/nope", "", nil); res.StatusCode != 404 {
		t.Fatalf("unknown slug: %d", res.StatusCode)
	}
}

func get(t *testing.T, url, etag string, out any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode == 200 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return res
}
