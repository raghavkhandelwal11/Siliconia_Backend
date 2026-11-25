-- Seller onboarding and KYC (LLD 5.2; TRS FR-SEL-01, FR-ADM-01).

-- +goose Up
CREATE TABLE seller_profiles (
  account_id        uuid PRIMARY KEY REFERENCES accounts (id),
  business_type     text CHECK (business_type IN ('INDIVIDUAL', 'PROPRIETORSHIP', 'PARTNERSHIP', 'LLP', 'PRIVATE_LIMITED', 'PUBLIC_LIMITED')),
  legal_name        text NOT NULL DEFAULT '',
  store_name        text NOT NULL DEFAULT '',
  -- PAN and bank account are envelope-encrypted (AES-256-GCM, aad = account id + field).
  pan_enc           bytea,
  pan_last4         text,
  pan_verified_at   timestamptz,
  gstin             text,
  bank_enc          bytea,
  bank_last4        text,
  ifsc              text,
  account_holder    text,
  bank_verified_at  timestamptz,
  pickup_address    jsonb,
  terms_version     text,
  terms_accepted_at timestamptz,
  submitted_at      timestamptz,
  decided_at        timestamptz,
  decided_by        uuid REFERENCES accounts (id),
  is_platform       boolean NOT NULL DEFAULT false,
  verified_tag      boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  version           int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX ux_seller_store_name ON seller_profiles (lower(store_name)) WHERE store_name <> '';

CREATE TABLE kyc_records (
  id             uuid PRIMARY KEY,
  account_id     uuid NOT NULL REFERENCES accounts (id),
  type           text NOT NULL CHECK (type IN ('PAN', 'BANK')),
  status         text NOT NULL CHECK (status IN ('VERIFIED', 'FAILED', 'NAME_MISMATCH')),
  provider_ref   text NOT NULL,
  name_on_record text,
  name_match     real NOT NULL DEFAULT 0,
  reason         text,
  checked_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_kyc_account ON kyc_records (account_id, type, checked_at DESC);

-- +goose Down
DROP TABLE kyc_records;
DROP TABLE seller_profiles;
