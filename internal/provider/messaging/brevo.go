package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BrevoConfig configures the Brevo transactional email adapter (free plan:
// 300 emails a day). The API key lives in the environment, never in code.
type BrevoConfig struct {
	APIKey   string
	From     string // a sender address verified in Brevo
	FromName string
	BaseURL  string // https://api.brevo.com (tests point it elsewhere)
}

// Brevo sends email through Brevo's transactional API.
type Brevo struct {
	cfg    BrevoConfig
	client *http.Client
}

// NewBrevo checks the settings and creates the adapter.
func NewBrevo(cfg BrevoConfig) (*Brevo, error) {
	if cfg.APIKey == "" || cfg.From == "" {
		return nil, errors.New("messaging: PROVIDER_EMAIL=brevo needs BREVO_API_KEY and EMAIL_FROM")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.brevo.com"
	}
	if cfg.FromName == "" {
		cfg.FromName = "Siliconics"
	}
	return &Brevo{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Send implements Messenger for email.
func (b *Brevo) Send(ctx context.Context, m Message) (string, error) {
	if m.Channel != Email {
		return "", fmt.Errorf("brevo: only email is supported, got %q", m.Channel)
	}
	subject := m.Subject
	if subject == "" {
		subject = "Siliconics"
	}
	body, _ := json.Marshal(map[string]any{
		"sender":      map[string]string{"email": b.cfg.From, "name": b.cfg.FromName},
		"to":          []map[string]string{{"email": m.To}},
		"subject":     subject,
		"textContent": m.Body,
		"tags":        []string{m.Template},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.BaseURL+"/v3/smtp/email", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("api-key", b.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("brevo: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("brevo: status %d: %s", res.StatusCode, msg)
	}
	var out struct {
		MessageID string `json:"messageId"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out.MessageID, nil
}
