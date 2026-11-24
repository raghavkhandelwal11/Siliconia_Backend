-- name: GetConfig :one
SELECT key, value, version, updated_by, updated_at FROM config WHERE key = $1;

-- name: ListConfig :many
SELECT key, value, version, updated_by, updated_at FROM config ORDER BY key;
