-- Payouts ---------------------------------------------------------------------------
-- name: SettleableSubOrders :many
-- Delivered sub-orders whose return window has closed, not yet in a payout;
-- platform launch stock and free replacements are excluded (LLD 8.10).
SELECT s.id, s.seller_id,
  coalesce(sum(i.line_total_paise) FILTER (WHERE i.status = 'ACTIVE'), 0)::bigint AS gross,
  coalesce(sum(i.line_total_paise * i.commission_bps / 10000) FILTER (WHERE i.status = 'ACTIVE'), 0)::bigint AS commission
FROM sub_orders s
JOIN order_items i ON i.sub_order_id = s.id
JOIN orders o ON o.id = s.order_id
JOIN seller_profiles sp ON sp.account_id = s.seller_id
WHERE s.status = 'DELIVERED' AND s.return_window_ends_at < now() AND s.settled_at IS NULL AND s.replaces_item_id IS NULL
  AND NOT sp.is_platform AND NOT o.is_synthetic
  AND NOT EXISTS (SELECT 1 FROM payout_claims c WHERE c.sub_order_id = s.id)
GROUP BY s.id, s.seller_id ORDER BY s.seller_id, s.id;

-- name: CreateBatch :one
INSERT INTO payout_batches (id, week_ending, status) VALUES ($1, $2, 'DRAFT') RETURNING *;

-- name: CreatePayout :one
INSERT INTO payouts (id, batch_id, seller_id, gross_paise, commission_paise, tds_paise, net_paise, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT') RETURNING *;

-- name: AddPayoutLine :exec
INSERT INTO payout_lines (payout_id, sub_order_id, gross_paise, commission_paise) VALUES ($1, $2, $3, $4);

-- name: ClaimSubOrder :exec
INSERT INTO payout_claims (sub_order_id, payout_id) VALUES ($1, $2);

-- name: ListBatches :many
SELECT b.*, count(p.id)::int AS payouts, coalesce(sum(p.net_paise), 0)::bigint AS net_paise,
  count(p.id) FILTER (WHERE p.status = 'PAID')::int AS paid, count(p.id) FILTER (WHERE p.status = 'FAILED')::int AS failed
FROM payout_batches b LEFT JOIN payouts p ON p.batch_id = b.id GROUP BY b.id ORDER BY b.created_at DESC LIMIT 50;

-- name: GetBatchForUpdate :one
SELECT * FROM payout_batches WHERE id = $1 FOR UPDATE;

-- name: SetBatchStatus :exec
UPDATE payout_batches SET status = $2, approved_by = coalesce(sqlc.narg(approved_by), approved_by),
  released_by = coalesce(sqlc.narg(released_by), released_by), updated_at = now() WHERE id = $1;

-- name: BatchPayouts :many
SELECT p.*, sp.store_name, sp.bank_enc, sp.ifsc FROM payouts p JOIN seller_profiles sp ON sp.account_id = p.seller_id
WHERE p.batch_id = $1 ORDER BY sp.store_name;

-- name: SetPayoutSent :exec
UPDATE payouts SET status = 'PROCESSING', provider_ref = $2, updated_at = now() WHERE id = $1;

-- name: SetPayoutFailedToSend :exec
UPDATE payouts SET status = 'FAILED', failure_reason = $2, updated_at = now() WHERE id = $1;

-- name: GetPayoutByRefForUpdate :one
SELECT * FROM payouts WHERE provider_ref = $1 FOR UPDATE;

-- name: FinishPayout :exec
UPDATE payouts SET status = $2, bank_ref = $3, failure_reason = $4, updated_at = now() WHERE id = $1;

-- name: SettlePayoutSubOrders :exec
UPDATE sub_orders SET settled_at = now() WHERE id IN (SELECT sub_order_id FROM payout_lines WHERE payout_id = $1);

-- name: ReleasePayoutClaims :exec
DELETE FROM payout_claims WHERE payout_id = $1;

-- name: SellerPayouts :many
SELECT * FROM payouts WHERE seller_id = $1 AND status <> 'DRAFT' ORDER BY created_at DESC LIMIT 100;

-- name: PayoutStatement :many
SELECT l.*, o.number, s.delivered_at FROM payout_lines l JOIN payouts p ON p.id = l.payout_id
JOIN sub_orders s ON s.id = l.sub_order_id JOIN orders o ON o.id = s.order_id
WHERE l.payout_id = @payout_id AND (p.seller_id = @seller_id OR @admin::bool) ORDER BY o.number;

-- name: GetPayout :one
SELECT * FROM payouts WHERE id = $1;

-- name: RefreshVerifiedTags :execrows
-- Rolling 12-month delivered, kept sales; verified at ₹4,00,000 (A-13).
UPDATE seller_profiles sp SET rolling_12m_sales_paise = t.sales, verified_tag = t.sales >= @threshold::bigint
FROM (SELECT p.account_id, coalesce((SELECT sum(i.line_total_paise) FROM sub_orders s JOIN order_items i ON i.sub_order_id = s.id
        WHERE s.seller_id = p.account_id AND s.status = 'DELIVERED' AND i.status IN ('ACTIVE', 'REPLACED') AND s.delivered_at > now() - interval '365 days'), 0)::bigint AS sales
      FROM seller_profiles p) t
WHERE sp.account_id = t.account_id AND (sp.rolling_12m_sales_paise <> t.sales OR sp.verified_tag <> (t.sales >= @threshold::bigint));

-- Admin console -------------------------------------------------------------------------
-- name: AdminOverview :one
SELECT
  (SELECT count(*) FROM orders WHERE placed_at >= date_trunc('day', now() AT TIME ZONE 'Asia/Kolkata') AT TIME ZONE 'Asia/Kolkata' AND status = 'CONFIRMED' AND NOT is_synthetic)::int AS orders_today,
  (SELECT coalesce(sum(payable_paise), 0) FROM orders WHERE placed_at >= date_trunc('day', now() AT TIME ZONE 'Asia/Kolkata') AT TIME ZONE 'Asia/Kolkata' AND status = 'CONFIRMED' AND NOT is_synthetic)::bigint AS revenue_today,
  (SELECT count(*) FROM accounts WHERE approval IN ('SUBMITTED', 'UNDER_REVIEW'))::int AS approvals_waiting,
  (SELECT count(*) FROM issues WHERE status IN ('OPEN', 'IN_PROGRESS') AND first_response_at IS NULL AND created_at < now() - interval '24 hours')::int AS issues_overdue,
  (SELECT count(*) FROM issues WHERE status IN ('OPEN', 'IN_PROGRESS'))::int AS issues_open,
  (SELECT count(*) FROM return_requests WHERE status = 'REQUESTED')::int AS returns_waiting,
  (SELECT count(*) FROM incidents WHERE resolved_at IS NULL)::int AS open_incidents,
  (SELECT count(*) FROM payout_batches WHERE status = 'DRAFT')::int AS batches_waiting;

-- name: SearchAccounts :many
SELECT id, role, name, phone_e164, email, status, approval, created_at FROM accounts
WHERE (@search::text = '' OR name ILIKE '%' || @search || '%' OR phone_e164 LIKE '%' || @search || '%' OR id::text = @search)
  AND (sqlc.narg(role)::account_role IS NULL OR role = sqlc.narg(role)::account_role)
ORDER BY created_at DESC LIMIT 50;

-- name: SetAccountStatus :one
UPDATE accounts SET status = $2, version = version + 1 WHERE id = $1 AND role <> 'ADMIN' RETURNING id, status;

-- name: RevokeAccountSessions :exec
UPDATE sessions SET revoked_at = now() WHERE account_id = $1 AND revoked_at IS NULL;

-- name: AdminOrders :many
SELECT o.id, o.number, o.status, o.pay_method, o.payable_paise, o.placed_at, o.is_synthetic, a.name AS buyer_name, a.phone_e164 AS buyer_phone
FROM orders o JOIN accounts a ON a.id = o.buyer_id
WHERE (@search::text = '' OR o.number ILIKE '%' || @search || '%' OR a.phone_e164 LIKE '%' || @search || '%')
ORDER BY o.placed_at DESC LIMIT 100;

-- name: AdminReturns :many
SELECT r.id, r.number, r.type, r.reason, r.status, r.created_at, i.product_name, o.number AS order_number
FROM return_requests r JOIN order_items i ON i.id = r.order_item_id JOIN sub_orders s ON s.id = r.sub_order_id JOIN orders o ON o.id = s.order_id
ORDER BY r.created_at DESC LIMIT 100;

-- name: InsertAuditM5 :exec
INSERT INTO audit_log (id, actor_id, acting_as_role, action, target_type, target_id, before, after) VALUES ($1, $2, 'ADMIN', $3, $4, $5, $6, $7);

-- name: ListAudit :many
SELECT l.*, a.name AS actor_name FROM audit_log l LEFT JOIN accounts a ON a.id = l.actor_id
WHERE (@target_type::text = '' OR l.target_type = @target_type) ORDER BY l.at DESC LIMIT 200;

-- Config ---------------------------------------------------------------------------------

-- name: GetConfigForUpdate :one
SELECT * FROM config WHERE key = $1 FOR UPDATE;

-- name: SetConfig :exec
UPDATE config SET value = $2, version = version + 1, updated_by = $3, updated_at = now() WHERE key = $1;

-- name: AddConfigHistory :exec
INSERT INTO config_history (key, version, value, changed_by) VALUES ($1, $2, $3, $4);

-- name: ConfigHistory :many
SELECT h.*, a.name AS changed_by_name FROM config_history h LEFT JOIN accounts a ON a.id = h.changed_by
WHERE h.key = $1 ORDER BY h.changed_at DESC LIMIT 50;

-- name: UpdateCategoryRates :one
UPDATE categories SET commission_bps = $2, gst_rate_bps = $3, hsn = $4 WHERE id = $1 RETURNING id, name, commission_bps, gst_rate_bps, hsn;

-- name: AdminCategories :many
SELECT id, parent_id, slug, name, commission_bps, gst_rate_bps, hsn FROM categories ORDER BY parent_id NULLS FIRST, name;

-- Legal ---------------------------------------------------------------------------------------
-- name: LatestLegal :many
SELECT DISTINCT ON (kind) * FROM legal_docs ORDER BY kind, version DESC;

-- name: LatestLegalOf :one
SELECT * FROM legal_docs WHERE kind = $1 ORDER BY version DESC LIMIT 1;

-- name: PublishLegal :one
INSERT INTO legal_docs (id, kind, version, title, body_md, published_by)
VALUES ($1, $2, (SELECT coalesce(max(version), 0) + 1 FROM legal_docs WHERE kind = $2), $3, $4, $5) RETURNING *;

-- name: AcceptLegal :exec
INSERT INTO legal_acceptances (account_id, doc_id, ip) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: PendingLegal :many
-- Latest TERMS and PRIVACY versions the account has not accepted.
SELECT d.* FROM (SELECT DISTINCT ON (kind) * FROM legal_docs WHERE kind IN ('TERMS', 'PRIVACY') ORDER BY kind, version DESC) d
WHERE NOT EXISTS (SELECT 1 FROM legal_acceptances a WHERE a.account_id = $1 AND a.doc_id = d.id);

-- Ledger adjustments ---------------------------------------------------------------------------
-- name: RequestAdjustment :one
INSERT INTO ledger_adjustment_requests (id, account_id, amount, reason, requested_by, status) VALUES ($1, $2, $3, $4, $5, 'REQUESTED') RETURNING *;

-- name: GetAdjustmentForUpdate :one
SELECT * FROM ledger_adjustment_requests WHERE id = $1 FOR UPDATE;

-- name: DecideAdjustment :exec
UPDATE ledger_adjustment_requests SET status = $2, approved_by = $3, decided_at = now() WHERE id = $1;

-- name: ListAdjustments :many
SELECT r.*, a.name AS account_name FROM ledger_adjustment_requests r JOIN accounts a ON a.id = r.account_id ORDER BY r.created_at DESC LIMIT 100;

-- Health ----------------------------------------------------------------------------------------
-- name: AddRun :exec
INSERT INTO synthetic_runs (id, check_name, ok, latency_ms, error) VALUES ($1, $2, $3, $4, $5);

-- name: RecentRuns :many
SELECT ok FROM synthetic_runs WHERE check_name = $1 ORDER BY at DESC LIMIT 2;

-- name: OpenIncident :execrows
INSERT INTO incidents (id, check_name, last_error) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: ResolveIncident :execrows
UPDATE incidents SET resolved_at = now() WHERE check_name = $1 AND resolved_at IS NULL;

-- name: HealthSummary :many
SELECT c.check_name,
  (SELECT ok FROM synthetic_runs r WHERE r.check_name = c.check_name ORDER BY at DESC LIMIT 1) AS last_ok,
  (SELECT error FROM synthetic_runs r WHERE r.check_name = c.check_name ORDER BY at DESC LIMIT 1) AS last_error,
  (SELECT max(at) FROM synthetic_runs r WHERE r.check_name = c.check_name)::timestamptz AS last_at,
  (SELECT coalesce(avg(latency_ms), 0) FROM synthetic_runs r WHERE r.check_name = c.check_name AND at > now() - interval '24 hours')::int AS avg_latency_ms,
  (SELECT coalesce(100.0 * count(*) FILTER (WHERE ok) / nullif(count(*), 0), 100) FROM synthetic_runs r WHERE r.check_name = c.check_name AND at > now() - interval '30 days')::float8 AS uptime_30d
FROM (SELECT DISTINCT check_name FROM synthetic_runs) c ORDER BY c.check_name;

-- name: OpenIncidents :many
SELECT * FROM incidents WHERE resolved_at IS NULL OR resolved_at > now() - interval '7 days' ORDER BY opened_at DESC LIMIT 50;

-- name: AdminIDs :many
SELECT id FROM accounts WHERE role = 'ADMIN' AND status = 'ACTIVE';

-- Reconciliation, invariants, retention -------------------------------------------------------
-- name: CapturedOn :many
SELECT ref, amount_paise FROM payments WHERE status = 'CAPTURED' AND captured_at >= @day_start AND captured_at < @day_end;

-- name: AddReconIssue :exec
INSERT INTO recon_issues (id, day, kind, ref, detail) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING;

-- name: ReconIssues :many
SELECT * FROM recon_issues ORDER BY created_at DESC LIMIT 100;

-- name: LedgerMismatches :many
-- Accounts whose balance-moving ledger total differs from their available lots.
SELECT a.account_id, a.ledger::int AS ledger, coalesce(l.avail, 0)::int AS available FROM
  (SELECT account_id, sum(amount) AS ledger FROM ledger_entries
   WHERE type NOT IN ('EARN_PURCHASE', 'EARN_CREATOR_POOL', 'EARN_PROMOTER', 'REVERSAL') GROUP BY account_id) a
LEFT JOIN (SELECT account_id, sum(remaining) AS avail FROM electron_lots WHERE status IN ('AVAILABLE', 'EXHAUSTED') GROUP BY account_id) l
  ON l.account_id = a.account_id
WHERE a.ledger <> coalesce(l.avail, 0);

-- name: PurgeOTP :execrows
DELETE FROM otp_challenges WHERE created_at < now() - interval '30 days';

-- name: PurgeWebhooks :execrows
DELETE FROM webhook_events WHERE received_at < now() - interval '90 days' AND processed_at IS NOT NULL;

-- name: PurgeNotifications :execrows
DELETE FROM notifications WHERE read_at IS NOT NULL AND created_at < now() - interval '180 days';

-- name: PurgeRuns :execrows
DELETE FROM synthetic_runs WHERE at < now() - interval '35 days';

-- name: PurgeSessions :execrows
DELETE FROM sessions WHERE (revoked_at IS NOT NULL OR expires_at < now()) AND created_at < now() - interval '30 days';

-- name: PurgeOutbox :execrows
DELETE FROM outbox WHERE published_at IS NOT NULL AND published_at < now() - interval '30 days';
