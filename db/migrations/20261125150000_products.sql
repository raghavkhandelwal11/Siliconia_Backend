-- Products, variants, stock and images (LLD 5.3; TRS FR-SEL-02, FR-CAT).

-- +goose Up
-- Files uploaded by an account; products can only use their owner's uploads.
CREATE TABLE uploads (
  id           uuid PRIMARY KEY,
  owner_id     uuid NOT NULL REFERENCES accounts (id),
  purpose      text NOT NULL CHECK (purpose IN ('PRODUCT_IMAGE')),
  storage_key  text NOT NULL UNIQUE,
  content_type text NOT NULL,
  bytes        int NOT NULL,
  width        int NOT NULL,
  height       int NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_uploads_owner ON uploads (owner_id, created_at DESC);

CREATE TABLE products (
  id             uuid PRIMARY KEY,
  seller_id      uuid NOT NULL REFERENCES seller_profiles (account_id),
  category_id    uuid NOT NULL REFERENCES categories (id),
  slug           text NOT NULL UNIQUE,
  name           text NOT NULL,
  brand          text NOT NULL DEFAULT '',
  model          text NOT NULL DEFAULT '',
  description    text NOT NULL DEFAULT '',
  warranty       text NOT NULL DEFAULT '',
  origin_country char(2) NOT NULL DEFAULT 'IN',
  is_local       boolean NOT NULL DEFAULT false,
  specs          jsonb NOT NULL DEFAULT '{}',
  status         text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'HIDDEN')),
  search         tsvector GENERATED ALWAYS AS (to_tsvector('english', name || ' ' || model || ' ' || brand)) STORED,
  rating_avg     numeric(3, 2) NOT NULL DEFAULT 0,
  rating_count   int NOT NULL DEFAULT 0,
  is_synthetic   boolean NOT NULL DEFAULT false,
  published_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  version        int NOT NULL DEFAULT 1
);
CREATE INDEX ix_products_seller ON products (seller_id, updated_at DESC);
CREATE INDEX ix_products_category ON products (category_id) WHERE status = 'ACTIVE';
CREATE INDEX ix_products_search ON products USING gin (search);
CREATE INDEX ix_products_name_trgm ON products USING gin (name gin_trgm_ops);

CREATE TABLE variants (
  id          uuid PRIMARY KEY,
  product_id  uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
  sku         text NOT NULL,
  attrs       jsonb NOT NULL DEFAULT '{}',
  mrp_paise   bigint NOT NULL,
  price_paise bigint NOT NULL,
  position    int NOT NULL DEFAULT 0,
  UNIQUE (product_id, sku),
  CHECK (price_paise > 0 AND price_paise <= mrp_paise AND mrp_paise <= 100000000000)
);

-- Seller stock; launch stock (LAUNCH_NORMAL) arrives with launches (M3).
CREATE TYPE lot_kind AS ENUM ('SELLER', 'LAUNCH_NORMAL');
CREATE TABLE inventory_lots (
  id              uuid PRIMARY KEY,
  variant_id      uuid NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
  owner_seller_id uuid NOT NULL,
  kind            lot_kind NOT NULL,
  launch_id       uuid,
  quantity        int NOT NULL,
  reserved        int NOT NULL DEFAULT 0,
  sold            int NOT NULL DEFAULT 0,
  unit_cost_paise bigint,
  CHECK (quantity >= 0 AND reserved >= 0 AND sold >= 0 AND reserved + sold <= quantity)
);
CREATE UNIQUE INDEX ux_lot_seller_variant ON inventory_lots (variant_id) WHERE kind = 'SELLER';

CREATE TABLE product_images (
  product_id uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
  upload_id  uuid NOT NULL REFERENCES uploads (id),
  position   int NOT NULL,
  alt        text NOT NULL DEFAULT '',
  PRIMARY KEY (product_id, position),
  UNIQUE (product_id, upload_id)
);

-- +goose Down
DROP TABLE product_images;
DROP TABLE inventory_lots;
DROP TYPE lot_kind;
DROP TABLE variants;
DROP TABLE products;
DROP TABLE uploads;
