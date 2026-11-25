-- name: CreatePayment :one
INSERT INTO payments (id, purpose, ref, account_id, provider, amount_paise)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: SetProviderOrder :exec
UPDATE payments SET provider_order_id = $2, updated_at = now() WHERE id = $1;

-- name: GetPayment :one
SELECT * FROM payments WHERE id = $1;

-- name: GetPaymentByRef :one
SELECT * FROM payments WHERE ref = $1;

-- name: GetPaymentByProviderOrder :one
SELECT * FROM payments WHERE provider_order_id = $1 FOR UPDATE;

-- name: MarkPaymentCaptured :one
-- Final state wins: a late capture after a failure still captures (money moved).
UPDATE payments
SET status = 'CAPTURED', provider_payment_id = $2, method = $3, mandate_id = $4, captured_at = coalesce(captured_at, now()),
    failure_code = NULL, updated_at = now()
WHERE id = $1 AND status <> 'CAPTURED'
RETURNING *;

-- name: MarkPaymentFailed :one
UPDATE payments SET status = 'FAILED', provider_payment_id = $2, method = $3, failure_code = $4, updated_at = now()
WHERE id = $1 AND status IN ('CREATED', 'PENDING')
RETURNING *;

-- name: CreateRefund :one
INSERT INTO refunds (id, payment_id, ref, amount_paise, electrons, reason)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: SetRefundProvider :exec
UPDATE refunds SET provider_refund_id = $2, status = 'PROCESSING', updated_at = now() WHERE id = $1;

-- name: MarkRefundDone :one
UPDATE refunds SET status = $2, updated_at = now() WHERE provider_refund_id = $1 AND status <> 'REFUNDED'
RETURNING *;

-- name: RefundedTotal :one
SELECT coalesce(sum(amount_paise), 0)::bigint FROM refunds WHERE payment_id = $1 AND status <> 'FAILED';

-- name: RecordWebhook :one
-- Returns a row only the first time an event arrives (dedupe).
INSERT INTO webhook_events (provider, event_id, payload) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING provider;

-- name: MarkWebhookProcessed :exec
UPDATE webhook_events SET processed_at = now() WHERE provider = $1 AND event_id = $2;
