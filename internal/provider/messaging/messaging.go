// Package messaging defines the Messenger interface for SMS, WhatsApp and email
// (LLD 12) and the free adapters used in this build: the sim-notify test inbox
// and a log adapter. Paid adapters are planned in paid-later/ only.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Channel is a delivery channel.
type Channel string

const (
	SMS      Channel = "sms"
	WhatsApp Channel = "whatsapp"
	Email    Channel = "email"
)

// Message is one outgoing message rendered from a template.
type Message struct {
	Channel  Channel           `json:"channel"`
	To       string            `json:"to"`
	Template string            `json:"template"`
	Vars     map[string]string `json:"vars,omitempty"`
	Body     string            `json:"body"`
}

// Messenger sends a message and returns the provider's reference.
type Messenger interface {
	Send(ctx context.Context, m Message) (string, error)
}

// SimNotify posts messages to the sim-notify test inbox (FR-NOT-05).
type SimNotify struct {
	BaseURL string
	Client  *http.Client
}

// NewSimNotify creates the test-inbox adapter.
func NewSimNotify(baseURL string) *SimNotify {
	return &SimNotify{BaseURL: baseURL, Client: &http.Client{Timeout: 3 * time.Second}}
}

// Send implements Messenger.
func (s *SimNotify) Send(ctx context.Context, m Message) (string, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/internal/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sim-notify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("sim-notify: status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("sim-notify: decode: %w", err)
	}
	return out.ID, nil
}

// Log writes messages to the log without their body (local development).
type Log struct{ Logger *slog.Logger }

// Send implements Messenger.
func (l Log) Send(_ context.Context, m Message) (string, error) {
	l.Logger.Info("message (log adapter)", "channel", m.Channel, "template", m.Template)
	return "log", nil
}

// Router picks the adapter for each channel from the PROVIDER_* settings.
type Router map[Channel]Messenger

// Send implements Messenger.
func (r Router) Send(ctx context.Context, m Message) (string, error) {
	adapter, ok := r[m.Channel]
	if !ok {
		return "", fmt.Errorf("messaging: no adapter for channel %q", m.Channel)
	}
	return adapter.Send(ctx, m)
}

// NewRouter builds the router from adapter names ("sim" or "log"). Brevo is
// added with the email module (T2.7).
func NewRouter(sms, whatsapp, email, simNotifyURL string, log *slog.Logger) (Router, error) {
	pick := func(name string) (Messenger, error) {
		switch name {
		case "sim":
			return NewSimNotify(simNotifyURL), nil
		case "log":
			return Log{Logger: log}, nil
		default:
			return nil, fmt.Errorf("messaging: unknown adapter %q (only free adapters exist in this build)", name)
		}
	}
	r := Router{}
	for ch, name := range map[Channel]string{SMS: sms, WhatsApp: whatsapp, Email: email} {
		m, err := pick(name)
		if err != nil {
			return nil, err
		}
		r[ch] = m
	}
	return r, nil
}
