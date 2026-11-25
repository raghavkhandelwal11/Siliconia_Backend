// Package payment records payments and refunds and applies provider webhooks
// (LLD 8.3, 12). Checkout (T2.3) and membership (M3) build on it: they create
// a payment for their reference and react to the outbox events published here.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Outbox topics published when payments change.
const (
	TopicCaptured       = "payment.captured"
	TopicFailed         = "payment.failed"
	TopicRefunded       = "refund.completed"
	TopicMandateActive  = "mandate.active"
	TopicMandateFailed  = "mandate.failed"
	TopicMandateRevoked = "mandate.revoked"
)

var ErrPaymentDown = httpx.ErrUnavailable.WithDetail("The payment service is not responding. Try again in a minute")

// Hook lets a module (checkout, membership) react to a payment inside the
// webhook's transaction. It may return work to run after the commit, such as
// refunding a payment that arrived for an expired order.
type Hook func(ctx context.Context, tx pgx.Tx, q *store.Queries, p store.Payment) (after func(context.Context), err error)

// Service creates payments and applies webhooks.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Provider provider.Provider
	Log      *slog.Logger
	// Hooks by payment purpose (ORDER, MEMBERSHIP, RENEWAL).
	Hooks map[string]Hook
}

// Checkout is what the web app needs to send the shopper to pay.
type Checkout struct {
	PaymentID   uuid.UUID
	CheckoutURL string
}

// Start creates a payment for ref and a checkout at the provider. Calling it
// again with the same ref returns the same checkout.
func (s *Service) Start(ctx context.Context, purpose, ref string, account *uuid.UUID, amount int64, mandate bool, returnURL string) (Checkout, error) {
	p, err := s.Q.GetPaymentByRef(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		id, _ := uuid.NewV7()
		p, err = s.Q.CreatePayment(ctx, store.CreatePaymentParams{ID: id, Purpose: purpose, Ref: ref, AccountID: account, Provider: s.Provider.Name(), AmountPaise: amount})
	}
	if err != nil {
		return Checkout{}, err
	}
	o, err := s.Provider.CreateOrder(ctx, ref, amount, mandate, returnURL) // idempotent on ref at the provider
	if err != nil {
		s.Log.Error("payment create", "ref", ref, "err", err)
		return Checkout{}, ErrPaymentDown
	}
	if err := s.Q.SetProviderOrder(ctx, store.SetProviderOrderParams{ID: p.ID, ProviderOrderID: &o.ProviderOrderID}); err != nil {
		return Checkout{}, err
	}
	if err := s.Q.SetPaymentCheckoutURL(ctx, store.SetPaymentCheckoutURLParams{ID: p.ID, CheckoutUrl: &o.CheckoutURL}); err != nil {
		return Checkout{}, err
	}
	return Checkout{PaymentID: p.ID, CheckoutURL: o.CheckoutURL}, nil
}

// ChargeMandate collects a renewal through a standing mandate. The capture
// arrives by webhook like any other payment (purpose RENEWAL).
func (s *Service) ChargeMandate(ctx context.Context, purpose, ref string, account *uuid.UUID, amount int64, mandateID string) (uuid.UUID, error) {
	p, err := s.Q.GetPaymentByRef(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		id, _ := uuid.NewV7()
		p, err = s.Q.CreatePayment(ctx, store.CreatePaymentParams{ID: id, Purpose: purpose, Ref: ref, AccountID: account, Provider: s.Provider.Name(), AmountPaise: amount})
	}
	if err != nil {
		return uuid.Nil, err
	}
	st, err := s.Provider.ChargeMandate(ctx, mandateID, amount, ref)
	if err != nil {
		s.Log.Error("mandate charge", "ref", ref, "err", err)
		return p.ID, ErrPaymentDown
	}
	return p.ID, s.Q.SetProviderOrder(ctx, store.SetProviderOrderParams{ID: p.ID, ProviderOrderID: &st.ProviderOrderID})
}

// RevokeMandate stops future renewals at the provider.
func (s *Service) RevokeMandate(ctx context.Context, mandateID string) error {
	if err := s.Provider.RevokeMandate(ctx, mandateID); err != nil && !errors.Is(err, provider.ErrRejected) {
		return ErrPaymentDown
	}
	return nil
}

// Refund sends money back for a captured payment.
func (s *Service) Refund(ctx context.Context, paymentID uuid.UUID, amount int64, electrons int32, ref, reason string) (store.Refund, error) {
	p, err := s.Q.GetPayment(ctx, paymentID)
	if err != nil {
		return store.Refund{}, err
	}
	if p.Status != "CAPTURED" || p.ProviderPaymentID == nil {
		return store.Refund{}, httpx.NewError(http.StatusConflict, "NOT_REFUNDABLE", "Only captured payments can be refunded")
	}
	done, err := s.Q.RefundedTotal(ctx, paymentID)
	if err != nil {
		return store.Refund{}, err
	}
	if done+amount > p.AmountPaise {
		return store.Refund{}, httpx.NewError(http.StatusConflict, "REFUND_TOO_LARGE", "The refund is more than what was paid")
	}
	id, _ := uuid.NewV7()
	r, err := s.Q.CreateRefund(ctx, store.CreateRefundParams{ID: id, PaymentID: paymentID, Ref: ref, AmountPaise: amount, Electrons: electrons, Reason: &reason})
	if err != nil {
		return store.Refund{}, err
	}
	pr, err := s.Provider.Refund(ctx, *p.ProviderPaymentID, amount, ref)
	if err != nil {
		s.Log.Error("refund create", "ref", ref, "err", err)
		return r, ErrPaymentDown // stays REQUESTED; a retry job picks it up
	}
	if err := s.Q.SetRefundProvider(ctx, store.SetRefundProviderParams{ID: r.ID, ProviderRefundID: &pr.ProviderRefundID}); err != nil {
		return r, err
	}
	r.Status, r.ProviderRefundID = "PROCESSING", &pr.ProviderRefundID
	return r, nil
}

// HandleWebhook verifies, de-duplicates and applies one provider event.
// The caller replies 200 only when this returns nil (after the commit).
func (s *Service) HandleWebhook(r *http.Request) error {
	evt, err := s.Provider.VerifyWebhook(r)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(evt)
	var after []func(context.Context)
	err = db.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.RecordWebhook(r.Context(), store.RecordWebhookParams{Provider: s.Provider.Name(), EventID: evt.ID, Payload: raw}); errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate delivery: already applied
		} else if err != nil {
			return err
		}
		fn, err := s.apply(r.Context(), tx, q, evt)
		if err != nil {
			return err
		}
		if fn != nil {
			after = append(after, fn)
		}
		return q.MarkWebhookProcessed(r.Context(), store.MarkWebhookProcessedParams{Provider: s.Provider.Name(), EventID: evt.ID})
	})
	if err != nil {
		return err
	}
	for _, fn := range after {
		fn(context.WithoutCancel(r.Context()))
	}
	return nil
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, q *store.Queries, e provider.Event) (func(context.Context), error) {
	switch e.Type {
	case "payment.captured", "payment.failed":
		p, err := q.GetPaymentByProviderOrder(ctx, ptr(e.Str("orderId")))
		if errors.Is(err, pgx.ErrNoRows) {
			s.Log.Warn("webhook for unknown payment", "event", e.ID, "order", e.Str("orderId"))
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		method := store.NullPayMethod{PayMethod: store.PayMethod(e.Str("method")), Valid: e.Str("method") != ""}
		var updated store.Payment
		topic := TopicCaptured
		if e.Type == "payment.captured" {
			updated, err = q.MarkPaymentCaptured(ctx, store.MarkPaymentCapturedParams{ID: p.ID, ProviderPaymentID: ptr(e.Str("paymentId")), Method: method, MandateID: ptr(e.Str("mandateId"))})
		} else {
			topic = TopicFailed
			updated, err = q.MarkPaymentFailed(ctx, store.MarkPaymentFailedParams{ID: p.ID, ProviderPaymentID: ptr(e.Str("paymentId")), Method: method, FailureCode: ptr(e.Str("failureCode"))})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // already in a final state
		}
		if err != nil {
			return nil, err
		}
		if _, err = outbox.Add(ctx, tx, topic, updated.Ref, map[string]any{
			"paymentId": updated.ID, "purpose": updated.Purpose, "ref": updated.Ref, "amountPaise": updated.AmountPaise,
			"status": updated.Status, "failureCode": e.Str("failureCode"), "mandateId": e.Str("mandateId"),
		}); err != nil {
			return nil, err
		}
		if hook := s.Hooks[updated.Purpose]; hook != nil {
			return hook(ctx, tx, q, updated)
		}
		return nil, nil
	case "refund.processed", "refund.failed":
		status := "REFUNDED"
		if e.Type == "refund.failed" {
			status = "FAILED"
		}
		r, err := q.MarkRefundDone(ctx, store.MarkRefundDoneParams{ProviderRefundID: ptr(e.Str("refundId")), Status: status})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		_, err = outbox.Add(ctx, tx, TopicRefunded, r.Ref, map[string]any{"refundId": r.ID, "paymentId": r.PaymentID, "ref": r.Ref, "amountPaise": r.AmountPaise, "status": r.Status})
		return nil, err
	case "mandate.active", "mandate.failed", "mandate.revoked":
		_, err := outbox.Add(ctx, tx, e.Type, e.Str("mandateId"), e.Data) // membership (M3) reacts
		return nil, err
	}
	s.Log.Info("webhook ignored", "type", e.Type, "event", e.ID)
	return nil, nil
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Handler serves POST /webhooks/payment.
type Handler struct{ Svc *Service }

// Routes mounts the webhook (no session, no CSRF: it is signed).
func (h *Handler) Routes(r interface {
	Post(string, http.HandlerFunc)
}) {
	r.Post("/webhooks/payment", h.webhook)
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.HandleWebhook(r); err != nil {
		h.Svc.Log.Warn("payment webhook rejected", "err", err)
		// 400 for bad signatures (no retry helps); 500 for our failures (provider retries).
		if errors.Is(err, context.Canceled) {
			return
		}
		status := http.StatusInternalServerError
		if isSignature(err) {
			status = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, http.StatusText(status)), status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func isSignature(err error) bool {
	return errors.Is(err, whsig.ErrBadSignature) || err.Error() == "payment: malformed event"
}
