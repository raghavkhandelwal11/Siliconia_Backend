-- Addresses, orders, sub-orders and items (LLD 5.6, 8.3; TRS FR-CHK, FR-ORD).

-- +goose Up
CREATE TABLE addresses (
  id         uuid PRIMARY KEY,
  account_id uuid NOT NULL REFERENCES accounts (id),
  name       text NOT NULL,
  phone      text NOT NULL,
  line1      text NOT NULL,
  line2      text NOT NULL DEFAULT '',
  city       text NOT NULL,
  state      text NOT NULL,
  pincode    text NOT NULL CHECK (pincode ~ '^[1-9][0-9]{5}$'),
  is_default boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_addresses_account ON addresses (account_id);
CREATE UNIQUE INDEX ux_addresses_default ON addresses (account_id) WHERE is_default;

CREATE TABLE orders (
  id                   uuid PRIMARY KEY,
  number               text NOT NULL UNIQUE,
  buyer_id             uuid NOT NULL REFERENCES accounts (id),
  address              jsonb NOT NULL, -- snapshot
  status               text NOT NULL CHECK (status IN ('PENDING_PAYMENT', 'CONFIRMED', 'EXPIRED', 'CLOSED')),
  item_total_paise     bigint NOT NULL,
  mrp_total_paise      bigint NOT NULL,
  shipping_paise       bigint NOT NULL DEFAULT 0,
  electrons_used       int NOT NULL DEFAULT 0,
  payable_paise        bigint NOT NULL,
  pay_method           text NOT NULL CHECK (pay_method IN ('ONLINE', 'COD', 'ELECTRONS_ONLY')),
  payment_id           uuid REFERENCES payments (id),
  is_member_at_order   boolean NOT NULL DEFAULT false,
  contains_launch_deal boolean NOT NULL DEFAULT false,
  rules_snapshot       jsonb NOT NULL,
  expires_at           timestamptz NOT NULL,
  placed_at            timestamptz NOT NULL DEFAULT now(),
  confirmed_at         timestamptz,
  is_synthetic         boolean NOT NULL DEFAULT false
);
CREATE INDEX ix_orders_buyer ON orders (buyer_id, placed_at DESC);
CREATE INDEX ix_orders_expiry ON orders (expires_at) WHERE status = 'PENDING_PAYMENT';

CREATE TYPE sub_status AS ENUM ('PENDING_PAYMENT', 'CONFIRMED', 'PACKED', 'SHIPPED', 'OUT_FOR_DELIVERY', 'DELIVERED', 'EXPIRED', 'CANCELLED', 'RTO');
CREATE TABLE sub_orders (
  id                    uuid PRIMARY KEY,
  order_id              uuid NOT NULL REFERENCES orders (id),
  seller_id             uuid NOT NULL REFERENCES accounts (id),
  status                sub_status NOT NULL,
  subtotal_paise        bigint NOT NULL,
  invoice_no            text UNIQUE,
  delivered_at          timestamptz,
  return_window_ends_at timestamptz,
  settled_at            timestamptz,
  version               int NOT NULL DEFAULT 1
);
CREATE INDEX ix_sub_orders_order ON sub_orders (order_id);
CREATE INDEX ix_sub_orders_seller ON sub_orders (seller_id, status);

CREATE TABLE order_items (
  id                uuid PRIMARY KEY,
  sub_order_id      uuid NOT NULL REFERENCES sub_orders (id),
  variant_id        uuid NOT NULL REFERENCES variants (id),
  lot_id            uuid NOT NULL REFERENCES inventory_lots (id),
  price_type        text NOT NULL DEFAULT 'NORMAL' CHECK (price_type IN ('NORMAL', 'LAUNCH_DEAL', 'LAUNCH_NORMAL')),
  launch_id         uuid,
  launch_claim_id   uuid,
  product_name      text NOT NULL, -- snapshots for invoices and history
  product_slug      text NOT NULL,
  variant_attrs     jsonb NOT NULL DEFAULT '{}',
  cover_key         text NOT NULL DEFAULT '',
  qty               int NOT NULL CHECK (qty > 0),
  unit_mrp_paise    bigint NOT NULL,
  unit_price_paise  bigint NOT NULL,
  line_total_paise  bigint NOT NULL,
  gst_rate_bps      int NOT NULL,
  hsn               text,
  commission_bps    int NOT NULL,
  electrons_spent   int NOT NULL DEFAULT 0,
  electrons_earned  int NOT NULL DEFAULT 0,
  status            text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CANCELLED', 'RETURNED', 'REPLACED'))
);
CREATE INDEX ix_order_items_sub ON order_items (sub_order_id);

ALTER TABLE payments ADD COLUMN checkout_url text;

-- +goose Down
ALTER TABLE payments DROP COLUMN checkout_url;
DROP TABLE order_items;
DROP TABLE sub_orders;
DROP TYPE sub_status;
DROP TABLE orders;
DROP TABLE addresses;
