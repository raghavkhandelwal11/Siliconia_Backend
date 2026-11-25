-- name: InsertUpload :one
INSERT INTO uploads (id, owner_id, purpose, storage_key, content_type, bytes, width, height)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetUploadByKey :one
SELECT * FROM uploads WHERE storage_key = $1;

-- name: GetOwnUploads :many
SELECT * FROM uploads WHERE owner_id = $1 AND id = ANY(@ids::uuid[]);

-- name: CreateProduct :one
INSERT INTO products (id, seller_id, category_id, slug, name, brand, model, description, warranty, origin_country, is_local, specs)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetProduct :one
SELECT * FROM products WHERE id = $1;

-- name: GetProductForUpdate :one
SELECT * FROM products WHERE id = $1 FOR UPDATE;

-- name: UpdateProduct :one
UPDATE products
SET category_id = $2, name = $3, brand = $4, model = $5, description = $6, warranty = $7,
    origin_country = $8, is_local = $9, specs = $10, updated_at = now(), version = version + 1
WHERE id = $1 AND version = $11
RETURNING *;

-- name: SetProductStatus :one
UPDATE products
SET status = $2, published_at = CASE WHEN $2 = 'ACTIVE' THEN coalesce(published_at, now()) ELSE published_at END,
    updated_at = now(), version = version + 1
WHERE id = $1
RETURNING *;

-- name: TouchProduct :exec
UPDATE products SET updated_at = now(), version = version + 1 WHERE id = $1;

-- name: ListSellerProducts :many
SELECT p.id, p.slug, p.name, p.brand, p.status, p.updated_at, p.version,
       c.name AS category_name,
       coalesce((SELECT min(v.price_paise) FROM variants v WHERE v.product_id = p.id), 0)::bigint AS min_price_paise,
       coalesce((SELECT max(v.price_paise) FROM variants v WHERE v.product_id = p.id), 0)::bigint AS max_price_paise,
       (SELECT count(*) FROM variants v WHERE v.product_id = p.id)::int AS variant_count,
       coalesce((SELECT sum(l.quantity - l.reserved - l.sold) FROM inventory_lots l JOIN variants v ON v.id = l.variant_id
                 WHERE v.product_id = p.id AND l.kind = 'SELLER'), 0)::int AS available,
       coalesce((SELECT u.storage_key FROM product_images i JOIN uploads u ON u.id = i.upload_id
        WHERE i.product_id = p.id ORDER BY i.position LIMIT 1), '')::text AS cover_key
FROM products p JOIN categories c ON c.id = p.category_id
WHERE p.seller_id = $1 AND p.status <> 'HIDDEN'
ORDER BY p.updated_at DESC
LIMIT 500;

-- name: ListVariants :many
SELECT v.*, coalesce(l.quantity, 0)::int AS quantity, coalesce(l.reserved, 0)::int AS reserved, coalesce(l.sold, 0)::int AS sold
FROM variants v LEFT JOIN inventory_lots l ON l.variant_id = v.id AND l.kind = 'SELLER'
WHERE v.product_id = $1
ORDER BY v.position, v.sku;

-- name: UpsertVariant :one
INSERT INTO variants (id, product_id, sku, attrs, mrp_paise, price_paise, position)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET sku = EXCLUDED.sku, attrs = EXCLUDED.attrs, mrp_paise = EXCLUDED.mrp_paise,
  price_paise = EXCLUDED.price_paise, position = EXCLUDED.position
WHERE variants.product_id = EXCLUDED.product_id
RETURNING *;

-- name: SetSellerStock :exec
-- Sets on-hand quantity; the CHECK refuses going below what is reserved or sold.
INSERT INTO inventory_lots (id, variant_id, owner_seller_id, kind, quantity)
VALUES ($1, $2, $3, 'SELLER', $4)
ON CONFLICT (variant_id) WHERE kind = 'SELLER' DO UPDATE SET quantity = EXCLUDED.quantity;

-- name: DeleteVariantsExcept :many
-- Removes variants left out of an edit, unless stock was ever reserved or sold.
DELETE FROM variants v
WHERE v.product_id = $1 AND NOT (v.id = ANY(@keep::uuid[]))
  AND NOT EXISTS (SELECT 1 FROM inventory_lots l WHERE l.variant_id = v.id AND (l.reserved > 0 OR l.sold > 0))
RETURNING v.id;

-- name: CountVariantsInUse :one
SELECT count(*)::int FROM variants v
WHERE v.product_id = $1 AND NOT (v.id = ANY(@keep::uuid[]))
  AND EXISTS (SELECT 1 FROM inventory_lots l WHERE l.variant_id = v.id AND (l.reserved > 0 OR l.sold > 0));

-- name: ListProductImages :many
SELECT i.position, i.alt, u.id AS upload_id, u.storage_key, u.width, u.height
FROM product_images i JOIN uploads u ON u.id = i.upload_id
WHERE i.product_id = $1
ORDER BY i.position;

-- name: DeleteProductImages :exec
DELETE FROM product_images WHERE product_id = $1;

-- name: InsertProductImage :exec
INSERT INTO product_images (product_id, upload_id, position, alt) VALUES ($1, $2, $3, $4);

-- name: GetActiveCategory :one
SELECT * FROM categories WHERE id = $1 AND status = 'ACTIVE';

-- name: GetPublicProduct :one
-- A live product from an active, approved seller (what shoppers may see).
SELECT sqlc.embed(p), sp.store_name, sp.verified_tag, sp.account_id AS seller_account
FROM products p
JOIN accounts a ON a.id = p.seller_id AND a.status = 'ACTIVE' AND a.approval = 'APPROVED'
JOIN seller_profiles sp ON sp.account_id = p.seller_id
WHERE p.slug = $1 AND p.status = 'ACTIVE';

-- name: GetPublicProductByID :one
SELECT sqlc.embed(p), sp.store_name, sp.verified_tag, sp.account_id AS seller_account
FROM products p
JOIN accounts a ON a.id = p.seller_id AND a.status = 'ACTIVE' AND a.approval = 'APPROVED'
JOIN seller_profiles sp ON sp.account_id = p.seller_id
WHERE p.id = $1 AND p.status = 'ACTIVE';
