// Command migrate applies database migrations: `migrate up|status|down`.
package main

import (
	"context"
	"os"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
)

func main() {
	log := logx.New("migrate")
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx := context.Background()
	switch cmd {
	case "up":
		err = migrate.Up(ctx, cfg.DatabaseURL)
	case "status":
		err = migrate.Status(ctx, cfg.DatabaseURL)
	case "down":
		if cfg.Mode == config.ModePrototype || cfg.Mode == config.ModeProduction {
			log.Error("down is only allowed locally (migrations are expand/contract)")
			os.Exit(2)
		}
		err = migrate.DownOne(ctx, cfg.DatabaseURL)
	default:
		log.Error("usage: migrate up|status|down")
		os.Exit(2)
	}
	if err != nil {
		log.Error("migrate", "cmd", cmd, "err", err)
		os.Exit(1)
	}
	log.Info("migrate done", "cmd", cmd)
}
