package search

import (
	"context"
)

// Suggestion is one row of the search box dropdown.
type Suggestion struct {
	Kind     string `json:"kind"` // product, brand, category
	Text     string `json:"text"`
	Slug     string `json:"slug,omitempty"`
	CoverURL string `json:"coverUrl,omitempty"`
	Brand    string `json:"brand,omitempty"`
}

// Suggest returns up to 5 products, 3 brands and 3 categories for typed text.
func (s *Service) Suggest(ctx context.Context, q string) ([]Suggestion, error) {
	out := []Suggestion{}
	like := "%" + escapeLike(q) + "%"
	prefix := escapeLike(q) + "%"
	rows, err := s.Pool.Query(ctx, "WITH l AS ("+listing+`) SELECT l.slug, l.name, l.brand, l.cover FROM l
		WHERE l.name ILIKE $1 OR l.name % $2 OR l.search @@ websearch_to_tsquery('english', $2)
		ORDER BY (l.name ILIKE $3) DESC, similarity(l.name, $2) DESC, l.sold DESC LIMIT 5`, like, q, prefix)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sg Suggestion
		var cover string
		if err := rows.Scan(&sg.Slug, &sg.Text, &sg.Brand, &cover); err != nil {
			rows.Close()
			return nil, err
		}
		sg.Kind = "product"
		if cover != "" {
			sg.CoverURL = "/api/v1/media/" + cover
		}
		out = append(out, sg)
	}
	rows.Close()
	rows, err = s.Pool.Query(ctx, "WITH l AS ("+listing+`) SELECT l.brand FROM l WHERE l.brand ILIKE $1 GROUP BY l.brand ORDER BY count(*) DESC LIMIT 3`, prefix)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Suggestion{Kind: "brand", Text: b})
	}
	rows.Close()
	rows, err = s.Pool.Query(ctx, `SELECT slug, name FROM categories WHERE status = 'ACTIVE' AND parent_id IS NOT NULL AND name ILIKE $1
		ORDER BY (name ILIKE $2) DESC, name LIMIT 3`, like, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sg Suggestion
		if err := rows.Scan(&sg.Slug, &sg.Text); err != nil {
			return nil, err
		}
		sg.Kind = "category"
		out = append(out, sg)
	}
	return out, rows.Err()
}
