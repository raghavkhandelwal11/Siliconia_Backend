// Package programmes runs the creator and promoter programmes (LLD 8.9,
// 8.10, 9.4): creator applications, KYC and approval; demo units shipped to
// creators; creator reviews that buyers agree or disagree with; monthly
// reward pools paid in Electrons; cash-out to a bank through sim-payout; and
// promoters who back a seller and promote its products for Electrons.
package programmes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payout"
	shipprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TermsVersion is the creator terms buyers of this build accept.
const TermsVersion = "creator-2026-11"

var (
	ErrNotApproved = httpx.NewError(http.StatusForbidden, "NOT_APPROVED", "Your programme application is not approved yet")
	ErrCriteria    = httpx.NewError(http.StatusUnprocessableEntity, "CRITERIA_NOT_MET", "Creators need at least the minimum followers across their channels")
	ErrNoKYC       = httpx.NewError(http.StatusConflict, "KYC_REQUIRED", "Verify your PAN and bank account first")
)

// Service runs the programmes.
type Service struct {
	Pool    *pgxpool.Pool
	Q       *store.Queries
	Box     *crypto.Box
	KYC     kyc.Provider
	Courier shipprovider.Provider
	Payout  *payout.Sim
	Log     *slog.Logger
	// SellerPayout settles seller payout events (refs "PO-…") inside the
	// webhook's transaction; the payouts package owns that flow.
	SellerPayout func(ctx context.Context, tx pgx.Tx, e payout.Event) error
}

func newID() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

func cfgInt(ctx context.Context, q *store.Queries, key string, def int64) int64 {
	if c, err := q.GetConfig(ctx, key); err == nil {
		var n int64
		if json.Unmarshal(c.Value, &n) == nil && n > 0 {
			return n
		}
	}
	return def
}

func approved(ctx context.Context, q *store.Queries, acc uuid.UUID) error {
	a, err := q.GetAccountApproval(ctx, acc)
	if err != nil {
		return err
	}
	if a.Approval != store.ApprovalStatusAPPROVED {
		return ErrNotApproved
	}
	return nil
}

// Social is a creator channel.
type Social struct {
	Platform  string `json:"platform"`
	URL       string `json:"url"`
	Followers int32  `json:"followers"`
}

// CreatorApplication is the creator's application.
type CreatorApplication struct {
	Niche        string     `json:"niche"`
	Bio          string     `json:"bio"`
	Socials      []Social   `json:"socials"`
	Approval     string     `json:"approval"`
	Reason       string     `json:"reason,omitempty"`
	MinFollowers int64      `json:"minFollowers"`
	KYCVerified  bool       `json:"kycVerified"`
	PANLast4     string     `json:"panLast4,omitempty"`
	BankLast4    string     `json:"bankLast4,omitempty"`
	SubmittedAt  *time.Time `json:"submittedAt,omitempty"`
}

// GetCreator returns the creator's application.
func (s *Service) GetCreator(ctx context.Context, acc uuid.UUID) (CreatorApplication, error) {
	a, err := s.Q.GetAccountApproval(ctx, acc)
	if err != nil {
		return CreatorApplication{}, err
	}
	out := CreatorApplication{Approval: string(a.Approval), Socials: []Social{}, MinFollowers: cfgInt(ctx, s.Q, "creator.min_followers", 1000)}
	if a.ApprovalReason != nil {
		out.Reason = *a.ApprovalReason
	}
	p, err := s.Q.GetCreatorProfile(ctx, acc)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return CreatorApplication{}, err
	}
	out.Niche, out.Bio, out.SubmittedAt, out.KYCVerified = p.Niche, p.Bio, p.SubmittedAt, p.KycVerifiedAt != nil
	out.PANLast4, out.BankLast4 = deref(p.PanLast4), deref(p.BankLast4)
	socials, err := s.Q.ListSocials(ctx, acc)
	if err != nil {
		return CreatorApplication{}, err
	}
	for _, so := range socials {
		out.Socials = append(out.Socials, Social{Platform: so.Platform, URL: so.Url, Followers: so.Followers})
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var platforms = map[string]bool{"YOUTUBE": true, "INSTAGRAM": true, "X": true, "OTHER": true}

// SaveCreator stores the application (editable until approved).
func (s *Service) SaveCreator(ctx context.Context, acc uuid.UUID, in CreatorApplication) (CreatorApplication, error) {
	in.Niche, in.Bio = strings.TrimSpace(in.Niche), strings.TrimSpace(in.Bio)
	if len([]rune(in.Niche)) < 2 || len([]rune(in.Niche)) > 60 || len([]rune(in.Bio)) > 1000 || len(in.Socials) == 0 || len(in.Socials) > 4 {
		return CreatorApplication{}, httpx.ErrValidation.WithDetail("Add your niche (2 to 60 characters) and 1 to 4 channels")
	}
	seen := map[string]bool{}
	for _, so := range in.Socials {
		u, err := url.Parse(so.URL)
		if !platforms[so.Platform] || seen[so.Platform] || err != nil || u.Scheme != "https" || u.Host == "" || so.Followers < 0 {
			return CreatorApplication{}, httpx.ErrValidation.WithDetail("Each channel needs a platform, an https link and a follower count")
		}
		seen[so.Platform] = true
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		a, err := q.GetAccountApproval(ctx, acc)
		if err != nil {
			return err
		}
		if a.Approval == store.ApprovalStatusAPPROVED || a.Approval == store.ApprovalStatusSUBMITTED || a.Approval == store.ApprovalStatusUNDERREVIEW {
			return httpx.NewError(http.StatusConflict, "NOT_EDITABLE", "Your application is under review or approved")
		}
		if _, err := q.UpsertCreatorProfile(ctx, store.UpsertCreatorProfileParams{AccountID: acc, Niche: in.Niche, Bio: in.Bio}); err != nil {
			return err
		}
		if err := q.ClearSocials(ctx, acc); err != nil {
			return err
		}
		for _, so := range in.Socials {
			if err := q.AddSocial(ctx, store.AddSocialParams{AccountID: acc, Platform: so.Platform, Url: so.URL, Followers: so.Followers}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CreatorApplication{}, err
	}
	return s.GetCreator(ctx, acc)
}

// SubmitCreator checks the follower criterion and sends the application for review.
func (s *Service) SubmitCreator(ctx context.Context, acc uuid.UUID, acceptTerms bool) (CreatorApplication, error) {
	if !acceptTerms {
		return CreatorApplication{}, httpx.ErrValidation.WithDetail("Accept the creator terms to submit")
	}
	app, err := s.GetCreator(ctx, acc)
	if err != nil {
		return CreatorApplication{}, err
	}
	var total int64
	for _, so := range app.Socials {
		total += int64(so.Followers)
	}
	if app.Niche == "" || total < app.MinFollowers {
		return CreatorApplication{}, ErrCriteria
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := q.SubmitCreator(ctx, store.SubmitCreatorParams{AccountID: acc, TermsVersion: ptr(TermsVersion)}); err != nil {
			return err
		}
		return q.SetAccountApproval(ctx, store.SetAccountApprovalParams{ID: acc, Approval: store.ApprovalStatusSUBMITTED})
	})
	if err != nil {
		return CreatorApplication{}, err
	}
	return s.GetCreator(ctx, acc)
}

func ptr[T any](v T) *T { return &v }

// QueueItem is an application waiting for an admin.
type QueueItem struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Approval  string    `json:"approval"`
	CreatedAt time.Time `json:"createdAt"`
}

// Queue lists creator or promoter applications to review.
func (s *Service) Queue(ctx context.Context, role string) ([]QueueItem, error) {
	if role != "CREATOR" && role != "PROMOTER" {
		return nil, httpx.ErrValidation.WithDetail("role must be CREATOR or PROMOTER")
	}
	rows, err := s.Q.ProgrammeQueue(ctx, store.ProgrammeQueueParams{Role: store.AccountRole(role), States: []string{"SUBMITTED", "UNDER_REVIEW"}})
	if err != nil {
		return nil, err
	}
	out := make([]QueueItem, len(rows))
	for i, r := range rows {
		out[i] = QueueItem{ID: r.ID, Name: r.Name, Role: string(r.Role), Approval: string(r.Approval), CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// Decide approves or rejects a creator or promoter application (admin).
func (s *Service) Decide(ctx context.Context, acc uuid.UUID, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && len([]rune(reason)) < 3 {
		return httpx.ErrValidation.WithDetail("Give a reason for the rejection")
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		a, err := q.GetAccountApproval(ctx, acc)
		if err != nil {
			return httpx.ErrNotFound
		}
		if (a.Role != store.AccountRoleCREATOR && a.Role != store.AccountRolePROMOTER) ||
			(a.Approval != store.ApprovalStatusSUBMITTED && a.Approval != store.ApprovalStatusUNDERREVIEW) {
			return httpx.NewError(http.StatusConflict, "INVALID_TRANSITION", "Only submitted applications can be decided")
		}
		to, why := store.ApprovalStatusAPPROVED, (*string)(nil)
		if !approve {
			to, why = store.ApprovalStatusREJECTED, &reason
		}
		if err := q.SetAccountApproval(ctx, store.SetAccountApprovalParams{ID: acc, Approval: to, ApprovalReason: why}); err != nil {
			return err
		}
		_, err = outbox.Add(ctx, tx, "account.approval_changed", acc.String(), map[string]string{"accountId": acc.String(), "role": string(a.Role), "approval": string(to)})
		return err
	})
}

// VerifyKYC checks PAN and bank (penny drop) and stores them encrypted.
func (s *Service) VerifyKYC(ctx context.Context, acc uuid.UUID, pan, account, ifsc string) (CreatorApplication, error) {
	pan, account, ifsc = strings.ToUpper(strings.TrimSpace(pan)), strings.TrimSpace(account), strings.ToUpper(strings.TrimSpace(ifsc))
	if err := approved(ctx, s.Q, acc); err != nil {
		return CreatorApplication{}, err
	}
	a, err := s.Q.GetAccount(ctx, acc)
	if err != nil {
		return CreatorApplication{}, err
	}
	r, err := s.KYC.VerifyPAN(ctx, pan, a.Name)
	if err != nil {
		return CreatorApplication{}, httpx.ErrValidation.WithDetail("Check the PAN")
	}
	if r.Status != kyc.Verified {
		return CreatorApplication{}, httpx.NewError(http.StatusUnprocessableEntity, "PAN_NOT_VERIFIED", "The PAN could not be verified for your name")
	}
	b, err := s.KYC.PennyDrop(ctx, account, ifsc, a.Name)
	if err != nil || b.Status != kyc.Verified {
		return CreatorApplication{}, httpx.NewError(http.StatusUnprocessableEntity, "BANK_NOT_VERIFIED", "The bank account could not be verified for your name")
	}
	panEnc, err := s.Box.Seal([]byte(pan), []byte("creator-pan:"+acc.String()))
	if err != nil {
		return CreatorApplication{}, err
	}
	bankEnc, err := s.Box.Seal([]byte(account+"|"+ifsc), []byte("creator-bank:"+acc.String()))
	if err != nil {
		return CreatorApplication{}, err
	}
	if err := s.Q.SetCreatorKYC(ctx, store.SetCreatorKYCParams{AccountID: acc, PanEnc: panEnc, PanLast4: ptr(pan[len(pan)-4:]),
		BankEnc: bankEnc, BankLast4: ptr(account[max(0, len(account)-4):])}); err != nil {
		return CreatorApplication{}, err
	}
	return s.GetCreator(ctx, acc)
}
