// Package returns handles returns and replacements within the return window
// (LLD 8.6, 10): the buyer asks, the seller decides, the courier collects the
// item, and on arrival the buyer is refunded or a replacement is sent.
package returns

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ids"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TopicStatusChanged is published whenever a return moves.
const TopicStatusChanged = "return.status_changed"

var (
	ErrWindowClosed = httpx.NewError(http.StatusConflict, "RETURN_WINDOW_CLOSED", "The return window for this item has closed")
	ErrNotDelivered = httpx.NewError(http.StatusConflict, "NOT_DELIVERED", "Items can be returned after they are delivered")
	ErrExists       = httpx.NewError(http.StatusConflict, "RETURN_EXISTS", "A return is already open for this item")
	ErrNotReturn    = httpx.NewError(http.StatusConflict, "NOT_RETURNABLE", "This item can no longer be returned")
	ErrDecided      = httpx.NewError(http.StatusConflict, "ALREADY_DECIDED", "This return has already been decided")
	ErrNoPickup     = httpx.NewError(http.StatusConflict, "PICKUP_ADDRESS_MISSING", "Add a pickup address to your seller profile first")
	ErrCourier      = httpx.ErrUnavailable.WithDetail("The courier service is not responding. Try again in a minute")
)

var reasons = map[string]bool{"DAMAGED": true, "DEFECTIVE": true, "WRONG_ITEM": true, "NOT_AS_DESCRIBED": true, "MISSING_PARTS": true, "NO_LONGER_NEEDED": true}

// restockable reasons leave the unit fit to sell again; the others go aside.
var restockable = map[string]bool{"NO_LONGER_NEEDED": true, "WRONG_ITEM": true}

// Service runs returns.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Courier  provider.Provider
	Payments *payment.Service
	Log      *slog.Logger
}

// Request is what the buyer asks for.
type Request struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// View is a return as buyers and sellers see it.
type View struct {
	ID                uuid.UUID         `json:"id"`
	Number            string            `json:"number"`
	OrderID           uuid.UUID         `json:"orderId"`
	OrderNumber       string            `json:"orderNumber"`
	ItemID            uuid.UUID         `json:"itemId"`
	ProductName       string            `json:"productName"`
	Attrs             map[string]string `json:"attrs"`
	CoverURL          string            `json:"coverUrl,omitempty"`
	Qty               int32             `json:"qty"`
	Type              string            `json:"type"`
	Reason            string            `json:"reason"`
	Note              string            `json:"note,omitempty"`
	Status            string            `json:"status"`
	RejectReason      string            `json:"rejectReason,omitempty"`
	PickupAWB         string            `json:"pickupAwb,omitempty"`
	RefundAmountPaise int64             `json:"refundAmountPaise"`
	OutcomeNote       string            `json:"outcomeNote,omitempty"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
}

// Create opens a return or replacement for a delivered item.
func (s *Service) Create(ctx context.Context, buyer, itemID uuid.UUID, in Request) (View, error) {
	in.Note = strings.TrimSpace(in.Note)
	if (in.Type != "RETURN" && in.Type != "REPLACE") || !reasons[in.Reason] || len([]rune(in.Note)) > 500 {
		return View{}, httpx.ErrValidation.WithDetail("Choose return or replacement, a reason, and keep the note under 500 characters")
	}
	var r store.ReturnRequest
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		it, err := q.GetItemForReturn(ctx, itemID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && it.BuyerID != buyer) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case it.SubStatus != store.SubStatusDELIVERED:
			return ErrNotDelivered
		case it.ItemStatus != "ACTIVE":
			return ErrNotReturn
		case it.ReturnWindowEndsAt == nil || time.Now().After(*it.ReturnWindowEndsAt):
			return ErrWindowClosed
		}
		id, _ := uuid.NewV7()
		r, err = q.CreateReturn(ctx, store.CreateReturnParams{ID: id, Number: ids.Readable("RT", time.Now()), OrderItemID: itemID,
			SubOrderID: it.SubOrderID, BuyerID: buyer, SellerID: it.SellerID, Type: in.Type, Reason: in.Reason, Note: in.Note})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrExists
		}
		if err != nil {
			return err
		}
		return s.event(ctx, tx, r)
	})
	if err != nil {
		return View{}, err
	}
	list, err := s.BuyerList(ctx, buyer)
	if err != nil {
		return View{}, err
	}
	for _, v := range list {
		if v.ID == r.ID {
			return v, nil
		}
	}
	return View{}, httpx.ErrNotFound
}

func (s *Service) event(ctx context.Context, tx pgx.Tx, r store.ReturnRequest) error {
	_, err := outbox.Add(ctx, tx, TopicStatusChanged, r.ID.String(), map[string]any{
		"returnId": r.ID, "number": r.Number, "buyerId": r.BuyerID, "sellerId": r.SellerID, "type": r.Type, "status": r.Status})
	return err
}

func view(r store.ReturnRequest, name string, attrs []byte, cover string, qty int32, orderNumber string, orderID uuid.UUID, awb *string) View {
	v := View{ID: r.ID, Number: r.Number, OrderID: orderID, OrderNumber: orderNumber, ItemID: r.OrderItemID, ProductName: name,
		Attrs: map[string]string{}, Qty: qty, Type: r.Type, Reason: r.Reason, Note: r.Note, Status: r.Status,
		RefundAmountPaise: r.RefundAmountPaise, OutcomeNote: r.OutcomeNote, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	_ = json.Unmarshal(attrs, &v.Attrs)
	if cover != "" {
		v.CoverURL = "/api/v1/media/" + cover
	}
	if r.RejectReason != nil {
		v.RejectReason = *r.RejectReason
	}
	if awb != nil {
		v.PickupAWB = *awb
	}
	return v
}

func buyerView(row store.ListBuyerReturnsRow) View {
	r := store.ReturnRequest{ID: row.ID, Number: row.Number, OrderItemID: row.OrderItemID, SubOrderID: row.SubOrderID, BuyerID: row.BuyerID,
		SellerID: row.SellerID, Type: row.Type, Reason: row.Reason, Note: row.Note, Status: row.Status, RejectReason: row.RejectReason,
		RefundAmountPaise: row.RefundAmountPaise, OutcomeNote: row.OutcomeNote, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	return view(r, row.ProductName, row.VariantAttrs, row.CoverKey, row.Qty, row.OrderNumber, row.OrderID, row.Awb)
}

// BuyerList lists the buyer's returns, newest first.
func (s *Service) BuyerList(ctx context.Context, buyer uuid.UUID) ([]View, error) {
	rows, err := s.Q.ListBuyerReturns(ctx, buyer)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(rows))
	for i, row := range rows {
		out[i] = buyerView(row)
	}
	return out, nil
}

// SellerList lists returns for the seller's items.
func (s *Service) SellerList(ctx context.Context, seller uuid.UUID, status string) ([]View, error) {
	var st *string
	if status != "" {
		st = &status
	}
	rows, err := s.Q.ListSellerReturns(ctx, store.ListSellerReturnsParams{SellerID: seller, Status: st})
	if err != nil {
		return nil, err
	}
	out := make([]View, len(rows))
	for i, row := range rows {
		r := store.ReturnRequest{ID: row.ID, Number: row.Number, OrderItemID: row.OrderItemID, SubOrderID: row.SubOrderID, BuyerID: row.BuyerID,
			SellerID: row.SellerID, Type: row.Type, Reason: row.Reason, Note: row.Note, Status: row.Status, RejectReason: row.RejectReason,
			RefundAmountPaise: row.RefundAmountPaise, OutcomeNote: row.OutcomeNote, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
		out[i] = view(r, row.ProductName, row.VariantAttrs, row.CoverKey, row.Qty, row.OrderNumber, row.OrderID, row.Awb)
	}
	return out, nil
}

type address struct {
	Name, Phone, Line1, Line2, City, State, Pincode string
}

// Decide approves (booking the courier pickup) or rejects a return.
func (s *Service) Decide(ctx context.Context, seller, id uuid.UUID, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && (len([]rune(reason)) < 3 || len([]rune(reason)) > 300) {
		return httpx.ErrValidation.WithDetail("Tell the buyer why (3 to 300 characters)")
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		r, err := q.GetReturnForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.SellerID != seller) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.Status != "REQUESTED" {
			return ErrDecided
		}
		if !approve {
			if r, err = q.RejectReturn(ctx, store.RejectReturnParams{ID: id, RejectReason: &reason}); err != nil {
				return err
			}
			return s.event(ctx, tx, r)
		}
		pickup, err := q.GetSellerPickup(ctx, seller)
		if err != nil {
			return err
		}
		var to struct{ Line1, Line2, City, State, Pincode, Phone string }
		if len(pickup.PickupAddress) == 0 || json.Unmarshal(pickup.PickupAddress, &to) != nil || to.Pincode == "" {
			return ErrNoPickup
		}
		if to.Phone == "" && pickup.PhoneE164 != nil {
			to.Phone = *pickup.PhoneE164
		}
		so, err := q.GetSubOrder(ctx, r.SubOrderID)
		if err != nil {
			return err
		}
		o, err := q.GetOrder(ctx, so.OrderID)
		if err != nil {
			return err
		}
		var from address
		_ = json.Unmarshal(o.Address, &from)
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		booked, err := s.Courier.CreateShipment(pctx, provider.ShipmentRequest{
			Ref: r.ID.String(), Kind: provider.Return,
			From: provider.Address{Name: from.Name, Phone: from.Phone, Line1: from.Line1, Line2: from.Line2, City: from.City, State: from.State, Pincode: from.Pincode},
			To:   provider.Address{Name: pickup.StoreName, Phone: to.Phone, Line1: to.Line1, Line2: to.Line2, City: to.City, State: to.State, Pincode: to.Pincode},
		})
		if err != nil {
			s.Log.Error("return pickup", "err", err)
			return ErrCourier
		}
		shipID, _ := uuid.NewV7()
		var eta *time.Time
		if !booked.ETA.IsZero() {
			eta = &booked.ETA
		}
		sh, err := q.CreateShipment(ctx, store.CreateShipmentParams{ID: shipID, SubOrderID: &r.SubOrderID, Kind: provider.Return, Ref: r.ID.String(),
			Provider: s.Courier.Name(), Awb: booked.AWB, Status: booked.Status, Eta: eta})
		if err != nil {
			return err
		}
		if r, err = q.ScheduleReturnPickup(ctx, store.ScheduleReturnPickupParams{ID: id, PickupShipmentID: &sh.ID}); err != nil {
			return err
		}
		return s.event(ctx, tx, r)
	})
}

// OnTracking applies a courier scan on a return pickup inside the webhook's
// transaction. It may return a refund to send after the commit.
func (s *Service) OnTracking(ctx context.Context, tx pgx.Tx, q *store.Queries, sh store.Shipment, status string) (func(context.Context), error) {
	r, err := q.GetReturnByShipmentForUpdate(ctx, &sh.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "PICKED_UP", "IN_TRANSIT":
		if r.Status != "PICKUP_SCHEDULED" {
			return nil, nil
		}
		if r, err = q.SetReturnStatus(ctx, store.SetReturnStatusParams{ID: r.ID, ToStatus: "PICKED_UP", FromStatus: []string{"PICKUP_SCHEDULED"}}); err != nil {
			return nil, err
		}
		return nil, s.event(ctx, tx, r)
	case "DELIVERED":
		return s.receive(ctx, tx, q, r)
	}
	return nil, nil
}

// receive completes a return when the item reaches the seller: a replacement
// is created when stock allows, otherwise the buyer is refunded.
func (s *Service) receive(ctx context.Context, tx pgx.Tx, q *store.Queries, r store.ReturnRequest) (func(context.Context), error) {
	it, err := q.GetItemForReturn(ctx, r.OrderItemID)
	if err != nil {
		return nil, err
	}
	if restockable[r.Reason] {
		if err := q.ReturnSoldStock(ctx, store.ReturnSoldStockParams{ID: it.LotID, Qty: it.Qty}); err != nil {
			return nil, err
		}
	}
	if err := q.SetActor(ctx, store.SetActorParams{Actor: "SYSTEM", Note: "Replacement for return " + r.Number}); err != nil {
		return nil, err
	}

	fallback := false
	if r.Type == "REPLACE" {
		_, err := q.ReserveStock(ctx, store.ReserveStockParams{ID: it.LotID, Qty: it.Qty})
		switch {
		case err == nil:
			if err := q.CommitReservedStock(ctx, store.CommitReservedStockParams{ID: it.LotID, Qty: it.Qty}); err != nil {
				return nil, err
			}
			subID, _ := uuid.NewV7()
			if _, err := q.CreateReplacementSubOrder(ctx, store.CreateReplacementSubOrderParams{ID: subID, OrderID: it.OrderID, SellerID: it.SellerID, ReplacesItemID: &it.ID}); err != nil {
				return nil, err
			}
			newItem, _ := uuid.NewV7()
			if err := q.CopyItemAsReplacement(ctx, store.CopyItemAsReplacementParams{NewID: newItem, SubOrderID: subID, ItemID: it.ID}); err != nil {
				return nil, err
			}
			if err := q.SetOrderItemStatus(ctx, store.SetOrderItemStatusParams{ID: it.ID, Status: "REPLACED"}); err != nil {
				return nil, err
			}
			if r, err = q.CompleteReturn(ctx, store.CompleteReturnParams{ID: r.ID, ToStatus: "RECEIVED", OutcomeNote: "Replacement on its way",
				ReplacementSubOrderID: &subID}); err != nil {
				return nil, err
			}
			if _, err := outbox.Add(ctx, tx, "suborder.status_changed", subID.String(), map[string]any{
				"subOrderId": subID, "orderId": it.OrderID, "sellerId": it.SellerID, "status": "CONFIRMED", "replacement": true}); err != nil {
				return nil, err
			}
			return nil, s.event(ctx, tx, r)
		case errors.Is(err, pgx.ErrNoRows):
			fallback = true // no stock to replace: refund instead
		default:
			return nil, err
		}
	}

	if err := q.SetOrderItemStatus(ctx, store.SetOrderItemStatusParams{ID: it.ID, Status: "RETURNED"}); err != nil {
		return nil, err
	}
	if err := electrons.ReverseItem(ctx, q, it.BuyerID, it.ID); err != nil {
		return nil, err
	}
	// Electrons spent on the item come back; the refund is the money part (LLD 8.6).
	if _, err := electrons.Restore(ctx, q, it.BuyerID, electrons.RefOrder, it.OrderID, it.ElectronsSpent, "Return "+r.Number); err != nil {
		return nil, err
	}
	amount := it.LineTotalPaise - int64(it.ElectronsSpent)*100
	var status, note string
	switch {
	case amount == 0:
		status, note = "REFUNDED", "Nothing to refund on a free replacement unit"
	case it.PayMethod == "COD" || it.PaymentID == nil:
		// Paid in cash: the refund goes to a bank account (payouts, M5).
		status, note = "REFUND_PENDING", "Refund to your bank account is being arranged"
	default:
		status, note = "REFUNDED", "Refunded to the original payment method"
	}
	if fallback {
		note = "Replacement was out of stock. " + note
	}
	r2, err := q.CompleteReturn(ctx, store.CompleteReturnParams{ID: r.ID, ToStatus: status, RefundAmountPaise: amount, OutcomeNote: note})
	if err != nil {
		return nil, err
	}
	if err := s.event(ctx, tx, r2); err != nil {
		return nil, err
	}
	if status != "REFUNDED" || amount == 0 || it.PaymentID == nil {
		return nil, nil
	}
	paymentID, ref, number := *it.PaymentID, r.ID.String()+"-REFUND", r.Number
	return func(ctx context.Context) {
		if _, err := s.Payments.Refund(ctx, paymentID, amount, 0, ref, "Return "+number); err != nil {
			s.Log.Error("return refund failed", "return", number, "err", err)
		}
	}, nil
}
