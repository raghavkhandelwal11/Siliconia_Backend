-- +goose Up
-- M4: reviews, creators (demos, reviews, reward pools, cash-out), promoters,
-- discovery (LLD 5.3, 8.9, 8.10, 9.4).
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('PRODUCT_IMAGE', 'REVIEW_IMAGE'));

-- Buyer reviews ----------------------------------------------------------------
CREATE TABLE reviews (
  id            uuid PRIMARY KEY,
  order_item_id uuid NOT NULL UNIQUE REFERENCES order_items (id),
  product_id    uuid NOT NULL REFERENCES products (id),
  account_id    uuid NOT NULL REFERENCES accounts (id),
  rating        int NOT NULL CHECK (rating BETWEEN 1 AND 5),
  title         text NOT NULL DEFAULT '',
  body          text NOT NULL DEFAULT '',
  status        text NOT NULL DEFAULT 'PUBLISHED' CHECK (status IN ('PUBLISHED', 'HIDDEN')),
  helpful_count int NOT NULL DEFAULT 0,
  report_count  int NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_reviews_product ON reviews (product_id, status, created_at DESC);
CREATE TABLE review_photos (
  review_id uuid NOT NULL REFERENCES reviews (id) ON DELETE CASCADE,
  upload_id uuid NOT NULL REFERENCES uploads (id),
  position  int NOT NULL,
  PRIMARY KEY (review_id, position)
);
CREATE TABLE helpful_votes (
  review_id  uuid NOT NULL REFERENCES reviews (id) ON DELETE CASCADE,
  account_id uuid NOT NULL REFERENCES accounts (id),
  PRIMARY KEY (review_id, account_id)
);
CREATE TABLE review_reports (
  review_id  uuid NOT NULL REFERENCES reviews (id) ON DELETE CASCADE,
  account_id uuid NOT NULL REFERENCES accounts (id),
  reason     text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (review_id, account_id)
);

-- Creators ---------------------------------------------------------------------
CREATE TABLE creator_profiles (
  account_id    uuid PRIMARY KEY REFERENCES accounts (id),
  niche         text NOT NULL,
  bio           text NOT NULL DEFAULT '',
  terms_version text,
  pan_enc       bytea,
  pan_last4     text,
  bank_enc      bytea,
  bank_last4    text,
  kyc_verified_at timestamptz,
  submitted_at  timestamptz,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE creator_socials (
  account_id   uuid NOT NULL REFERENCES creator_profiles (account_id) ON DELETE CASCADE,
  platform     text NOT NULL CHECK (platform IN ('YOUTUBE', 'INSTAGRAM', 'X', 'OTHER')),
  url          text NOT NULL,
  followers    int NOT NULL CHECK (followers >= 0),
  PRIMARY KEY (account_id, platform)
);

CREATE TABLE demo_requests (
  id          uuid PRIMARY KEY,
  creator_id  uuid NOT NULL REFERENCES accounts (id),
  product_id  uuid NOT NULL REFERENCES products (id),
  seller_id   uuid NOT NULL REFERENCES accounts (id),
  status      text NOT NULL CHECK (status IN ('REQUESTED', 'APPROVED', 'DECLINED', 'SHIPPED', 'DELIVERED', 'REVIEWED', 'OVERDUE')),
  note        text NOT NULL DEFAULT '',
  decline_reason text,
  shipment_id uuid REFERENCES shipments (id),
  due_at      timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ux_demo_open ON demo_requests (creator_id, product_id) WHERE status NOT IN ('DECLINED');

-- Demo units ship to creators without an order.
ALTER TABLE shipments ALTER COLUMN sub_order_id DROP NOT NULL;
ALTER TABLE shipments ADD COLUMN demo_request_id uuid REFERENCES demo_requests (id);
ALTER TABLE shipments ADD CONSTRAINT shipments_owner CHECK (sub_order_id IS NOT NULL OR demo_request_id IS NOT NULL);

CREATE TABLE creator_reviews (
  id           uuid PRIMARY KEY,
  creator_id   uuid NOT NULL REFERENCES accounts (id),
  product_id   uuid NOT NULL REFERENCES products (id),
  rating       int NOT NULL CHECK (rating BETWEEN 1 AND 5),
  body         text NOT NULL,
  media        jsonb NOT NULL DEFAULT '[]',
  status       text NOT NULL DEFAULT 'PUBLISHED' CHECK (status IN ('PUBLISHED', 'HIDDEN')),
  rank         int NOT NULL,       -- 1 = first to publish for the product
  published_at timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (creator_id, product_id)
);
CREATE TABLE creator_votes (
  creator_review_id uuid NOT NULL REFERENCES creator_reviews (id) ON DELETE CASCADE,
  buyer_id          uuid NOT NULL REFERENCES accounts (id),
  value             int NOT NULL CHECK (value IN (-1, 1)),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (creator_review_id, buyer_id)
);

CREATE TABLE reward_pools (
  id          uuid PRIMARY KEY,
  product_id  uuid NOT NULL REFERENCES products (id),
  period      text NOT NULL CHECK (period ~ '^[0-9]{4}-[0-9]{2}$'),
  size        int NOT NULL CHECK (size >= 0), -- Electrons (Q-02: set by admins)
  funded_by   text NOT NULL DEFAULT 'PLATFORM',
  settled_at  timestamptz,
  UNIQUE (product_id, period)
);
CREATE TABLE pool_settlements (
  pool_id         uuid NOT NULL REFERENCES reward_pools (id),
  creator_id      uuid NOT NULL REFERENCES accounts (id),
  verified_agrees int NOT NULL,
  weight          int NOT NULL,
  score           int NOT NULL,
  share           int NOT NULL,
  lot_id          uuid REFERENCES electron_lots (id),
  PRIMARY KEY (pool_id, creator_id)
);

CREATE TABLE cashout_requests (
  id          uuid PRIMARY KEY,
  creator_id  uuid NOT NULL REFERENCES accounts (id),
  electrons   int NOT NULL CHECK (electrons >= 250),
  status      text NOT NULL CHECK (status IN ('REQUESTED', 'IN_BATCH', 'PAID', 'FAILED', 'CANCELLED')),
  provider_ref text,
  bank_ref    text,
  failure_reason text,
  batch_id    uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_cashouts_status ON cashout_requests (status, created_at);

-- Promoters --------------------------------------------------------------------
CREATE TABLE promoter_profiles (
  account_id uuid PRIMARY KEY REFERENCES accounts (id),
  type       text NOT NULL CHECK (type IN ('INDIVIDUAL', 'COMPANY')),
  display_name text NOT NULL,
  company    jsonb,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE promoter_investments (
  id                  uuid PRIMARY KEY,
  promoter_id         uuid NOT NULL REFERENCES accounts (id),
  seller_id           uuid NOT NULL REFERENCES accounts (id),
  proof_note          text NOT NULL,
  seller_confirmed_at timestamptz,
  declined_at         timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (promoter_id, seller_id)
);
CREATE TABLE promotions (
  promoter_id uuid NOT NULL REFERENCES accounts (id),
  product_id  uuid NOT NULL REFERENCES products (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (promoter_id, product_id)
);
CREATE TABLE promoter_reward_rules (
  product_id uuid PRIMARY KEY REFERENCES products (id),
  type       text NOT NULL CHECK (type IN ('ELECTRONS_PER_UNIT')), -- profit share waits for Q-03
  value      int NOT NULL CHECK (value BETWEEN 0 AND 100000)
);

-- Discovery ----------------------------------------------------------------------
CREATE TABLE products_of_week (
  week_start date NOT NULL,
  product_id uuid NOT NULL REFERENCES products (id),
  rank       int NOT NULL,
  score      numeric NOT NULL,
  PRIMARY KEY (week_start, product_id)
);
CREATE TABLE product_feedback (
  id          uuid PRIMARY KEY,
  product_id  uuid NOT NULL REFERENCES products (id),
  buyer_id    uuid NOT NULL REFERENCES accounts (id),
  category    text NOT NULL CHECK (category IN ('QUALITY', 'FEATURE', 'PACKAGING', 'DOCUMENTATION', 'OTHER')),
  body        text NOT NULL,
  seller_reply text,
  replied_at  timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_feedback_product ON product_feedback (product_id, created_at DESC);

-- +goose Down
ALTER TABLE shipments DROP CONSTRAINT shipments_owner;
ALTER TABLE shipments DROP COLUMN demo_request_id;
DROP TABLE product_feedback;
DROP TABLE products_of_week;
DROP TABLE promoter_reward_rules;
DROP TABLE promotions;
DROP TABLE promoter_investments;
DROP TABLE promoter_profiles;
DROP TABLE cashout_requests;
DROP TABLE pool_settlements;
DROP TABLE reward_pools;
DROP TABLE creator_votes;
DROP TABLE creator_reviews;
DROP TABLE demo_requests;
DROP TABLE creator_socials;
DROP TABLE creator_profiles;
DROP TABLE review_reports;
DROP TABLE helpful_votes;
DROP TABLE review_photos;
DROP TABLE reviews;
