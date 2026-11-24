-- name: GetAccount :one
SELECT * FROM accounts WHERE id = $1;

-- name: GetAccountByPhoneRole :one
SELECT * FROM accounts WHERE phone_e164 = $1 AND role = $2 AND status <> 'DELETED';

-- name: GetAccountByGoogleRole :one
SELECT * FROM accounts WHERE google_sub = $1 AND role = $2 AND status <> 'DELETED';

-- name: CreateAccount :one
INSERT INTO accounts (id, role, phone_e164, phone_verified_at, google_sub, email, name, approval)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateProfile :one
UPDATE accounts SET name = $2, email = $3, version = version + 1
WHERE id = $1 AND status = 'ACTIVE'
RETURNING *;

-- name: SetPhoneVerified :one
UPDATE accounts SET phone_e164 = $2, phone_verified_at = now(), version = version + 1
WHERE id = $1
RETURNING *;

-- name: SetTOTPSecret :exec
UPDATE accounts SET totp_secret_enc = $2, totp_confirmed_at = NULL, version = version + 1
WHERE id = $1 AND totp_confirmed_at IS NULL;

-- name: ConfirmTOTP :exec
UPDATE accounts SET totp_confirmed_at = now() WHERE id = $1 AND totp_confirmed_at IS NULL;

-- name: TouchSession :exec
UPDATE sessions SET last_used_at = now() WHERE id = $1;

-- name: CountActiveAdmins :one
SELECT count(*) FROM accounts WHERE role = 'ADMIN' AND status = 'ACTIVE';

-- name: CreateSession :one
INSERT INTO sessions (id, account_id, family_id, refresh_hash, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetSessionByRefreshHash :one
SELECT * FROM sessions WHERE refresh_hash = $1;

-- name: GetSessionByPrevHash :one
SELECT * FROM sessions WHERE prev_refresh_hash = $1;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: RotateSession :one
-- Replaces the refresh token of a live session; returns no row if it was used or revoked meanwhile.
UPDATE sessions SET prev_refresh_hash = refresh_hash, refresh_hash = $3, last_used_at = now()
WHERE id = $1 AND refresh_hash = $2 AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL;

-- name: RevokeFamily :many
UPDATE sessions SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL RETURNING id;

-- name: ListActiveSessions :many
SELECT * FROM sessions
WHERE account_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_used_at DESC;

-- name: SetSessionMFA :exec
UPDATE sessions SET mfa_at = now() WHERE id = $1;

-- name: SetSessionActingRole :exec
UPDATE sessions SET acting_role = $2 WHERE id = $1;

-- name: CreateOTPChallenge :one
INSERT INTO otp_challenges (id, phone_e164, role, purpose, account_id, code_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetOTPChallengeForUpdate :one
SELECT * FROM otp_challenges WHERE id = $1 FOR UPDATE;

-- name: IncrementOTPAttempts :exec
UPDATE otp_challenges SET attempts = attempts + 1 WHERE id = $1;

-- name: ConsumeOTPChallenge :exec
UPDATE otp_challenges SET consumed_at = now() WHERE id = $1;

-- name: InsertAudit :exec
INSERT INTO audit_log (id, actor_id, acting_as_role, action, target_type, target_id, before, after, ip)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
