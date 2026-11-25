package admin

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// SetAccountStatus suspends or reactivates a non-admin account. Suspending
// signs it out everywhere: refresh sessions are revoked at once and access
// tokens lapse within their short lifetime.
func (s *Service) SetAccountStatus(ctx context.Context, admin, account uuid.UUID, suspend bool, reason string) error {
	status := "ACTIVE"
	if suspend {
		status = "SUSPENDED"
		if strings.TrimSpace(reason) == "" {
			return httpx.ErrValidation.WithDetail("Give a reason for the suspension")
		}
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.SetAccountStatus(ctx, store.SetAccountStatusParams{ID: account, Status: status}); errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		} else if err != nil {
			return err
		}
		if suspend {
			if err := q.RevokeAccountSessions(ctx, account); err != nil {
				return err
			}
		}
		return audit(ctx, q, admin, "account."+strings.ToLower(status), "account", account.String(), nil, map[string]any{"reason": reason})
	})
}

// SetConfig changes one existing key. expectedVersion guards against two
// admins editing at once; every change is kept in config_history.
func (s *Service) SetConfig(ctx context.Context, admin uuid.UUID, key string, value json.RawMessage, expectedVersion int32) error {
	if !json.Valid(value) || len(value) > 8<<10 {
		return httpx.ErrValidation.WithDetail("The value must be valid JSON under 8 KB")
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		c, err := q.GetConfigForUpdate(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Version != expectedVersion {
			return errState.WithDetail("Someone else changed this setting; reload and try again")
		}
		if err := q.SetConfig(ctx, store.SetConfigParams{Key: key, Value: value, UpdatedBy: &admin}); err != nil {
			return err
		}
		if err := q.AddConfigHistory(ctx, store.AddConfigHistoryParams{Key: key, Version: c.Version + 1, Value: value, ChangedBy: &admin}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "config.set", "config", key, json.RawMessage(c.Value), value)
	})
}

// CategoryRates are a category's commission and GST.
type CategoryRates struct {
	CommissionBps *int32  `json:"commissionBps"`
	GstRateBps    int32   `json:"gstRateBps"`
	Hsn           *string `json:"hsn"`
}

// SetCategoryRates changes commission and GST for new orders in a category.
func (s *Service) SetCategoryRates(ctx context.Context, admin, id uuid.UUID, in CategoryRates) (store.UpdateCategoryRatesRow, error) {
	if (in.CommissionBps != nil && (*in.CommissionBps < 0 || *in.CommissionBps > 5000)) || in.GstRateBps < 0 || in.GstRateBps > 4000 {
		return store.UpdateCategoryRatesRow{}, httpx.ErrValidation.WithDetail("Commission must be 0-50% and GST 0-40%")
	}
	var out store.UpdateCategoryRatesRow
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		var err error
		out, err = q.UpdateCategoryRates(ctx, store.UpdateCategoryRatesParams{ID: id, CommissionBps: in.CommissionBps, GstRateBps: in.GstRateBps, Hsn: in.Hsn})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit(ctx, q, admin, "category.rates", "category", id.String(), nil, in)
	})
	return out, err
}

var legalKinds = map[string]bool{"TERMS": true, "PRIVACY": true, "SELLER_TERMS": true, "CREATOR_TERMS": true, "RETURNS": true}

// PublishLegal adds a new version; for terms and privacy every account is
// asked to accept it at next sign-in.
func (s *Service) PublishLegal(ctx context.Context, admin uuid.UUID, kind, title, body string) (store.LegalDoc, error) {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if !legalKinds[kind] || title == "" || body == "" || len(body) > 200<<10 {
		return store.LegalDoc{}, httpx.ErrValidation.WithDetail("Choose a document type and give a title and text (up to 200 KB)")
	}
	var d store.LegalDoc
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		var err error
		if d, err = q.PublishLegal(ctx, store.PublishLegalParams{ID: newID(), Kind: kind, Title: title, BodyMd: body, PublishedBy: &admin}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "legal.publish", "legal_doc", d.ID.String(), nil, map[string]any{"kind": kind, "version": d.Version})
	})
	return d, err
}

// AcceptLegal records acceptance of the given documents.
func (s *Service) AcceptLegal(ctx context.Context, account uuid.UUID, docs []uuid.UUID, ip string) error {
	var addr *netip.Addr
	if a, err := netip.ParseAddr(ip); err == nil {
		addr = &a
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		for _, d := range docs {
			if err := q.AcceptLegal(ctx, store.AcceptLegalParams{AccountID: account, DocID: d, Ip: addr}); err != nil {
				return httpx.ErrValidation.WithDetail("Unknown document")
			}
		}
		return nil
	})
}

// RequestAdjustment asks for an Electrons correction; a second admin applies it.
func (s *Service) RequestAdjustment(ctx context.Context, admin, account uuid.UUID, amount int32, reason string) (store.LedgerAdjustmentRequest, error) {
	reason = strings.TrimSpace(reason)
	if amount == 0 || amount > 100_000 || amount < -100_000 || len(reason) < 5 {
		return store.LedgerAdjustmentRequest{}, httpx.ErrValidation.WithDetail("Give a non-zero amount up to 1,00,000 and a reason")
	}
	var r store.LedgerAdjustmentRequest
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		var err error
		if r, err = q.RequestAdjustment(ctx, store.RequestAdjustmentParams{ID: newID(), AccountID: account, Amount: amount, Reason: reason, RequestedBy: admin}); err != nil {
			return httpx.ErrValidation.WithDetail("Unknown account")
		}
		return audit(ctx, q, admin, "ledger_adjustment.request", "account", account.String(), nil, map[string]any{"amount": amount, "reason": reason})
	})
	return r, err
}

// DecideAdjustment applies or rejects a request. The requester can never
// approve their own request (four-eyes rule, also enforced by the database).
func (s *Service) DecideAdjustment(ctx context.Context, admin, id uuid.UUID, approve bool) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		r, err := q.GetAdjustmentForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.Status != "REQUESTED" {
			return errState
		}
		if r.RequestedBy == admin {
			return errSameAdmin
		}
		status := "REJECTED"
		if approve {
			status = "APPLIED"
			note := "Adjustment: " + r.Reason
			if r.Amount > 0 {
				err = electrons.Credit(ctx, q, r.AccountID, "ADJUSTMENT", "ADMIN_ADJUSTMENT", "ADJUSTMENT", r.ID, r.Amount, note)
			} else {
				err = electrons.Spend(ctx, q, r.AccountID, -r.Amount, "ADMIN_ADJUSTMENT", "ADJUSTMENT", r.ID)
			}
			if err != nil {
				return err
			}
		}
		if err := q.DecideAdjustment(ctx, store.DecideAdjustmentParams{ID: id, Status: status, ApprovedBy: &admin}); err != nil {
			return err
		}
		return audit(ctx, q, admin, "ledger_adjustment."+strings.ToLower(status), "account", r.AccountID.String(), nil, map[string]any{"amount": r.Amount})
	})
}

// csvCell neutralises spreadsheet formulas in exported text.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func paise(p int64) string { return strconv.FormatFloat(float64(p)/100, 'f', 2, 64) }

// Report renders a CSV report: orders, payouts or audit.
func (s *Service) Report(ctx context.Context, name string) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	write := func(rec ...string) {
		for i := range rec {
			rec[i] = csvCell(rec[i])
		}
		_ = w.Write(rec)
	}
	switch name {
	case "orders":
		rows, err := s.Q.AdminOrders(ctx, "")
		if err != nil {
			return nil, err
		}
		write("number", "placed_at", "status", "pay_method", "payable_inr", "buyer", "synthetic")
		for _, r := range rows {
			write(r.Number, r.PlacedAt.Format(time.RFC3339), r.Status, r.PayMethod, paise(r.PayablePaise), r.BuyerName, strconv.FormatBool(r.IsSynthetic))
		}
	case "payouts":
		bs, err := s.Q.ListBatches(ctx)
		if err != nil {
			return nil, err
		}
		write("batch", "week_ending", "store", "status", "gross_inr", "commission_inr", "tds_inr", "net_inr", "bank_ref")
		for _, b := range bs {
			ps, err := s.Q.BatchPayouts(ctx, b.ID)
			if err != nil {
				return nil, err
			}
			for _, p := range ps {
				write(b.ID.String(), b.WeekEnding.Time.Format("2006-01-02"), p.StoreName, p.Status, paise(p.GrossPaise), paise(p.CommissionPaise),
					paise(p.TdsPaise), paise(p.NetPaise), deref(p.BankRef))
			}
		}
	case "audit":
		rows, err := s.Q.ListAudit(ctx, "")
		if err != nil {
			return nil, err
		}
		write("at", "actor", "action", "target_type", "target_id")
		for _, r := range rows {
			write(r.At.Format(time.RFC3339), deref(r.ActorName), r.Action, r.TargetType, r.TargetID)
		}
	default:
		return nil, httpx.ErrNotFound
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Statement is a seller's CSV for one payout.
func (s *Service) Statement(ctx context.Context, seller, payoutID uuid.UUID, isAdmin bool) ([]byte, error) {
	rows, err := s.Q.PayoutStatement(ctx, store.PayoutStatementParams{PayoutID: payoutID, SellerID: seller, Admin: isAdmin})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, httpx.ErrNotFound
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"order", "delivered_at", "gross_inr", "commission_inr"})
	for _, r := range rows {
		d := ""
		if r.DeliveredAt != nil {
			d = r.DeliveredAt.Format("2006-01-02")
		}
		_ = w.Write([]string{csvCell(r.Number), d, paise(r.GrossPaise), paise(r.CommissionPaise)})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
