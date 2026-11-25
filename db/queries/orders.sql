-- name: ListAddresses :many
SELECT * FROM addresses WHERE account_id = $1 ORDER BY is_default DESC, created_at DESC;

-- name: CountAddresses :one
SELECT count(*)::int FROM addresses WHERE account_id = $1;

-- name: GetAddress :one
SELECT * FROM addresses WHERE id = $1 AND account_id = $2;

-- name: ClearDefaultAddress :exec
UPDATE addresses SET is_default = false WHERE account_id = $1 AND is_default;

-- name: CreateAddress :one
INSERT INTO addresses (id, account_id, name, phone, line1, line2, city, state, pincode, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: UpdateAddress :one
UPDATE addresses SET name = $3, phone = $4, line1 = $5, line2 = $6, city = $7, state = $8, pincode = $9, is_default = $10
WHERE id = $1 AND account_id = $2
RETURNING *;

-- name: DeleteAddress :execrows
DELETE FROM addresses WHERE id = $1 AND account_id = $2;

-- name: LockCart :one
SELECT * FROM carts WHERE account_id = $1 FOR UPDATE;

-- name: CheckoutLines :many
-- Cart lines with what an order item snapshots (seller lot, GST, commission).
SELECT ci.variant_id, ci.qty, v.price_paise, v.mrp_paise, v.attrs,
       p.id AS product_id, p.name, p.slug, p.seller_id, sp.store_name,
       c.gst_rate_bps, c.hsn, c.commission_bps,
       (a.status = 'ACTIVE' AND a.approval = 'APPROVED' AND p.status = 'ACTIVE') AS buyable,
       l.id AS lot_id, coalesce(l.quantity - l.reserved - l.sold, 0)::int AS available,
       coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
                 WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover
FROM cart_items ci
JOIN variants v ON v.id = ci.variant_id
JOIN products p ON p.id = v.product_id
JOIN categories c ON c.id = p.category_id
JOIN accounts a ON a.id = p.seller_id
JOIN seller_profiles sp ON sp.account_id = p.seller_id
LEFT JOIN inventory_lots l ON l.variant_id = v.id AND l.kind = 'SELLER'
WHERE ci.cart_id = $1
ORDER BY sp.store_name, p.seller_id, ci.added_at;

-- name: ReserveStock :one
-- Never oversells: 0 rows means someone else took the stock (LLD 8.3).
UPDATE inventory_lots SET reserved = reserved + @qty::int
WHERE id = @id AND quantity - reserved - sold >= @qty::int
RETURNING id;

-- name: CommitReservedStock :exec
UPDATE inventory_lots SET reserved = reserved - @qty::int, sold = sold + @qty::int WHERE id = @id;

-- name: ReleaseReservedStock :exec
UPDATE inventory_lots SET reserved = greatest(reserved - @qty::int, 0) WHERE id = @id;

-- name: ClearCart :exec
DELETE FROM cart_items WHERE cart_id = $1;

-- name: CreateOrder :one
INSERT INTO orders (id, number, buyer_id, address, status, item_total_paise, mrp_total_paise, shipping_paise, payable_paise,
                    pay_method, rules_snapshot, expires_at, confirmed_at, is_synthetic)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: CreateSubOrder :one
INSERT INTO sub_orders (id, order_id, seller_id, status, subtotal_paise) VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateOrderItem :exec
INSERT INTO order_items (id, sub_order_id, variant_id, lot_id, product_name, product_slug, variant_attrs, cover_key, qty,
                         unit_mrp_paise, unit_price_paise, line_total_paise, gst_rate_bps, hsn, commission_bps)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: SetOrderPayment :exec
UPDATE orders SET payment_id = $2 WHERE id = $1;

-- name: SetPaymentCheckoutURL :exec
UPDATE payments SET checkout_url = $2 WHERE id = $1;

-- name: GetOrder :one
SELECT * FROM orders WHERE id = $1;

-- name: GetOrderForUpdate :one
SELECT * FROM orders WHERE id = $1 FOR UPDATE;

-- name: GetOrderByNumberForUpdate :one
SELECT * FROM orders WHERE number = $1 FOR UPDATE;

-- name: ListOrders :many
SELECT o.id, o.number, o.status, o.payable_paise, o.placed_at, o.pay_method,
       (SELECT count(*) FROM sub_orders s JOIN order_items i ON i.sub_order_id = s.id WHERE s.order_id = o.id)::int AS item_count,
       coalesce((SELECT i.cover_key FROM sub_orders s JOIN order_items i ON i.sub_order_id = s.id WHERE s.order_id = o.id LIMIT 1), '')::text AS cover_key
FROM orders o WHERE o.buyer_id = $1 ORDER BY o.placed_at DESC LIMIT 100;

-- name: ListSubOrders :many
SELECT s.*, sp.store_name FROM sub_orders s JOIN seller_profiles sp ON sp.account_id = s.seller_id
WHERE s.order_id = $1 ORDER BY sp.store_name, s.replaces_item_id IS NOT NULL, s.id;

-- name: ListOrderItems :many
SELECT i.* FROM order_items i JOIN sub_orders s ON s.id = i.sub_order_id WHERE s.order_id = $1 ORDER BY i.id;

-- name: ConfirmOrder :exec
UPDATE orders SET status = 'CONFIRMED', confirmed_at = now() WHERE id = $1;

-- name: SetSubOrdersStatus :exec
UPDATE sub_orders SET status = $2, version = version + 1 WHERE order_id = $1;

-- name: ExpireOrder :exec
UPDATE orders SET status = 'EXPIRED' WHERE id = $1;

-- name: DueForExpiry :many
SELECT id FROM orders WHERE status = 'PENDING_PAYMENT' AND expires_at < now()
ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED;

-- name: BeginIdempotency :one
INSERT INTO idempotency_keys (account_id, route, key, request_hash) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
RETURNING key;

-- name: GetIdempotency :one
SELECT * FROM idempotency_keys WHERE account_id = $1 AND route = $2 AND key = $3;

-- name: FinishIdempotency :exec
UPDATE idempotency_keys SET status_code = $4, response = $5 WHERE account_id = $1 AND route = $2 AND key = $3;
