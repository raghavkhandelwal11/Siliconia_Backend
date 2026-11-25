// Package admin holds M5: seller payouts, the admin console, configuration,
// legal documents, ledger adjustments, health checks and data-hygiene jobs
// (LLD 5.3, 8.10-8.12).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Service runs M5.
type Service struct {
	Pool   *pgxpool.Pool
	Q      *store.Queries
	Box    *crypto.Box
	Payout *payout.Sim
	Log    *slog.Logger
}

var (
	errState     = httpx.NewError(http.StatusConflict, "INVALID_STATE", "This action is not possible in the current state")
	errSameAdmin = httpx.NewError(http.StatusConflict, "SECOND_ADMIN_REQUIRED", "A different admin must approve this")
	// ErrNothingDue means no delivered sub-order is due for payout.
	ErrNothingDue = httpx.NewError(http.StatusConflict, "NOTHING_DUE", "No delivered orders are due for payout")
)

func newID() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

// configInt reads an integer config value, falling back to def.
func configInt(ctx context.Context, q *store.Queries, key string, def int64) int64 {
	if c, err := q.GetConfig(ctx, key); err == nil {
		var n int64
		if json.Unmarshal(c.Value, &n) == nil {
			return n
		}
	}
	return def
}

func audit(ctx context.Context, q *store.Queries, actor uuid.UUID, action, targetType, targetID string, before, after any) error {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	return q.InsertAuditM5(ctx, store.InsertAuditM5Params{ID: newID(), ActorID: &actor, Action: action, TargetType: targetType,
		TargetID: targetID, Before: b, After: a})
}

// CreateBatch drafts this week's payouts: one per seller, covering every
// delivered sub-order whose return window has closed. TDS under section
// 194-O is withheld at payouts.tds_bps of gross (default 0.1%).
func (s *Service) CreateBatch(ctx context.Context, actor *uuid.UUID) (uuid.UUID, error) {
	batch := newID()
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		rows, err := q.SettleableSubOrders(ctx)
		if err != nil {
			return err
		}
		tdsBps := configInt(ctx, q, "payouts.tds_bps", 10)
		bySeller := map[uuid.UUID][]store.SettleableSubOrdersRow{}
		var order []uuid.UUID
		for _, r := range rows {
			if r.Gross <= 0 { // everything returned: nothing to pay
				continue
			}
			if _, ok := bySeller[r.SellerID]; !ok {
				order = append(order, r.SellerID)
			}
			bySeller[r.SellerID] = append(bySeller[r.SellerID], r)
		}
		if len(order) == 0 {
			return ErrNothingDue
		}
		today := time.Now().In(time.FixedZone("IST", 19800))
		if _, err := q.CreateBatch(ctx, store.CreateBatchParams{ID: batch,
			WeekEnding: pgtype.Date{Time: time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC), Valid: true}}); err != nil {
			return err
		}
		for _, seller := range order {
			var gross, commission int64
			for _, r := range bySeller[seller] {
				gross += r.Gross
				commission += r.Commission
			}
			tds := gross * tdsBps / 10000
			p, err := q.CreatePayout(ctx, store.CreatePayoutParams{ID: newID(), BatchID: batch, SellerID: seller, GrossPaise: gross,
				CommissionPaise: commission, TdsPaise: tds, NetPaise: gross - commission - tds})
			if err != nil {
				return err
			}
			for _, r := range bySeller[seller] {
				if err := q.AddPayoutLine(ctx, store.AddPayoutLineParams{PayoutID: p.ID, SubOrderID: r.ID, GrossPaise: r.Gross, CommissionPaise: r.Commission}); err != nil {
					return err
				}
				// The primary key stops a sub-order joining two live batches.
				if err := q.ClaimSubOrder(ctx, store.ClaimSubOrderParams{SubOrderID: r.ID, PayoutID: p.ID}); err != nil {
					return err
				}
			}
		}
		if actor != nil {
			return audit(ctx, q, *actor, "payout_batch.create", "payout_batch", batch.String(), nil, map[string]any{"payouts": len(order)})
		}
		return nil
	})
	return batch, err
}

// Approve marks a draft batch ready to send.
func (s *Service) Approve(ctx context.Context, admin, batch uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		b, err := q.GetBatchForUpdate(ctx, batch)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if b.Status != "DRAFT" {
			return errState
		}
		if err := q.SetBatchStatus(ctx, store.SetBatchStatusParams{ID: batch, Status: "APPROVED", ApprovedBy: &admin}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "payout_batch.approve", "payout_batch", batch.String(), b.Status, "APPROVED")
	})
}

// Cancel drops a batch that has not been sent; its sub-orders return to the
// next batch.
func (s *Service) Cancel(ctx context.Context, admin, batch uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		b, err := q.GetBatchForUpdate(ctx, batch)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if b.Status != "DRAFT" && b.Status != "APPROVED" {
			return errState
		}
		ps, err := q.BatchPayouts(ctx, batch)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if err := q.FinishPayout(ctx, store.FinishPayoutParams{ID: p.ID, Status: "CANCELLED"}); err != nil {
				return err
			}
			if err := q.ReleasePayoutClaims(ctx, p.ID); err != nil {
				return err
			}
		}
		if err := q.SetBatchStatus(ctx, store.SetBatchStatusParams{ID: batch, Status: "CANCELLED"}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "payout_batch.cancel", "payout_batch", batch.String(), b.Status, "CANCELLED")
	})
}

// Release sends an approved batch to the bank. Transfers are idempotent on
// the "PO-<payout>" reference; the webhook settles each payout.
func (s *Service) Release(ctx context.Context, admin, batch uuid.UUID) error {
	type send struct {
		id            uuid.UUID
		ref           string
		amount        int64
		account, ifsc string
	}
	var sends []send
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		b, err := q.GetBatchForUpdate(ctx, batch)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if b.Status != "APPROVED" {
			return errState
		}
		ps, err := q.BatchPayouts(ctx, batch)
		if err != nil {
			return err
		}
		for _, p := range ps {
			var acct []byte
			if len(p.BankEnc) > 0 {
				acct, err = s.Box.Open(p.BankEnc, []byte(p.SellerID.String()+":bank"))
			}
			if len(p.BankEnc) == 0 || err != nil || p.Ifsc == nil {
				reason := "No usable bank account on file"
				if err := q.SetPayoutFailedToSend(ctx, store.SetPayoutFailedToSendParams{ID: p.ID, FailureReason: &reason}); err != nil {
					return err
				}
				if err := q.ReleasePayoutClaims(ctx, p.ID); err != nil {
					return err
				}
				continue
			}
			ref := "PO-" + p.ID.String()
			if err := q.SetPayoutSent(ctx, store.SetPayoutSentParams{ID: p.ID, ProviderRef: &ref}); err != nil {
				return err
			}
			sends = append(sends, send{p.ID, ref, p.NetPaise, string(acct), *p.Ifsc})
		}
		if err := q.SetBatchStatus(ctx, store.SetBatchStatusParams{ID: batch, Status: "RELEASED", ReleasedBy: &admin}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "payout_batch.release", "payout_batch", batch.String(), "APPROVED", map[string]any{"sent": len(sends)})
	})
	if err != nil {
		return err
	}
	for _, x := range sends {
		if err := s.Payout.Transfer(ctx, x.ref, x.amount, x.account, x.ifsc); err != nil {
			s.Log.Error("seller payout transfer", "ref", x.ref, "err", err)
			s.failToSend(ctx, x.id, "The bank transfer could not be started")
		}
	}
	return nil
}

func (s *Service) failToSend(ctx context.Context, id uuid.UUID, reason string) {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := q.FinishPayout(ctx, store.FinishPayoutParams{ID: id, Status: "FAILED", FailureReason: &reason}); err != nil {
			return err
		}
		return q.ReleasePayoutClaims(ctx, id)
	})
	if err != nil {
		s.Log.Error("seller payout fail", "id", id, "err", err)
	}
}

// OnPayoutEvent settles a seller payout from the bank webhook, inside the
// webhook's transaction. A failed transfer frees its sub-orders for the next
// batch; a paid one marks them settled.
func (s *Service) OnPayoutEvent(ctx context.Context, tx pgx.Tx, e payout.Event) error {
	q := s.Q.WithTx(tx)
	p, err := q.GetPayoutByRefForUpdate(ctx, &e.Ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("payout: unknown ref %s", e.Ref)
	}
	if err != nil {
		return err
	}
	if p.Status != "PROCESSING" {
		return nil
	}
	if e.Status == "PAID" {
		if err := q.FinishPayout(ctx, store.FinishPayoutParams{ID: p.ID, Status: "PAID", BankRef: &e.BankRef}); err != nil {
			return err
		}
		if err := q.SettlePayoutSubOrders(ctx, p.ID); err != nil {
			return err
		}
	} else {
		if err := q.FinishPayout(ctx, store.FinishPayoutParams{ID: p.ID, Status: "FAILED", FailureReason: &e.Reason}); err != nil {
			return err
		}
		if err := q.ReleasePayoutClaims(ctx, p.ID); err != nil {
			return err
		}
	}
	_, err = outbox.Add(ctx, tx, "payout.finished", p.ID.String(), map[string]any{"accountId": p.SellerID, "status": e.Status, "net": p.NetPaise})
	return err
}

// RefreshVerifiedTags recomputes rolling 12-month sales and the verified
// seller tag (daily job; threshold in config, default ₹4,00,000).
func (s *Service) RefreshVerifiedTags(ctx context.Context) (int64, error) {
	return s.Q.RefreshVerifiedTags(ctx, configInt(ctx, s.Q, "verified_tag.threshold_paise", 40_000_000))
}
