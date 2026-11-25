-- +goose Up
-- Sub-order history for the buyer timeline and audits (LLD 10), cancellation
-- details and per-seller GST invoice numbering (FR-ORD).
CREATE TABLE sub_order_events (
  id           uuid PRIMARY KEY,
  sub_order_id uuid NOT NULL REFERENCES sub_orders (id) ON DELETE CASCADE,
  status       sub_status NOT NULL,
  actor        text NOT NULL CHECK (actor IN ('BUYER', 'SELLER', 'ADMIN', 'COURIER', 'SYSTEM')),
  note         text NOT NULL DEFAULT '',
  at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_sub_order_events ON sub_order_events (sub_order_id, at);

ALTER TABLE sub_orders
  ADD COLUMN cancel_reason text,
  ADD COLUMN cancelled_by  text,
  ADD COLUMN invoiced_at   timestamptz;

ALTER TABLE seller_profiles ADD COLUMN invoice_counter int NOT NULL DEFAULT 0;

-- Every status change is recorded by a trigger, so no code path can skip the
-- timeline. The transaction names the actor and an optional note with
-- set_config('siliconics.actor' / 'siliconics.note', …, true); default SYSTEM.
-- +goose StatementBegin
CREATE FUNCTION record_sub_order_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' OR NEW.status IS DISTINCT FROM OLD.status THEN
    INSERT INTO sub_order_events (id, sub_order_id, status, actor, note)
    VALUES (gen_random_uuid(), NEW.id, NEW.status,
            coalesce(nullif(current_setting('siliconics.actor', true), ''), 'SYSTEM'),
            coalesce(current_setting('siliconics.note', true), ''));
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_sub_order_events AFTER INSERT OR UPDATE OF status ON sub_orders
  FOR EACH ROW EXECUTE FUNCTION record_sub_order_event();

-- History for orders placed before this table existed.
INSERT INTO sub_order_events (id, sub_order_id, status, actor, at)
SELECT gen_random_uuid(), s.id, s.status, 'SYSTEM', coalesce(o.confirmed_at, o.placed_at)
FROM sub_orders s JOIN orders o ON o.id = s.order_id;

-- +goose Down
DROP TRIGGER trg_sub_order_events ON sub_orders;
DROP FUNCTION record_sub_order_event();
ALTER TABLE seller_profiles DROP COLUMN invoice_counter;
ALTER TABLE sub_orders DROP COLUMN invoiced_at, DROP COLUMN cancelled_by, DROP COLUMN cancel_reason;
DROP TABLE sub_order_events;
