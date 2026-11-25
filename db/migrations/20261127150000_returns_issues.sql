-- +goose Up
-- Returns and replacements within the return window, and buyer issues with a
-- support conversation (LLD 7.4, 8.6, 10).
CREATE TABLE return_requests (
  id                       uuid PRIMARY KEY,
  number                   text NOT NULL UNIQUE,
  order_item_id            uuid NOT NULL REFERENCES order_items (id),
  sub_order_id             uuid NOT NULL REFERENCES sub_orders (id),
  buyer_id                 uuid NOT NULL REFERENCES accounts (id),
  seller_id                uuid NOT NULL REFERENCES accounts (id),
  type                     text NOT NULL CHECK (type IN ('RETURN', 'REPLACE')),
  reason                   text NOT NULL CHECK (reason IN ('DAMAGED', 'DEFECTIVE', 'WRONG_ITEM', 'NOT_AS_DESCRIBED', 'MISSING_PARTS', 'NO_LONGER_NEEDED')),
  note                     text NOT NULL DEFAULT '',
  status                   text NOT NULL CHECK (status IN ('REQUESTED', 'REJECTED', 'PICKUP_SCHEDULED', 'PICKED_UP', 'RECEIVED',
                                                         'REFUNDED', 'REFUND_PENDING', 'REPLACEMENT_SHIPPED')),
  reject_reason            text,
  decided_at               timestamptz,
  pickup_shipment_id       uuid REFERENCES shipments (id),
  replacement_sub_order_id uuid REFERENCES sub_orders (id),
  refund_amount_paise      bigint NOT NULL DEFAULT 0,
  outcome_note             text NOT NULL DEFAULT '',
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now()
);
-- One live request per item; a rejected one can be followed by another.
CREATE UNIQUE INDEX ux_return_item_active ON return_requests (order_item_id) WHERE status <> 'REJECTED';
CREATE INDEX ix_returns_buyer ON return_requests (buyer_id, created_at DESC);
CREATE INDEX ix_returns_seller ON return_requests (seller_id, status);

ALTER TABLE sub_orders ADD COLUMN replaces_item_id uuid REFERENCES order_items (id);

CREATE TABLE issues (
  id                uuid PRIMARY KEY,
  number            text NOT NULL UNIQUE,
  buyer_id          uuid NOT NULL REFERENCES accounts (id),
  order_id          uuid NOT NULL REFERENCES orders (id),
  order_item_id     uuid REFERENCES order_items (id),
  type              text NOT NULL CHECK (type IN ('NOT_DELIVERED', 'DAMAGED', 'WRONG_ITEM', 'MISSING_PARTS', 'PAYMENT', 'RETURN_REFUND', 'OTHER')),
  status            text NOT NULL CHECK (status IN ('OPEN', 'IN_PROGRESS', 'WAITING_CUSTOMER', 'RESOLVED', 'CLOSED')),
  first_response_at timestamptz,
  resolved_at       timestamptz,
  closed_at         timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_issues_buyer ON issues (buyer_id, updated_at DESC);
CREATE INDEX ix_issues_status ON issues (status, updated_at);

CREATE TABLE issue_messages (
  id          uuid PRIMARY KEY,
  issue_id    uuid NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
  author_id   uuid NOT NULL REFERENCES accounts (id),
  author_role text NOT NULL CHECK (author_role IN ('BUYER', 'SUPPORT')),
  body        text NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_issue_messages ON issue_messages (issue_id, created_at);

-- +goose Down
DROP TABLE issue_messages;
DROP TABLE issues;
ALTER TABLE sub_orders DROP COLUMN replaces_item_id;
DROP TABLE return_requests;
