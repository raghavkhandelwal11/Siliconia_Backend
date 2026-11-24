// Package outbox writes domain events inside business transactions and relays
// them to the job queue (LLD 4.4).
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one row of the outbox.
type Event struct {
	ID      uuid.UUID       `json:"id"`
	Topic   string          `json:"topic"`
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload"`
}

// Add inserts an event using the caller's transaction so it commits with the
// business change.
func Add(ctx context.Context, tx pgx.Tx, topic, key string, payload any) (uuid.UUID, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("outbox: marshal %s: %w", topic, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox (id, topic, key, payload) VALUES ($1, $2, $3, $4)`, id, topic, key, body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("outbox: insert %s: %w", topic, err)
	}
	return id, nil
}

// Publisher hands a claimed event to the queue. It must be idempotent per
// event ID because a crash after publishing but before marking can repeat it.
type Publisher func(ctx context.Context, e Event) error

// Relay moves unpublished events to the queue in batches using
// FOR UPDATE SKIP LOCKED, so several workers can run safely.
type Relay struct {
	Pool      *pgxpool.Pool
	Publish   Publisher
	Log       *slog.Logger
	BatchSize int
	Interval  time.Duration
}

// RunOnce relays one batch and returns how many events were published.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	batch := r.BatchSize
	if batch <= 0 {
		batch = 200
	}
	n := 0
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, topic, key, payload FROM outbox
			WHERE published_at IS NULL
			ORDER BY created_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, batch)
		if err != nil {
			return err
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
			var e Event
			err := row.Scan(&e.ID, &e.Topic, &e.Key, &e.Payload)
			return e, err
		})
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := r.Publish(ctx, e); err != nil {
				return fmt.Errorf("outbox: publish %s: %w", e.ID, err)
			}
			if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1`, e.ID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Run relays until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				r.Log.Error("outbox relay", "err", err)
			}
		}
	}
}
