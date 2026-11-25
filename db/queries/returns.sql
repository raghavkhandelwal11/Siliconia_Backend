-- name: GetItemForReturn :one
-- The item with its sub-order, locked while a return is requested.
SELECT i.id, i.status AS item_status, i.line_total_paise, i.variant_id, i.lot_id, i.qty, i.product_name, i.variant_attrs, i.cover_key,
       s.id AS sub_order_id, s.status AS sub_status, s.return_window_ends_at, s.seller_id, s.order_id,
       o.buyer_id, o.pay_method, o.payment_id, o.number AS order_number
FROM order_items i
JOIN sub_orders s ON s.id = i.sub_order_id
JOIN orders o ON o.id = s.order_id
WHERE i.id = $1
FOR UPDATE OF i;

-- name: CreateReturn :one
INSERT INTO return_requests (id, number, order_item_id, sub_order_id, buyer_id, seller_id, type, reason, note, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'REQUESTED')
RETURNING *;

-- name: GetReturnForUpdate :one
SELECT * FROM return_requests WHERE id = $1 FOR UPDATE;

-- name: GetReturnByShipmentForUpdate :one
SELECT * FROM return_requests WHERE pickup_shipment_id = $1 FOR UPDATE;

-- name: SetReturnStatus :one
UPDATE return_requests SET status = @to_status, updated_at = now()
WHERE id = @id AND status = ANY(@from_status::text[])
RETURNING *;

-- name: RejectReturn :one
UPDATE return_requests SET status = 'REJECTED', reject_reason = $2, decided_at = now(), updated_at = now()
WHERE id = $1 AND status = 'REQUESTED' RETURNING *;

-- name: ScheduleReturnPickup :one
UPDATE return_requests SET status = 'PICKUP_SCHEDULED', pickup_shipment_id = $2, decided_at = now(), updated_at = now()
WHERE id = $1 AND status = 'REQUESTED' RETURNING *;

-- name: CompleteReturn :one
UPDATE return_requests SET status = @to_status, refund_amount_paise = @refund_amount_paise, outcome_note = @outcome_note,
  replacement_sub_order_id = sqlc.narg(replacement_sub_order_id), updated_at = now()
WHERE id = @id AND status IN ('PICKED_UP', 'PICKUP_SCHEDULED') RETURNING *;

-- name: MarkReplacementShipped :exec
UPDATE return_requests SET status = 'REPLACEMENT_SHIPPED', updated_at = now()
WHERE replacement_sub_order_id = $1 AND status = 'RECEIVED';

-- name: SetOrderItemStatus :exec
UPDATE order_items SET status = $2 WHERE id = $1;

-- name: CreateReplacementSubOrder :one
INSERT INTO sub_orders (id, order_id, seller_id, status, subtotal_paise, replaces_item_id)
VALUES ($1, $2, $3, 'CONFIRMED', 0, $4) RETURNING *;

-- name: CopyItemAsReplacement :exec
-- The replacement unit ships at zero value; tax was charged on the original.
INSERT INTO order_items (id, sub_order_id, variant_id, lot_id, price_type, product_name, product_slug, variant_attrs, cover_key,
  qty, unit_mrp_paise, unit_price_paise, line_total_paise, gst_rate_bps, hsn, commission_bps)
SELECT @new_id::uuid, @sub_order_id::uuid, src.variant_id, src.lot_id, src.price_type, src.product_name, src.product_slug,
  src.variant_attrs, src.cover_key, src.qty, src.unit_mrp_paise, 0, 0, src.gst_rate_bps, src.hsn, 0
FROM order_items src WHERE src.id = @item_id::uuid;

-- name: ListBuyerReturns :many
SELECT r.*, i.product_name, i.variant_attrs, i.cover_key, i.qty, o.number AS order_number, o.id AS order_id, sh.awb
FROM return_requests r
JOIN order_items i ON i.id = r.order_item_id
JOIN sub_orders s ON s.id = r.sub_order_id
JOIN orders o ON o.id = s.order_id
LEFT JOIN shipments sh ON sh.id = r.pickup_shipment_id
WHERE r.buyer_id = $1 ORDER BY r.created_at DESC LIMIT 100;

-- name: ListSellerReturns :many
SELECT r.*, i.product_name, i.variant_attrs, i.cover_key, i.qty, o.number AS order_number, o.id AS order_id, sh.awb
FROM return_requests r
JOIN order_items i ON i.id = r.order_item_id
JOIN sub_orders s ON s.id = r.sub_order_id
JOIN orders o ON o.id = s.order_id
LEFT JOIN shipments sh ON sh.id = r.pickup_shipment_id
WHERE r.seller_id = @seller_id AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status)::text)
ORDER BY r.created_at DESC LIMIT 200;

-- name: ListOrderReturns :many
SELECT * FROM return_requests WHERE sub_order_id IN (SELECT id FROM sub_orders WHERE order_id = $1) ORDER BY created_at;

-- name: GetSellerPickup :one
SELECT sp.store_name, sp.pickup_address, a.phone_e164 FROM seller_profiles sp JOIN accounts a ON a.id = sp.account_id
WHERE sp.account_id = $1;

-- name: CreateIssue :one
INSERT INTO issues (id, number, buyer_id, order_id, order_item_id, type, status) VALUES ($1, $2, $3, $4, $5, $6, 'OPEN')
RETURNING *;

-- name: AddIssueMessage :one
INSERT INTO issue_messages (id, issue_id, author_id, author_role, body) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetIssue :one
SELECT i.*, o.number AS order_number FROM issues i JOIN orders o ON o.id = i.order_id WHERE i.id = $1;

-- name: GetIssueForUpdate :one
SELECT * FROM issues WHERE id = $1 FOR UPDATE;

-- name: ListIssueMessages :many
SELECT * FROM issue_messages WHERE issue_id = $1 ORDER BY created_at, id;

-- name: SetIssueStatus :one
UPDATE issues SET status = @to_status, updated_at = now(),
  first_response_at = CASE WHEN @support_reply::bool AND first_response_at IS NULL THEN now() ELSE first_response_at END,
  resolved_at = CASE WHEN @to_status = 'RESOLVED' THEN now() WHEN @to_status = 'IN_PROGRESS' THEN NULL ELSE resolved_at END,
  closed_at = CASE WHEN @to_status = 'CLOSED' THEN now() ELSE closed_at END
WHERE id = @id RETURNING *;

-- name: ListBuyerIssues :many
SELECT i.*, o.number AS order_number FROM issues i JOIN orders o ON o.id = i.order_id
WHERE i.buyer_id = $1 ORDER BY i.updated_at DESC LIMIT 100;

-- name: ListSupportIssues :many
SELECT i.*, o.number AS order_number FROM issues i JOIN orders o ON o.id = i.order_id
WHERE (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status)::text)
ORDER BY (i.status IN ('OPEN', 'IN_PROGRESS')) DESC, i.updated_at ASC LIMIT 200;

-- name: CloseResolvedIssues :execrows
UPDATE issues SET status = 'CLOSED', closed_at = now(), updated_at = now()
WHERE status = 'RESOLVED' AND resolved_at < now() - interval '7 days';
