// Command seed loads default business settings (TRS section 7, LLD 4.7).
// It is safe to run repeatedly: existing keys are left unchanged.
// It also inserts the 12 default categories (T1.4). Demo data comes with T6.5.
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
)

// Defaults from LLD 4.7. Money in paise, rates in basis points.
var defaults = map[string]any{
	"membership.fee_paise":           65000,
	"electrons.nonmember.min_paise":  250000,
	"electrons.nonmember.rate_bps":   50,
	"electrons.member.min_paise":     80000,
	"electrons.member.rate_bps":      200,
	"electrons.expiry_months":        12,
	"electrons.expiry_reminder_days": []int{30, 7},
	"returns.window_days":            7,
	"cod.max_order_paise":            800000,
	"cashout.min_electrons":          250,
	"launch.deal_discount_bps":       8000,
	"launch.min_profit_bps":          400,
	"launch.hold_minutes":            10,
	"checkout.hold_minutes":          10,
	"creator.min_followers":          5000,
	"creator.pool.weights":           []float64{1.0, 0.75, 0.5, 0.25},
	"creator.pool.default_size":      0,
	"verified_tag.threshold_paise":   40000000,
	"pow.weights":                    map[string]float64{"units": 0.6, "rating": 0.4},
	"refund.reissue_expiry_days":     30,
}

func main() {
	log := logx.New("seed")
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	inserted := 0
	for key, value := range defaults {
		raw, err := json.Marshal(value)
		if err != nil {
			log.Error("marshal", "key", key, "err", err)
			os.Exit(1)
		}
		tag, err := pool.Exec(ctx, `INSERT INTO config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, key, raw)
		if err != nil {
			log.Error("insert", "key", key, "err", err)
			os.Exit(1)
		}
		inserted += int(tag.RowsAffected())
	}
	cats, err := catalogue.Seed(ctx, pool)
	if err != nil {
		log.Error("categories", "err", err)
		os.Exit(1)
	}
	log.Info("seed done", "config_inserted", inserted, "config_total", len(defaults), "categories_inserted", cats)
}
