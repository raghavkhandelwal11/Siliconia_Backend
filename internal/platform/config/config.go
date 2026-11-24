// Package config loads runtime settings from environment variables.
//
// Only deployment settings live here (LLD 4.7). Business rules that admins can
// change (fees, rates, thresholds) live in the database `config` table.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode is the deployment mode. Only "prototype" exists in the free build;
// "production" is reserved and refuses to start with simulators (LLD 4.7).
type Mode string

const (
	ModeLocal      Mode = "local"
	ModeCI         Mode = "ci"
	ModePrototype  Mode = "prototype"
	ModeProduction Mode = "production"
)

// Config holds every deployment setting used by the API and worker.
type Config struct {
	Mode            Mode
	Version         string
	HTTPAddr        string
	DatabaseURL     string
	RedisURL        string
	AllowedOrigins  []string
	ShutdownTimeout time.Duration

	// Provider adapters (all free in this build; see docs/free-services.md).
	ProviderSMS      string
	ProviderWhatsApp string
	ProviderEmail    string
	SimNotifyURL     string

	FeatureTestInbox bool

	// Rate limits for unauthenticated traffic (per IP, per minute).
	GuestRateLimitPerMinute int
}

// Load reads the environment and validates the result.
func Load() (Config, error) {
	c := Config{
		Mode:                    Mode(env("APP_MODE", string(ModeLocal))),
		Version:                 env("APP_VERSION", "dev"),
		HTTPAddr:                env("HTTP_ADDR", ":8080"),
		DatabaseURL:             env("DATABASE_URL", "postgres://siliconia:siliconia@localhost:5432/siliconia?sslmode=disable"),
		RedisURL:                env("REDIS_URL", "redis://localhost:6379/0"),
		AllowedOrigins:          splitList(env("ALLOWED_ORIGINS", "http://localhost:3000,http://localhost:8080")),
		ShutdownTimeout:         envDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
		ProviderSMS:             env("PROVIDER_SMS", "sim"),
		ProviderWhatsApp:        env("PROVIDER_WHATSAPP", "sim"),
		ProviderEmail:           env("PROVIDER_EMAIL", "log"),
		SimNotifyURL:            env("SIM_NOTIFY_URL", "http://localhost:9005"),
		FeatureTestInbox:        envBool("FEATURE_TEST_INBOX", true),
		GuestRateLimitPerMinute: envInt("RATELIMIT_GUEST_PER_MINUTE", 60),
	}
	return c, c.validate()
}

func (c Config) validate() error {
	switch c.Mode {
	case ModeLocal, ModeCI, ModePrototype:
	case ModeProduction:
		// Production is planned in paid-later/ and is not part of this build.
		for name, v := range map[string]string{"PROVIDER_SMS": c.ProviderSMS, "PROVIDER_WHATSAPP": c.ProviderWhatsApp} {
			if v == "sim" {
				return fmt.Errorf("config: %s=sim is not allowed in production mode", name)
			}
		}
		if c.FeatureTestInbox {
			return fmt.Errorf("config: FEATURE_TEST_INBOX must be false in production mode")
		}
	default:
		return fmt.Errorf("config: unknown APP_MODE %q", c.Mode)
	}
	if c.DatabaseURL == "" || c.RedisURL == "" {
		return fmt.Errorf("config: DATABASE_URL and REDIS_URL are required")
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
