-- name: GetMembership :one
SELECT * FROM memberships WHERE account_id = $1;

-- name: GetMembershipForUpdate :one
SELECT * FROM memberships WHERE account_id = $1 FOR UPDATE;

-- name: IsMember :one
SELECT EXISTS (SELECT 1 FROM memberships WHERE account_id = $1 AND status = 'ACTIVE' AND expires_at > now());

-- name: ActivateMembership :one
-- New members start now; renewals extend from the current expiry (never lose days).
INSERT INTO memberships (id, account_id, status, started_at, expires_at, auto_renew, mandate_id)
VALUES (@id, @account_id, 'ACTIVE', now(), now() + interval '365 days', @auto_renew, @mandate_id)
ON CONFLICT (account_id) DO UPDATE SET
  status = 'ACTIVE',
  started_at = CASE WHEN memberships.status = 'ACTIVE' AND memberships.expires_at > now() THEN memberships.started_at ELSE now() END,
  expires_at = greatest(memberships.expires_at, now()) + interval '365 days',
  auto_renew = EXCLUDED.auto_renew,
  mandate_id = coalesce(EXCLUDED.mandate_id, memberships.mandate_id),
  renew_attempts = 0, updated_at = now()
RETURNING *;

-- name: SetAutoRenew :exec
UPDATE memberships SET auto_renew = $2, mandate_id = CASE WHEN $2 THEN mandate_id ELSE NULL END, updated_at = now() WHERE account_id = $1;

-- name: CreateMembershipPurchase :one
INSERT INTO membership_purchases (id, ref, account_id, kind, electrons_used, amount_paise, auto_renew, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: PendingPurchase :one
SELECT * FROM membership_purchases WHERE account_id = $1 AND kind = 'PURCHASE' AND status = 'PENDING' ORDER BY created_at DESC LIMIT 1;

-- name: GetPurchaseByRefForUpdate :one
SELECT * FROM membership_purchases WHERE ref = $1 FOR UPDATE;

-- name: SetPurchaseStatus :exec
UPDATE membership_purchases SET status = $2 WHERE id = $1;

-- name: SetPurchasePayment :exec
UPDATE membership_purchases SET payment_id = $2 WHERE id = $1;

-- name: StalePurchases :many
SELECT * FROM membership_purchases WHERE status = 'PENDING' AND kind = 'PURCHASE' AND created_at < now() - interval '30 minutes'
FOR UPDATE SKIP LOCKED;

-- name: MembershipsDue :many
-- Active memberships expiring within two days (pre-debit notice) or already expired (charge or lapse).
SELECT * FROM memberships WHERE status = 'ACTIVE' AND expires_at <= now() + interval '2 days' FOR UPDATE SKIP LOCKED;

-- name: MarkPredebitSent :exec
UPDATE memberships SET predebit_sent_for = expires_at WHERE id = $1;

-- name: BumpRenewAttempt :exec
UPDATE memberships SET renew_attempts = renew_attempts + 1, updated_at = now() WHERE id = $1;

-- name: LapseMembership :exec
UPDATE memberships SET status = 'LAPSED', updated_at = now() WHERE id = $1;
