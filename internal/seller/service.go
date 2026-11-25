// Package seller implements seller onboarding (UI-SEL-01, FR-SEL-01): business
// details, PAN and bank checks through the KYC provider, submission, and the
// admin approval queue (UI-ADM-02, FR-ADM-01).
package seller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TermsVersion is the seller agreement a submission accepts.
const TermsVersion = "seller-terms-2026-11"

// BusinessTypes accepted on the application. Individuals may sell without a
// GSTIN; every registered business type needs one.
var BusinessTypes = []string{"INDIVIDUAL", "PROPRIETORSHIP", "PARTNERSHIP", "LLP", "PRIVATE_LIMITED", "PUBLIC_LIMITED"}

var (
	ErrNotEditable  = httpx.NewError(http.StatusConflict, "APPLICATION_LOCKED", "This application is under review and cannot be changed")
	ErrStoreTaken   = httpx.NewError(http.StatusConflict, "STORE_NAME_TAKEN", "Another seller already uses this store name")
	ErrIncomplete   = httpx.NewError(http.StatusUnprocessableEntity, "APPLICATION_INCOMPLETE", "Finish every step before submitting")
	ErrNeedLegal    = httpx.NewError(http.StatusUnprocessableEntity, "LEGAL_NAME_REQUIRED", "Enter your legal name before the PAN and bank checks")
	ErrKYCDown      = httpx.ErrUnavailable.WithDetail("The verification service is not responding. Try again in a minute")
	ErrNotPending   = httpx.NewError(http.StatusConflict, "NOT_PENDING", "This application is not waiting for a decision")
	ErrActingDenied = httpx.NewError(http.StatusForbidden, "SWITCH_BACK_REQUIRED", "Switch back to Admin to do this")
)

// Address is the pickup address used for shipments.
type Address struct {
	Line1   string `json:"line1"`
	Line2   string `json:"line2,omitempty"`
	City    string `json:"city"`
	State   string `json:"state"`
	Pincode string `json:"pincode"`
	Phone   string `json:"phone"`
}

var pincodeRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

func (a Address) complete() bool {
	return strings.TrimSpace(a.Line1) != "" && strings.TrimSpace(a.City) != "" && strings.TrimSpace(a.State) != "" &&
		pincodeRE.MatchString(a.Pincode) && len(a.Phone) >= 10
}

// Check is the latest PAN or bank check shown to the seller and to admins.
type Check struct {
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	NameMatch float64   `json:"nameMatch"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Application is the seller's view of their onboarding (masked: PAN and bank
// numbers are never returned in full).
type Application struct {
	AccountID     uuid.UUID  `json:"accountId"`
	Approval      string     `json:"approval"`
	Reason        string     `json:"reason,omitempty"`
	Editable      bool       `json:"editable"`
	PhoneVerified bool       `json:"phoneVerified"`
	BusinessType  string     `json:"businessType,omitempty"`
	LegalName     string     `json:"legalName"`
	StoreName     string     `json:"storeName"`
	GSTIN         string     `json:"gstin,omitempty"`
	GSTINRequired bool       `json:"gstinRequired"`
	PANLast4      string     `json:"panLast4,omitempty"`
	PANVerified   bool       `json:"panVerified"`
	BankLast4     string     `json:"bankLast4,omitempty"`
	IFSC          string     `json:"ifsc,omitempty"`
	BankVerified  bool       `json:"bankVerified"`
	Pickup        *Address   `json:"pickupAddress,omitempty"`
	PANCheck      *Check     `json:"panCheck,omitempty"`
	BankCheck     *Check     `json:"bankCheck,omitempty"`
	SubmittedAt   *time.Time `json:"submittedAt,omitempty"`
	DecidedAt     *time.Time `json:"decidedAt,omitempty"`
	TermsVersion  string     `json:"termsVersion"`
	Missing       []string   `json:"missing"`
}

// Service runs onboarding.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Box      *crypto.Box
	KYC      kyc.Provider
	Limiter  *ratelimit.Limiter
	Messages messaging.Messenger
	Log      *slog.Logger
}

func editable(approval store.ApprovalStatus) bool {
	return approval == store.ApprovalStatusDRAFT || approval == store.ApprovalStatusREJECTED
}

// Get returns the application, creating an empty profile on first visit.
func (s *Service) Get(ctx context.Context, accountID uuid.UUID) (Application, error) {
	acc, err := s.Q.GetAccount(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	p, err := s.Q.EnsureSellerProfile(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	checks, err := s.Q.LatestKYCRecords(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	return build(acc, p, checks), nil
}

func build(acc store.Account, p store.SellerProfile, checks []store.KycRecord) Application {
	a := Application{
		AccountID: acc.ID, Approval: string(acc.Approval), Editable: editable(acc.Approval),
		PhoneVerified: acc.PhoneVerifiedAt != nil, LegalName: p.LegalName, StoreName: p.StoreName,
		PANVerified: p.PanVerifiedAt != nil, BankVerified: p.BankVerifiedAt != nil,
		SubmittedAt: p.SubmittedAt, DecidedAt: p.DecidedAt, TermsVersion: TermsVersion,
	}
	if acc.ApprovalReason != nil {
		a.Reason = *acc.ApprovalReason
	}
	a.BusinessType = deref(p.BusinessType)
	a.GSTINRequired = a.BusinessType != "" && a.BusinessType != "INDIVIDUAL"
	a.GSTIN = deref(p.Gstin)
	a.PANLast4 = deref(p.PanLast4)
	a.BankLast4 = deref(p.BankLast4)
	a.IFSC = deref(p.Ifsc)
	if len(p.PickupAddress) > 0 {
		var addr Address
		if json.Unmarshal(p.PickupAddress, &addr) == nil {
			a.Pickup = &addr
		}
	}
	for _, r := range checks {
		c := &Check{Status: r.Status, Reason: deref(r.Reason), NameMatch: float64(r.NameMatch), CheckedAt: r.CheckedAt}
		if r.Type == "PAN" {
			a.PANCheck = c
		} else {
			a.BankCheck = c
		}
	}
	a.Missing = missing(a)
	return a
}

// missing lists the steps still to do, as stable keys the web app translates.
func missing(a Application) []string {
	out := []string{}
	if !a.PhoneVerified {
		out = append(out, "phone")
	}
	if a.BusinessType == "" || strings.TrimSpace(a.LegalName) == "" || strings.TrimSpace(a.StoreName) == "" {
		out = append(out, "business")
	}
	if !a.PANVerified {
		out = append(out, "pan")
	}
	if a.GSTINRequired && a.GSTIN == "" {
		out = append(out, "gstin")
	}
	if !a.BankVerified {
		out = append(out, "bank")
	}
	if a.Pickup == nil || !a.Pickup.complete() {
		out = append(out, "pickup")
	}
	return out
}

// BusinessInput is the business step of the wizard. Fields may be saved
// incomplete; submission checks completeness.
type BusinessInput struct {
	BusinessType string   `json:"businessType"`
	LegalName    string   `json:"legalName"`
	StoreName    string   `json:"storeName"`
	GSTIN        string   `json:"gstin"`
	Pickup       *Address `json:"pickupAddress"`
}

// SaveBusiness stores the business details and pickup address.
func (s *Service) SaveBusiness(ctx context.Context, accountID uuid.UUID, in BusinessInput) (Application, error) {
	in.LegalName = strings.Join(strings.Fields(in.LegalName), " ")
	in.StoreName = strings.Join(strings.Fields(in.StoreName), " ")
	in.GSTIN = strings.ToUpper(strings.TrimSpace(in.GSTIN))
	switch {
	case in.BusinessType != "" && !slices.Contains(BusinessTypes, in.BusinessType):
		return Application{}, httpx.ErrValidation.WithDetail("Choose a business type")
	case len([]rune(in.LegalName)) > 120 || len([]rune(in.StoreName)) > 60:
		return Application{}, httpx.ErrValidation.WithDetail("Legal name can be 120 characters and store name 60")
	case in.GSTIN != "" && !ValidGSTIN(in.GSTIN, ""):
		return Application{}, httpx.ErrValidation.WithDetail("Enter a valid 15-character GSTIN")
	case in.Pickup != nil && in.Pickup.Pincode != "" && !pincodeRE.MatchString(in.Pickup.Pincode):
		return Application{}, httpx.ErrValidation.WithDetail("Enter a 6-digit PIN code")
	}
	var addr []byte
	if in.Pickup != nil {
		addr, _ = json.Marshal(in.Pickup)
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		acc, err := q.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !editable(acc.Approval) {
			return ErrNotEditable
		}
		if _, err := q.EnsureSellerProfile(ctx, accountID); err != nil {
			return err
		}
		_, err = q.UpdateSellerBusiness(ctx, store.UpdateSellerBusinessParams{
			AccountID: accountID, BusinessType: ptr(in.BusinessType), LegalName: in.LegalName, StoreName: in.StoreName,
			Gstin: ptr(in.GSTIN), PickupAddress: addr,
		})
		if isUnique(err) {
			return ErrStoreTaken
		}
		return err
	})
	if err != nil {
		return Application{}, err
	}
	return s.Get(ctx, accountID)
}

// CheckPAN verifies the PAN against the legal name and stores it encrypted.
func (s *Service) CheckPAN(ctx context.Context, accountID uuid.UUID, pan string) (Application, error) {
	pan = strings.ToUpper(strings.TrimSpace(pan))
	if !kyc.ValidPAN(pan) {
		return Application{}, httpx.ErrValidation.WithDetail("Enter a 10-character PAN like ABCDE1234F")
	}
	p, err := s.prepareCheck(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	res, err := s.KYC.VerifyPAN(ctx, pan, p.LegalName)
	if err != nil {
		s.Log.Error("kyc pan", "err", err)
		return Application{}, ErrKYCDown
	}
	sealed, err := s.Box.Seal([]byte(pan), aad(accountID, "pan"))
	if err != nil {
		return Application{}, err
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.record(ctx, q, accountID, "PAN", res); err != nil {
			return err
		}
		_, err := q.SetSellerPAN(ctx, store.SetSellerPANParams{
			AccountID: accountID, PanEnc: sealed, PanLast4: ptr(pan[6:]), PanVerifiedAt: verifiedAt(res),
		})
		return err
	})
	if err != nil {
		return Application{}, err
	}
	return s.Get(ctx, accountID)
}

// CheckBank runs a penny-drop against the legal name and stores the account encrypted.
func (s *Service) CheckBank(ctx context.Context, accountID uuid.UUID, account, ifsc string) (Application, error) {
	account = strings.ReplaceAll(strings.TrimSpace(account), " ", "")
	ifsc = strings.ToUpper(strings.TrimSpace(ifsc))
	if !kyc.ValidAccount(account) || !kyc.ValidIFSC(ifsc) {
		return Application{}, httpx.ErrValidation.WithDetail("Enter a 9–18 digit account number and an 11-character IFSC")
	}
	p, err := s.prepareCheck(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	res, err := s.KYC.PennyDrop(ctx, account, ifsc, p.LegalName)
	if err != nil {
		s.Log.Error("kyc bank", "err", err)
		return Application{}, ErrKYCDown
	}
	sealed, err := s.Box.Seal([]byte(account), aad(accountID, "bank"))
	if err != nil {
		return Application{}, err
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.record(ctx, q, accountID, "BANK", res); err != nil {
			return err
		}
		_, err := q.SetSellerBank(ctx, store.SetSellerBankParams{
			AccountID: accountID, BankEnc: sealed, BankLast4: ptr(account[len(account)-4:]), Ifsc: ptr(ifsc),
			AccountHolder: ptr(res.NameOnRecord), BankVerifiedAt: verifiedAt(res),
		})
		return err
	})
	if err != nil {
		return Application{}, err
	}
	return s.Get(ctx, accountID)
}

// prepareCheck enforces editability, a legal name and the per-account limit
// (10 checks an hour, so the simulator and later paid APIs are not abused).
func (s *Service) prepareCheck(ctx context.Context, accountID uuid.UUID) (store.SellerProfile, error) {
	acc, err := s.Q.GetAccount(ctx, accountID)
	if err != nil {
		return store.SellerProfile{}, err
	}
	if !editable(acc.Approval) {
		return store.SellerProfile{}, ErrNotEditable
	}
	p, err := s.Q.EnsureSellerProfile(ctx, accountID)
	if err != nil {
		return store.SellerProfile{}, err
	}
	if strings.TrimSpace(p.LegalName) == "" {
		return store.SellerProfile{}, ErrNeedLegal
	}
	if s.Limiter != nil {
		if r, err := s.Limiter.Allow(ctx, "kyc-check", accountID.String(), 10, time.Hour); err == nil && !r.Allowed {
			return store.SellerProfile{}, httpx.ErrRateLimited.WithDetail("Too many checks. Try again in an hour")
		}
	}
	return p, nil
}

func (s *Service) record(ctx context.Context, q *store.Queries, accountID uuid.UUID, typ string, r kyc.Result) error {
	id, _ := uuid.NewV7()
	_, err := q.InsertKYCRecord(ctx, store.InsertKYCRecordParams{
		ID: id, AccountID: accountID, Type: typ, Status: string(r.Status), ProviderRef: r.Reference,
		NameOnRecord: ptr(r.NameOnRecord), NameMatch: float32(r.NameMatch), Reason: ptr(r.Reason),
	})
	return err
}

// Submit sends a complete application for review (DRAFT/REJECTED → SUBMITTED).
func (s *Service) Submit(ctx context.Context, accountID uuid.UUID, acceptTerms bool) (Application, error) {
	app, err := s.Get(ctx, accountID)
	if err != nil {
		return Application{}, err
	}
	if !app.Editable {
		return Application{}, ErrNotEditable
	}
	if app.GSTIN != "" {
		pan, err := s.pan(ctx, accountID)
		if err != nil {
			return Application{}, err
		}
		if !ValidGSTIN(app.GSTIN, pan) {
			return Application{}, ErrIncomplete.WithDetail("The GSTIN was not issued to your PAN")
		}
	}
	if len(app.Missing) > 0 {
		return Application{}, ErrIncomplete.WithDetail("Still to do: " + strings.Join(app.Missing, ", "))
	}
	if !acceptTerms {
		return Application{}, ErrIncomplete.WithDetail("Accept the seller terms to submit")
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.SetApproval(ctx, store.SetApprovalParams{
			ID: accountID, Approval: store.ApprovalStatusSUBMITTED, FromStates: []string{"DRAFT", "REJECTED"},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotEditable
			}
			return err
		}
		if _, err := q.SubmitSellerApplication(ctx, store.SubmitSellerApplicationParams{AccountID: accountID, TermsVersion: ptr(TermsVersion)}); err != nil {
			return err
		}
		_, err := outbox.Add(ctx, tx, "account.approval_changed", accountID.String(),
			map[string]string{"accountId": accountID.String(), "role": "SELLER", "approval": "SUBMITTED"})
		return err
	})
	if err != nil {
		return Application{}, err
	}
	return s.Get(ctx, accountID)
}

func (s *Service) pan(ctx context.Context, accountID uuid.UUID) (string, error) {
	p, err := s.Q.GetSellerProfile(ctx, accountID)
	if err != nil || p.PanEnc == nil {
		return "", err
	}
	raw, err := s.Box.Open(p.PanEnc, aad(accountID, "pan"))
	return string(raw), err
}

func aad(id uuid.UUID, field string) []byte { return []byte(id.String() + ":" + field) }

func verifiedAt(r kyc.Result) *time.Time {
	if r.Status != kyc.Verified {
		return nil
	}
	now := time.Now()
	return &now
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func parseIP(s string) *netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &a
}
