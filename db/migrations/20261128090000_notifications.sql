-- +goose Up
-- Notifications (LLD 13): an in-app inbox row per notice, one delivery row
-- per outside channel (SMS, WhatsApp, email), and each account's choices.
CREATE TABLE notifications (
  id          uuid PRIMARY KEY,
  account_id  uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  template    text NOT NULL,
  title       text NOT NULL,
  body        text NOT NULL,
  link        text NOT NULL DEFAULT '',
  dedupe_key  text NOT NULL UNIQUE, -- event id + template + account: a retried event never notifies twice
  read_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_notifications_account ON notifications (account_id, created_at DESC);
CREATE INDEX ix_notifications_unread ON notifications (account_id) WHERE read_at IS NULL;

CREATE TABLE notification_deliveries (
  notification_id uuid NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
  channel         text NOT NULL CHECK (channel IN ('sms', 'whatsapp', 'email')),
  status          text NOT NULL CHECK (status IN ('SENT', 'FAILED', 'SKIPPED')),
  provider_ref    text,
  error           text,
  attempts        int NOT NULL DEFAULT 1,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (notification_id, channel)
);

CREATE TABLE notification_prefs (
  account_id uuid PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
  whatsapp   boolean NOT NULL DEFAULT true,
  email      boolean NOT NULL DEFAULT true,
  marketing  boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE notification_prefs;
DROP TABLE notification_deliveries;
DROP TABLE notifications;
