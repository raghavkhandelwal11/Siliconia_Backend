// Package notify turns domain events into notifications (LLD 13): an in-app
// inbox entry pushed live to open tabs, plus SMS, WhatsApp and email through
// the messaging adapters (the test inbox in this build; Brevo for email when
// configured). Delivery is per channel and idempotent: a retried event never
// notifies twice, and only failed channels are tried again.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Item is a notification as the web app shows it.
type Item struct {
	ID        uuid.UUID  `json:"id"`
	Template  string     `json:"template"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link,omitempty"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// Publisher pushes a new in-app notification to the account's open tabs.
type Publisher func(ctx context.Context, account uuid.UUID, it Item) error

// Channel name for live pushes (Redis pub/sub).
func Channel(account uuid.UUID) string { return "notif:" + account.String() }

// Service delivers notifications.
type Service struct {
	Pool      *pgxpool.Pool
	Q         *store.Queries
	Messenger messaging.Messenger
	Publish   Publisher
	PublicURL string // for links in email
	Log       *slog.Logger
}

// Handle routes one outbox event. eventID makes delivery idempotent.
func (s *Service) Handle(ctx context.Context, topic, eventID string, payload []byte) error {
	notices, err := route(ctx, s.Q, topic, payload)
	if err != nil {
		return err
	}
	var failed []error
	for _, n := range notices {
		if err := s.deliver(ctx, eventID, n); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...) // the queue retries; delivered channels are skipped next time
}

func (s *Service) deliver(ctx context.Context, eventID string, n Notice) error {
	row, err := s.Q.UpsertNotification(ctx, store.UpsertNotificationParams{
		ID: mustV7(), AccountID: n.Account, Template: n.Template, Title: n.Title, Body: n.Body, Link: n.Link,
		DedupeKey: eventID + ":" + n.Template + ":" + n.Account.String(),
	})
	if err != nil {
		return err
	}
	if row.Inserted && s.Publish != nil {
		it := Item{ID: row.ID, Template: row.Template, Title: row.Title, Body: row.Body, Link: row.Link, CreatedAt: row.CreatedAt}
		if err := s.Publish(ctx, n.Account, it); err != nil {
			s.Log.Warn("live push failed", "err", err) // the inbox still has it
		}
	}
	who, err := s.Q.GetRecipient(ctx, n.Account)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ch := templates[n.Template]
	var failed []error
	send := func(channel messaging.Channel, wanted bool, to string, msg messaging.Message) {
		if !wanted {
			return
		}
		prev, err := s.Q.GetDelivery(ctx, store.GetDeliveryParams{NotificationID: row.ID, Channel: string(channel)})
		if err == nil && prev.Status != "FAILED" {
			return // already sent or skipped
		}
		if to == "" {
			_ = s.Q.RecordDelivery(ctx, store.RecordDeliveryParams{NotificationID: row.ID, Channel: string(channel), Status: "SKIPPED", Error: ptr("no address")})
			return
		}
		msg.Channel, msg.To, msg.Template = channel, to, n.Template
		ref, err := s.Messenger.Send(ctx, msg)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", channel, err))
			_ = s.Q.RecordDelivery(ctx, store.RecordDeliveryParams{NotificationID: row.ID, Channel: string(channel), Status: "FAILED", Error: ptr(trim(err.Error()))})
			return
		}
		_ = s.Q.RecordDelivery(ctx, store.RecordDeliveryParams{NotificationID: row.ID, Channel: string(channel), Status: "SENT", ProviderRef: &ref})
	}

	phone := ""
	if who.PhoneE164 != nil && who.PhoneVerifiedAt != nil {
		phone = *who.PhoneE164
	}
	email := ""
	if who.Email != nil {
		email = *who.Email
	}
	short := messaging.Message{Body: "Siliconics: " + n.Body}
	send(messaging.SMS, ch.sms, phone, short)
	send(messaging.WhatsApp, ch.whatsapp && who.Whatsapp, phone, short)
	link := strings.TrimRight(s.PublicURL, "/") + n.Link
	send(messaging.Email, ch.email && who.EmailOn, email, messaging.Message{
		Subject: n.Title,
		Body:    fmt.Sprintf("Hello %s,\n\n%s\n\nOpen in Siliconics: %s\n\nYou receive this because of activity on your Siliconics account.", firstName(who.Name), n.Body, link),
	})
	return errors.Join(failed...)
}

func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return "there"
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func mustV7() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

// List returns the newest notifications (30 per page) and the unread count.
func (s *Service) List(ctx context.Context, account uuid.UUID, before *time.Time) ([]Item, int32, error) {
	rows, err := s.Q.ListNotifications(ctx, store.ListNotificationsParams{AccountID: account, Before: before})
	if err != nil {
		return nil, 0, err
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = Item{ID: r.ID, Template: r.Template, Title: r.Title, Body: r.Body, Link: r.Link, ReadAt: r.ReadAt, CreatedAt: r.CreatedAt}
	}
	unread, err := s.Q.CountUnread(ctx, account)
	return out, unread, err
}

// MarkRead marks the given notifications (or all when ids is empty) as read.
func (s *Service) MarkRead(ctx context.Context, account uuid.UUID, ids []uuid.UUID) (int32, error) {
	var err error
	if len(ids) == 0 {
		_, err = s.Q.MarkAllNotificationsRead(ctx, account)
	} else {
		_, err = s.Q.MarkNotificationsRead(ctx, store.MarkNotificationsReadParams{AccountID: account, Ids: ids})
	}
	if err != nil {
		return 0, err
	}
	return s.Q.CountUnread(ctx, account)
}

// Prefs are the account's channel choices. SMS and in-app are always on for
// order updates (transactional messages, LLD 13).
type Prefs struct {
	WhatsApp  bool `json:"whatsapp"`
	Email     bool `json:"email"`
	Marketing bool `json:"marketing"`
}

// GetPrefs returns the account's choices (defaults when never saved).
func (s *Service) GetPrefs(ctx context.Context, account uuid.UUID) (Prefs, error) {
	p, err := s.Q.GetPrefs(ctx, account)
	return Prefs{WhatsApp: p.Whatsapp, Email: p.Email, Marketing: p.Marketing}, err
}

// SavePrefs stores the account's choices.
func (s *Service) SavePrefs(ctx context.Context, account uuid.UUID, p Prefs) error {
	return s.Q.SavePrefs(ctx, store.SavePrefsParams{AccountID: account, Whatsapp: p.WhatsApp, Email: p.Email, Marketing: p.Marketing})
}

// MarshalItem encodes an item for a live push.
func MarshalItem(it Item) string {
	b, _ := json.Marshal(it)
	return string(b)
}
