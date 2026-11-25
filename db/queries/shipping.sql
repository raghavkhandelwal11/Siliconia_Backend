-- name: GetSubOrderForShipping :one
-- Locks the sub-order while a shipment is booked for it.
SELECT s.id, s.status, s.seller_id, s.subtotal_paise, o.id AS order_id, o.number, o.address, o.pay_method,
       sp.store_name, sp.pickup_address, a.phone_e164 AS seller_phone
FROM sub_orders s
JOIN orders o ON o.id = s.order_id
JOIN seller_profiles sp ON sp.account_id = s.seller_id
JOIN accounts a ON a.id = s.seller_id
WHERE s.id = $1
FOR UPDATE OF s;

-- name: GetForwardShipment :one
SELECT * FROM shipments WHERE sub_order_id = $1 AND kind = 'FORWARD' AND status <> 'CANCELLED';

-- name: CreateShipment :one
INSERT INTO shipments (id, sub_order_id, kind, ref, provider, awb, status, eta, cod_amount_paise)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetShipmentByAWBForUpdate :one
SELECT * FROM shipments WHERE awb = $1 FOR UPDATE;

-- name: UpdateShipmentStatus :exec
UPDATE shipments SET status = $2, eta = coalesce(sqlc.narg(eta), eta), last_event_at = $3, updated_at = now() WHERE id = $1;

-- name: AddTrackingEvent :exec
INSERT INTO tracking_events (id, shipment_id, status, location, at, raw) VALUES ($1, $2, $3, $4, $5, $6);

-- name: SetSubOrderStatus :one
UPDATE sub_orders SET status = $2, version = version + 1 WHERE id = $1 RETURNING *;

-- name: MarkSubOrderDelivered :one
UPDATE sub_orders SET status = 'DELIVERED', delivered_at = @delivered_at, return_window_ends_at = @return_window_ends_at,
  cod_collected_paise = @cod_collected_paise, version = version + 1
WHERE id = @id RETURNING *;

-- name: ListOrderShipments :many
SELECT sh.* FROM shipments sh JOIN sub_orders s ON s.id = sh.sub_order_id
WHERE s.order_id = $1 ORDER BY sh.created_at;

-- name: ListOrderTracking :many
SELECT t.shipment_id, t.status, t.location, t.at FROM tracking_events t
JOIN shipments sh ON sh.id = t.shipment_id JOIN sub_orders s ON s.id = sh.sub_order_id
WHERE s.order_id = $1 ORDER BY t.at, t.id;

-- name: GetSubOrder :one
SELECT * FROM sub_orders WHERE id = $1;
