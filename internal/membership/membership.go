// Package membership runs Electra Membership (LLD 8.8): buy for a year with
// money and/or Electrons, optional auto-renew through a payment mandate with
// a pre-debit notice, renewal retries, and lapse.
package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ids"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

var (
	ErrTooManyElectrons = httpx.ErrValidation.WithDetail("Use at most the membership fee in Electrons")
	ErrNoMandate        = httpx.NewError(http.StatusConflict, "MANDATE_REQUIRED", "Renew with auto-renew on to set up automatic payments")
	ErrNotMember        = httpx.NewError(http.StatusConflict, "NOT_A_MEMBER", "You don't have a membership yet")
)

// Service runs membership.
type Service struct {
	Pool      *pgxpool.Pool
	Q         *store.Queries
	Payments  *payment.Service
	PublicURL string
	Log       *slog.Logger
}

// IsMember reports whether the account has an active membership now.
func IsMember(ctx context.Context, q *store.Queries, account uuid.UUID) bool {
	ok, err := q.IsMember(ctx, account)
	return err == nil && ok
}

func fee(ctx context.Context, q *store.Queries) int64 {
	if c, err := q.GetConfig(ctx, "membership.fee_paise"); err == nil {
		var n int64
		if json.Unmarshal(c.Value, &n) == nil && n > 0 {
			return n
		}
	}
	return 65000
}

// View is the membership page.
type View struct {
	Status       string     `json:"status"` // NONE, ACTIVE, LAPSED
	IsMember     bool       `json:"isMember"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	AutoRenew    bool       `json:"autoRenew"`
	HasMandate   bool       `json:"hasMandate"`
	NextChargeAt *time.Time `json:"nextChargeAt,omitempty"`
	FeePaise     int64      `json:"feePaise"`
	Pending      *Checkout  `json:"pending,omitempty"`
}

// Checkout is a purchase waiting for payment.
type Checkout struct {
	PurchaseID  uuid.UUID `json:"purchaseId"`
	CheckoutURL string    `json:"checkoutUrl,omitempty"`
	Status      string    `json:"status"` // ACTIVE or PENDING_PAYMENT
}

// Get returns the account's membership.
func (s *Service) Get(ctx context.Context, account uuid.UUID) (View, error) {
	v := View{Status: "NONE", FeePaise: fee(ctx, s.Q)}
	m, err := s.Q.GetMembership(ctx, account)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return View{}, err
	}
	if err == nil {
		v.Status, v.ExpiresAt, v.AutoRenew, v.HasMandate = m.Status, &m.ExpiresAt, m.AutoRenew, m.MandateID != nil
		v.IsMember = m.Status == "ACTIVE" && m.ExpiresAt.After(time.Now())
		if v.IsMember && m.AutoRenew && m.MandateID != nil {
			v.NextChargeAt = &m.ExpiresAt
		}
	}
	if p, err := s.Q.PendingPurchase(ctx, account); err == nil && p.PaymentID != nil {
		if pay, err := s.Q.GetPayment(ctx, *p.PaymentID); err == nil && pay.CheckoutUrl != nil && pay.Status != "CAPTURED" {
			v.Pending = &Checkout{PurchaseID: p.ID, CheckoutURL: *pay.CheckoutUrl, Status: "PENDING_PAYMENT"}
		}
	}
	return v, nil
}

// Purchase buys (or renews early) a year of membership. Electrons pay first;
// the rest goes to the payment page, with a mandate when autoRenew is on.
func (s *Service) Purchase(ctx context.Context, account uuid.UUID, useElectrons int32, autoRenew bool) (Checkout, error) {
	if useElectrons < 0 {
		return Checkout{}, ErrTooManyElectrons
	}
	var p store.MembershipPurchase
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if prev, err := q.PendingPurchase(ctx, account); err == nil {
			p = prev // one purchase at a time: send them back to the same payment page
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		f := fee(ctx, q)
		if int64(useElectrons)*100 > f {
			return ErrTooManyElectrons
		}
		amount := f - int64(useElectrons)*100
		status := "PENDING"
		if amount == 0 {
			status = "PAID"
		}
		var err error
		p, err = q.CreateMembershipPurchase(ctx, store.CreateMembershipPurchaseParams{ID: newID(), Ref: ids.Readable("MB", time.Now()), AccountID: account,
			Kind: "PURCHASE", ElectronsUsed: useElectrons, AmountPaise: amount, AutoRenew: autoRenew, Status: status})
		if err != nil {
			return err
		}
		if err := electrons.Spend(ctx, q, account, useElectrons, "SPEND_MEMBERSHIP", electrons.RefMembership, p.ID); err != nil {
			return err
		}
		if amount == 0 {
			// Paid in full with Electrons: no mandate, so auto-renew needs a later money purchase.
			return activate(ctx, tx, q, account, false, nil, p.Ref)
		}
		return nil
	})
	if err != nil {
		return Checkout{}, err
	}
	if p.Status == "PAID" {
		return Checkout{PurchaseID: p.ID, Status: "ACTIVE"}, nil
	}
	ret := strings.TrimRight(s.PublicURL, "/") + "/membership?purchase=" + p.ID.String()
	co, err := s.Payments.Start(ctx, "MEMBERSHIP", p.Ref, &account, p.AmountPaise, p.AutoRenew, ret)
	if err != nil {
		return Checkout{}, err
	}
	if err := s.Q.SetPurchasePayment(ctx, store.SetPurchasePaymentParams{ID: p.ID, PaymentID: &co.PaymentID}); err != nil {
		return Checkout{}, err
	}
	return Checkout{PurchaseID: p.ID, CheckoutURL: co.CheckoutURL, Status: "PENDING_PAYMENT"}, nil
}

func newID() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

func activate(ctx context.Context, tx pgx.Tx, q *store.Queries, account uuid.UUID, autoRenew bool, mandate *string, ref string) error {
	m, err := q.ActivateMembership(ctx, store.ActivateMembershipParams{ID: newID(), AccountID: account, AutoRenew: autoRenew && mandate != nil, MandateID: mandate})
	if err != nil {
		return err
	}
	_, err = outbox.Add(ctx, tx, "membership.activated", account.String(), map[string]any{
		"accountId": account, "expiresAt": m.ExpiresAt, "ref": ref, "autoRenew": m.AutoRenew})
	return err
}

// OnPayment is the payment hook for MEMBERSHIP and RENEWAL payments.
func (s *Service) OnPayment(ctx context.Context, tx pgx.Tx, q *store.Queries, p store.Payment) (func(context.Context), error) {
	if p.Status != "CAPTURED" || p.AccountID == nil {
		return nil, nil // failures are retried on the payment page or by the renewal job
	}
	pur, err := q.GetPurchaseByRefForUpdate(ctx, p.Ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pur.Status == "PAID" {
		return nil, nil
	}
	if err := q.SetPurchaseStatus(ctx, store.SetPurchaseStatusParams{ID: pur.ID, Status: "PAID"}); err != nil {
		return nil, err
	}
	mandate := p.MandateID
	if pur.Kind == "RENEWAL" {
		if m, err := q.GetMembership(ctx, pur.AccountID); err == nil {
			mandate = m.MandateID
		}
	}
	return nil, activate(ctx, tx, q, pur.AccountID, pur.AutoRenew, mandate, pur.Ref)
}

// SetAutoRenew turns renewals off (revoking the mandate; benefits last until
// expiry) or back on when a mandate still exists.
func (s *Service) SetAutoRenew(ctx context.Context, account uuid.UUID, on bool) (View, error) {
	m, err := s.Q.GetMembership(ctx, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotMember
	}
	if err != nil {
		return View{}, err
	}
	if on && m.MandateID == nil {
		return View{}, ErrNoMandate
	}
	if !on && m.MandateID != nil {
		if err := s.Payments.RevokeMandate(ctx, *m.MandateID); err != nil {
			return View{}, err
		}
	}
	if err := s.Q.SetAutoRenew(ctx, store.SetAutoRenewParams{AccountID: account, AutoRenew: on}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, account)
}

// Sweep gives back Electrons held by purchases never paid (every 30 s job).
func (s *Service) Sweep(ctx context.Context) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		stale, err := q.StalePurchases(ctx)
		if err != nil {
			return err
		}
		for _, p := range stale {
			if err := q.SetPurchaseStatus(ctx, store.SetPurchaseStatusParams{ID: p.ID, Status: "EXPIRED"}); err != nil {
				return err
			}
			if _, err := electrons.Restore(ctx, q, p.AccountID, electrons.RefMembership, p.ID, p.ElectronsUsed, "Membership payment not completed"); err != nil {
				return err
			}
		}
		return nil
	})
}

// Renew is the daily job (09:00 IST): pre-debit notices two days ahead,
// mandate charges on the expiry day with two retries, then lapse.
func (s *Service) Renew(ctx context.Context) (charged, lapsed int, err error) {
	type charge struct {
		account uuid.UUID
		mandate string
		ref     string
		amount  int64
	}
	var charges []charge
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		due, err := q.MembershipsDue(ctx)
		if err != nil {
			return err
		}
		f := fee(ctx, q)
		now := time.Now()
		for _, m := range due {
			canCharge := m.AutoRenew && m.MandateID != nil
			if m.ExpiresAt.After(now) {
				if canCharge && (m.PredebitSentFor == nil || !m.PredebitSentFor.Equal(m.ExpiresAt)) {
					if _, err := outbox.Add(ctx, tx, "membership.predebit", m.AccountID.String(), map[string]any{
						"accountId": m.AccountID, "amountPaise": f, "chargeOn": m.ExpiresAt}); err != nil {
						return err
					}
					if err := q.MarkPredebitSent(ctx, m.ID); err != nil {
						return err
					}
				}
				continue
			}
			if !canCharge || m.RenewAttempts >= 3 {
				if err := q.LapseMembership(ctx, m.ID); err != nil {
					return err
				}
				if _, err := outbox.Add(ctx, tx, "membership.lapsed", m.AccountID.String(), map[string]any{"accountId": m.AccountID}); err != nil {
					return err
				}
				lapsed++
				continue
			}
			ref := fmt.Sprintf("RN-%s-%s-%d", m.ID.String()[:8], m.ExpiresAt.Format("060102"), m.RenewAttempts+1)
			if _, err := q.CreateMembershipPurchase(ctx, store.CreateMembershipPurchaseParams{ID: newID(), Ref: ref, AccountID: m.AccountID,
				Kind: "RENEWAL", AmountPaise: f, AutoRenew: true, Status: "PENDING"}); err != nil {
				return err
			}
			if err := q.BumpRenewAttempt(ctx, m.ID); err != nil {
				return err
			}
			charges = append(charges, charge{m.AccountID, *m.MandateID, ref, f})
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	for _, c := range charges { // after commit: the capture webhook activates the renewal
		acc := c.account
		if _, err := s.Payments.ChargeMandate(ctx, "RENEWAL", c.ref, &acc, c.amount, c.mandate); err != nil {
			s.Log.Warn("renewal charge failed; retried tomorrow", "ref", c.ref, "err", err)
			continue
		}
		charged++
	}
	return charged, lapsed, nil
}
