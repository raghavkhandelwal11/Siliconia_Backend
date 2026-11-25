-- name: GetSellerProfile :one
SELECT * FROM seller_profiles WHERE account_id = $1;

-- name: EnsureSellerProfile :one
INSERT INTO seller_profiles (account_id) VALUES ($1)
ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
RETURNING *;

-- name: UpdateSellerBusiness :one
UPDATE seller_profiles
SET business_type = $2, legal_name = $3, store_name = $4, gstin = $5, pickup_address = $6,
    -- A new legal name must be checked again against PAN and bank records.
    pan_verified_at  = CASE WHEN legal_name = $3 THEN pan_verified_at END,
    bank_verified_at = CASE WHEN legal_name = $3 THEN bank_verified_at END,
    updated_at = now(), version = version + 1
WHERE account_id = $1
RETURNING *;

-- name: SetSellerPAN :one
UPDATE seller_profiles
SET pan_enc = $2, pan_last4 = $3, pan_verified_at = $4, updated_at = now(), version = version + 1
WHERE account_id = $1
RETURNING *;

-- name: SetSellerBank :one
UPDATE seller_profiles
SET bank_enc = $2, bank_last4 = $3, ifsc = $4, account_holder = $5, bank_verified_at = $6,
    updated_at = now(), version = version + 1
WHERE account_id = $1
RETURNING *;

-- name: SubmitSellerApplication :one
UPDATE seller_profiles
SET terms_version = $2, terms_accepted_at = now(), submitted_at = now(), updated_at = now(), version = version + 1
WHERE account_id = $1
RETURNING *;

-- name: DecideSellerApplication :exec
UPDATE seller_profiles SET decided_at = now(), decided_by = $2, updated_at = now(), version = version + 1
WHERE account_id = $1;

-- name: InsertKYCRecord :one
INSERT INTO kyc_records (id, account_id, type, status, provider_ref, name_on_record, name_match, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: LatestKYCRecords :many
SELECT DISTINCT ON (type) * FROM kyc_records WHERE account_id = $1 ORDER BY type, checked_at DESC;

-- name: SetApproval :one
-- Moves an account between approval states; the WHERE guards the allowed from-states.
UPDATE accounts SET approval = $2, approval_reason = $3, version = version + 1
WHERE id = $1 AND approval::text = ANY(@from_states::text[])
RETURNING *;

-- name: ListSellerApplications :many
SELECT a.id, a.phone_e164, a.approval, a.approval_reason, a.created_at,
       p.business_type, p.legal_name, p.store_name, p.submitted_at, p.decided_at,
       p.pan_verified_at, p.bank_verified_at, p.gstin
FROM accounts a JOIN seller_profiles p ON p.account_id = a.id
WHERE a.role = 'SELLER' AND a.status = 'ACTIVE' AND a.approval::text = ANY(@states::text[])
ORDER BY p.submitted_at NULLS LAST, a.created_at
LIMIT 200;
