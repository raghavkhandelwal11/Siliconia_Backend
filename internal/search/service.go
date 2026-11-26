package search

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Card is a product in a listing.
type Card struct {
	ID             uuid.UUID `json:"id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Brand          string    `json:"brand"`
	CoverURL       string    `json:"coverUrl,omitempty"`
	PricePaise     int64     `json:"pricePaise"`
	MrpPaise       int64     `json:"mrpPaise"`
	DiscountPct    int       `json:"discountPct"`
	InStock        bool      `json:"inStock"`
	RatingAvg      float64   `json:"ratingAvg"`
	RatingCount    int32     `json:"ratingCount"`
	IsLocal        bool      `json:"isLocal"`
	VerifiedSeller bool      `json:"verifiedSeller"`
}

// Count is one facet value.
type Count struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
	Count int64  `json:"count"`
}

// SpecFacet is a category spec filter with its options or range.
type SpecFacet struct {
	Key     string              `json:"key"`
	Label   string              `json:"label"`
	Type    catalogue.FieldType `json:"type"`
	Unit    string              `json:"unit,omitempty"`
	Options []Count             `json:"options,omitempty"`
	Min     *float64            `json:"min,omitempty"`
	Max     *float64            `json:"max,omitempty"`
}

// Facets describe what the shopper can narrow by.
type Facets struct {
	Categories []Count     `json:"categories"`
	Brands     []Count     `json:"brands"`
	Price      *[2]int64   `json:"price,omitempty"`
	Specs      []SpecFacet `json:"specs"`
}

// CategoryInfo names the listed category.
type CategoryInfo struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Icon       string `json:"icon,omitempty"`
	ParentSlug string `json:"parentSlug,omitempty"`
	ParentName string `json:"parentName,omitempty"`
}

// Result is one page of a listing.
type Result struct {
	Items    []Card        `json:"items"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
	Facets   Facets        `json:"facets"`
	Category *CategoryInfo `json:"category,omitempty"`
	Ignored  []string      `json:"ignoredFilters,omitempty"`
}

// Service runs listing queries.
type Service struct {
	Pool *pgxpool.Pool
	Q    *store.Queries
}

// listing is every product a shopper may see, with its price, stock and cover.
const listing = `
SELECT p.id, p.slug, p.name, p.brand, p.is_local, p.rating_avg::float8 AS rating_avg, p.rating_count, p.created_at,
       p.specs, p.category_id, p.search, sp.verified_tag,
       to_tsvector('english', c.name || ' ' || coalesce(pc.name, '')) AS cat_search, c.name AS cat_name,
       pv.price_paise, pv.mrp_paise, st.available, st.sold, coalesce(cov.storage_key, '') AS cover
FROM products p
JOIN accounts a ON a.id = p.seller_id AND a.status = 'ACTIVE' AND a.approval = 'APPROVED'
JOIN seller_profiles sp ON sp.account_id = p.seller_id
JOIN categories c ON c.id = p.category_id
LEFT JOIN categories pc ON pc.id = c.parent_id
JOIN LATERAL (SELECT v.price_paise, v.mrp_paise FROM variants v WHERE v.product_id = p.id ORDER BY v.price_paise, v.position LIMIT 1) pv ON TRUE
JOIN LATERAL (SELECT coalesce(sum(l.quantity - l.reserved - l.sold), 0)::int AS available, coalesce(sum(l.sold), 0)::int AS sold
              FROM inventory_lots l JOIN variants v ON v.id = l.variant_id WHERE v.product_id = p.id AND l.kind = 'SELLER') st ON TRUE
LEFT JOIN LATERAL (SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
                   WHERE i.product_id = p.id ORDER BY i.position LIMIT 1) cov ON TRUE
WHERE p.status = 'ACTIVE'`

// Search returns one page plus facets.
func (s *Service) Search(ctx context.Context, p Params) (Result, error) {
	res := Result{Items: []Card{}, Page: p.Page, PageSize: p.PageSize, Facets: Facets{Categories: []Count{}, Brands: []Count{}, Specs: []SpecFacet{}}}
	var schema *catalogue.SpecSchema
	var categoryIDs []uuid.UUID
	if p.Category != "" {
		info, ids, sch, err := s.category(ctx, p.Category)
		if err != nil {
			return res, err
		}
		res.Category, categoryIDs, schema = info, ids, sch
	}
	if schema != nil {
		if err := p.checkSpecs(*schema); err != nil {
			return res, httpx.ErrValidation.WithDetail(err.Error())
		}
	} else {
		for k := range p.Specs {
			p.unknownSpecs = append(p.unknownSpecs, k)
		}
		for k := range p.SpecRanges {
			p.unknownSpecs = append(p.unknownSpecs, k)
		}
		p.Specs, p.SpecRanges = map[string][]string{}, map[string][2]*float64{}
	}
	slices.Sort(p.unknownSpecs)
	res.Ignored = p.unknownSpecs

	// base: category, text, price, rating, local, stock, verified (B).
	base := &builder{}
	if categoryIDs != nil {
		base.where("l.category_id = ANY(" + base.arg(categoryIDs) + ")")
	}
	var qArg string
	if p.Q != "" {
		qArg = base.arg(p.Q)
		like := base.arg("%" + escapeLike(p.Q) + "%")
		// Category names count too, so "power bank" or "tv" finds those products.
		base.where("(l.search @@ websearch_to_tsquery('english', " + qArg + ") OR l.cat_search @@ websearch_to_tsquery('english', " + qArg + ") OR l.name % " + qArg + " OR l.name ILIKE " + like + " OR l.brand ILIKE " + like + " OR l.cat_name ILIKE " + like + ")")
	}
	if p.PriceMin > 0 {
		base.where("l.price_paise >= " + base.arg(p.PriceMin))
	}
	if p.PriceMax > 0 {
		base.where("l.price_paise <= " + base.arg(p.PriceMax))
	}
	if p.MinRating > 0 {
		base.where("l.rating_avg >= " + base.arg(p.MinRating))
	}
	if p.Local {
		base.where("l.is_local")
	}
	if p.InStock {
		base.where("l.available > 0")
	}
	if p.Verified {
		base.where("l.verified_tag")
	}
	withBrand := func(b *builder) {
		if len(p.Brands) > 0 {
			lower := make([]string, len(p.Brands))
			for i, x := range p.Brands {
				lower[i] = strings.ToLower(x)
			}
			b.where("lower(l.brand) = ANY(" + b.arg(lower) + ")")
		}
	}
	withSpecs := func(b *builder) {
		for k, vals := range p.Specs {
			key := b.arg(k)
			f := fieldOf(schema, k)
			switch f.Type {
			case catalogue.TypeMulti:
				b.where("(l.specs -> " + key + ") ?| " + b.arg(vals))
			default: // enum, boolean (stored true/false → 'true'/'false')
				b.where("(l.specs ->> " + key + ") = ANY(" + b.arg(vals) + ")")
			}
		}
		for k, r := range p.SpecRanges {
			key := b.arg(k)
			num := "CASE WHEN jsonb_typeof(l.specs -> " + key + ") = 'number' THEN (l.specs ->> " + key + ")::numeric END"
			if r[0] != nil {
				b.where(num + " >= " + b.arg(*r[0]))
			}
			if r[1] != nil {
				b.where(num + " <= " + b.arg(*r[1]))
			}
		}
	}

	// 1. The page of results.
	all := base.clone()
	withBrand(all)
	withSpecs(all)
	order := orderBy(p.Sort, qArg)
	sql := "WITH l AS (" + listing + ") SELECT l.id, l.slug, l.name, l.brand, l.cover, l.price_paise, l.mrp_paise, l.available, " +
		"l.rating_avg, l.rating_count, l.is_local, l.verified_tag, count(*) OVER () FROM l WHERE " + all.clause() +
		" ORDER BY " + order + " LIMIT " + all.arg(p.PageSize) + " OFFSET " + all.arg((p.Page-1)*p.PageSize)
	rows, err := s.Pool.Query(ctx, sql, all.args...)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var c Card
		var cover string
		var available int32
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Brand, &cover, &c.PricePaise, &c.MrpPaise, &available,
			&c.RatingAvg, &c.RatingCount, &c.IsLocal, &c.VerifiedSeller, &res.Total); err != nil {
			rows.Close()
			return res, err
		}
		if cover != "" {
			c.CoverURL = "/api/v1/media/" + cover
		}
		c.InStock = available > 0
		if c.MrpPaise > c.PricePaise {
			c.DiscountPct = int((c.MrpPaise - c.PricePaise) * 100 / c.MrpPaise)
		}
		res.Items = append(res.Items, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(res.Items) == 0 && p.Page > 1 { // past the end: still report the total
		cnt := base.clone()
		withBrand(cnt)
		withSpecs(cnt)
		if err := s.Pool.QueryRow(ctx, "WITH l AS ("+listing+") SELECT count(*) FROM l WHERE "+cnt.clause(), cnt.args...).Scan(&res.Total); err != nil {
			return res, err
		}
	}

	// 2. Brands: everything except the brand filter.
	nb := base.clone()
	withSpecs(nb)
	if err := s.counts(ctx, "WITH l AS ("+listing+") SELECT l.brand, '', count(*) FROM l WHERE "+nb.clause()+
		" AND l.brand <> '' GROUP BY l.brand ORDER BY count(*) DESC, l.brand LIMIT 30", nb.args, &res.Facets.Brands); err != nil {
		return res, err
	}

	// 3. Price range and categories: the base set.
	var lo, hi *int64
	if err := s.Pool.QueryRow(ctx, "WITH l AS ("+listing+") SELECT min(l.price_paise), max(l.price_paise) FROM l WHERE "+base.clause(), base.args...).Scan(&lo, &hi); err != nil {
		return res, err
	}
	if lo != nil && hi != nil {
		res.Facets.Price = &[2]int64{*lo, *hi}
	}
	cb := base.clone()
	withBrand(cb)
	withSpecs(cb)
	if err := s.counts(ctx, "WITH l AS ("+listing+") SELECT c.slug, c.name, count(*) FROM l JOIN categories c ON c.id = l.category_id WHERE "+
		cb.clause()+" GROUP BY c.slug, c.name ORDER BY count(*) DESC, c.name LIMIT 20", cb.args, &res.Facets.Categories); err != nil {
		return res, err
	}

	// 4. Spec facets (sub-categories only): everything except spec filters.
	if schema != nil {
		sb := base.clone()
		withBrand(sb)
		facets, err := s.specFacets(ctx, *schema, sb)
		if err != nil {
			return res, err
		}
		res.Facets.Specs = facets
	}
	return res, nil
}

func (s *Service) counts(ctx context.Context, sql string, args []any, out *[]Count) error {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Value, &c.Label, &c.Count); err != nil {
			return err
		}
		*out = append(*out, c)
	}
	return rows.Err()
}

// specFacets counts choice values and finds number ranges in one query.
func (s *Service) specFacets(ctx context.Context, schema catalogue.SpecSchema, b *builder) ([]SpecFacet, error) {
	var keys []string
	for _, f := range schema.Fields {
		if f.Filter {
			keys = append(keys, f.Key)
		}
	}
	if len(keys) == 0 {
		return []SpecFacet{}, nil
	}
	k := b.arg(keys)
	where := b.clause()
	sql := "WITH l AS (" + listing + "), m AS (SELECT l.specs FROM l WHERE " + where + "), kv AS (SELECT e.key, e.value FROM m, jsonb_each(m.specs) e WHERE e.key = ANY(" + k + "))" +
		" SELECT key, value #>> '{}', count(*), NULL::float8, NULL::float8 FROM kv WHERE jsonb_typeof(value) IN ('string', 'boolean') GROUP BY 1, 2" +
		" UNION ALL SELECT kv.key, x, count(*), NULL, NULL FROM kv, jsonb_array_elements_text(kv.value) x WHERE jsonb_typeof(kv.value) = 'array' GROUP BY 1, 2" +
		" UNION ALL SELECT key, NULL, count(*), min((value #>> '{}')::float8), max((value #>> '{}')::float8) FROM kv WHERE jsonb_typeof(value) = 'number' GROUP BY 1"
	rows, err := s.Pool.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]map[string]int64{}
	ranges := map[string][2]float64{}
	for rows.Next() {
		var key string
		var val *string
		var n int64
		var lo, hi *float64
		if err := rows.Scan(&key, &val, &n, &lo, &hi); err != nil {
			return nil, err
		}
		if lo != nil && hi != nil {
			ranges[key] = [2]float64{*lo, *hi}
		} else if val != nil {
			if counts[key] == nil {
				counts[key] = map[string]int64{}
			}
			counts[key][*val] = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []SpecFacet{}
	for _, f := range schema.Fields {
		if !f.Filter {
			continue
		}
		sf := SpecFacet{Key: f.Key, Label: f.Label, Type: f.Type, Unit: f.Unit}
		switch f.Type {
		case catalogue.TypeNumber:
			r, ok := ranges[f.Key]
			if !ok {
				continue
			}
			sf.Min, sf.Max = &r[0], &r[1]
		case catalogue.TypeBoolean:
			if n := counts[f.Key]["true"]; n > 0 {
				sf.Options = []Count{{Value: "true", Label: "Yes", Count: n}}
			}
		default:
			for _, o := range f.Options { // schema order, values that occur
				if n := counts[f.Key][o]; n > 0 {
					sf.Options = append(sf.Options, Count{Value: o, Count: n})
				}
			}
		}
		if sf.Options != nil || sf.Min != nil {
			out = append(out, sf)
		}
	}
	return out, nil
}

// category resolves a slug to the IDs to list (a top-level category lists
// its sub-categories) and, for a sub-category, its spec schema.
func (s *Service) category(ctx context.Context, slug string) (*CategoryInfo, []uuid.UUID, *catalogue.SpecSchema, error) {
	c, err := s.Q.GetCategoryBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, httpx.ErrNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}
	info := &CategoryInfo{Slug: c.Slug, Name: c.Name, Icon: c.Icon}
	if c.ParentID != nil {
		parent, err := s.Q.GetCategory(ctx, *c.ParentID)
		if err != nil {
			return nil, nil, nil, err
		}
		if parent.Status != "ACTIVE" {
			return nil, nil, nil, httpx.ErrNotFound
		}
		info.ParentSlug, info.ParentName, info.Icon = parent.Slug, parent.Name, parent.Icon
		var schema catalogue.SpecSchema
		_ = json.Unmarshal(c.SpecSchema, &schema)
		return info, []uuid.UUID{c.ID}, &schema, nil
	}
	all, err := s.Q.ListActiveCategories(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := []uuid.UUID{}
	for _, x := range all {
		if x.ParentID != nil && *x.ParentID == c.ID {
			ids = append(ids, x.ID)
		}
	}
	return info, ids, nil, nil
}

func fieldOf(schema *catalogue.SpecSchema, key string) catalogue.SpecField {
	if schema != nil {
		for _, f := range schema.Fields {
			if f.Key == key {
				return f
			}
		}
	}
	return catalogue.SpecField{}
}

func orderBy(sort, qArg string) string {
	switch sort {
	case "price_asc":
		return "l.price_paise, l.id"
	case "price_desc":
		return "l.price_paise DESC, l.id"
	case "newest":
		return "l.created_at DESC, l.id"
	case "rating":
		return "l.rating_avg DESC, l.rating_count DESC, l.id"
	case "popular":
		return "l.sold DESC, l.rating_count DESC, l.id"
	}
	if qArg != "" {
		return "ts_rank(l.search, websearch_to_tsquery('english', " + qArg + ")) + similarity(l.name, " + qArg + ") DESC, l.id"
	}
	return "l.sold DESC, l.created_at DESC, l.id" // relevance without text: popular, then new
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
