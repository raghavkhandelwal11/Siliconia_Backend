-- +goose Up
-- M5: seller payouts, config history, legal documents, ledger adjustments,
-- health monitoring and reconciliation (LLD 5.3, 8.10-8.12).
CREATE TABLE payout_batches (
  id          uuid PRIMARY KEY,
  week_ending date NOT NULL,
  status      text NOT NULL CHECK (status IN ('DRAFT', 'APPROVED', 'RELEASED', 'CANCELLED')),
  approved_by uuid REFERENCES accounts (id),
  released_by uuid REFERENCES accounts (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE payouts (
  id               uuid PRIMARY KEY,
  batch_id         uuid NOT NULL REFERENCES payout_batches (id),
  seller_id        uuid NOT NULL REFERENCES accounts (id),
  gross_paise      bigint NOT NULL,
  commission_paise bigint NOT NULL,
  tds_paise        bigint NOT NULL DEFAULT 0,
  net_paise        bigint NOT NULL,
  status           text NOT NULL CHECK (status IN ('DRAFT', 'PROCESSING', 'PAID', 'FAILED', 'CANCELLED')),
  provider_ref     text UNIQUE,
  bank_ref         text,
  failure_reason   text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_payouts_seller ON payouts (seller_id, created_at DESC);
CREATE TABLE payout_lines (
  payout_id        uuid NOT NULL REFERENCES payouts (id) ON DELETE CASCADE,
  sub_order_id     uuid NOT NULL REFERENCES sub_orders (id),
  gross_paise      bigint NOT NULL,
  commission_paise bigint NOT NULL,
  PRIMARY KEY (payout_id, sub_order_id)
);
-- A sub-order is in at most one live payout.
CREATE TABLE payout_claims (
  sub_order_id uuid PRIMARY KEY REFERENCES sub_orders (id),
  payout_id    uuid NOT NULL REFERENCES payouts (id) ON DELETE CASCADE
);
ALTER TABLE seller_profiles ADD COLUMN IF NOT EXISTS rolling_12m_sales_paise bigint NOT NULL DEFAULT 0;

CREATE TABLE legal_docs (
  id           uuid PRIMARY KEY,
  kind         text NOT NULL CHECK (kind IN ('TERMS', 'PRIVACY', 'SELLER_TERMS', 'CREATOR_TERMS', 'RETURNS')),
  version      int NOT NULL,
  title        text NOT NULL,
  body_md      text NOT NULL,
  published_at timestamptz NOT NULL DEFAULT now(),
  published_by uuid REFERENCES accounts (id),
  UNIQUE (kind, version)
);
CREATE TABLE legal_acceptances (
  account_id  uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  doc_id      uuid NOT NULL REFERENCES legal_docs (id),
  accepted_at timestamptz NOT NULL DEFAULT now(),
  ip          inet,
  PRIMARY KEY (account_id, doc_id)
);

CREATE TABLE ledger_adjustment_requests (
  id           uuid PRIMARY KEY,
  account_id   uuid NOT NULL REFERENCES accounts (id),
  amount       int NOT NULL CHECK (amount <> 0),
  reason       text NOT NULL,
  requested_by uuid NOT NULL REFERENCES accounts (id),
  approved_by  uuid REFERENCES accounts (id),
  status       text NOT NULL CHECK (status IN ('REQUESTED', 'APPLIED', 'REJECTED')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  decided_at   timestamptz,
  CHECK (approved_by IS NULL OR approved_by <> requested_by) -- four-eyes rule
);

CREATE TABLE synthetic_runs (
  id          uuid PRIMARY KEY,
  check_name  text NOT NULL,
  ok          boolean NOT NULL,
  latency_ms  int NOT NULL,
  error       text,
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_synthetic_runs ON synthetic_runs (check_name, at DESC);
CREATE TABLE incidents (
  id          uuid PRIMARY KEY,
  check_name  text NOT NULL,
  opened_at   timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  last_error  text
);
CREATE UNIQUE INDEX ux_incident_open ON incidents (check_name) WHERE resolved_at IS NULL;

CREATE TABLE recon_issues (
  id         uuid PRIMARY KEY,
  day        date NOT NULL,
  kind       text NOT NULL,
  ref        text NOT NULL,
  detail     text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (day, kind, ref)
);

INSERT INTO legal_docs (id, kind, version, title, body_md) VALUES
  (gen_random_uuid(), 'TERMS', 1, 'Terms of use', 'Siliconics prototype terms of use. This is test data for the prototype.'),
  (gen_random_uuid(), 'PRIVACY', 1, 'Privacy policy', 'How the Siliconics prototype handles personal data (DPDP Act 2023).');

-- +goose Down
DROP TABLE recon_issues;
DROP TABLE incidents;
DROP TABLE synthetic_runs;
DROP TABLE ledger_adjustment_requests;
DROP TABLE legal_acceptances;
DROP TABLE legal_docs;
DROP TABLE payout_claims;
DROP TABLE payout_lines;
DROP TABLE payouts;
DROP TABLE payout_batches;
ALTER TABLE seller_profiles DROP COLUMN IF EXISTS rolling_12m_sales_paise;
