-- name: SetActor :exec
-- Names who changes sub-orders in this transaction (read by the history trigger).
SELECT set_config('siliconics.actor', @actor::text, true), set_config('siliconics.note', @note::text, true);

-- name: TransitionSubOrder :one
-- Moves a sub-order only from an allowed status (LLD 10); no row means the
-- move is not allowed from where it is now.
UPDATE sub_orders SET status = @to_status, version = version + 1
WHERE id = @id AND status::text = ANY(@from_status::text[])
RETURNING *;

-- name: CancelSubOrder :one
UPDATE sub_orders SET status = 'CANCELLED', cancel_reason = @reason, cancelled_by = @cancelled_by, version = version + 1
WHERE id = @id AND status IN ('CONFIRMED', 'PACKED')
RETURNING *;

-- name: GetSubOrderForUpdate :one
SELECT * FROM sub_orders WHERE id = $1 FOR UPDATE;

-- name: SetOrderItemsStatus :exec
UPDATE order_items SET status = $2 WHERE sub_order_id = $1 AND status = 'ACTIVE';

-- name: ReturnSoldStock :exec
UPDATE inventory_lots SET sold = sold - @qty::int WHERE id = @id AND sold >= @qty::int;

-- name: ListSubOrderItems :many
SELECT * FROM order_items WHERE sub_order_id = $1 ORDER BY id;

-- name: ListSubOrderEvents :many
SELECT e.sub_order_id, e.status, e.actor, e.note, e.at FROM sub_order_events e
JOIN sub_orders s ON s.id = e.sub_order_id WHERE s.order_id = $1 ORDER BY e.at, e.id;

-- name: SellerSubOrders :many
SELECT s.id, s.status, s.subtotal_paise, s.version, s.invoice_no, s.delivered_at, s.cancel_reason,
       o.id AS order_id, o.number, o.placed_at, o.pay_method, o.address,
       (SELECT count(*)::int FROM order_items i WHERE i.sub_order_id = s.id) AS item_count,
       sh.awb, sh.status AS shipment_status
FROM sub_orders s
JOIN orders o ON o.id = s.order_id
LEFT JOIN shipments sh ON sh.sub_order_id = s.id AND sh.kind = 'FORWARD' AND sh.status <> 'CANCELLED'
WHERE s.seller_id = @seller_id AND s.status <> 'PENDING_PAYMENT' AND s.status <> 'EXPIRED'
  AND (sqlc.narg(status)::sub_status IS NULL OR s.status = sqlc.narg(status)::sub_status)
ORDER BY o.placed_at DESC
LIMIT 200;

-- name: SellerStatusCounts :many
SELECT status, count(*)::int AS n FROM sub_orders
WHERE seller_id = $1 AND status NOT IN ('PENDING_PAYMENT', 'EXPIRED') GROUP BY status;

-- name: GetSubOrderDetail :one
SELECT s.*, o.number, o.placed_at, o.pay_method, o.address, o.buyer_id, o.payment_id, o.is_synthetic,
       sp.store_name, sp.legal_name, sp.gstin, sp.pickup_address
FROM sub_orders s
JOIN orders o ON o.id = s.order_id
JOIN seller_profiles sp ON sp.account_id = s.seller_id
WHERE s.id = $1;

-- name: NextInvoiceNumber :one
UPDATE seller_profiles SET invoice_counter = invoice_counter + 1 WHERE account_id = $1 RETURNING invoice_counter;

-- name: SetInvoice :exec
UPDATE sub_orders SET invoice_no = $2, invoiced_at = now() WHERE id = $1 AND invoice_no IS NULL;
