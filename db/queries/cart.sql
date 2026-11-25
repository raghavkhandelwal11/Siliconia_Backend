-- name: GetCartByAccount :one
SELECT * FROM carts WHERE account_id = $1;

-- name: GetCartByDevice :one
SELECT * FROM carts WHERE device_id = $1;

-- name: CreateCart :one
INSERT INTO carts (id, account_id, device_id) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: TouchCart :exec
UPDATE carts SET updated_at = now() WHERE id = $1;

-- name: DeleteCart :exec
DELETE FROM carts WHERE id = $1;

-- name: CartLines :many
-- Lines with everything the cart page shows; unavailable products stay listed.
SELECT ci.variant_id, ci.qty, ci.price_at_add_paise, ci.added_at,
       v.sku, v.attrs, v.price_paise, v.mrp_paise,
       p.id AS product_id, p.slug, p.name, p.brand, p.status AS product_status, p.seller_id,
       sp.store_name,
       (a.status = 'ACTIVE' AND a.approval = 'APPROVED' AND p.status = 'ACTIVE') AS buyable,
       coalesce((SELECT sum(l.quantity - l.reserved - l.sold) FROM inventory_lots l WHERE l.variant_id = v.id AND l.kind = 'SELLER'), 0)::int AS available,
       coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
                 WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover
FROM cart_items ci
JOIN variants v ON v.id = ci.variant_id
JOIN products p ON p.id = v.product_id
JOIN accounts a ON a.id = p.seller_id
JOIN seller_profiles sp ON sp.account_id = p.seller_id
WHERE ci.cart_id = $1
ORDER BY sp.store_name, ci.added_at;

-- name: GetBuyableVariant :one
SELECT v.id, v.price_paise, v.product_id,
       coalesce((SELECT sum(l.quantity - l.reserved - l.sold) FROM inventory_lots l WHERE l.variant_id = v.id AND l.kind = 'SELLER'), 0)::int AS available
FROM variants v
JOIN products p ON p.id = v.product_id AND p.status = 'ACTIVE'
JOIN accounts a ON a.id = p.seller_id AND a.status = 'ACTIVE' AND a.approval = 'APPROVED'
WHERE v.id = $1;

-- name: SetCartItem :exec
INSERT INTO cart_items (cart_id, variant_id, qty, price_at_add_paise) VALUES ($1, $2, $3, $4)
ON CONFLICT (cart_id, variant_id) DO UPDATE SET qty = EXCLUDED.qty;

-- name: DeleteCartItem :exec
DELETE FROM cart_items WHERE cart_id = $1 AND variant_id = $2;

-- name: ListCartItems :many
SELECT * FROM cart_items WHERE cart_id = $1;

-- name: WishlistLines :many
SELECT w.variant_id, w.price_at_add_paise, w.notify_stock, w.notify_price, w.created_at,
       v.attrs, v.price_paise, v.mrp_paise,
       p.slug, p.name, p.brand,
       (a.status = 'ACTIVE' AND a.approval = 'APPROVED' AND p.status = 'ACTIVE') AS buyable,
       coalesce((SELECT sum(l.quantity - l.reserved - l.sold) FROM inventory_lots l WHERE l.variant_id = v.id AND l.kind = 'SELLER'), 0)::int AS available,
       coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
                 WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover
FROM wishlist_items w
JOIN variants v ON v.id = w.variant_id
JOIN products p ON p.id = v.product_id
JOIN accounts a ON a.id = p.seller_id
WHERE w.account_id = $1
ORDER BY w.created_at DESC
LIMIT 200;

-- name: AddWishlist :exec
INSERT INTO wishlist_items (account_id, variant_id, price_at_add_paise) VALUES ($1, $2, $3)
ON CONFLICT (account_id, variant_id) DO NOTHING;

-- name: RemoveWishlist :exec
DELETE FROM wishlist_items WHERE account_id = $1 AND variant_id = $2;

-- name: WishlistHas :many
SELECT variant_id FROM wishlist_items WHERE account_id = $1 AND variant_id = ANY(@ids::uuid[]);
