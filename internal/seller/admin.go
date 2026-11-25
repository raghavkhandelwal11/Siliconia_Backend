package seller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// QueueItem is one row of the admin approval queue.
type QueueItem struct {
	AccountID    uuid.UUID  `json:"accountId"`
	Approval     string     `json:"approval"`
	BusinessType string     `json:"businessType,omitempty"`
	LegalName    string     `json:"legalName"`
	StoreName    string     `json:"storeName"`
	Phone        string     `json:"phone,omitempty"`
	PANVerified  bool       `json:"panVerified"`
	BankVerified bool       `json:"bankVerified"`
	HasGSTIN     bool       `json:"hasGstin"`
	SubmittedAt  *time.Time `json:"submittedAt,omitempty"`
	DecidedAt    *time.Time `json:"decidedAt,omitempty"`
}

// Queue lists seller applications in the given approval states.
func (s *Service) Queue(ctx context.Context, states []string) ([]QueueItem, error) {
	rows, err := s.Q.ListSellerApplications(ctx, states)
	if err != nil {
		return nil, err
	}
	out := make([]QueueItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, QueueItem{
			AccountID: r.ID, Approval: string(r.Approval), BusinessType: deref(r.BusinessType), LegalName: r.LegalName,
			StoreName: r.StoreName, Phone: deref(r.PhoneE164), PANVerified: r.PanVerifiedAt != nil,
			BankVerified: r.BankVerifiedAt != nil, HasGSTIN: r.Gstin != nil, SubmittedAt: r.SubmittedAt, DecidedAt: r.DecidedAt,
		})
	}
	return out, nil
}

// AdminView is the application as an admin sees it (still masked).
type AdminView struct {
	Application
	Phone        string `json:"phone,omitempty"`
	GSTINMatches bool   `json:"gstinMatchesPan"`
}

// Detail returns one application for review.
func (s *Service) Detail(ctx context.Context, accountID uuid.UUID) (AdminView, error) {
	acc, err := s.Q.GetAccount(ctx, accountID)
	if err != nil {
		return AdminView{}, err
	}
	if acc.Role != store.AccountRoleSELLER {
		return AdminView{}, httpx.ErrNotFound
	}
	app, err := s.Get(ctx, accountID)
	if err != nil {
		return AdminView{}, err
	}
	v := AdminView{Application: app, Phone: deref(acc.PhoneE164)}
	if app.GSTIN != "" {
		pan, err := s.pan(ctx, accountID)
		if err != nil {
			return AdminView{}, err
		}
		v.GSTINMatches = ValidGSTIN(app.GSTIN, pan)
	}
	return v, nil
}

// Decision is an admin's approve or reject.
type Decision struct {
	Approve bool
	Reason  string
}

// Actor is the admin making a decision.
type Actor struct {
	ID     uuid.UUID
	Acting string // non-empty while the admin views the site as another role
	IP     string
}

// Decide approves or rejects a submitted application, audits it and tells the seller.
func (s *Service) Decide(ctx context.Context, actor Actor, accountID uuid.UUID, d Decision) (AdminView, error) {
	if actor.Acting != "" {
		return AdminView{}, ErrActingDenied // LLD 4.2: approvals are blocked while switched
	}
	d.Reason = strings.TrimSpace(d.Reason)
	if !d.Approve && len([]rune(d.Reason)) < 10 {
		return AdminView{}, httpx.ErrValidation.WithDetail("Tell the seller what to fix (at least 10 characters)")
	}
	if len([]rune(d.Reason)) > 500 {
		return AdminView{}, httpx.ErrValidation.WithDetail("Keep the reason under 500 characters")
	}
	to := store.ApprovalStatusREJECTED
	if d.Approve {
		to = store.ApprovalStatusAPPROVED
	}
	var acc store.Account
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		before, err := q.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if before.Role != store.AccountRoleSELLER {
			return httpx.ErrNotFound
		}
		acc, err = q.SetApproval(ctx, store.SetApprovalParams{
			ID: accountID, Approval: to, ApprovalReason: ptr(d.Reason), FromStates: []string{"SUBMITTED", "UNDER_REVIEW"},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPending
		}
		if err != nil {
			return err
		}
		if err := q.DecideSellerApplication(ctx, store.DecideSellerApplicationParams{AccountID: accountID, DecidedBy: &actor.ID}); err != nil {
			return err
		}
		auditID, _ := uuid.NewV7()
		b, _ := json.Marshal(map[string]string{"approval": string(before.Approval)})
		a, _ := json.Marshal(map[string]string{"approval": string(to), "reason": d.Reason})
		if err := q.InsertAudit(ctx, store.InsertAuditParams{
			ID: auditID, ActorID: &actor.ID, Action: "seller.application_decided", TargetType: "account",
			TargetID: accountID.String(), Before: b, After: a, Ip: parseIP(actor.IP),
		}); err != nil {
			return err
		}
		_, err = outbox.Add(ctx, tx, "account.approval_changed", accountID.String(),
			map[string]string{"accountId": accountID.String(), "role": "SELLER", "approval": string(to)})
		return err
	})
	if err != nil {
		return AdminView{}, err
	}
	s.notify(ctx, acc, d)
	return s.Detail(ctx, accountID)
}

// notify sends the approval_result SMS. Failure is logged, not returned: the
// decision is already saved and shows in the seller portal.
func (s *Service) notify(ctx context.Context, acc store.Account, d Decision) {
	if s.Messages == nil || acc.PhoneE164 == nil {
		return
	}
	body := "Your Siliconics seller account is approved. You can now add your products."
	result := "approved"
	if !d.Approve {
		result = "rejected"
		body = fmt.Sprintf("Your Siliconics seller application needs changes: %s. Update it in the seller portal and submit again.", d.Reason)
	}
	if _, err := s.Messages.Send(ctx, messaging.Message{
		Channel: messaging.SMS, To: *acc.PhoneE164, Template: "approval_result",
		Vars: map[string]string{"role": "seller", "result": result, "reason": d.Reason}, Body: body,
	}); err != nil {
		s.Log.Warn("approval sms failed", "account", acc.ID, "err", err)
	}
}
