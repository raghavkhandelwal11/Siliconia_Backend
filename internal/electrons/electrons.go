// Package electrons is the Electrons ledger (LLD 8.7, 9.2): lots earned on
// paid orders mature when the return window closes, are spent first-to-expire
// first, expire after 12 months, and are restored or reversed exactly. One
// Electron is worth ₹1. Functions that change balances take the caller's
// transaction so a spend or earn commits with the order it belongs to.
package electrons

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// ErrInsufficient means the account has fewer available Electrons than asked.
var ErrInsufficient = httpx.NewError(http.StatusConflict, "INSUFFICIENT_ELECTRONS", "You don't have that many Electrons available")

// Reference types for ledger entries.
const (
	RefOrder      = "ORDER"
	RefMembership = "MEMBERSHIP"
	RefOrderItem  = "ORDER_ITEM"
	RefJob        = "JOB"
)

func newID() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

// Spend takes amount Electrons first-to-expire first and records one SPEND
// entry with its allocations. ref identifies what they paid for.
func Spend(ctx context.Context, q *store.Queries, account uuid.UUID, amount int32, typ, refType string, refID uuid.UUID) error {
	if amount <= 0 {
		return nil
	}
	lots, err := q.LockSpendableLots(ctx, account)
	if err != nil {
		return err
	}
	var have int32
	for _, l := range lots {
		have += l.Remaining
	}
	if have < amount {
		return ErrInsufficient
	}
	entry, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: account, Type: typ, Amount: -amount, RefType: refType, RefID: refID})
	if err != nil {
		return err
	}
	left := amount
	for _, l := range lots {
		if left == 0 {
			break
		}
		take := min(left, l.Remaining)
		if err := q.TakeFromLot(ctx, store.TakeFromLotParams{ID: l.ID, Amount: take}); err != nil {
			return err
		}
		if err := q.AddAllocation(ctx, store.AddAllocationParams{LedgerEntryID: entry.ID, LotID: l.ID, Amount: take}); err != nil {
			return err
		}
		left -= take
	}
	return nil
}

// Restore gives back up to amount Electrons spent for ref, into the lots they
// came from (newest lots first), never more than was spent. It returns how
// many were restored.
func Restore(ctx context.Context, q *store.Queries, account uuid.UUID, refType string, refID uuid.UUID, amount int32, note string) (int32, error) {
	if amount <= 0 {
		return 0, nil
	}
	allocs, err := q.SpendAllocations(ctx, store.SpendAllocationsParams{RefType: refType, RefID: refID})
	if err != nil || len(allocs) == 0 {
		return 0, err
	}
	done, err := q.RestoredByLot(ctx, store.RestoredByLotParams{RefType: refType, RefID: refID})
	if err != nil {
		return 0, err
	}
	back := map[uuid.UUID]int32{}
	for _, d := range done {
		if d.LotID != nil {
			back[*d.LotID] = d.Amount
		}
	}
	// Newest lots first (ids are time-ordered), so restored Electrons last longest.
	sort.SliceStable(allocs, func(i, j int) bool { return allocs[i].LotID.String() > allocs[j].LotID.String() })
	var restored int32
	for _, a := range allocs {
		if restored == amount {
			break
		}
		room := a.Amount - back[a.LotID]
		put := min(room, amount-restored)
		if put <= 0 {
			continue
		}
		if err := q.PutBackToLot(ctx, store.PutBackToLotParams{ID: a.LotID, Amount: put}); err != nil {
			return restored, err
		}
		lot := a.LotID
		if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: account, Type: "RESTORE", Amount: put, LotID: &lot,
			RefType: refType, RefID: refID, Note: note}); err != nil {
			return restored, err
		}
		restored += put
	}
	return restored, nil
}

// Rules are the Electrons rules snapshotted on the order.
type snapshot struct {
	Electrons pdp.ElectronRules `json:"electrons"`
}

// EarnForOrder creates one PENDING lot per item for a confirmed order (LLD
// 8.7): earned on the item total less Electrons spent, at the member or
// non-member rate in force when the order was placed, split across items pro
// rata (largest remainder). Safe to call twice.
func EarnForOrder(ctx context.Context, q *store.Queries, orderID uuid.UUID) error {
	o, err := q.GetOrder(ctx, orderID)
	if err != nil {
		return err
	}
	items, err := q.ListOrderItems(ctx, orderID)
	if err != nil || len(items) == 0 {
		return err
	}
	for _, it := range items {
		if it.ElectronsEarned > 0 {
			return nil // already earned
		}
	}
	var snap snapshot
	_ = json.Unmarshal(o.RulesSnapshot, &snap)
	eligible := o.ItemTotalPaise - int64(o.ElectronsUsed)*100
	total := snap.Electrons.Earn(eligible, o.IsMemberAtOrder)
	if total <= 0 {
		return nil
	}
	weights := make([]int64, len(items))
	for i, it := range items {
		weights[i] = max(0, it.LineTotalPaise-int64(it.ElectronsSpent)*100)
	}
	shares := Split(total, weights)
	for i, it := range items {
		if shares[i] == 0 {
			continue
		}
		lot, err := q.CreateLot(ctx, store.CreateLotParams{ID: newID(), AccountID: o.BuyerID, Source: "PURCHASE", SourceRef: it.ID,
			Amount: int32(shares[i]), Remaining: int32(shares[i]), Status: store.LotStatusPENDING, IsSynthetic: o.IsSynthetic})
		if err != nil {
			return err
		}
		if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: o.BuyerID, Type: "EARN_PURCHASE", Amount: int32(shares[i]),
			LotID: &lot.ID, RefType: RefOrderItem, RefID: it.ID, Note: o.Number}); err != nil {
			return err
		}
		if err := q.SetItemEarned(ctx, store.SetItemEarnedParams{ID: it.ID, ElectronsEarned: int32(shares[i])}); err != nil {
			return err
		}
	}
	return nil
}

// Split divides total across weights with the largest-remainder method, so
// the parts always add up to total.
func Split(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var sum int64
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 || total <= 0 {
		return out
	}
	type rem struct {
		i int
		r int64
	}
	var given int64
	rems := make([]rem, len(weights))
	for i, w := range weights {
		out[i] = total * w / sum
		rems[i] = rem{i, total * w % sum}
		given += out[i]
	}
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].r > rems[b].r })
	for k := 0; given < total; k++ {
		out[rems[k%len(rems)].i]++
		given++
	}
	return out
}

// ReverseItem cancels Electrons still pending for an item that was cancelled
// or returned (informational REVERSAL entry; they were never available).
func ReverseItem(ctx context.Context, q *store.Queries, account, itemID uuid.UUID) error {
	lots, err := q.LotsForItem(ctx, itemID)
	if err != nil {
		return err
	}
	for _, l := range lots {
		if l.Status != store.LotStatusPENDING {
			continue
		}
		if err := q.ReverseLot(ctx, l.ID); err != nil {
			return err
		}
		lot := l.ID
		if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: account, Type: "REVERSAL", Amount: -l.Amount, LotID: &lot,
			RefType: RefOrderItem, RefID: itemID}); err != nil {
			return err
		}
	}
	return nil
}

// Service runs the Electrons jobs and balance pages.
type Service struct {
	Pool *pgxpool.Pool
	Q    *store.Queries
}

func expiryMonths(ctx context.Context, q *store.Queries) int {
	if c, err := q.GetConfig(ctx, "electrons.expiry_months"); err == nil {
		var n int
		if json.Unmarshal(c.Value, &n) == nil && n > 0 {
			return n
		}
	}
	return 12
}

// Mature makes pending Electrons available once the return window has
// closed and nothing is being returned (hourly job).
func (s *Service) Mature(ctx context.Context) (int, error) {
	n := 0
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		lots, err := q.DueToMature(ctx)
		if err != nil {
			return err
		}
		months := expiryMonths(ctx, q)
		for _, l := range lots {
			exp := l.AvailableAt.AddDate(0, months, 0)
			if err := q.MatureLot(ctx, store.MatureLotParams{ID: l.ID, ExpiresAt: &exp}); err != nil {
				return err
			}
			lot := l.ID
			if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: l.AccountID, Type: "MATURE", Amount: l.Amount, LotID: &lot,
				RefType: RefOrderItem, RefID: l.SourceRef}); err != nil {
				return err
			}
			if _, err := outbox.Add(ctx, tx, "electrons.lot_available", l.AccountID.String(), map[string]any{
				"accountId": l.AccountID, "amount": l.Amount, "expiresAt": exp}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Expire removes what is left of lots past their expiry (daily job).
func (s *Service) Expire(ctx context.Context) (int, error) {
	n := 0
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		lots, err := q.DueToExpire(ctx)
		if err != nil {
			return err
		}
		for _, l := range lots {
			if err := q.ExpireLot(ctx, l.ID); err != nil {
				return err
			}
			if l.Remaining == 0 {
				continue
			}
			lot := l.ID
			if _, err := q.AddLedger(ctx, store.AddLedgerParams{ID: newID(), AccountID: l.AccountID, Type: "EXPIRY", Amount: -l.Remaining, LotID: &lot,
				RefType: RefJob, RefID: l.ID}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Balance is what the Electrons page shows.
type Balance struct {
	Available  int32       `json:"available"`
	Pending    int32       `json:"pending"`
	NextExpiry *NextExpiry `json:"nextExpiry,omitempty"`
}

// NextExpiry is the soonest batch to expire.
type NextExpiry struct {
	Amount int32     `json:"amount"`
	At     time.Time `json:"at"`
}

// GetBalance returns an account's balances.
func (s *Service) GetBalance(ctx context.Context, account uuid.UUID) (Balance, error) {
	b, err := s.Q.Balance(ctx, account)
	if err != nil {
		return Balance{}, err
	}
	out := Balance{Available: b.Available, Pending: b.Pending}
	if ne, err := s.Q.NextExpiry(ctx, account); err == nil && ne.ExpiresAt != nil {
		out.NextExpiry = &NextExpiry{Amount: ne.Amount, At: *ne.ExpiresAt}
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, err
	}
	return out, nil
}

// Entry is a ledger line for the history page.
type Entry struct {
	ID      uuid.UUID `json:"id"`
	Type    string    `json:"type"`
	Amount  int32     `json:"amount"`
	RefType string    `json:"refType"`
	RefID   uuid.UUID `json:"refId"`
	Note    string    `json:"note,omitempty"`
	At      time.Time `json:"at"`
}

// History lists ledger entries, newest first (50 per page).
func (s *Service) History(ctx context.Context, account uuid.UUID, before *time.Time) ([]Entry, error) {
	rows, err := s.Q.ListLedger(ctx, store.ListLedgerParams{AccountID: account, Before: before})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = Entry{ID: r.ID, Type: r.Type, Amount: r.Amount, RefType: r.RefType, RefID: r.RefID, Note: r.Note, At: r.CreatedAt}
	}
	return out, nil
}
