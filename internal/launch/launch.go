// Package launch runs launch deals (LLD 8.4, 9.1): admins set up a launch
// with platform-owned stock and a deal-unit count that keeps a minimum
// profit; members reserve deal units through a Redis gate backed by a
// conditional SQL update (never oversell), hold them for 10 minutes and buy
// through checkout; a waiting room admits members at a set rate.
package launch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/membership"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

var (
	ErrNotViable     = httpx.NewError(http.StatusUnprocessableEntity, "LAUNCH_NOT_VIABLE", "No deal units fit the minimum profit. Lower the discount or the cost")
	ErrMembersOnly   = httpx.NewError(http.StatusForbidden, "MEMBERS_ONLY", "Launch deals are for Electra Members")
	ErrNotLive       = httpx.NewError(http.StatusConflict, "LAUNCH_NOT_LIVE", "This launch deal is not open right now")
	ErrSoldOut       = httpx.NewError(http.StatusConflict, "SOLD_OUT", "All discounted units are taken")
	ErrLimit         = httpx.NewError(http.StatusConflict, "MEMBER_LIMIT", "You've reached the limit for this deal")
	ErrQueueRequired = httpx.NewError(http.StatusPreconditionRequired, "LAUNCH_QUEUE_REQUIRED", "Join the waiting room first")
	ErrBadToken      = httpx.NewError(http.StatusForbidden, "ADMISSION_INVALID", "Your waiting-room pass is not valid. Join the queue again")
	ErrTooFast       = httpx.ErrRateLimited
	ErrNotPlatform   = httpx.ErrValidation.WithDetail("Launch stock must be owned by a platform seller")
)

// DealUnits returns the most units that can sell at the deal price while the
// launch still makes minProfitBps on cost (LLD 9.1). All money in paise.
func DealUnits(n, unitCost, normal, dealDiscountBps, minProfitBps int64) (k, dealPrice int64, err error) {
	dealPrice = normal * (10000 - dealDiscountBps) / 10000
	if n <= 0 || unitCost <= 0 || normal <= dealPrice || dealPrice <= 0 {
		return 0, dealPrice, ErrNotViable
	}
	revenue := n * normal
	required := (n*unitCost*(10000+minProfitBps) + 9999) / 10000
	if revenue < required {
		return 0, dealPrice, ErrNotViable
	}
	k = min((revenue-required)/(normal-dealPrice), n)
	if k == 0 {
		return 0, dealPrice, ErrNotViable
	}
	return k, dealPrice, nil
}

// Service runs launches.
type Service struct {
	Pool    *pgxpool.Pool
	Q       *store.Queries
	Redis   *redis.Client
	Limiter *ratelimit.Limiter
	Secret  []byte // signs waiting-room passes
	Log     *slog.Logger
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
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

// ---- Admin ------------------------------------------------------------------

// Draft is the admin's launch form.
type Draft struct {
	ProductID        uuid.UUID  `json:"productId"`
	VariantID        uuid.UUID  `json:"variantId"`
	SellerID         uuid.UUID  `json:"sellerId"`
	UnitsBought      int32      `json:"unitsBought"`
	UnitCostPaise    int64      `json:"unitCostPaise"`
	NormalPricePaise int64      `json:"normalPricePaise"`
	DealDiscountBps  int32      `json:"dealDiscountBps"`
	MinProfitBps     int32      `json:"minProfitBps"`
	PerMemberLimit   int32      `json:"perMemberLimit"`
	StartsAt         time.Time  `json:"startsAt"`
	NormalSaleMode   string     `json:"normalSaleMode"`
	NormalSaleAt     *time.Time `json:"normalSaleAt"`
	WaitingRoomRPS   *int32     `json:"waitingRoomRps"`
}

// Preview is the deal-unit calculator result.
type Preview struct {
	DealUnits       int64 `json:"dealUnits"`
	DealPricePaise  int64 `json:"dealPricePaise"`
	NormalUnits     int64 `json:"normalUnits"`
	RevenuePaise    int64 `json:"revenuePaise"`
	CostPaise       int64 `json:"costPaise"`
	ProfitPaise     int64 `json:"profitPaise"`
	DealDiscountBps int32 `json:"dealDiscountBps"`
	MinProfitBps    int32 `json:"minProfitBps"`
}

func (s *Service) defaults(ctx context.Context, d *Draft) {
	if d.DealDiscountBps == 0 {
		d.DealDiscountBps = int32(cfgInt(ctx, s.Q, "launch.deal_discount_bps", 8000))
	}
	if d.MinProfitBps == 0 {
		d.MinProfitBps = int32(cfgInt(ctx, s.Q, "launch.min_profit_bps", 400))
	}
	if d.PerMemberLimit == 0 {
		d.PerMemberLimit = 1
	}
	if d.NormalSaleMode == "" {
		d.NormalSaleMode = "IMMEDIATE"
	}
}

// Calculate previews deal units and profit for a draft.
func (s *Service) Calculate(ctx context.Context, d Draft) (Preview, error) {
	s.defaults(ctx, &d)
	k, price, err := DealUnits(int64(d.UnitsBought), d.UnitCostPaise, d.NormalPricePaise, int64(d.DealDiscountBps), int64(d.MinProfitBps))
	if err != nil {
		return Preview{}, err
	}
	n := int64(d.UnitsBought)
	rev := k*price + (n-k)*d.NormalPricePaise
	cost := n * d.UnitCostPaise
	return Preview{DealUnits: k, DealPricePaise: price, NormalUnits: n - k, RevenuePaise: rev, CostPaise: cost, ProfitPaise: rev - cost,
		DealDiscountBps: d.DealDiscountBps, MinProfitBps: d.MinProfitBps}, nil
}

// Create saves a DRAFT launch after checking the numbers.
func (s *Service) Create(ctx context.Context, admin uuid.UUID, d Draft) (store.Launch, error) {
	s.defaults(ctx, &d)
	switch {
	case d.UnitsBought <= 0 || d.UnitsBought > 100000 || d.UnitCostPaise <= 0 || d.NormalPricePaise <= 0:
		return store.Launch{}, httpx.ErrValidation.WithDetail("Units, cost and normal price must be positive")
	case d.PerMemberLimit < 1 || d.PerMemberLimit > 10:
		return store.Launch{}, httpx.ErrValidation.WithDetail("Per-member limit must be 1 to 10")
	case !d.StartsAt.After(s.now()):
		return store.Launch{}, httpx.ErrValidation.WithDetail("Start time must be in the future")
	case d.NormalSaleMode != "IMMEDIATE" && d.NormalSaleMode != "SCHEDULED":
		return store.Launch{}, httpx.ErrValidation.WithDetail("Normal sale mode must be IMMEDIATE or SCHEDULED")
	case d.NormalSaleMode == "SCHEDULED" && (d.NormalSaleAt == nil || !d.NormalSaleAt.After(d.StartsAt)):
		return store.Launch{}, httpx.ErrValidation.WithDetail("A scheduled normal sale needs a time after the start")
	case d.WaitingRoomRPS != nil && (*d.WaitingRoomRPS < 1 || *d.WaitingRoomRPS > 1000):
		return store.Launch{}, httpx.ErrValidation.WithDetail("Waiting-room rate must be 1 to 1000 per second")
	}
	if _, err := s.Q.PlatformSeller(ctx, d.SellerID); err != nil {
		return store.Launch{}, ErrNotPlatform
	}
	if ok, err := s.Q.VariantOfProduct(ctx, store.VariantOfProductParams{ID: d.VariantID, ProductID: d.ProductID}); err != nil || !ok {
		return store.Launch{}, httpx.ErrValidation.WithDetail("Choose a variant of the product")
	}
	k, price, err := DealUnits(int64(d.UnitsBought), d.UnitCostPaise, d.NormalPricePaise, int64(d.DealDiscountBps), int64(d.MinProfitBps))
	if err != nil {
		return store.Launch{}, err
	}
	var normalAt *time.Time
	if d.NormalSaleMode == "SCHEDULED" {
		normalAt = d.NormalSaleAt
	}
	return s.Q.CreateLaunch(ctx, store.CreateLaunchParams{ID: newID(), ProductID: d.ProductID, VariantID: d.VariantID, SellerID: d.SellerID,
		UnitsBought: d.UnitsBought, UnitCostPaise: d.UnitCostPaise, NormalPricePaise: d.NormalPricePaise, DealDiscountBps: d.DealDiscountBps,
		DealPricePaise: price, MinProfitBps: d.MinProfitBps, DealUnits: int32(k), PerMemberLimit: d.PerMemberLimit, StartsAt: d.StartsAt,
		NormalSaleMode: d.NormalSaleMode, NormalSaleAt: normalAt, WaitingRoomRps: d.WaitingRoomRPS, CreatedBy: admin})
}

// Schedule books the launch stock as platform-owned lots and schedules it.
func (s *Service) Schedule(ctx context.Context, id uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		l, err := q.GetLaunchForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if l.Status != store.LaunchStatusDRAFT {
			return httpx.NewError(http.StatusConflict, "INVALID_TRANSITION", "Only drafts can be scheduled")
		}
		cost := l.UnitCostPaise
		deal, err := q.CreateLaunchLot(ctx, store.CreateLaunchLotParams{ID: newID(), VariantID: l.VariantID, OwnerSellerID: l.SellerID,
			Kind: store.LotKindLAUNCHDEAL, LaunchID: &l.ID, Quantity: l.DealUnits, UnitCostPaise: &cost})
		if err != nil {
			return err
		}
		normal, err := q.CreateLaunchLot(ctx, store.CreateLaunchLotParams{ID: newID(), VariantID: l.VariantID, OwnerSellerID: l.SellerID,
			Kind: store.LotKindLAUNCHNORMAL, LaunchID: &l.ID, Quantity: l.UnitsBought - l.DealUnits, UnitCostPaise: &cost})
		if err != nil {
			return err
		}
		return q.SetLaunchLots(ctx, store.SetLaunchLotsParams{ID: id, DealLotID: &deal.ID, NormalLotID: &normal.ID})
	})
}

// Cancel stops a launch that has not gone live.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) error {
	_, err := s.Q.SetLaunchStatus(ctx, store.SetLaunchStatusParams{ID: id, ToStatus: store.LaunchStatusCANCELLED, FromStatus: []string{"DRAFT", "SCHEDULED"}})
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NewError(http.StatusConflict, "INVALID_TRANSITION", "Only drafts and scheduled launches can be cancelled")
	}
	return err
}

// ---- Redis gate ---------------------------------------------------------------

func remainingKey(id uuid.UUID) string { return "launch:" + id.String() + ":remaining" }
func memberKey(id, acc uuid.UUID) string {
	return "launch:" + id.String() + ":member:" + acc.String()
}
func streamChannel(id uuid.UUID) string { return "launch:" + id.String() + ":updates" }
func queueKey(id uuid.UUID) string      { return "launch:" + id.String() + ":queue" }
func admittedKey(id uuid.UUID) string   { return "launch:" + id.String() + ":admitted" }

// reserveScript is the fast gate in front of the database (LLD 8.4). It
// answers MISSING when the counter has not been loaded yet.
var reserveScript = redis.NewScript(`
local rem = redis.call('GET', KEYS[1])
if not rem then return 'MISSING' end
rem = tonumber(rem)
local mine = tonumber(redis.call('GET', KEYS[2]) or '0')
local q, lim = tonumber(ARGV[1]), tonumber(ARGV[2])
if rem < q then return 'SOLD_OUT' end
if mine + q > lim then return 'LIMIT' end
redis.call('DECRBY', KEYS[1], q)
redis.call('INCRBY', KEYS[2], q)
redis.call('EXPIRE', KEYS[2], 604800)
return 'OK'`)

func (s *Service) warm(ctx context.Context, l store.Launch) {
	if err := s.Redis.Set(ctx, remainingKey(l.ID), l.DealRemaining, 7*24*time.Hour).Err(); err != nil {
		s.Log.Warn("launch warm", "err", err)
	}
}

func (s *Service) compensate(ctx context.Context, id, acc uuid.UUID, qty int32) {
	pipe := s.Redis.TxPipeline()
	pipe.IncrBy(ctx, remainingKey(id), int64(qty))
	pipe.DecrBy(ctx, memberKey(id, acc), int64(qty))
	if _, err := pipe.Exec(ctx); err != nil {
		s.Log.Warn("launch compensate", "err", err)
	}
}

// ---- Reserve ----------------------------------------------------------------

// Claim is a held deal unit.
type Claim struct {
	ID        uuid.UUID `json:"id"`
	LaunchID  uuid.UUID `json:"launchId"`
	Qty       int32     `json:"qty"`
	Status    string    `json:"status"`
	HeldUntil time.Time `json:"heldUntil"`
}

func claimView(c store.LaunchClaim) Claim {
	return Claim{ID: c.ID, LaunchID: c.LaunchID, Qty: c.Qty, Status: c.Status, HeldUntil: c.HeldUntil}
}

// Reserve holds qty deal units for a member for the hold time.
func (s *Service) Reserve(ctx context.Context, acc, id uuid.UUID, qty int32, pass string) (Claim, error) {
	if qty < 1 || qty > 10 {
		return Claim{}, httpx.ErrValidation.WithDetail("Quantity must be 1 to 10")
	}
	if !membership.IsMember(ctx, s.Q, acc) {
		return Claim{}, ErrMembersOnly
	}
	if s.Limiter != nil {
		if res, err := s.Limiter.Allow(ctx, "launch-reserve", acc.String(), 5, time.Minute); err == nil && !res.Allowed {
			return Claim{}, ErrTooFast
		}
	}
	l, err := s.Q.GetLaunch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, httpx.ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	if l.Status != store.LaunchStatusLIVE {
		if l.Status == store.LaunchStatusDEALSOLDOUT || l.Status == store.LaunchStatusNORMALONSALE || l.Status == store.LaunchStatusCOMPLETED {
			return Claim{}, ErrSoldOut
		}
		return Claim{}, ErrNotLive
	}
	if l.WaitingRoomRps != nil {
		if pass == "" {
			return Claim{}, ErrQueueRequired
		}
		if !s.validPass(pass, id, acc) {
			return Claim{}, ErrBadToken
		}
	}

	// Fast path: Redis turns away most requests once units are gone.
	keys := []string{remainingKey(id), memberKey(id, acc)}
	res, err := reserveScript.Run(ctx, s.Redis, keys, qty, l.PerMemberLimit).Text()
	if err == nil && res == "MISSING" {
		s.warm(ctx, l)
		res, err = reserveScript.Run(ctx, s.Redis, keys, qty, l.PerMemberLimit).Text()
	}
	if err != nil {
		return Claim{}, httpx.ErrUnavailable
	}
	switch res {
	case "SOLD_OUT":
		return Claim{}, ErrSoldOut
	case "LIMIT":
		return Claim{}, ErrLimit
	}

	// Source of truth: the conditional update can never take more than exists.
	hold := time.Duration(cfgInt(ctx, s.Q, "launch.hold_minutes", 10)) * time.Minute
	var c store.LaunchClaim
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.TakeDealUnits(ctx, store.TakeDealUnitsParams{ID: id, Qty: qty}); errors.Is(err, pgx.ErrNoRows) {
			return ErrSoldOut
		} else if err != nil {
			return err
		}
		if err := q.EnsureMemberCounter(ctx, store.EnsureMemberCounterParams{LaunchID: id, AccountID: acc}); err != nil {
			return err
		}
		if n, err := q.AddMemberClaim(ctx, store.AddMemberClaimParams{LaunchID: id, AccountID: acc, Qty: qty, Lim: l.PerMemberLimit}); err != nil {
			return err
		} else if n == 0 {
			return ErrLimit
		}
		c, err = q.CreateClaim(ctx, store.CreateClaimParams{ID: newID(), LaunchID: id, AccountID: acc, Qty: qty, HeldUntil: s.now().Add(hold)})
		return err
	})
	if err != nil {
		s.compensate(context.WithoutCancel(ctx), id, acc, qty)
		return Claim{}, err
	}
	s.publish(ctx, id)
	return claimView(c), nil
}

// releaseInTx gives a claim's units back inside the caller's transaction and
// returns work for after the commit (Redis counters, live update).
func (s *Service) releaseInTx(ctx context.Context, q *store.Queries, claimID uuid.UUID) (func(context.Context), error) {
	c, err := q.ReleaseClaim(ctx, claimID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // already released or paid
	}
	if err != nil {
		return nil, err
	}
	if err := q.ReturnDealUnits(ctx, store.ReturnDealUnitsParams{ID: c.LaunchID, Qty: c.Qty}); err != nil {
		return nil, err
	}
	if err := q.SubMemberClaim(ctx, store.SubMemberClaimParams{LaunchID: c.LaunchID, AccountID: c.AccountID, Qty: c.Qty}); err != nil {
		return nil, err
	}
	return func(ctx context.Context) {
		s.compensate(ctx, c.LaunchID, c.AccountID, c.Qty)
		s.publish(ctx, c.LaunchID)
	}, nil
}

// Release lets a member give back a held claim early.
func (s *Service) Release(ctx context.Context, acc, claimID uuid.UUID) error {
	var after func(context.Context)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		c, err := q.GetClaimForUpdate(ctx, claimID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.AccountID != acc) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Status != "HELD" {
			return httpx.NewError(http.StatusConflict, "CLAIM_IN_USE", "This claim is part of an order now")
		}
		after, err = s.releaseInTx(ctx, q, claimID)
		return err
	})
	if err == nil && after != nil {
		after(context.WithoutCancel(ctx))
	}
	return err
}

// ReleaseForOrder frees claims of an order that will not be paid (checkout's
// expiry sweep), inside its transaction.
func (s *Service) ReleaseForOrder(ctx context.Context, q *store.Queries, orderID uuid.UUID) (func(context.Context), error) {
	claims, err := q.ClaimsForOrder(ctx, &orderID)
	if err != nil {
		return nil, err
	}
	var afters []func(context.Context)
	for _, c := range claims {
		fn, err := s.releaseInTx(ctx, q, c.ID)
		if err != nil {
			return nil, err
		}
		if fn != nil {
			afters = append(afters, fn)
		}
	}
	return func(ctx context.Context) {
		for _, fn := range afters {
			fn(ctx)
		}
	}, nil
}

// Sweep releases held claims past their hold (every 30 s job).
func (s *Service) Sweep(ctx context.Context) (int, error) {
	var afters []func(context.Context)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		stale, err := q.StaleClaims(ctx)
		if err != nil {
			return err
		}
		for _, c := range stale {
			fn, err := s.releaseInTx(ctx, q, c.ID)
			if err != nil {
				return err
			}
			if fn != nil {
				afters = append(afters, fn)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, fn := range afters {
		fn(ctx)
	}
	return len(afters), nil
}

// ---- Status job -----------------------------------------------------------------

// Tick moves launches through their statuses (every few seconds) and keeps
// the Redis counter in step with the database.
func (s *Service) Tick(ctx context.Context) error {
	var changed []store.Launch
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		rows, err := q.LaunchesToTick(ctx)
		if err != nil {
			return err
		}
		now := s.now()
		for _, r := range rows {
			l := store.Launch{ID: r.ID, Status: r.Status, DealRemaining: r.DealRemaining}
			to := r.Status
			switch r.Status {
			case store.LaunchStatusSCHEDULED:
				if !r.StartsAt.After(now) {
					to = store.LaunchStatusLIVE
				}
			case store.LaunchStatusLIVE:
				if r.DealRemaining == 0 && r.Held == 0 {
					to = store.LaunchStatusDEALSOLDOUT
				}
			case store.LaunchStatusDEALSOLDOUT:
				if r.NormalSaleMode == "IMMEDIATE" || (r.NormalSaleAt != nil && !r.NormalSaleAt.After(now)) {
					to = store.LaunchStatusNORMALONSALE
					if r.NormalAvailable == 0 {
						to = store.LaunchStatusCOMPLETED
					}
				}
			case store.LaunchStatusNORMALONSALE:
				if r.NormalAvailable == 0 {
					to = store.LaunchStatusCOMPLETED
				}
			}
			if to != r.Status {
				if _, err := q.SetLaunchStatus(ctx, store.SetLaunchStatusParams{ID: r.ID, ToStatus: to, FromStatus: []string{string(r.Status)}}); err != nil {
					return err
				}
				if _, err := outbox.Add(ctx, tx, "launch.status_changed", r.ID.String(), map[string]any{"launchId": r.ID, "status": to}); err != nil {
					return err
				}
				l.Status = to
				changed = append(changed, l)
			} else if r.Status == store.LaunchStatusLIVE {
				changed = append(changed, l) // re-sync the counter
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, l := range changed {
		if l.Status == store.LaunchStatusLIVE {
			s.warm(ctx, l)
		}
		s.publish(ctx, l.ID)
	}
	return nil
}

// ---- Waiting room -----------------------------------------------------------

// QueueState is where a member stands in the waiting room.
type QueueState struct {
	Position      int64  `json:"position"` // 0 once admitted
	Admitted      bool   `json:"admitted"`
	AdmissionPass string `json:"admissionToken,omitempty"`
}

// Join puts a member in the waiting room (keeps their place on repeat).
func (s *Service) Join(ctx context.Context, acc, id uuid.UUID) (QueueState, error) {
	if !membership.IsMember(ctx, s.Q, acc) {
		return QueueState{}, ErrMembersOnly
	}
	l, err := s.Q.GetLaunch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return QueueState{}, httpx.ErrNotFound
	}
	if err != nil {
		return QueueState{}, err
	}
	if l.WaitingRoomRps == nil {
		return QueueState{Admitted: true}, nil
	}
	if err := s.Redis.ZAddNX(ctx, queueKey(id), redis.Z{Score: float64(s.now().UnixNano()), Member: acc.String()}).Err(); err != nil {
		return QueueState{}, httpx.ErrUnavailable
	}
	return s.QueueStatus(ctx, acc, id)
}

// QueueStatus reports position and, once admitted, a pass valid 10 minutes.
func (s *Service) QueueStatus(ctx context.Context, acc, id uuid.UUID) (QueueState, error) {
	rank, err := s.Redis.ZRank(ctx, queueKey(id), acc.String()).Result()
	if errors.Is(err, redis.Nil) {
		return QueueState{}, httpx.NewError(http.StatusNotFound, "NOT_IN_QUEUE", "Join the waiting room first")
	}
	if err != nil {
		return QueueState{}, httpx.ErrUnavailable
	}
	admitted, _ := s.Redis.Get(ctx, admittedKey(id)).Int64()
	if rank < admitted {
		return QueueState{Admitted: true, AdmissionPass: s.pass(id, acc, s.now().Add(10*time.Minute))}, nil
	}
	return QueueState{Position: rank - admitted + 1}, nil
}

// Admit lets the next members in at each launch's rate (every second job).
func (s *Service) Admit(ctx context.Context) error {
	rows, err := s.Q.ListLaunches(ctx, store.ListLaunchesParams{Status: store.NullLaunchStatus{LaunchStatus: store.LaunchStatusLIVE, Valid: true}})
	if err != nil {
		return err
	}
	for _, r := range rows {
		l, err := s.Q.GetLaunch(ctx, r.ID)
		if err != nil || l.WaitingRoomRps == nil {
			continue
		}
		size, _ := s.Redis.ZCard(ctx, queueKey(l.ID)).Result()
		cur, _ := s.Redis.Get(ctx, admittedKey(l.ID)).Int64()
		if cur < size {
			s.Redis.IncrBy(ctx, admittedKey(l.ID), min(int64(*l.WaitingRoomRps), size-cur))
		}
	}
	return nil
}

// pass is launch|account|expiry signed with HMAC-SHA256.
func (s *Service) pass(id, acc uuid.UUID, exp time.Time) string {
	body := id.String() + "|" + acc.String() + "|" + strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, s.Secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) validPass(token string, id, acc uuid.UUID) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	m := hmac.New(sha256.New, s.Secret)
	m.Write(body)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return false
	}
	f := strings.Split(string(body), "|")
	if len(f) != 3 || f[0] != id.String() || f[1] != acc.String() {
		return false
	}
	exp, err := strconv.ParseInt(f[2], 10, 64)
	return err == nil && s.now().Unix() < exp
}

// ---- Public view ------------------------------------------------------------

// View is the launch page.
type View struct {
	ID               uuid.UUID         `json:"id"`
	ProductName      string            `json:"productName"`
	ProductSlug      string            `json:"productSlug"`
	CoverURL         string            `json:"coverUrl,omitempty"`
	Attrs            map[string]string `json:"attrs"`
	Status           string            `json:"status"`
	StartsAt         time.Time         `json:"startsAt"`
	NormalSaleAt     *time.Time        `json:"normalSaleAt,omitempty"`
	MrpPaise         int64             `json:"mrpPaise"`
	NormalPricePaise int64             `json:"normalPricePaise"`
	DealPricePaise   int64             `json:"dealPricePaise"`
	DealDiscountBps  int32             `json:"dealDiscountBps"`
	DealUnits        int32             `json:"dealUnits"`
	DealRemaining    int32             `json:"dealRemaining"`
	Held             int32             `json:"held"`
	NormalAvailable  int32             `json:"normalAvailable"`
	PerMemberLimit   int32             `json:"perMemberLimit"`
	WaitingRoom      bool              `json:"waitingRoom"`
	ServerTime       time.Time         `json:"serverTime"`
	MyClaims         []Claim           `json:"myClaims,omitempty"`
	IsMember         bool              `json:"isMember"`
}

// Live is the part of the view the stream refreshes.
type Live struct {
	Status          string `json:"status"`
	DealRemaining   int32  `json:"dealRemaining"`
	Held            int32  `json:"held"`
	NormalAvailable int32  `json:"normalAvailable"`
}

// Get returns a launch (drafts and cancelled launches are hidden).
func (s *Service) Get(ctx context.Context, id uuid.UUID, viewer *uuid.UUID) (View, error) {
	r, err := s.Q.LaunchView(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (r.Status == store.LaunchStatusDRAFT || r.Status == store.LaunchStatusCANCELLED)) {
		return View{}, httpx.ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	v := View{ID: r.ID, ProductName: r.ProductName, ProductSlug: r.ProductSlug, Attrs: map[string]string{}, Status: string(r.Status),
		StartsAt: r.StartsAt, NormalSaleAt: r.NormalSaleAt, MrpPaise: r.MrpPaise, NormalPricePaise: r.NormalPricePaise, DealPricePaise: r.DealPricePaise,
		DealDiscountBps: r.DealDiscountBps, DealUnits: r.DealUnits, DealRemaining: r.DealRemaining, Held: r.Held, NormalAvailable: r.NormalAvailable,
		PerMemberLimit: r.PerMemberLimit, WaitingRoom: r.WaitingRoomRps != nil, ServerTime: s.now()}
	_ = json.Unmarshal(r.VariantAttrs, &v.Attrs)
	if r.CoverKey != "" {
		v.CoverURL = "/api/v1/media/" + r.CoverKey
	}
	if viewer != nil {
		v.IsMember = membership.IsMember(ctx, s.Q, *viewer)
		claims, err := s.Q.MyClaims(ctx, store.MyClaimsParams{LaunchID: id, AccountID: *viewer})
		if err == nil {
			for _, c := range claims {
				v.MyClaims = append(v.MyClaims, claimView(c))
			}
		}
	}
	return v, nil
}

func (s *Service) live(ctx context.Context, id uuid.UUID) (Live, error) {
	r, err := s.Q.LaunchView(ctx, id)
	if err != nil {
		return Live{}, err
	}
	return Live{Status: string(r.Status), DealRemaining: r.DealRemaining, Held: r.Held, NormalAvailable: r.NormalAvailable}, nil
}

func (s *Service) publish(ctx context.Context, id uuid.UUID) {
	lv, err := s.live(ctx, id)
	if err != nil {
		return
	}
	b, _ := json.Marshal(lv)
	if err := s.Redis.Publish(ctx, streamChannel(id), string(b)).Err(); err != nil {
		s.Log.Warn("launch publish", "err", err)
	}
}

// Summary is a launch card.
type Summary struct {
	ID               uuid.UUID `json:"id"`
	ProductName      string    `json:"productName"`
	ProductSlug      string    `json:"productSlug"`
	CoverURL         string    `json:"coverUrl,omitempty"`
	Status           string    `json:"status"`
	StartsAt         time.Time `json:"startsAt"`
	DealPricePaise   int64     `json:"dealPricePaise"`
	NormalPricePaise int64     `json:"normalPricePaise"`
	DealUnits        int32     `json:"dealUnits"`
	DealRemaining    int32     `json:"dealRemaining"`
}

var validStatus = map[string]bool{"DRAFT": true, "SCHEDULED": true, "LIVE": true, "DEAL_SOLD_OUT": true, "NORMAL_ON_SALE": true, "COMPLETED": true, "CANCELLED": true}

// List returns launches; admins also see drafts.
func (s *Service) List(ctx context.Context, status string, admin bool) ([]Summary, error) {
	var st store.NullLaunchStatus
	if status != "" {
		st = store.NullLaunchStatus{LaunchStatus: store.LaunchStatus(status), Valid: true}
		if !validStatus[status] {
			return nil, httpx.ErrValidation.WithDetail("Unknown status")
		}
	}
	rows, err := s.Q.ListLaunches(ctx, store.ListLaunchesParams{Status: st, IncludeDrafts: admin})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, len(rows))
	for i, r := range rows {
		out[i] = Summary{ID: r.ID, ProductName: r.ProductName, ProductSlug: r.ProductSlug, Status: string(r.Status), StartsAt: r.StartsAt,
			DealPricePaise: r.DealPricePaise, NormalPricePaise: r.NormalPricePaise, DealUnits: r.DealUnits, DealRemaining: r.DealRemaining}
		if r.CoverKey != "" {
			out[i].CoverURL = "/api/v1/media/" + r.CoverKey
		}
	}
	return out, nil
}
