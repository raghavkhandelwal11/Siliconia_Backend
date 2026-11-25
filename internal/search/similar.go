package search

import (
	"context"

	"github.com/google/uuid"
)

// Similar returns live products from the same sub-category, closest in price
// first (FR-PDP-08, FR-DIS-04). Recommendations from co-purchases come later.
func (s *Service) Similar(ctx context.Context, productID uuid.UUID, limit int) ([]Card, error) {
	sql := "WITH l AS (" + listing + `), me AS (SELECT category_id, price_paise FROM l WHERE id = $1)
		SELECT l.id, l.slug, l.name, l.brand, l.cover, l.price_paise, l.mrp_paise, l.available,
		       l.rating_avg, l.rating_count, l.is_local, l.verified_tag, 0::bigint
		FROM l, me
		WHERE l.category_id = me.category_id AND l.id <> $1
		ORDER BY abs(l.price_paise - me.price_paise), l.sold DESC, l.id
		LIMIT $2`
	rows, err := s.Pool.Query(ctx, sql, productID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Card{}
	for rows.Next() {
		var c Card
		var cover string
		var available int32
		var ignore int64
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Brand, &cover, &c.PricePaise, &c.MrpPaise, &available,
			&c.RatingAvg, &c.RatingCount, &c.IsLocal, &c.VerifiedSeller, &ignore); err != nil {
			return nil, err
		}
		if cover != "" {
			c.CoverURL = "/api/v1/media/" + cover
		}
		c.InStock = available > 0
		if c.MrpPaise > c.PricePaise {
			c.DiscountPct = int((c.MrpPaise - c.PricePaise) * 100 / c.MrpPaise)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
