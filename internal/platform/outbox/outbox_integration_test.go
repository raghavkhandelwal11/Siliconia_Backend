package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
)

// Runs only when TEST_DATABASE_URL points at a disposable PostgreSQL (CI service
// container or `make test-integration`).
func TestOutboxRelayPublishesOnceAcrossWorkers(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE outbox`); err != nil {
		t.Fatal(err)
	}

	const total = 50
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		for i := 0; i < total; i++ {
			if _, err := outbox.Add(ctx, tx, "test.event", "k", map[string]int{"i": i}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	publish := func(_ context.Context, e outbox.Event) error {
		mu.Lock()
		seen[e.ID.String()]++
		mu.Unlock()
		return nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &outbox.Relay{Pool: pool, Publish: publish, Log: log, BatchSize: 7}
			for {
				n, err := r.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("published %d distinct events, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s published %d times", id, n)
		}
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE outbox`); err != nil {
		t.Fatal(err)
	}
	_ = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := outbox.Add(ctx, tx, "test.rollback", "", map[string]string{}); err != nil {
			return err
		}
		return io.EOF // any error rolls back
	})
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back event was stored (%d rows)", n)
	}
}
