// Package issues lets buyers raise a problem with an order and talk to
// support until it is resolved (LLD 7.4, 10): OPEN → IN_PROGRESS ⇄
// WAITING_CUSTOMER → RESOLVED → CLOSED (automatically after 7 days), and a
// resolved issue can be reopened.
package issues

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ids"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TopicUpdated is published when support replies or the status changes.
const TopicUpdated = "issue.updated"

var (
	ErrClosed     = httpx.NewError(http.StatusConflict, "ISSUE_CLOSED", "This issue is closed. Raise a new one if you still need help")
	ErrResolved   = httpx.NewError(http.StatusConflict, "ISSUE_RESOLVED", "This issue is resolved. Reopen it to continue")
	ErrNotReopen  = httpx.NewError(http.StatusConflict, "CANNOT_REOPEN", "Only resolved issues can be reopened")
	ErrBadMessage = httpx.ErrValidation.WithDetail("Write a message (1 to 2000 characters)")
)

var types = map[string]bool{"NOT_DELIVERED": true, "DAMAGED": true, "WRONG_ITEM": true, "MISSING_PARTS": true, "PAYMENT": true, "RETURN_REFUND": true, "OTHER": true}

// Support may move an issue to these statuses when replying.
var supportStatuses = map[string]bool{"IN_PROGRESS": true, "WAITING_CUSTOMER": true, "RESOLVED": true}

// Service runs issues.
type Service struct {
	Pool *pgxpool.Pool
	Q    *store.Queries
	Log  *slog.Logger
}

// Message is one message in the conversation.
type Message struct {
	ID         uuid.UUID `json:"id"`
	AuthorRole string    `json:"authorRole"`
	Body       string    `json:"body"`
	At         time.Time `json:"at"`
}

// Issue is an issue with its conversation.
type Issue struct {
	ID              uuid.UUID  `json:"id"`
	Number          string     `json:"number"`
	OrderID         uuid.UUID  `json:"orderId"`
	OrderNumber     string     `json:"orderNumber"`
	OrderItemID     *uuid.UUID `json:"orderItemId,omitempty"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	FirstResponseAt *time.Time `json:"firstResponseAt,omitempty"`
	ResolvedAt      *time.Time `json:"resolvedAt,omitempty"`
	Messages        []Message  `json:"messages,omitempty"`
}

func cleanBody(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := len([]rune(s)); n < 1 || n > 2000 {
		return "", ErrBadMessage
	}
	return s, nil
}

// Raise is the 3-step form: the order (and item), the type, and the story.
type Raise struct {
	OrderID     uuid.UUID  `json:"orderId"`
	OrderItemID *uuid.UUID `json:"orderItemId"`
	Type        string     `json:"type"`
	Body        string     `json:"body"`
}

// Create raises an issue on one of the buyer's orders.
func (s *Service) Create(ctx context.Context, buyer uuid.UUID, in Raise) (Issue, error) {
	body, err := cleanBody(in.Body)
	if err != nil {
		return Issue{}, err
	}
	if !types[in.Type] {
		return Issue{}, httpx.ErrValidation.WithDetail("Choose what went wrong")
	}
	var id uuid.UUID
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		o, err := q.GetOrder(ctx, in.OrderID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (o.BuyerID != buyer || o.Status == "PENDING_PAYMENT" || o.Status == "EXPIRED")) {
			return httpx.ErrValidation.WithDetail("Choose one of your orders")
		}
		if err != nil {
			return err
		}
		if in.OrderItemID != nil {
			items, err := q.ListOrderItems(ctx, o.ID)
			if err != nil {
				return err
			}
			found := false
			for _, it := range items {
				found = found || it.ID == *in.OrderItemID
			}
			if !found {
				return httpx.ErrValidation.WithDetail("That item is not part of this order")
			}
		}
		id, _ = uuid.NewV7()
		is, err := q.CreateIssue(ctx, store.CreateIssueParams{ID: id, Number: ids.Readable("IS", time.Now()), BuyerID: buyer, OrderID: o.ID,
			OrderItemID: in.OrderItemID, Type: in.Type})
		if err != nil {
			return err
		}
		mid, _ := uuid.NewV7()
		if _, err := q.AddIssueMessage(ctx, store.AddIssueMessageParams{ID: mid, IssueID: id, AuthorID: buyer, AuthorRole: "BUYER", Body: body}); err != nil {
			return err
		}
		return s.event(ctx, tx, is.ID, is.Number, is.BuyerID, is.Status, "BUYER")
	})
	if err != nil {
		return Issue{}, err
	}
	return s.Get(ctx, Viewer{ID: buyer}, id)
}

func (s *Service) event(ctx context.Context, tx pgx.Tx, id uuid.UUID, number string, buyer uuid.UUID, status, by string) error {
	_, err := outbox.Add(ctx, tx, TopicUpdated, id.String(), map[string]any{"issueId": id, "number": number, "buyerId": buyer, "status": status, "by": by})
	return err
}

// Viewer is the buyer or a support agent (admin).
type Viewer struct {
	ID      uuid.UUID
	Support bool
}

// Get returns an issue with its messages.
func (s *Service) Get(ctx context.Context, v Viewer, id uuid.UUID) (Issue, error) {
	row, err := s.Q.GetIssue(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !v.Support && row.BuyerID != v.ID) {
		return Issue{}, httpx.ErrNotFound
	}
	if err != nil {
		return Issue{}, err
	}
	out := Issue{ID: row.ID, Number: row.Number, OrderID: row.OrderID, OrderNumber: row.OrderNumber, OrderItemID: row.OrderItemID, Type: row.Type,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, FirstResponseAt: row.FirstResponseAt, ResolvedAt: row.ResolvedAt,
		Messages: []Message{}}
	msgs, err := s.Q.ListIssueMessages(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, Message{ID: m.ID, AuthorRole: m.AuthorRole, Body: m.Body, At: m.CreatedAt})
	}
	return out, nil
}

// List returns the buyer's issues (or, for support, the queue by status).
func (s *Service) List(ctx context.Context, v Viewer, status string) ([]Issue, error) {
	out := []Issue{}
	if v.Support {
		var st *string
		if status != "" {
			st = &status
		}
		rows, err := s.Q.ListSupportIssues(ctx, st)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, Issue{ID: r.ID, Number: r.Number, OrderID: r.OrderID, OrderNumber: r.OrderNumber, OrderItemID: r.OrderItemID,
				Type: r.Type, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FirstResponseAt: r.FirstResponseAt, ResolvedAt: r.ResolvedAt})
		}
		return out, nil
	}
	rows, err := s.Q.ListBuyerIssues(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, Issue{ID: r.ID, Number: r.Number, OrderID: r.OrderID, OrderNumber: r.OrderNumber, OrderItemID: r.OrderItemID,
			Type: r.Type, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FirstResponseAt: r.FirstResponseAt, ResolvedAt: r.ResolvedAt})
	}
	return out, nil
}

// Reply adds a message. A buyer reply moves WAITING_CUSTOMER back to
// IN_PROGRESS; support may set IN_PROGRESS, WAITING_CUSTOMER or RESOLVED.
func (s *Service) Reply(ctx context.Context, v Viewer, id uuid.UUID, body, status string) (Issue, error) {
	body, err := cleanBody(body)
	if err != nil {
		return Issue{}, err
	}
	if v.Support && status != "" && !supportStatuses[status] {
		return Issue{}, httpx.ErrValidation.WithDetail("Status must be IN_PROGRESS, WAITING_CUSTOMER or RESOLVED")
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		is, err := q.GetIssueForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !v.Support && is.BuyerID != v.ID) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch is.Status {
		case "CLOSED":
			return ErrClosed
		case "RESOLVED":
			if !v.Support {
				return ErrResolved
			}
		}
		role, next := "BUYER", is.Status
		if v.Support {
			role, next = "SUPPORT", status
			if next == "" {
				next = "IN_PROGRESS"
			}
		} else if is.Status == "WAITING_CUSTOMER" {
			next = "IN_PROGRESS"
		}
		mid, _ := uuid.NewV7()
		if _, err := q.AddIssueMessage(ctx, store.AddIssueMessageParams{ID: mid, IssueID: id, AuthorID: v.ID, AuthorRole: role, Body: body}); err != nil {
			return err
		}
		if _, err := q.SetIssueStatus(ctx, store.SetIssueStatusParams{ID: id, ToStatus: next, SupportReply: v.Support}); err != nil {
			return err
		}
		return s.event(ctx, tx, id, is.Number, is.BuyerID, next, role)
	})
	if err != nil {
		return Issue{}, err
	}
	return s.Get(ctx, v, id)
}

// Reopen moves a resolved issue back to support.
func (s *Service) Reopen(ctx context.Context, buyer, id uuid.UUID) (Issue, error) {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		is, err := q.GetIssueForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && is.BuyerID != buyer) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if is.Status != "RESOLVED" {
			return ErrNotReopen
		}
		if _, err := q.SetIssueStatus(ctx, store.SetIssueStatusParams{ID: id, ToStatus: "IN_PROGRESS"}); err != nil {
			return err
		}
		return s.event(ctx, tx, id, is.Number, is.BuyerID, "IN_PROGRESS", "BUYER")
	})
	if err != nil {
		return Issue{}, err
	}
	return s.Get(ctx, Viewer{ID: buyer}, id)
}

// CloseResolved closes issues resolved more than 7 days ago (daily job).
func (s *Service) CloseResolved(ctx context.Context) (int64, error) {
	return s.Q.CloseResolvedIssues(ctx)
}
