-- Category tree with per-sub-category spec schemas (LLD 5.3; TRS FR-CAT-01..04).

-- +goose Up
CREATE TABLE categories (
  id            uuid PRIMARY KEY,
  parent_id     uuid REFERENCES categories (id),
  slug          text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
  name          text NOT NULL,
  icon          text NOT NULL DEFAULT '',
  examples      text NOT NULL DEFAULT '',
  position      int NOT NULL DEFAULT 0,
  -- {"fields":[{key,label,type,unit,options,required,filter,main}]}; only sub-categories hold fields.
  spec_schema   jsonb NOT NULL DEFAULT '{"fields":[]}',
  -- NULL = platform default (config "commission.default_bps"); agreed per category later (TRS Q-01).
  commission_bps int CHECK (commission_bps BETWEEN 0 AND 10000),
  -- 18% is the usual GST rate for consumer electronics; confirm per HSN with a CA (TRS Q-01).
  gst_rate_bps  int NOT NULL DEFAULT 1800 CHECK (gst_rate_bps BETWEEN 0 AND 10000),
  hsn           text,
  status        text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RETIRED')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       int NOT NULL DEFAULT 1,
  CHECK (parent_id IS DISTINCT FROM id)
);
CREATE INDEX ix_categories_parent ON categories (parent_id, position);

-- +goose Down
DROP TABLE categories;
