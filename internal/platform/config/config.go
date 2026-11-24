// Package config loads runtime settings from environment variables.
//
// Only deployment settings live here (LLD 4.7). Business rules that admins can
// change (fees, rates, thresholds) live in the database `config` table.
package config

import (
	"crypto/sha256"
	"encoding/base64"
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

	// Auth (LLD 4.2). Keys are base64; local mode falls back to fixed dev keys.
	AuthSigningKey      string // ed25519 seed (32 bytes) for access tokens
	OTPPepper           string // HMAC key for OTP hashes
	EncryptionMasterKey string // AES-256 key for envelope encryption (TOTP secrets, KYC data)
	CookieSecure        bool   // Secure cookies (true behind HTTPS)
	PublicBaseURL       string // e.g. http://localhost:8080; used for OAuth redirects
	GoogleClientID      string
	GoogleClientSecret  string
	GoogleIssuer        string // overridable for tests
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
		AuthSigningKey:          os.Getenv("AUTH_SIGNING_KEY"),
		OTPPepper:               os.Getenv("OTP_PEPPER"),
		EncryptionMasterKey:     os.Getenv("ENCRYPTION_MASTER_KEY"),
		CookieSecure:            envBool("COOKIE_SECURE", false),
		PublicBaseURL:           env("PUBLIC_BASE_URL", "http://localhost:8080"),
		GoogleClientID:          os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:      os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleIssuer:            env("GOOGLE_ISSUER", "https://accounts.google.com"),
	}
	if c.Mode == ModeLocal || c.Mode == ModeCI {
		// Development keys derived from fixed labels, so every API copy agrees
		// locally without a secret in the repository. Never used in the prototype.
		c.AuthSigningKey = orDefault(c.AuthSigningKey, DevKey("auth-signing"))
		c.OTPPepper = orDefault(c.OTPPepper, DevKey("otp-pepper"))
		c.EncryptionMasterKey = orDefault(c.EncryptionMasterKey, DevKey("encryption-master"))
	}
	return c, c.validate()
}

// DevKey returns a deterministic 32-byte base64 key for local and CI use only.
func DevKey(label string) string {
	sum := sha256.Sum256([]byte("siliconics-local-dev:" + label))
	return base64.StdEncoding.EncodeToString(sum[:])
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
	if c.AuthSigningKey == "" || c.OTPPepper == "" || c.EncryptionMasterKey == "" {
		return fmt.Errorf("config: AUTH_SIGNING_KEY, OTP_PEPPER and ENCRYPTION_MASTER_KEY are required in %s mode", c.Mode)
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// GoogleEnabled reports whether Google sign-in is configured (free OAuth client).
func (c Config) GoogleEnabled() bool { return c.GoogleClientID != "" && c.GoogleClientSecret != "" }

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
