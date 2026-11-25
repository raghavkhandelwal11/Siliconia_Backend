-- Cart and wishlist (LLD 5.4, 7.3; TRS FR-CHK-01, FR-CHK-10, FR-AUTH-09).

-- +goose Up
-- A cart belongs to a buyer account or, before sign-in, to a device cookie.
CREATE TABLE carts (
  id         uuid PRIMARY KEY,
  account_id uuid UNIQUE REFERENCES accounts (id),
  device_id  text UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((account_id IS NULL) <> (device_id IS NULL))
);
CREATE INDEX ix_carts_guest_age ON carts (updated_at) WHERE device_id IS NOT NULL;

CREATE TABLE cart_items (
  cart_id              uuid NOT NULL REFERENCES carts (id) ON DELETE CASCADE,
  variant_id           uuid NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
  qty                  int NOT NULL CHECK (qty BETWEEN 1 AND 10),
  price_at_add_paise   bigint NOT NULL,
  added_at             timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (cart_id, variant_id)
);

CREATE TABLE wishlist_items (
  account_id         uuid NOT NULL REFERENCES accounts (id),
  variant_id         uuid NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
  price_at_add_paise bigint NOT NULL,
  notify_stock       boolean NOT NULL DEFAULT true,
  notify_price       boolean NOT NULL DEFAULT true,
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, variant_id)
);

-- +goose Down
DROP TABLE wishlist_items;
DROP TABLE cart_items;
DROP TABLE carts;
