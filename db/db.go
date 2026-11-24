// Package db embeds the SQL migrations so every binary carries its schema.
package db

import "embed"

// Migrations holds goose migrations named by UTC timestamp (dev-workflow.md 2).
//
//go:embed migrations/*.sql
var Migrations embed.FS
