-- Accounts, sessions and OTP challenges (LLD 4.2, 5.2; TRS FR-ROLE, FR-AUTH).

-- +goose Up
CREATE TYPE account_role AS ENUM ('BUYER', 'SELLER', 'PROMOTER', 'CREATOR', 'ADMIN');
CREATE TYPE approval_status AS ENUM ('DRAFT', 'SUBMITTED', 'UNDER_REVIEW', 'APPROVED', 'REJECTED', 'SUSPENDED');

CREATE TABLE accounts (
  id                uuid PRIMARY KEY,
  role              account_role NOT NULL,
  phone_e164        text,
  phone_verified_at timestamptz,
  google_sub        text,
  email             citext,
  name              text NOT NULL DEFAULT '',
  status            text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DELETED')),
  approval          approval_status NOT NULL,
  approval_reason   text,
  totp_secret_enc   bytea,
  totp_confirmed_at timestamptz,
  is_synthetic      boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  version           int NOT NULL DEFAULT 1,
  CHECK (phone_e164 IS NOT NULL OR google_sub IS NOT NULL)
);
-- One account per (identity, role) (FR-ROLE-02).
CREATE UNIQUE INDEX ux_acc_phone_role ON accounts (phone_e164, role) WHERE phone_e164 IS NOT NULL AND status <> 'DELETED';
CREATE UNIQUE INDEX ux_acc_google_role ON accounts (google_sub, role) WHERE google_sub IS NOT NULL AND status <> 'DELETED';

-- At most 3 active admins, enforced in the database (FR-ROLE-06).
-- +goose StatementBegin
CREATE FUNCTION enforce_admin_limit() RETURNS trigger AS $$
BEGIN
  IF NEW.role = 'ADMIN' AND NEW.status = 'ACTIVE' THEN
    PERFORM pg_advisory_xact_lock(hashtext('admin_limit'));
    IF (SELECT count(*) FROM accounts WHERE role = 'ADMIN' AND status = 'ACTIVE' AND id <> NEW.id) >= 3 THEN
      RAISE EXCEPTION 'ADMIN_LIMIT_REACHED' USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_admin_limit
  BEFORE INSERT OR UPDATE OF role, status ON accounts
  FOR EACH ROW EXECUTE FUNCTION enforce_admin_limit();

CREATE TABLE sessions (
  id           uuid PRIMARY KEY,
  account_id   uuid NOT NULL REFERENCES accounts (id),
  family_id    uuid NOT NULL,
  refresh_hash bytea NOT NULL UNIQUE,
  prev_refresh_hash bytea,
  user_agent   text NOT NULL DEFAULT '',
  ip           inet,
  mfa_at       timestamptz,
  acting_role  account_role,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  revoked_at   timestamptz
);
CREATE INDEX ix_sessions_account ON sessions (account_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_sessions_family ON sessions (family_id);
CREATE INDEX ix_sessions_prev_hash ON sessions (prev_refresh_hash) WHERE prev_refresh_hash IS NOT NULL;

CREATE TABLE otp_challenges (
  id           uuid PRIMARY KEY,
  phone_e164   text NOT NULL,
  role         account_role,
  purpose      text NOT NULL CHECK (purpose IN ('SIGN_IN', 'VERIFY_PHONE')),
  account_id   uuid REFERENCES accounts (id),
  code_hash    bytea NOT NULL,
  attempts     int NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  consumed_at  timestamptz
);
CREATE INDEX ix_otp_created ON otp_challenges (created_at);

-- +goose Down
DROP TABLE otp_challenges;
DROP TABLE sessions;
DROP TRIGGER trg_admin_limit ON accounts;
DROP FUNCTION enforce_admin_limit();
DROP TABLE accounts;
DROP TYPE approval_status;
DROP TYPE account_role;
