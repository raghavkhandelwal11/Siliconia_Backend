package programmes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	shipprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func unique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- Demo requests (T4.3) ---------------------------------------------------------

// RequestDemo asks the product's seller for a demo unit.
func (s *Service) RequestDemo(ctx context.Context, creator, productID uuid.UUID, note string) (store.DemoRequest, error) {
	if err := approved(ctx, s.Q, creator); err != nil {
		return store.DemoRequest{}, err
	}
	seller, err := s.Q.ProductSeller(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.DemoRequest{}, httpx.ErrNotFound
	}
	if err != nil {
		return store.DemoRequest{}, err
	}
	d, err := s.Q.CreateDemo(ctx, store.CreateDemoParams{ID: newID(), CreatorID: creator, ProductID: productID, SellerID: seller, Note: strings.TrimSpace(note)})
	if unique(err) {
		return store.DemoRequest{}, httpx.NewError(http.StatusConflict, "DEMO_EXISTS", "You already asked for a demo of this product")
	}
	return d, err
}

// DecideDemo approves (booking the courier to the creator's default
// address) or declines a demo request (seller).
func (s *Service) DecideDemo(ctx context.Context, seller, id uuid.UUID, approve bool, reason string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		d, err := q.GetDemoForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && d.SellerID != seller) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.Status != "REQUESTED" {
			return httpx.NewError(http.StatusConflict, "ALREADY_DECIDED", "This request has already been decided")
		}
		if !approve {
			reason = strings.TrimSpace(reason)
			if len([]rune(reason)) < 3 {
				return httpx.ErrValidation.WithDetail("Give a reason")
			}
			_, err := q.SetDemoStatus(ctx, store.SetDemoStatusParams{ID: id, ToStatus: "DECLINED", Reason: &reason, FromStatus: []string{"REQUESTED"}})
			return err
		}
		to, err := q.DemoShipTo(ctx, d.CreatorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NewError(http.StatusConflict, "CREATOR_ADDRESS_MISSING", "The creator has no delivery address yet")
		}
		if err != nil {
			return err
		}
		pick, err := q.GetSellerPickup(ctx, seller)
		if err != nil {
			return err
		}
		var from struct{ Line1, Line2, City, State, Pincode, Phone string }
		if json.Unmarshal(pick.PickupAddress, &from) != nil || from.Pincode == "" {
			return httpx.NewError(http.StatusConflict, "PICKUP_ADDRESS_MISSING", "Add a pickup address to your seller profile first")
		}
		booked, err := s.Courier.CreateShipment(ctx, shipprovider.ShipmentRequest{Ref: "DEMO-" + id.String(), Kind: shipprovider.Forward,
			From: shipprovider.Address{Name: pick.StoreName, Phone: from.Phone, Line1: from.Line1, Line2: from.Line2, City: from.City, State: from.State, Pincode: from.Pincode},
			To:   shipprovider.Address{Name: to.Name, Phone: to.PhoneE164, Line1: to.Line1, Line2: to.Line2, City: to.City, State: to.State, Pincode: to.Pincode}})
		if err != nil {
			s.Log.Error("demo shipment", "err", err)
			return httpx.ErrUnavailable.WithDetail("The courier service is not responding. Try again in a minute")
		}
		var eta *time.Time
		if !booked.ETA.IsZero() {
			eta = &booked.ETA
		}
		sh, err := q.CreateDemoShipment(ctx, store.CreateDemoShipmentParams{ID: newID(), DemoRequestID: &id, Ref: "DEMO-" + id.String(),
			Provider: s.Courier.Name(), Awb: booked.AWB, Status: booked.Status, Eta: eta})
		if err != nil {
			return err
		}
		_, err = q.SetDemoStatus(ctx, store.SetDemoStatusParams{ID: id, ToStatus: "SHIPPED", ShipmentID: &sh.ID, FromStatus: []string{"REQUESTED"}})
		return err
	})
}

// OnDemoTracking marks a demo delivered; the creator then has 14 days to review.
func (s *Service) OnDemoTracking(ctx context.Context, tx pgx.Tx, q *store.Queries, sh store.Shipment, status string) error {
	if status != "DELIVERED" || sh.DemoRequestID == nil {
		return nil
	}
	due := time.Now().Add(14 * 24 * time.Hour)
	_, err := q.SetDemoStatus(ctx, store.SetDemoStatusParams{ID: *sh.DemoRequestID, ToStatus: "DELIVERED", DueAt: &due, FromStatus: []string{"SHIPPED"}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// Demos lists demo requests for a creator or a seller.
func (s *Service) Demos(ctx context.Context, creator, seller *uuid.UUID) ([]store.ListDemosRow, error) {
	return s.Q.ListDemos(ctx, store.ListDemosParams{CreatorID: creator, SellerID: seller})
}

// ---- Creator reviews and votes (T4.3) --------------------------------------------------

// CreatorReviewInput is a creator's review with video links.
type CreatorReviewInput struct {
	Rating int32    `json:"rating"`
	Body   string   `json:"body"`
	Videos []string `json:"videos"`
}

func (in *CreatorReviewInput) clean() error {
	in.Body = strings.TrimSpace(in.Body)
	if in.Rating < 1 || in.Rating > 5 || len([]rune(in.Body)) < 20 || len([]rune(in.Body)) > 5000 || len(in.Videos) > 5 {
		return httpx.ErrValidation.WithDetail("Rate 1 to 5 stars, write 20 to 5000 characters and add up to 5 video links")
	}
	for _, v := range in.Videos {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return httpx.ErrValidation.WithDetail("Video links must be https URLs")
		}
	}
	return nil
}

// PostCreatorReview publishes a creator's review of a product they received
// as a demo. Rank is the order of publication for the product.
func (s *Service) PostCreatorReview(ctx context.Context, creator, productID uuid.UUID, in CreatorReviewInput) (store.CreatorReview, error) {
	if err := in.clean(); err != nil {
		return store.CreatorReview{}, err
	}
	if err := approved(ctx, s.Q, creator); err != nil {
		return store.CreatorReview{}, err
	}
	var out store.CreatorReview
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		demos, err := q.ListDemos(ctx, store.ListDemosParams{CreatorID: &creator})
		if err != nil {
			return err
		}
		var demo *store.ListDemosRow
		for i := range demos {
			if demos[i].ProductID == productID && (demos[i].Status == "DELIVERED" || demos[i].Status == "OVERDUE") {
				demo = &demos[i]
			}
		}
		if demo == nil {
			return httpx.NewError(http.StatusConflict, "NO_DEMO", "Review products after their demo unit is delivered to you")
		}
		if err := q.LockProductForReview(ctx, productID); err != nil { // serialises rank numbers
			return err
		}
		media, _ := json.Marshal(in.Videos)
		out, err = q.CreateCreatorReview(ctx, store.CreateCreatorReviewParams{ID: newID(), CreatorID: creator, ProductID: productID,
			Rating: in.Rating, Body: in.Body, Media: media})
		if unique(err) {
			return httpx.NewError(http.StatusConflict, "ALREADY_REVIEWED", "You've already reviewed this product. Edit your review instead")
		}
		if err != nil {
			return err
		}
		_, err = q.SetDemoStatus(ctx, store.SetDemoStatusParams{ID: demo.ID, ToStatus: "REVIEWED", FromStatus: []string{"DELIVERED", "OVERDUE"}})
		return err
	})
	return out, err
}

// EditCreatorReview changes the creator's own review (rank stays).
func (s *Service) EditCreatorReview(ctx context.Context, creator, id uuid.UUID, in CreatorReviewInput) (store.CreatorReview, error) {
	if err := in.clean(); err != nil {
		return store.CreatorReview{}, err
	}
	media, _ := json.Marshal(in.Videos)
	r, err := s.Q.UpdateCreatorReview(ctx, store.UpdateCreatorReviewParams{ID: id, CreatorID: creator, Rating: in.Rating, Body: in.Body, Media: media})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CreatorReview{}, httpx.ErrNotFound
	}
	return r, err
}

// Vote records a buyer's agree (+1) or disagree (-1); 0 removes it. Only
// votes from buyers with a delivered purchase count towards rewards.
func (s *Service) Vote(ctx context.Context, buyer, reviewID uuid.UUID, value int32) error {
	if value < -1 || value > 1 {
		return httpx.ErrValidation.WithDetail("Vote 1 (agree), -1 (disagree) or 0 (clear)")
	}
	if _, err := s.Q.GetCreatorReview(ctx, reviewID); err != nil {
		return httpx.ErrNotFound
	}
	if value == 0 {
		return s.Q.UnvoteCreatorReview(ctx, store.UnvoteCreatorReviewParams{CreatorReviewID: reviewID, BuyerID: buyer})
	}
	return s.Q.VoteCreatorReview(ctx, store.VoteCreatorReviewParams{CreatorReviewID: reviewID, BuyerID: buyer, Value: value})
}

// CreatorReview is a creator review on the product page.
type CreatorReview struct {
	ID          uuid.UUID `json:"id"`
	CreatorName string    `json:"creatorName"`
	Rating      int32     `json:"rating"`
	Body        string    `json:"body"`
	Videos      []string  `json:"videos"`
	Rank        int32     `json:"rank"`
	Agrees      int32     `json:"agrees"`
	Disagrees   int32     `json:"disagrees"`
	MyVote      int32     `json:"myVote"`
	At          time.Time `json:"at"`
}

// CreatorReviews lists a product's creator reviews in publication order.
func (s *Service) CreatorReviews(ctx context.Context, productID uuid.UUID, viewer *uuid.UUID) ([]CreatorReview, error) {
	rows, err := s.Q.ListCreatorReviews(ctx, store.ListCreatorReviewsParams{ProductID: productID, Viewer: viewer})
	if err != nil {
		return nil, err
	}
	out := make([]CreatorReview, len(rows))
	for i, r := range rows {
		out[i] = CreatorReview{ID: r.ID, CreatorName: r.CreatorName, Rating: r.Rating, Body: r.Body, Videos: []string{}, Rank: r.Rank,
			Agrees: r.Agrees, Disagrees: r.Disagrees, MyVote: r.MyVote, At: r.PublishedAt}
		_ = json.Unmarshal(r.Media, &out[i].Videos)
	}
	return out, nil
}

// ---- Reward pools (T4.4) ----------------------------------------------------------------

// Shares splits a pool by score = verified agrees × rank weight (LLD 9.4):
// floor each share, then hand out the remainder one by one to the largest
// fractional parts (ties to the earlier rank). Pool 1000 with scores
// 120/150/20 gives 414/517/69.
func Shares(pool int64, scores []int64) []int64 {
	out := make([]int64, len(scores))
	var sum int64
	for _, s := range scores {
		sum += s
	}
	if sum == 0 || pool <= 0 {
		return out
	}
	type frac struct {
		i int
		r int64
	}
	fr := make([]frac, len(scores))
	var given int64
	for i, sc := range scores {
		out[i] = pool * sc / sum
		fr[i] = frac{i, pool * sc % sum}
		given += out[i]
	}
	sort.SliceStable(fr, func(a, b int) bool { return fr[a].r > fr[b].r })
	for k := 0; given < pool; k++ {
		out[fr[k].i]++
		given++
	}
	return out
}

func weights(ctx context.Context, q *store.Queries) []int64 {
	w := []int64{3, 2, 1}
	if c, err := q.GetConfig(ctx, "creator.rank_weights"); err == nil {
		var v []int64
		if json.Unmarshal(c.Value, &v) == nil && len(v) > 0 {
			w = v
		}
	}
	return w
}

// SetPool sets a product's pool for a month (admin; Q-02 decides the size).
func (s *Service) SetPool(ctx context.Context, productID uuid.UUID, period string, size int32) (store.RewardPool, error) {
	if _, err := time.Parse("2006-01", period); err != nil || size < 0 || size > 10_000_000 {
		return store.RewardPool{}, httpx.ErrValidation.WithDetail("Period is YYYY-MM and size 0 to 1,00,00,000 Electrons")
	}
	p, err := s.Q.UpsertPool(ctx, store.UpsertPoolParams{ID: newID(), ProductID: productID, Period: period, Size: size})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RewardPool{}, httpx.NewError(http.StatusConflict, "POOL_SETTLED", "This pool is already settled")
	}
	return p, err
}

// Settle pays out a month's pools (monthly job, the 1st at 02:00 IST).
func (s *Service) Settle(ctx context.Context, period string) (int, error) {
	start, err := time.ParseInLocation("2006-01", period, time.FixedZone("IST", 19800))
	if err != nil {
		return 0, err
	}
	end := start.AddDate(0, 1, 0)
	paid := 0
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		pools, err := q.PoolsToSettle(ctx, period)
		if err != nil {
			return err
		}
		w := weights(ctx, q)
		now := time.Now()
		for _, p := range pools {
			rows, err := q.VerifiedAgrees(ctx, store.VerifiedAgreesParams{ProductID: p.ProductID, PeriodStart: start, PeriodEnd: end})
			if err != nil {
				return err
			}
			scores := make([]int64, len(rows))
			ws := make([]int64, len(rows))
			for i, r := range rows {
				ws[i] = w[min(int(r.Rank), len(w))-1]
				scores[i] = int64(r.Agrees) * ws[i]
			}
			shares := Shares(int64(p.Size), scores)
			for i, r := range rows {
				var lot *uuid.UUID
				if shares[i] > 0 {
					id, err := electrons.Grant(ctx, q, r.CreatorID, "CREATOR_POOL", "EARN_CREATOR_POOL", "REWARD_POOL", p.ID, int32(shares[i]), &now, "Reward pool "+period)
					if err != nil {
						return err
					}
					lot = &id
					paid++
				}
				if err := q.AddSettlement(ctx, store.AddSettlementParams{PoolID: p.ID, CreatorID: r.CreatorID, VerifiedAgrees: r.Agrees,
					Weight: int32(ws[i]), Score: int32(scores[i]), Share: int32(shares[i]), LotID: lot}); err != nil {
					return err
				}
			}
			if err := q.MarkPoolSettled(ctx, p.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return paid, err
}

// ---- Cash-out (T4.5) -------------------------------------------------------------------

// RequestCashout holds Electrons for a bank payout in the next weekly batch.
func (s *Service) RequestCashout(ctx context.Context, creator uuid.UUID, amount int32) (store.CashoutRequest, error) {
	if amount < 250 {
		return store.CashoutRequest{}, httpx.ErrValidation.WithDetail("Cash out at least 250 Electrons")
	}
	p, err := s.Q.GetCreatorProfile(ctx, creator)
	if err != nil || p.KycVerifiedAt == nil {
		return store.CashoutRequest{}, ErrNoKYC
	}
	var c store.CashoutRequest
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		var err error
		if c, err = q.CreateCashout(ctx, store.CreateCashoutParams{ID: newID(), CreatorID: creator, Electrons: amount}); err != nil {
			return err
		}
		return electrons.Spend(ctx, q, creator, amount, "CASHOUT_HOLD", "CASHOUT", c.ID)
	})
	return c, err
}

// CancelCashout gives held Electrons back before the batch runs.
func (s *Service) CancelCashout(ctx context.Context, creator, id uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		c, err := q.CancelCashout(ctx, store.CancelCashoutParams{ID: id, CreatorID: creator})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NewError(http.StatusConflict, "CASHOUT_IN_BATCH", "This cash-out is already being paid")
		}
		if err != nil {
			return err
		}
		_, err = electrons.Restore(ctx, q, creator, "CASHOUT", c.ID, c.Electrons, "Cash-out cancelled")
		return err
	})
}

// Batch sends requested cash-outs to the bank (weekly, Monday 06:00 IST).
// 1 Electron = ₹1.
func (s *Service) Batch(ctx context.Context) (int, error) {
	type send struct {
		ref           string
		amount        int64
		account, ifsc string
	}
	var sends []send
	batch := newID()
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		rows, err := q.CashoutsToBatch(ctx)
		if err != nil {
			return err
		}
		for _, c := range rows {
			plain, err := s.Box.Open(c.BankEnc, []byte("creator-bank:"+c.CreatorID.String()))
			if err != nil {
				return err
			}
			acct, ifsc, _ := strings.Cut(string(plain), "|")
			ref := "CO-" + c.ID.String()
			if err := q.SetCashoutBatched(ctx, store.SetCashoutBatchedParams{ID: c.ID, BatchID: &batch, ProviderRef: &ref}); err != nil {
				return err
			}
			sends = append(sends, send{ref, int64(c.Electrons) * 100, acct, ifsc})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, x := range sends { // idempotent on ref; a failure is retried next batch by support
		if err := s.Payout.Transfer(ctx, x.ref, x.amount, x.account, x.ifsc); err != nil {
			s.Log.Error("payout transfer", "ref", x.ref, "err", err)
		}
	}
	return len(sends), nil
}

// HandlePayoutWebhook applies a transfer result once.
func (s *Service) HandlePayoutWebhook(r *http.Request) error {
	e, err := s.Payout.VerifyWebhook(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.RecordWebhook(ctx, store.RecordWebhookParams{Provider: s.Payout.Name(), EventID: e.ID, Payload: e.Raw}); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		c, err := q.GetCashoutByRefForUpdate(ctx, &e.Ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("payout: unknown ref %s", e.Ref)
		}
		if err != nil {
			return err
		}
		if c.Status != "IN_BATCH" {
			return nil
		}
		if e.Status == "PAID" {
			if err := q.FinishCashout(ctx, store.FinishCashoutParams{ID: c.ID, Status: "PAID", BankRef: &e.BankRef}); err != nil {
				return err
			}
			if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: c.CreatorID, Type: "CASHOUT_PAID", Amount: 0,
				RefType: "CASHOUT", RefID: c.ID, Note: "Bank ref " + e.BankRef}); err != nil {
				return err
			}
		} else {
			if err := q.FinishCashout(ctx, store.FinishCashoutParams{ID: c.ID, Status: "FAILED", FailureReason: &e.Reason}); err != nil {
				return err
			}
			if _, err := electrons.Restore(ctx, q, c.CreatorID, "CASHOUT", c.ID, c.Electrons, "Cash-out failed: "+e.Reason); err != nil {
				return err
			}
		}
		_, err = outbox.Add(ctx, tx, "cashout.finished", c.ID.String(), map[string]any{"accountId": c.CreatorID, "status": e.Status, "electrons": c.Electrons})
		return err
	})
}

// ---- Promoters (T4.6) ---------------------------------------------------------------------

// PromoterProfile is a promoter's application.
type PromoterProfile struct {
	Type        string          `json:"type"`
	DisplayName string          `json:"displayName"`
	Company     json.RawMessage `json:"company,omitempty"`
}

// SavePromoter stores the promoter's profile and submits it for review.
func (s *Service) SavePromoter(ctx context.Context, acc uuid.UUID, in PromoterProfile) error {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if (in.Type != "INDIVIDUAL" && in.Type != "COMPANY") || len([]rune(in.DisplayName)) < 2 || len([]rune(in.DisplayName)) > 80 {
		return httpx.ErrValidation.WithDetail("Choose individual or company and a display name (2 to 80 characters)")
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		a, err := q.GetAccountApproval(ctx, acc)
		if err != nil {
			return err
		}
		if _, err := q.UpsertPromoterProfile(ctx, store.UpsertPromoterProfileParams{AccountID: acc, Type: in.Type, DisplayName: in.DisplayName, Company: in.Company}); err != nil {
			return err
		}
		if a.Approval == store.ApprovalStatusDRAFT || a.Approval == store.ApprovalStatusREJECTED {
			return q.SetAccountApproval(ctx, store.SetAccountApprovalParams{ID: acc, Approval: store.ApprovalStatusSUBMITTED})
		}
		return nil
	})
}

// AddInvestment records that the promoter invested in a seller's startup;
// the seller must confirm it.
func (s *Service) AddInvestment(ctx context.Context, promoter, seller uuid.UUID, proof string) error {
	proof = strings.TrimSpace(proof)
	if len([]rune(proof)) < 5 || len([]rune(proof)) > 1000 {
		return httpx.ErrValidation.WithDetail("Describe the investment (5 to 1000 characters)")
	}
	if err := approved(ctx, s.Q, promoter); err != nil {
		return err
	}
	if _, err := s.Q.GetSellerPickup(ctx, seller); err != nil {
		return httpx.ErrValidation.WithDetail("Choose a seller on Siliconics")
	}
	_, err := s.Q.AddInvestment(ctx, store.AddInvestmentParams{ID: newID(), PromoterID: promoter, SellerID: seller, ProofNote: proof})
	return err
}

// DecideInvestment confirms or declines a promoter's claim (seller).
func (s *Service) DecideInvestment(ctx context.Context, seller, id uuid.UUID, confirm bool) error {
	n, err := s.Q.DecideInvestment(ctx, store.DecideInvestmentParams{ID: id, SellerID: seller, Confirm: confirm})
	if err == nil && n == 0 {
		return httpx.ErrNotFound
	}
	return err
}

// SetPromotion adds or removes the promoter's Promote on a product of a
// startup they backed (confirmed by the seller).
func (s *Service) SetPromotion(ctx context.Context, promoter, productID uuid.UUID, on bool) error {
	if !on {
		return s.Q.Unpromote(ctx, store.UnpromoteParams{PromoterID: promoter, ProductID: productID})
	}
	if err := approved(ctx, s.Q, promoter); err != nil {
		return err
	}
	ok, err := s.Q.CanPromote(ctx, store.CanPromoteParams{ID: productID, PromoterID: promoter})
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NewError(http.StatusForbidden, "NOT_CONFIRMED", "You can promote products of startups that confirmed your investment")
	}
	return s.Q.Promote(ctx, store.PromoteParams{PromoterID: promoter, ProductID: productID})
}

// SetRewardRule sets Electrons per unit sold for the product's promoters (seller).
func (s *Service) SetRewardRule(ctx context.Context, seller, productID uuid.UUID, perUnit int32) error {
	if perUnit < 0 || perUnit > 100000 {
		return httpx.ErrValidation.WithDetail("Electrons per unit must be 0 to 1,00,000")
	}
	n, err := s.Q.SetRewardRule(ctx, store.SetRewardRuleParams{ProductID: productID, Value: perUnit, SellerID: seller})
	if err == nil && n == 0 {
		return httpx.ErrNotFound
	}
	return err
}

// RewardPromoters grants pending Electrons to promoters of delivered items;
// they mature with the buyer's return window and reverse on returns
// (interim rule until Q-03: every promoter of the product earns the rule).
func RewardPromoters(ctx context.Context, q *store.Queries, items []uuid.UUID, availableAt time.Time) error {
	rows, err := q.PromoterRewardsForItems(ctx, items)
	if err != nil {
		return err
	}
	for _, r := range rows {
		amount := r.Value * r.Qty
		if amount <= 0 {
			continue
		}
		if _, err := electrons.Grant(ctx, q, r.PromoterID, "PROMOTER", "EARN_PROMOTER", electrons.RefOrderItem, r.ItemID, amount, &availableAt, "Promoter reward"); err != nil {
			return err
		}
	}
	return nil
}
