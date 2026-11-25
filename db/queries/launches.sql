-- name: CreateLaunch :one
INSERT INTO launches (id, product_id, variant_id, seller_id, units_bought, unit_cost_paise, normal_price_paise, deal_discount_bps,
  deal_price_paise, min_profit_bps, deal_units, deal_remaining, per_member_limit, starts_at, normal_sale_mode, normal_sale_at,
  waiting_room_rps, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: GetLaunch :one
SELECT * FROM launches WHERE id = $1;

-- name: GetLaunchForUpdate :one
SELECT * FROM launches WHERE id = $1 FOR UPDATE;

-- name: LaunchView :one
SELECT l.*, p.name AS product_name, p.slug AS product_slug, v.attrs AS variant_attrs, v.mrp_paise,
  (SELECT u.storage_key FROM product_images pi JOIN uploads u ON u.id = pi.upload_id WHERE pi.product_id = p.id ORDER BY pi.position LIMIT 1) AS cover_key,
  coalesce((SELECT sum(qty) FROM launch_claims c WHERE c.launch_id = l.id AND c.status IN ('HELD', 'ORDERED')), 0)::int AS held,
  coalesce((SELECT quantity - reserved - sold FROM inventory_lots WHERE id = l.normal_lot_id), 0)::int AS normal_available
FROM launches l JOIN products p ON p.id = l.product_id JOIN variants v ON v.id = l.variant_id
WHERE l.id = $1;

-- name: ListLaunches :many
SELECT l.id, l.status, l.starts_at, l.deal_price_paise, l.normal_price_paise, l.deal_units, l.deal_remaining, p.name AS product_name, p.slug AS product_slug,
  (SELECT u.storage_key FROM product_images pi JOIN uploads u ON u.id = pi.upload_id WHERE pi.product_id = p.id ORDER BY pi.position LIMIT 1) AS cover_key
FROM launches l JOIN products p ON p.id = l.product_id
WHERE (sqlc.narg(status)::launch_status IS NULL OR l.status = sqlc.narg(status)::launch_status)
  AND (@include_drafts::bool OR l.status NOT IN ('DRAFT', 'CANCELLED'))
ORDER BY l.starts_at DESC LIMIT 100;

-- name: ActiveLaunchForProduct :one
SELECT id, status, deal_price_paise, deal_remaining, starts_at FROM launches
WHERE product_id = $1 AND status IN ('SCHEDULED', 'LIVE', 'DEAL_SOLD_OUT') ORDER BY starts_at LIMIT 1;

-- name: SetLaunchLots :exec
UPDATE launches SET deal_lot_id = $2, normal_lot_id = $3, status = 'SCHEDULED', version = version + 1 WHERE id = $1 AND status = 'DRAFT';

-- name: CreateLaunchLot :one
INSERT INTO inventory_lots (id, variant_id, owner_seller_id, kind, launch_id, quantity, unit_cost_paise)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: SetLaunchStatus :one
UPDATE launches SET status = @to_status, version = version + 1 WHERE id = @id AND status::text = ANY(@from_status::text[]) RETURNING *;

-- name: TakeDealUnits :one
-- The source of truth for deal units (LLD 8.4): no row means sold out.
UPDATE launches SET deal_remaining = deal_remaining - @qty::int
WHERE id = @id AND status = 'LIVE' AND deal_remaining >= @qty::int RETURNING deal_remaining;

-- name: ReturnDealUnits :exec
UPDATE launches SET deal_remaining = least(deal_units, deal_remaining + @qty::int),
  status = CASE WHEN status = 'DEAL_SOLD_OUT' THEN 'LIVE'::launch_status ELSE status END
WHERE id = @id;

-- name: EnsureMemberCounter :exec
INSERT INTO launch_member_counters (launch_id, account_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: AddMemberClaim :execrows
UPDATE launch_member_counters SET claimed = claimed + @qty::int
WHERE launch_id = @launch_id AND account_id = @account_id AND claimed + @qty::int <= @lim::int;

-- name: SubMemberClaim :exec
UPDATE launch_member_counters SET claimed = greatest(0, claimed - @qty::int) WHERE launch_id = @launch_id AND account_id = @account_id;

-- name: CreateClaim :one
INSERT INTO launch_claims (id, launch_id, account_id, qty, status, held_until) VALUES ($1, $2, $3, $4, 'HELD', $5) RETURNING *;

-- name: GetClaimForUpdate :one
SELECT * FROM launch_claims WHERE id = $1 FOR UPDATE;

-- name: SetClaimOrdered :exec
UPDATE launch_claims SET status = 'ORDERED', order_id = $2, held_until = $3 WHERE id = $1 AND status = 'HELD';

-- name: SetClaimsPaid :exec
UPDATE launch_claims SET status = 'PAID' WHERE order_id = $1 AND status = 'ORDERED';

-- name: ReleaseClaim :one
UPDATE launch_claims SET status = 'RELEASED' WHERE id = $1 AND status IN ('HELD', 'ORDERED') RETURNING *;

-- name: ClaimsForOrder :many
SELECT * FROM launch_claims WHERE order_id = $1 FOR UPDATE;

-- name: StaleClaims :many
SELECT * FROM launch_claims WHERE status = 'HELD' AND held_until < now() FOR UPDATE SKIP LOCKED LIMIT 500;

-- name: MyClaims :many
SELECT * FROM launch_claims WHERE launch_id = $1 AND account_id = $2 AND status IN ('HELD', 'ORDERED') ORDER BY created_at;

-- name: LaunchesToTick :many
SELECT l.*, coalesce((SELECT sum(qty) FROM launch_claims c WHERE c.launch_id = l.id AND c.status IN ('HELD', 'ORDERED')), 0)::int AS held,
  coalesce((SELECT quantity - reserved - sold FROM inventory_lots WHERE id = l.normal_lot_id), 0)::int AS normal_available
FROM launches l WHERE l.status IN ('SCHEDULED', 'LIVE', 'DEAL_SOLD_OUT', 'NORMAL_ON_SALE') FOR UPDATE OF l SKIP LOCKED;

-- name: PlatformSeller :one
SELECT account_id FROM seller_profiles WHERE account_id = $1 AND is_platform;

-- name: LaunchLine :one
-- What an order line snapshots for a launch unit (deal or launch-normal).
SELECT la.id AS launch_id, la.status, la.deal_price_paise, la.normal_price_paise, la.deal_lot_id, la.normal_lot_id,
       v.id AS variant_id, v.mrp_paise, v.attrs, p.id AS product_id, p.name, p.slug, la.seller_id, sp.store_name,
       c.gst_rate_bps, c.hsn, c.commission_bps,
       coalesce((SELECT quantity - reserved - sold FROM inventory_lots WHERE id = la.normal_lot_id), 0)::int AS normal_available,
       coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
                 WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover
FROM launches la
JOIN variants v ON v.id = la.variant_id
JOIN products p ON p.id = la.product_id
JOIN categories c ON c.id = p.category_id
JOIN seller_profiles sp ON sp.account_id = la.seller_id
WHERE la.id = $1;

-- name: VariantOfProduct :one
SELECT EXISTS (SELECT 1 FROM variants WHERE id = $1 AND product_id = $2);
