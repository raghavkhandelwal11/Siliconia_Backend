-- Payments and refunds (LLD 5.5, 8.3; TRS FR-PAY). Orders arrive in T2.3 and
-- link here through payments.ref.

-- +goose Up
CREATE TYPE pay_method AS ENUM ('UPI', 'CARD', 'NETBANKING', 'COD', 'ELECTRONS', 'MANDATE');

CREATE TABLE payments (
  id                  uuid PRIMARY KEY,
  purpose             text NOT NULL CHECK (purpose IN ('ORDER', 'MEMBERSHIP', 'RENEWAL')),
  ref                 text NOT NULL UNIQUE,  -- our reference sent to the provider (order number etc.)
  account_id          uuid REFERENCES accounts (id),
  provider            text NOT NULL,
  provider_order_id   text UNIQUE,
  provider_payment_id text UNIQUE,
  method              pay_method,
  amount_paise        bigint NOT NULL CHECK (amount_paise > 0),
  status              text NOT NULL DEFAULT 'CREATED' CHECK (status IN ('CREATED', 'PENDING', 'CAPTURED', 'FAILED', 'EXPIRED')),
  failure_code        text,
  mandate_id          text,
  captured_at         timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_payments_account ON payments (account_id, created_at DESC);

CREATE TABLE refunds (
  id                 uuid PRIMARY KEY,
  payment_id         uuid NOT NULL REFERENCES payments (id),
  ref                text NOT NULL UNIQUE,
  amount_paise       bigint NOT NULL CHECK (amount_paise > 0),
  electrons          int NOT NULL DEFAULT 0,
  status             text NOT NULL DEFAULT 'REQUESTED' CHECK (status IN ('REQUESTED', 'PROCESSING', 'REFUNDED', 'FAILED')),
  provider_refund_id text UNIQUE,
  reason             text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE refunds;
DROP TABLE payments;
DROP TYPE pay_method;
