-- +goose Up
-- M3: Electrons ledger, Electra Membership and launch deals (LLD 5.2, 8.4, 8.7, 8.8).

-- Electrons ------------------------------------------------------------------
CREATE TYPE lot_status AS ENUM ('PENDING', 'AVAILABLE', 'EXHAUSTED', 'EXPIRED', 'REVERSED');
CREATE TABLE electron_lots (
  id           uuid PRIMARY KEY,
  account_id   uuid NOT NULL REFERENCES accounts (id),
  source       text NOT NULL CHECK (source IN ('PURCHASE', 'CREATOR_POOL', 'PROMOTER', 'REFUND_REISSUE', 'ADJUSTMENT')),
  source_ref   uuid NOT NULL, -- order item for purchases
  amount       int NOT NULL CHECK (amount > 0),
  remaining    int NOT NULL CHECK (remaining BETWEEN 0 AND amount),
  status       lot_status NOT NULL,
  available_at timestamptz,
  expires_at   timestamptz,
  is_synthetic boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_lots_fifo ON electron_lots (account_id, expires_at, id) WHERE status = 'AVAILABLE' AND remaining > 0;
CREATE INDEX ix_lots_mature ON electron_lots (available_at) WHERE status = 'PENDING' AND available_at IS NOT NULL;
CREATE INDEX ix_lots_source ON electron_lots (source_ref);

-- Append-only: amounts are signed. EARN and REVERSAL on pending lots are
-- informational; every other type moves the available balance, so
-- sum(amount) over those types = available (checked in tests and reports).
CREATE TABLE ledger_entries (
  id         uuid PRIMARY KEY,
  account_id uuid NOT NULL REFERENCES accounts (id),
  type       text NOT NULL CHECK (type IN ('EARN_PURCHASE', 'EARN_CREATOR_POOL', 'EARN_PROMOTER', 'MATURE', 'SPEND_ORDER',
                                           'SPEND_MEMBERSHIP', 'RESTORE', 'REVERSAL', 'EXPIRY', 'CASHOUT_HOLD', 'CASHOUT_PAID',
                                           'CASHOUT_RELEASE', 'ADMIN_ADJUSTMENT')),
  amount     int NOT NULL,
  lot_id     uuid,
  ref_type   text NOT NULL,
  ref_id     uuid NOT NULL,
  note       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_ledger_account ON ledger_entries (account_id, created_at DESC);
-- +goose StatementBegin
CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger_entries is append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_ledger_append_only BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION ledger_append_only();

CREATE TABLE spend_allocations (
  ledger_entry_id uuid NOT NULL REFERENCES ledger_entries (id),
  lot_id          uuid NOT NULL REFERENCES electron_lots (id),
  amount          int NOT NULL CHECK (amount > 0),
  PRIMARY KEY (ledger_entry_id, lot_id)
);

-- Membership -----------------------------------------------------------------
CREATE TABLE memberships (
  id                 uuid PRIMARY KEY,
  account_id         uuid NOT NULL UNIQUE REFERENCES accounts (id),
  status             text NOT NULL CHECK (status IN ('ACTIVE', 'LAPSED')),
  started_at         timestamptz NOT NULL,
  expires_at         timestamptz NOT NULL,
  auto_renew         boolean NOT NULL DEFAULT true,
  mandate_id         text,
  renew_attempts     int NOT NULL DEFAULT 0,
  predebit_sent_for  timestamptz, -- the expiry a pre-debit notice went out for
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_memberships_due ON memberships (expires_at) WHERE status = 'ACTIVE';

CREATE TABLE membership_purchases (
  id             uuid PRIMARY KEY,
  ref            text NOT NULL UNIQUE,
  account_id     uuid NOT NULL REFERENCES accounts (id),
  kind           text NOT NULL CHECK (kind IN ('PURCHASE', 'RENEWAL')),
  electrons_used int NOT NULL DEFAULT 0,
  amount_paise   bigint NOT NULL,
  auto_renew     boolean NOT NULL,
  status         text NOT NULL CHECK (status IN ('PENDING', 'PAID', 'FAILED', 'EXPIRED')),
  payment_id     uuid REFERENCES payments (id),
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_membership_purchases_pending ON membership_purchases (created_at) WHERE status = 'PENDING';

-- Launch deals ---------------------------------------------------------------
ALTER TYPE lot_kind ADD VALUE IF NOT EXISTS 'LAUNCH_DEAL';
CREATE TYPE launch_status AS ENUM ('DRAFT', 'SCHEDULED', 'LIVE', 'DEAL_SOLD_OUT', 'NORMAL_ON_SALE', 'COMPLETED', 'CANCELLED');
CREATE TABLE launches (
  id                 uuid PRIMARY KEY,
  product_id         uuid NOT NULL REFERENCES products (id),
  variant_id         uuid NOT NULL REFERENCES variants (id),
  seller_id          uuid NOT NULL REFERENCES accounts (id), -- platform-owned stock (D-10)
  units_bought       int NOT NULL CHECK (units_bought > 0),
  unit_cost_paise    bigint NOT NULL CHECK (unit_cost_paise > 0),
  normal_price_paise bigint NOT NULL CHECK (normal_price_paise > 0),
  deal_discount_bps  int NOT NULL CHECK (deal_discount_bps BETWEEN 1 AND 9999),
  deal_price_paise   bigint NOT NULL CHECK (deal_price_paise > 0),
  min_profit_bps     int NOT NULL CHECK (min_profit_bps >= 0),
  deal_units         int NOT NULL CHECK (deal_units BETWEEN 1 AND units_bought),
  deal_remaining     int NOT NULL CHECK (deal_remaining BETWEEN 0 AND deal_units), -- never oversell (FR-LAU-07)
  per_member_limit   int NOT NULL CHECK (per_member_limit >= 1),
  starts_at          timestamptz NOT NULL,
  normal_sale_mode   text NOT NULL CHECK (normal_sale_mode IN ('IMMEDIATE', 'SCHEDULED')),
  normal_sale_at     timestamptz,
  waiting_room_rps   int CHECK (waiting_room_rps > 0),
  deal_lot_id        uuid REFERENCES inventory_lots (id),
  normal_lot_id      uuid REFERENCES inventory_lots (id),
  status             launch_status NOT NULL DEFAULT 'DRAFT',
  created_by         uuid NOT NULL REFERENCES accounts (id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  version            int NOT NULL DEFAULT 1
);
CREATE INDEX ix_launches_status ON launches (status, starts_at);
CREATE UNIQUE INDEX ux_launch_variant_active ON launches (variant_id) WHERE status IN ('SCHEDULED', 'LIVE', 'DEAL_SOLD_OUT', 'NORMAL_ON_SALE');

CREATE TABLE launch_member_counters (
  launch_id  uuid NOT NULL REFERENCES launches (id),
  account_id uuid NOT NULL REFERENCES accounts (id),
  claimed    int NOT NULL DEFAULT 0 CHECK (claimed >= 0),
  PRIMARY KEY (launch_id, account_id)
);

CREATE TABLE launch_claims (
  id         uuid PRIMARY KEY,
  launch_id  uuid NOT NULL REFERENCES launches (id),
  account_id uuid NOT NULL REFERENCES accounts (id),
  qty        int NOT NULL CHECK (qty > 0),
  status     text NOT NULL CHECK (status IN ('HELD', 'ORDERED', 'PAID', 'RELEASED')),
  order_id   uuid REFERENCES orders (id),
  held_until timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_claims_expiry ON launch_claims (held_until) WHERE status IN ('HELD', 'ORDERED');
CREATE INDEX ix_claims_account ON launch_claims (account_id, launch_id);

-- The demo store is the platform seller that holds launch stock.
UPDATE seller_profiles SET is_platform = true WHERE store_name = 'Siliconics Demo Store';

-- +goose Down
DROP TABLE launch_claims;
DROP TABLE launch_member_counters;
DROP TABLE launches;
DROP TYPE launch_status;
DROP TABLE membership_purchases;
DROP TABLE memberships;
DROP TABLE spend_allocations;
DROP TRIGGER trg_ledger_append_only ON ledger_entries;
DROP FUNCTION ledger_append_only();
DROP TABLE ledger_entries;
DROP TABLE electron_lots;
DROP TYPE lot_status;
