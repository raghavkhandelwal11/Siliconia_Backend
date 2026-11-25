-- name: CreateLot :one
INSERT INTO electron_lots (id, account_id, source, source_ref, amount, remaining, status, available_at, expires_at, is_synthetic)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: AddLedger :one
INSERT INTO ledger_entries (id, account_id, type, amount, lot_id, ref_type, ref_id, note)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: AddAllocation :exec
INSERT INTO spend_allocations (ledger_entry_id, lot_id, amount) VALUES ($1, $2, $3);

-- name: LockSpendableLots :many
-- FIFO by expiry (LLD 8.7).
SELECT * FROM electron_lots
WHERE account_id = $1 AND status = 'AVAILABLE' AND remaining > 0 AND (expires_at IS NULL OR expires_at > now())
ORDER BY expires_at, id FOR UPDATE;

-- name: TakeFromLot :exec
UPDATE electron_lots SET remaining = remaining - @amount::int,
  status = CASE WHEN remaining - @amount::int = 0 THEN 'EXHAUSTED'::lot_status ELSE status END
WHERE id = @id AND remaining >= @amount::int;

-- name: SpendAllocations :many
-- What a spend took from each lot, less anything already restored for it.
SELECT a.lot_id, a.amount FROM spend_allocations a
JOIN ledger_entries e ON e.id = a.ledger_entry_id
WHERE e.ref_type = @ref_type AND e.ref_id = @ref_id AND e.type IN ('SPEND_ORDER', 'SPEND_MEMBERSHIP')
ORDER BY a.lot_id;

-- name: RestoredAmount :one
SELECT coalesce(sum(amount), 0)::int FROM ledger_entries WHERE type = 'RESTORE' AND ref_type = @ref_type AND ref_id = @ref_id;

-- name: PutBackToLot :exec
UPDATE electron_lots SET remaining = remaining + @amount::int,
  status = CASE WHEN status = 'EXHAUSTED' THEN 'AVAILABLE'::lot_status ELSE status END
WHERE id = @id;

-- name: Balance :one
SELECT coalesce(sum(remaining) FILTER (WHERE status = 'AVAILABLE' AND (expires_at IS NULL OR expires_at > now())), 0)::int AS available,
       coalesce(sum(amount) FILTER (WHERE status = 'PENDING'), 0)::int AS pending
FROM electron_lots WHERE account_id = $1;

-- name: NextExpiry :one
SELECT expires_at, sum(remaining)::int AS amount FROM electron_lots
WHERE account_id = $1 AND status = 'AVAILABLE' AND remaining > 0 AND expires_at > now()
GROUP BY expires_at ORDER BY expires_at LIMIT 1;

-- name: ListLedger :many
SELECT * FROM ledger_entries WHERE account_id = @account_id
  AND (sqlc.narg(before)::timestamptz IS NULL OR created_at < sqlc.narg(before)::timestamptz)
ORDER BY created_at DESC, id DESC LIMIT 50;

-- name: LotsForItem :many
SELECT * FROM electron_lots WHERE source = 'PURCHASE' AND source_ref = $1 FOR UPDATE;

-- name: SetLotAvailableAt :exec
UPDATE electron_lots SET available_at = @available_at::timestamptz WHERE source = 'PURCHASE' AND source_ref = ANY(@items::uuid[]) AND status = 'PENDING';

-- name: ReverseLot :exec
UPDATE electron_lots SET status = 'REVERSED', remaining = 0 WHERE id = $1 AND status = 'PENDING';

-- name: DueToMature :many
-- Lots whose return window has closed, for items still kept.
SELECT l.* FROM electron_lots l
JOIN order_items i ON i.id = l.source_ref
WHERE l.status = 'PENDING' AND l.source = 'PURCHASE' AND l.available_at <= now() AND i.status IN ('ACTIVE', 'REPLACED')
  AND NOT EXISTS (SELECT 1 FROM return_requests r WHERE r.order_item_id = i.id AND r.status NOT IN ('REJECTED', 'REPLACEMENT_SHIPPED'))
ORDER BY l.available_at LIMIT 500
FOR UPDATE OF l SKIP LOCKED;

-- name: MatureLot :exec
UPDATE electron_lots SET status = 'AVAILABLE', expires_at = @expires_at WHERE id = @id AND status = 'PENDING';

-- name: DueToExpire :many
SELECT * FROM electron_lots WHERE status = 'AVAILABLE' AND expires_at <= now() ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED;

-- name: ExpireLot :exec
UPDATE electron_lots SET status = 'EXPIRED', remaining = 0 WHERE id = $1;

-- name: SetItemElectrons :exec
UPDATE order_items SET electrons_spent = $2, electrons_earned = $3 WHERE id = $1;

-- name: SetItemEarned :exec
UPDATE order_items SET electrons_earned = $2 WHERE id = $1;

-- name: AvailableInvariant :one
-- sum of balance-moving ledger amounts; must equal the available balance.
SELECT coalesce(sum(amount), 0)::int FROM ledger_entries
WHERE account_id = $1 AND type NOT IN ('EARN_PURCHASE', 'EARN_CREATOR_POOL', 'EARN_PROMOTER', 'REVERSAL');

-- name: RestoredByLot :many
SELECT lot_id, sum(amount)::int AS amount FROM ledger_entries
WHERE type = 'RESTORE' AND ref_type = @ref_type AND ref_id = @ref_id AND lot_id IS NOT NULL GROUP BY lot_id;

-- name: SetOrderExtras :exec
UPDATE orders SET electrons_used = $2, is_member_at_order = $3, contains_launch_deal = $4 WHERE id = $1;

-- name: SetItemLaunch :exec
UPDATE order_items SET price_type = $2, launch_id = $3, launch_claim_id = $4 WHERE id = $1;
