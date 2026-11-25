-- Creators -------------------------------------------------------------------------
-- name: UpsertCreatorProfile :one
INSERT INTO creator_profiles (account_id, niche, bio) VALUES ($1, $2, $3)
ON CONFLICT (account_id) DO UPDATE SET niche = EXCLUDED.niche, bio = EXCLUDED.bio, updated_at = now() RETURNING *;

-- name: GetCreatorProfile :one
SELECT * FROM creator_profiles WHERE account_id = $1;

-- name: ClearSocials :exec
DELETE FROM creator_socials WHERE account_id = $1;

-- name: AddSocial :exec
INSERT INTO creator_socials (account_id, platform, url, followers) VALUES ($1, $2, $3, $4);

-- name: ListSocials :many
SELECT * FROM creator_socials WHERE account_id = $1 ORDER BY followers DESC;

-- name: SubmitCreator :exec
UPDATE creator_profiles SET terms_version = $2, submitted_at = now(), updated_at = now() WHERE account_id = $1;

-- name: SetAccountApproval :exec
UPDATE accounts SET approval = $2, approval_reason = $3 WHERE id = $1;

-- name: GetAccountApproval :one
SELECT approval, approval_reason, role FROM accounts WHERE id = $1;

-- name: ProgrammeQueue :many
SELECT a.id, a.name, a.phone_e164, a.approval, a.role, a.created_at FROM accounts a
WHERE a.role = @role AND a.approval::text = ANY(@states::text[]) ORDER BY a.created_at LIMIT 200;

-- name: SetCreatorKYC :exec
UPDATE creator_profiles SET pan_enc = $2, pan_last4 = $3, bank_enc = $4, bank_last4 = $5, kyc_verified_at = now(), updated_at = now() WHERE account_id = $1;

-- Demos ----------------------------------------------------------------------------
-- name: ProductSeller :one
SELECT seller_id FROM products WHERE id = $1 AND status = 'ACTIVE';

-- name: CreateDemo :one
INSERT INTO demo_requests (id, creator_id, product_id, seller_id, status, note) VALUES ($1, $2, $3, $4, 'REQUESTED', $5) RETURNING *;

-- name: GetDemoForUpdate :one
SELECT * FROM demo_requests WHERE id = $1 FOR UPDATE;

-- name: SetDemoStatus :one
UPDATE demo_requests SET status = @to_status, decline_reason = coalesce(sqlc.narg(reason), decline_reason),
  shipment_id = coalesce(sqlc.narg(shipment_id), shipment_id), due_at = coalesce(sqlc.narg(due_at), due_at), updated_at = now()
WHERE id = @id AND status = ANY(@from_status::text[]) RETURNING *;

-- name: ListDemos :many
SELECT d.*, p.name AS product_name, p.slug AS product_slug, c.name AS creator_name, sh.awb FROM demo_requests d
JOIN products p ON p.id = d.product_id JOIN accounts c ON c.id = d.creator_id
LEFT JOIN shipments sh ON sh.id = d.shipment_id
WHERE (sqlc.narg(creator_id)::uuid IS NULL OR d.creator_id = sqlc.narg(creator_id))
  AND (sqlc.narg(seller_id)::uuid IS NULL OR d.seller_id = sqlc.narg(seller_id))
ORDER BY d.created_at DESC LIMIT 200;

-- name: DemoShipTo :one
SELECT ad.name, ad.phone AS phone_e164, ad.line1, ad.line2, ad.city, ad.state, ad.pincode FROM accounts a
JOIN addresses ad ON ad.account_id = a.id AND ad.is_default WHERE a.id = $1;

-- name: CreateDemoShipment :one
INSERT INTO shipments (id, demo_request_id, kind, ref, provider, awb, status, eta) VALUES ($1, $2, 'DEMO', $3, $4, $5, $6, $7) RETURNING *;

-- name: OverdueDemos :exec
UPDATE demo_requests SET status = 'OVERDUE', updated_at = now() WHERE status = 'DELIVERED' AND due_at < now();

-- Creator reviews --------------------------------------------------------------------
-- name: CreateCreatorReview :one
INSERT INTO creator_reviews (id, creator_id, product_id, rating, body, media, rank)
VALUES ($1, $2, $3, $4, $5, $6, (SELECT count(*) + 1 FROM creator_reviews WHERE product_id = $3)) RETURNING *;

-- name: UpdateCreatorReview :one
UPDATE creator_reviews SET rating = $3, body = $4, media = $5, updated_at = now() WHERE id = $1 AND creator_id = $2 RETURNING *;

-- name: LockProductForReview :exec
SELECT id FROM products WHERE id = $1 FOR UPDATE;

-- name: ListCreatorReviews :many
SELECT r.*, a.name AS creator_name,
  coalesce((SELECT sum(value) FILTER (WHERE value = 1) FROM creator_votes v WHERE v.creator_review_id = r.id), 0)::int AS agrees,
  coalesce((SELECT count(*) FILTER (WHERE value = -1) FROM creator_votes v WHERE v.creator_review_id = r.id), 0)::int AS disagrees,
  coalesce((SELECT value FROM creator_votes v WHERE v.creator_review_id = r.id AND v.buyer_id = sqlc.narg(viewer)::uuid), 0)::int AS my_vote
FROM creator_reviews r JOIN accounts a ON a.id = r.creator_id
WHERE r.product_id = @product_id AND r.status = 'PUBLISHED' ORDER BY r.rank;

-- name: MyCreatorReviews :many
SELECT r.*, p.name AS product_name, p.slug AS product_slug FROM creator_reviews r JOIN products p ON p.id = r.product_id
WHERE r.creator_id = $1 ORDER BY r.published_at DESC;

-- name: VoteCreatorReview :exec
INSERT INTO creator_votes (creator_review_id, buyer_id, value) VALUES ($1, $2, $3)
ON CONFLICT (creator_review_id, buyer_id) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: UnvoteCreatorReview :exec
DELETE FROM creator_votes WHERE creator_review_id = $1 AND buyer_id = $2;

-- name: GetCreatorReview :one
SELECT * FROM creator_reviews WHERE id = $1;

-- Reward pools ---------------------------------------------------------------------
-- name: UpsertPool :one
INSERT INTO reward_pools (id, product_id, period, size) VALUES ($1, $2, $3, $4)
ON CONFLICT (product_id, period) DO UPDATE SET size = EXCLUDED.size WHERE reward_pools.settled_at IS NULL RETURNING *;

-- name: PoolsToSettle :many
SELECT * FROM reward_pools WHERE period = $1 AND settled_at IS NULL AND size > 0 FOR UPDATE SKIP LOCKED;

-- name: VerifiedAgrees :many
-- Agree votes cast in the period by buyers who still own a delivered purchase of the product (LLD 8.9).
SELECT r.creator_id, r.rank, count(v.buyer_id)::int AS agrees FROM creator_reviews r
JOIN creator_votes v ON v.creator_review_id = r.id AND v.value = 1 AND v.updated_at >= @period_start AND v.updated_at < @period_end
WHERE r.product_id = @product_id AND r.status = 'PUBLISHED'
  AND EXISTS (SELECT 1 FROM order_items i JOIN sub_orders s ON s.id = i.sub_order_id JOIN orders o ON o.id = s.order_id
    JOIN variants vv ON vv.id = i.variant_id
    WHERE o.buyer_id = v.buyer_id AND vv.product_id = r.product_id AND s.status = 'DELIVERED' AND i.status IN ('ACTIVE', 'REPLACED'))
GROUP BY r.creator_id, r.rank ORDER BY r.rank;

-- name: AddSettlement :exec
INSERT INTO pool_settlements (pool_id, creator_id, verified_agrees, weight, score, share, lot_id) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: MarkPoolSettled :exec
UPDATE reward_pools SET settled_at = now() WHERE id = $1;

-- name: CreatorEarnings :many
SELECT p.period, pr.name AS product_name, s.verified_agrees, s.share FROM pool_settlements s
JOIN reward_pools p ON p.id = s.pool_id JOIN products pr ON pr.id = p.product_id
WHERE s.creator_id = $1 ORDER BY p.period DESC LIMIT 100;

-- name: ListPools :many
SELECT p.*, pr.name AS product_name FROM reward_pools p JOIN products pr ON pr.id = p.product_id ORDER BY p.period DESC, pr.name LIMIT 200;

-- Cash-out ---------------------------------------------------------------------------
-- name: CreateCashout :one
INSERT INTO cashout_requests (id, creator_id, electrons, status) VALUES ($1, $2, $3, 'REQUESTED') RETURNING *;

-- name: ListCashouts :many
SELECT * FROM cashout_requests WHERE creator_id = $1 ORDER BY created_at DESC LIMIT 50;

-- name: CashoutsToBatch :many
SELECT c.*, cp.bank_enc FROM cashout_requests c JOIN creator_profiles cp ON cp.account_id = c.creator_id
WHERE c.status = 'REQUESTED' ORDER BY c.created_at FOR UPDATE OF c SKIP LOCKED;

-- name: SetCashoutBatched :exec
UPDATE cashout_requests SET status = 'IN_BATCH', batch_id = $2, provider_ref = $3, updated_at = now() WHERE id = $1;

-- name: GetCashoutByRefForUpdate :one
SELECT * FROM cashout_requests WHERE provider_ref = $1 FOR UPDATE;

-- name: FinishCashout :exec
UPDATE cashout_requests SET status = $2, bank_ref = $3, failure_reason = $4, updated_at = now() WHERE id = $1 AND status = 'IN_BATCH';

-- name: CancelCashout :one
UPDATE cashout_requests SET status = 'CANCELLED', updated_at = now() WHERE id = $1 AND creator_id = $2 AND status = 'REQUESTED' RETURNING *;

-- Promoters --------------------------------------------------------------------------
-- name: UpsertPromoterProfile :one
INSERT INTO promoter_profiles (account_id, type, display_name, company) VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id) DO UPDATE SET type = EXCLUDED.type, display_name = EXCLUDED.display_name, company = EXCLUDED.company, updated_at = now()
RETURNING *;

-- name: GetPromoterProfile :one
SELECT * FROM promoter_profiles WHERE account_id = $1;

-- name: AddInvestment :one
INSERT INTO promoter_investments (id, promoter_id, seller_id, proof_note) VALUES ($1, $2, $3, $4)
ON CONFLICT (promoter_id, seller_id) DO UPDATE SET proof_note = EXCLUDED.proof_note, declined_at = NULL RETURNING *;

-- name: ListInvestments :many
SELECT i.*, sp.store_name, pp.display_name AS promoter_name FROM promoter_investments i
JOIN seller_profiles sp ON sp.account_id = i.seller_id JOIN promoter_profiles pp ON pp.account_id = i.promoter_id
WHERE (sqlc.narg(promoter_id)::uuid IS NULL OR i.promoter_id = sqlc.narg(promoter_id))
  AND (sqlc.narg(seller_id)::uuid IS NULL OR i.seller_id = sqlc.narg(seller_id))
ORDER BY i.created_at DESC;

-- name: DecideInvestment :execrows
UPDATE promoter_investments SET seller_confirmed_at = CASE WHEN @confirm::bool THEN now() END,
  declined_at = CASE WHEN @confirm::bool THEN NULL ELSE now() END
WHERE id = @id AND seller_id = @seller_id;

-- name: CanPromote :one
SELECT EXISTS (SELECT 1 FROM products p JOIN promoter_investments i ON i.seller_id = p.seller_id
  WHERE p.id = $1 AND i.promoter_id = $2 AND i.seller_confirmed_at IS NOT NULL AND p.status = 'ACTIVE');

-- name: Promote :exec
INSERT INTO promotions (promoter_id, product_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: Unpromote :exec
DELETE FROM promotions WHERE promoter_id = $1 AND product_id = $2;

-- name: ProductPromoters :many
SELECT pp.account_id, pp.display_name, pp.type FROM promotions pr JOIN promoter_profiles pp ON pp.account_id = pr.promoter_id
WHERE pr.product_id = $1 ORDER BY pr.created_at;

-- name: PromotableProducts :many
SELECT p.id, p.name, p.slug, sp.store_name, EXISTS (SELECT 1 FROM promotions x WHERE x.product_id = p.id AND x.promoter_id = @promoter_id) AS promoted,
  coalesce((SELECT value FROM promoter_reward_rules r WHERE r.product_id = p.id), 0)::int AS reward_per_unit
FROM products p JOIN promoter_investments i ON i.seller_id = p.seller_id AND i.promoter_id = @promoter_id AND i.seller_confirmed_at IS NOT NULL
JOIN seller_profiles sp ON sp.account_id = p.seller_id
WHERE p.status = 'ACTIVE' ORDER BY p.name;

-- name: SetRewardRule :execrows
INSERT INTO promoter_reward_rules (product_id, type, value)
SELECT @product_id, 'ELECTRONS_PER_UNIT', @value FROM products WHERE id = @product_id AND seller_id = @seller_id
ON CONFLICT (product_id) DO UPDATE SET value = EXCLUDED.value;

-- name: PromoterRewardsForItems :many
-- One row per (delivered item, promoter) that earns: promoters of the product under its reward rule.
SELECT i.id AS item_id, i.qty, pr.promoter_id, r.value FROM order_items i
JOIN variants v ON v.id = i.variant_id JOIN promoter_reward_rules r ON r.product_id = v.product_id AND r.value > 0
JOIN promotions pr ON pr.product_id = v.product_id
WHERE i.id = ANY(@item_ids::uuid[]) AND i.status = 'ACTIVE';
