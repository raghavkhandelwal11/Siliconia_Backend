-- Reviews ------------------------------------------------------------------------
-- name: ReviewableItem :one
SELECT i.id, i.status, s.status AS sub_status, o.buyer_id, v.product_id FROM order_items i
JOIN sub_orders s ON s.id = i.sub_order_id JOIN orders o ON o.id = s.order_id JOIN variants v ON v.id = i.variant_id
WHERE i.id = $1;

-- name: CreateReview :one
INSERT INTO reviews (id, order_item_id, product_id, account_id, rating, title, body) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: AddReviewPhoto :exec
INSERT INTO review_photos (review_id, upload_id, position)
SELECT @review_id, u.id, @position FROM uploads u WHERE u.id = @upload_id AND u.owner_id = @owner_id AND u.purpose = 'REVIEW_IMAGE';

-- name: RefreshProductRating :exec
UPDATE products p SET rating_count = r.n, rating_avg = coalesce(r.avg, 0)
FROM (SELECT count(*)::int AS n, round(avg(rating), 2) AS avg FROM reviews WHERE product_id = $1 AND status = 'PUBLISHED') r
WHERE p.id = $1;

-- name: ListProductReviews :many
SELECT r.id, r.rating, r.title, r.body, r.helpful_count, r.created_at, a.name AS author,
  coalesce((SELECT array_agg(u.storage_key ORDER BY ph.position) FROM review_photos ph JOIN uploads u ON u.id = ph.upload_id WHERE ph.review_id = r.id), '{}')::text[] AS photos
FROM reviews r JOIN accounts a ON a.id = r.account_id
WHERE r.product_id = @product_id AND r.status = 'PUBLISHED'
ORDER BY CASE WHEN @sort::text = 'helpful' THEN r.helpful_count END DESC NULLS LAST, r.created_at DESC
LIMIT 10 OFFSET @off;

-- name: ReviewSummary :one
SELECT count(*)::int AS n, coalesce(round(avg(rating), 2), 0)::float8 AS avg,
  count(*) FILTER (WHERE rating = 5)::int AS s5, count(*) FILTER (WHERE rating = 4)::int AS s4, count(*) FILTER (WHERE rating = 3)::int AS s3,
  count(*) FILTER (WHERE rating = 2)::int AS s2, count(*) FILTER (WHERE rating = 1)::int AS s1
FROM reviews WHERE product_id = $1 AND status = 'PUBLISHED';

-- name: AddHelpful :execrows
INSERT INTO helpful_votes (review_id, account_id) SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM reviews WHERE id = $1 AND account_id <> $2) ON CONFLICT DO NOTHING;

-- name: BumpHelpful :exec
UPDATE reviews SET helpful_count = helpful_count + 1 WHERE id = $1;

-- name: AddReport :execrows
INSERT INTO review_reports (review_id, account_id, reason) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: BumpReport :one
-- Three reports hide a review until an admin looks at it.
UPDATE reviews SET report_count = report_count + 1, status = CASE WHEN report_count + 1 >= 3 THEN 'HIDDEN' ELSE status END
WHERE id = $1 RETURNING product_id, status;

-- name: ReportedReviews :many
SELECT r.*, p.name AS product_name FROM reviews r JOIN products p ON p.id = r.product_id
WHERE r.report_count > 0 ORDER BY r.status DESC, r.report_count DESC LIMIT 100;

-- name: SetReviewStatus :one
UPDATE reviews SET status = $2, report_count = CASE WHEN $2 = 'PUBLISHED' THEN 0 ELSE report_count END, updated_at = now() WHERE id = $1 RETURNING product_id;

-- name: MyReviewItems :many
SELECT order_item_id FROM reviews WHERE account_id = $1;

-- Feedback -------------------------------------------------------------------------
-- name: CreateFeedback :one
INSERT INTO product_feedback (id, product_id, buyer_id, category, body) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: SellerFeedback :many
SELECT f.*, p.name AS product_name FROM product_feedback f JOIN products p ON p.id = f.product_id
WHERE p.seller_id = $1 ORDER BY f.replied_at IS NOT NULL, f.created_at DESC LIMIT 200;

-- name: ReplyFeedback :execrows
UPDATE product_feedback f SET seller_reply = $3, replied_at = now()
FROM products p WHERE f.id = $1 AND p.id = f.product_id AND p.seller_id = $2;

-- name: HasDeliveredPurchase :one
SELECT EXISTS (SELECT 1 FROM order_items i JOIN sub_orders s ON s.id = i.sub_order_id JOIN orders o ON o.id = s.order_id
  JOIN variants v ON v.id = i.variant_id
  WHERE o.buyer_id = $1 AND v.product_id = $2 AND s.status = 'DELIVERED' AND i.status IN ('ACTIVE', 'REPLACED'));

-- Discovery ------------------------------------------------------------------------
-- name: ComputeProductsOfWeek :exec
-- Units sold in the last 7 days plus a rating bonus; top 10 (weekly job).
INSERT INTO products_of_week (week_start, product_id, rank, score)
SELECT @week_start::date, product_id, row_number() OVER (ORDER BY score DESC, product_id)::int, score FROM (
  SELECT v.product_id, (sum(i.qty) + max(p.rating_avg) * least(max(p.rating_count), 20) / 10.0)::numeric AS score
  FROM order_items i JOIN sub_orders s ON s.id = i.sub_order_id JOIN orders o ON o.id = s.order_id
  JOIN variants v ON v.id = i.variant_id JOIN products p ON p.id = v.product_id
  WHERE o.status = 'CONFIRMED' AND o.placed_at > now() - interval '7 days' AND p.status = 'ACTIVE' AND NOT o.is_synthetic
  GROUP BY v.product_id ORDER BY score DESC LIMIT 10) t
ON CONFLICT (week_start, product_id) DO UPDATE SET rank = EXCLUDED.rank, score = EXCLUDED.score;

-- name: ProductsOfWeek :many
SELECT w.rank, p.id, p.slug, p.name, p.brand, p.rating_avg::float8 AS rating_avg, p.rating_count,
  (SELECT min(price_paise) FROM variants WHERE product_id = p.id)::bigint AS price_paise,
  coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover
FROM products_of_week w JOIN products p ON p.id = w.product_id
WHERE w.week_start = (SELECT max(week_start) FROM products_of_week) AND p.status = 'ACTIVE' ORDER BY w.rank;

-- name: BoughtTogether :many
-- Products bought in the same orders as this one (recommendations).
SELECT p.id, p.slug, p.name, p.brand, p.rating_avg::float8 AS rating_avg, p.rating_count, count(*)::int AS n,
  (SELECT min(price_paise) FROM variants WHERE product_id = p.id)::bigint AS price_paise,
  coalesce((SELECT u.storage_key FROM product_images im JOIN uploads u ON u.id = im.upload_id WHERE im.product_id = p.id ORDER BY im.position LIMIT 1), '')::text AS cover
FROM order_items a JOIN sub_orders sa ON sa.id = a.sub_order_id JOIN variants va ON va.id = a.variant_id
JOIN sub_orders sb ON sb.order_id = sa.order_id JOIN order_items b ON b.sub_order_id = sb.id JOIN variants vb ON vb.id = b.variant_id
JOIN products p ON p.id = vb.product_id
WHERE va.product_id = $1 AND vb.product_id <> $1 AND p.status = 'ACTIVE'
GROUP BY p.id ORDER BY n DESC LIMIT 8;

-- name: ForYou :many
-- Popular products in the categories the buyer bought from, excluding what they bought.
SELECT p.id, p.slug, p.name, p.brand, p.rating_avg::float8 AS rating_avg, p.rating_count,
  (SELECT min(price_paise) FROM variants WHERE product_id = p.id)::bigint AS price_paise,
  coalesce((SELECT u.storage_key FROM product_images im JOIN uploads u ON u.id = im.upload_id WHERE im.product_id = p.id ORDER BY im.position LIMIT 1), '')::text AS cover
FROM products p
WHERE p.status = 'ACTIVE' AND p.category_id IN (
  SELECT DISTINCT pp.category_id FROM orders o JOIN sub_orders s ON s.order_id = o.id JOIN order_items i ON i.sub_order_id = s.id
  JOIN variants v ON v.id = i.variant_id JOIN products pp ON pp.id = v.product_id WHERE o.buyer_id = $1)
  AND p.id NOT IN (SELECT v.product_id FROM orders o JOIN sub_orders s ON s.order_id = o.id JOIN order_items i ON i.sub_order_id = s.id
  JOIN variants v ON v.id = i.variant_id WHERE o.buyer_id = $1)
ORDER BY p.rating_count DESC, p.created_at DESC LIMIT 8;
