-- name: UpsertNotification :one
-- Returns the row and whether this call created it.
INSERT INTO notifications (id, account_id, template, title, body, link, dedupe_key)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (dedupe_key) DO UPDATE SET dedupe_key = EXCLUDED.dedupe_key
RETURNING id, account_id, template, title, body, link, read_at, created_at, (xmax = 0)::bool AS inserted;

-- name: GetDelivery :one
SELECT * FROM notification_deliveries WHERE notification_id = $1 AND channel = $2;

-- name: RecordDelivery :exec
INSERT INTO notification_deliveries (notification_id, channel, status, provider_ref, error)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (notification_id, channel) DO UPDATE
SET status = EXCLUDED.status, provider_ref = EXCLUDED.provider_ref, error = EXCLUDED.error,
    attempts = notification_deliveries.attempts + 1, updated_at = now();

-- name: GetRecipient :one
SELECT a.id, a.role, a.name, a.phone_e164, a.phone_verified_at, a.email,
       coalesce(p.whatsapp, true) AS whatsapp, coalesce(p.email, true) AS email_on, coalesce(p.marketing, false) AS marketing
FROM accounts a LEFT JOIN notification_prefs p ON p.account_id = a.id
WHERE a.id = $1;

-- name: ListNotifications :many
SELECT id, template, title, body, link, read_at, created_at FROM notifications
WHERE account_id = @account_id AND (sqlc.narg(before)::timestamptz IS NULL OR created_at < sqlc.narg(before)::timestamptz)
ORDER BY created_at DESC LIMIT 30;

-- name: CountUnread :one
SELECT count(*)::int FROM notifications WHERE account_id = $1 AND read_at IS NULL;

-- name: MarkNotificationsRead :execrows
UPDATE notifications SET read_at = now() WHERE account_id = @account_id AND read_at IS NULL AND id = ANY(@ids::uuid[]);

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications SET read_at = now() WHERE account_id = $1 AND read_at IS NULL;

-- name: GetPrefs :one
SELECT coalesce(p.whatsapp, true) AS whatsapp, coalesce(p.email, true) AS email, coalesce(p.marketing, false) AS marketing
FROM accounts a LEFT JOIN notification_prefs p ON p.account_id = a.id WHERE a.id = $1;

-- name: SavePrefs :exec
INSERT INTO notification_prefs (account_id, whatsapp, email, marketing) VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id) DO UPDATE SET whatsapp = EXCLUDED.whatsapp, email = EXCLUDED.email, marketing = EXCLUDED.marketing, updated_at = now();
