// Package migrate applies the embedded goose migrations.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/raghavkhandelwal11/Siliconia_Backend/db"
)

// Up applies all pending migrations.
func Up(ctx context.Context, databaseURL string) error {
	return run(ctx, databaseURL, func(sqlDB *sql.DB) error { return goose.UpContext(ctx, sqlDB, "migrations") })
}

// Status prints the migration status.
func Status(ctx context.Context, databaseURL string) error {
	return run(ctx, databaseURL, func(sqlDB *sql.DB) error { return goose.StatusContext(ctx, sqlDB, "migrations") })
}

// DownOne rolls back the latest migration (local use only).
func DownOne(ctx context.Context, databaseURL string) error {
	return run(ctx, databaseURL, func(sqlDB *sql.DB) error { return goose.DownContext(ctx, sqlDB, "migrations") })
}

func run(ctx context.Context, databaseURL string, fn func(*sql.DB) error) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("migrate: ping: %w", err)
	}
	// Serialise concurrent runs (parallel test packages, two replicas starting
	// together) with a session advisory lock held on a dedicated connection.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('siliconia_migrate'))`); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('siliconia_migrate'))`)
	}()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return fn(sqlDB)
}
