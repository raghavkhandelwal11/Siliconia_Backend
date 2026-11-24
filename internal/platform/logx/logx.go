// Package logx creates the JSON logger used by every binary (LLD 16).
package logx

import (
	"log/slog"
	"os"
)

// New returns a JSON logger tagged with the service name.
func New(service string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("service", service)
}
