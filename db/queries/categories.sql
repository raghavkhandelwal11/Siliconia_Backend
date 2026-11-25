-- name: ListActiveCategories :many
SELECT * FROM categories WHERE status = 'ACTIVE' ORDER BY parent_id NULLS FIRST, position, name;

-- name: GetCategoryBySlug :one
SELECT * FROM categories WHERE slug = $1 AND status = 'ACTIVE';

-- name: GetCategory :one
SELECT * FROM categories WHERE id = $1;

-- name: SeedCategory :one
-- Inserts a seed row once; later runs keep whatever admins changed.
INSERT INTO categories (id, parent_id, slug, name, icon, examples, position, spec_schema)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
RETURNING id, (xmax = 0) AS inserted;

-- name: CategoriesVersion :one
-- Cheap change marker for HTTP caching of the tree.
SELECT count(*)::bigint AS n, coalesce(max(updated_at), 'epoch')::timestamptz AS last FROM categories;
