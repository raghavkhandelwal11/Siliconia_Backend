-- +goose Up
-- Shipments and tracking (LLD 5, 8.5). One forward shipment per sub-order;
-- return pickups (T2.6) and demo units (M4) use the same tables.
CREATE TABLE shipments (
  id               uuid PRIMARY KEY,
  sub_order_id     uuid NOT NULL REFERENCES sub_orders (id),
  kind             text NOT NULL CHECK (kind IN ('FORWARD', 'RETURN', 'DEMO')),
  ref              text NOT NULL,
  provider         text NOT NULL,
  awb              text NOT NULL UNIQUE,
  status           text NOT NULL,
  eta              timestamptz,
  cod_amount_paise bigint NOT NULL DEFAULT 0,
  last_event_at    timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (kind, ref)
);
CREATE INDEX ix_shipments_sub_order ON shipments (sub_order_id);
CREATE UNIQUE INDEX ux_shipments_forward_active ON shipments (sub_order_id) WHERE kind = 'FORWARD' AND status <> 'CANCELLED';

CREATE TABLE tracking_events (
  id          uuid PRIMARY KEY,
  shipment_id uuid NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
  status      text NOT NULL,
  location    text NOT NULL DEFAULT '',
  at          timestamptz NOT NULL,
  raw         jsonb NOT NULL
);
CREATE INDEX ix_tracking_events_shipment ON tracking_events (shipment_id, at);

ALTER TABLE sub_orders ADD COLUMN cod_collected_paise bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sub_orders DROP COLUMN cod_collected_paise;
DROP TABLE tracking_events;
DROP TABLE shipments;
