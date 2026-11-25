// Package orders runs fulfilment after checkout (LLD 8.5, 10): the seller's
// order queue, packing and cancelling sub-orders, buyer cancellation with
// restock and refund, and GST invoices.
package orders

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TopicStatusChanged matches shipping.TopicStatusChanged.
const TopicStatusChanged = "suborder.status_changed"

var (
	ErrCannotCancel = httpx.NewError(http.StatusConflict, "CANNOT_CANCEL", "This order has already shipped and can no longer be cancelled. You can return it after delivery")
	ErrCannotPack   = httpx.NewError(http.StatusConflict, "INVALID_TRANSITION", "Only new orders can be marked as packed")
	ErrNoInvoice    = httpx.NewError(http.StatusConflict, "NO_INVOICE_YET", "The invoice is issued when the order ships")
	ErrBadReason    = httpx.ErrValidation.WithDetail("Give a reason (3 to 200 characters)")
)

// Service runs fulfilment.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Payments *payment.Service // refunds for cancelled prepaid orders
	Log      *slog.Logger
}

// Address is the delivery address snapshot on the order.
type Address struct {
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Line1   string `json:"line1"`
	Line2   string `json:"line2"`
	City    string `json:"city"`
	State   string `json:"state"`
	Pincode string `json:"pincode"`
}

// SellerOrder is a row in the seller's order queue.
type SellerOrder struct {
	ID             uuid.UUID  `json:"id"`
	OrderNumber    string     `json:"orderNumber"`
	Status         string     `json:"status"`
	PlacedAt       time.Time  `json:"placedAt"`
	PayMethod      string     `json:"payMethod"`
	SubtotalPaise  int64      `json:"subtotalPaise"`
	ItemCount      int32      `json:"itemCount"`
	BuyerName      string     `json:"buyerName"`
	City           string     `json:"city"`
	Pincode        string     `json:"pincode"`
	AWB            string     `json:"awb,omitempty"`
	ShipmentStatus string     `json:"shipmentStatus,omitempty"`
	InvoiceNo      string     `json:"invoiceNo,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
}

var validStatus = map[string]bool{"CONFIRMED": true, "PACKED": true, "SHIPPED": true, "OUT_FOR_DELIVERY": true,
	"DELIVERED": true, "CANCELLED": true, "RTO": true}

// Queue lists the seller's sub-orders, newest first, with counts per status.
func (s *Service) Queue(ctx context.Context, seller uuid.UUID, status string) ([]SellerOrder, map[string]int32, error) {
	var filter store.NullSubStatus
	if status != "" {
		filter = store.NullSubStatus{SubStatus: store.SubStatus(status), Valid: true}
		if !validStatus[status] {
			return nil, nil, httpx.ErrValidation.WithDetail("Unknown status filter")
		}
	}
	rows, err := s.Q.SellerSubOrders(ctx, store.SellerSubOrdersParams{SellerID: seller, Status: filter})
	if err != nil {
		return nil, nil, err
	}
	out := make([]SellerOrder, len(rows))
	for i, r := range rows {
		var a Address
		_ = json.Unmarshal(r.Address, &a)
		out[i] = SellerOrder{ID: r.ID, OrderNumber: r.Number, Status: string(r.Status), PlacedAt: r.PlacedAt, PayMethod: r.PayMethod,
			SubtotalPaise: r.SubtotalPaise, ItemCount: r.ItemCount, BuyerName: a.Name, City: a.City, Pincode: a.Pincode,
			AWB: deref(r.Awb), ShipmentStatus: deref(r.ShipmentStatus), InvoiceNo: deref(r.InvoiceNo), DeliveredAt: r.DeliveredAt}
	}
	counts := map[string]int32{}
	cs, err := s.Q.SellerStatusCounts(ctx, seller)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cs {
		counts[string(c.Status)] = c.N
	}
	return out, counts, nil
}

// Item is an order line from the snapshot.
type Item struct {
	ID             uuid.UUID         `json:"id"`
	Name           string            `json:"name"`
	Attrs          map[string]string `json:"attrs"`
	CoverURL       string            `json:"coverUrl,omitempty"`
	Qty            int32             `json:"qty"`
	UnitPricePaise int64             `json:"unitPricePaise"`
	LineTotalPaise int64             `json:"lineTotalPaise"`
	Status         string            `json:"status"`
}

// Event is one step in a sub-order's history.
type Event struct {
	Status string    `json:"status"`
	Actor  string    `json:"actor"`
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"`
}

// SellerOrderDetail is one sub-order with everything needed to fulfil it.
type SellerOrderDetail struct {
	SellerOrder
	Address      Address `json:"address"`
	Items        []Item  `json:"items"`
	Events       []Event `json:"events"`
	CancelReason string  `json:"cancelReason,omitempty"`
	CanPack      bool    `json:"canPack"`
	CanShip      bool    `json:"canShip"`
	CanCancel    bool    `json:"canCancel"`
}

// SellerDetail returns one of the seller's sub-orders.
func (s *Service) SellerDetail(ctx context.Context, seller, id uuid.UUID) (SellerOrderDetail, error) {
	d, err := s.Q.GetSubOrderDetail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (d.SellerID != seller || d.Status == store.SubStatusPENDINGPAYMENT || d.Status == store.SubStatusEXPIRED)) {
		return SellerOrderDetail{}, httpx.ErrNotFound
	}
	if err != nil {
		return SellerOrderDetail{}, err
	}
	var a Address
	_ = json.Unmarshal(d.Address, &a)
	out := SellerOrderDetail{
		SellerOrder: SellerOrder{ID: d.ID, OrderNumber: d.Number, Status: string(d.Status), PlacedAt: d.PlacedAt, PayMethod: d.PayMethod,
			SubtotalPaise: d.SubtotalPaise, BuyerName: a.Name, City: a.City, Pincode: a.Pincode, InvoiceNo: deref(d.InvoiceNo), DeliveredAt: d.DeliveredAt},
		Address: a, Items: []Item{}, Events: []Event{}, CancelReason: deref(d.CancelReason),
		CanPack:   d.Status == store.SubStatusCONFIRMED,
		CanShip:   d.Status == store.SubStatusCONFIRMED || d.Status == store.SubStatusPACKED,
		CanCancel: d.Status == store.SubStatusCONFIRMED || d.Status == store.SubStatusPACKED,
	}
	items, err := s.Q.ListSubOrderItems(ctx, id)
	if err != nil {
		return SellerOrderDetail{}, err
	}
	for _, it := range items {
		out.Items = append(out.Items, itemView(it))
		out.ItemCount += it.Qty
	}
	if sh, err := s.Q.GetForwardShipment(ctx, id); err == nil {
		out.AWB, out.ShipmentStatus = sh.Awb, sh.Status
	}
	evs, err := s.Q.ListSubOrderEvents(ctx, d.OrderID)
	if err != nil {
		return SellerOrderDetail{}, err
	}
	for _, e := range evs {
		if e.SubOrderID == id {
			out.Events = append(out.Events, Event{Status: string(e.Status), Actor: e.Actor, Note: e.Note, At: e.At})
		}
	}
	return out, nil
}

func itemView(it store.OrderItem) Item {
	v := Item{ID: it.ID, Name: it.ProductName, Attrs: map[string]string{}, Qty: it.Qty, UnitPricePaise: it.UnitPricePaise,
		LineTotalPaise: it.LineTotalPaise, Status: it.Status}
	_ = json.Unmarshal(it.VariantAttrs, &v.Attrs)
	if it.CoverKey != "" {
		v.CoverURL = "/api/v1/media/" + it.CoverKey
	}
	return v
}

// Pack marks a new sub-order as packed (CONFIRMED → PACKED).
func (s *Service) Pack(ctx context.Context, seller, id uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		so, err := q.GetSubOrderForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && so.SellerID != seller) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := q.SetActor(ctx, store.SetActorParams{Actor: "SELLER", Note: ""}); err != nil {
			return err
		}
		if _, err := q.TransitionSubOrder(ctx, store.TransitionSubOrderParams{ID: id, ToStatus: store.SubStatusPACKED, FromStatus: []string{"CONFIRMED"}}); errors.Is(err, pgx.ErrNoRows) {
			return ErrCannotPack
		} else if err != nil {
			return err
		}
		_, err = outbox.Add(ctx, tx, TopicStatusChanged, id.String(), map[string]any{"subOrderId": id, "orderId": so.OrderID, "status": store.SubStatusPACKED})
		return err
	})
}

// Actor is who asks for a cancellation.
type Actor struct {
	ID   uuid.UUID
	Role string // BUYER or SELLER
}

// Cancel cancels a sub-order before it ships: stock goes back on sale and a
// prepaid sub-order is refunded in full.
func (s *Service) Cancel(ctx context.Context, by Actor, id uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if n := len([]rune(reason)); n < 3 || n > 200 {
		return ErrBadReason
	}
	var refund func(context.Context)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.GetSubOrderForUpdate(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		} else if err != nil {
			return err
		}
		d, err := q.GetSubOrderDetail(ctx, id)
		if err != nil {
			return err
		}
		if (by.Role == "BUYER" && d.BuyerID != by.ID) || (by.Role == "SELLER" && d.SellerID != by.ID) {
			return httpx.ErrNotFound
		}
		if err := q.SetActor(ctx, store.SetActorParams{Actor: by.Role, Note: reason}); err != nil {
			return err
		}
		if _, err := q.CancelSubOrder(ctx, store.CancelSubOrderParams{ID: id, Reason: &reason, CancelledBy: &by.Role}); errors.Is(err, pgx.ErrNoRows) {
			return ErrCannotCancel
		} else if err != nil {
			return err
		}
		items, err := q.ListSubOrderItems(ctx, id)
		if err != nil {
			return err
		}
		var spent int32
		for _, it := range items {
			if it.Status != "ACTIVE" {
				continue
			}
			if err := q.ReturnSoldStock(ctx, store.ReturnSoldStockParams{ID: it.LotID, Qty: it.Qty}); err != nil {
				return err
			}
			if err := electrons.ReverseItem(ctx, q, d.BuyerID, it.ID); err != nil {
				return err
			}
			spent += it.ElectronsSpent
		}
		// Electrons paid for these items come back; only the money part is refunded.
		if _, err := electrons.Restore(ctx, q, d.BuyerID, electrons.RefOrder, d.OrderID, spent, "Order cancelled"); err != nil {
			return err
		}
		money := d.SubtotalPaise - int64(spent)*100
		if err := q.SetOrderItemsStatus(ctx, store.SetOrderItemsStatusParams{SubOrderID: id, Status: "CANCELLED"}); err != nil {
			return err
		}
		if d.PayMethod == "ONLINE" && d.PaymentID != nil && money > 0 {
			paymentID, amount, ref := *d.PaymentID, money, id.String()+"-CANCEL"
			refund = func(ctx context.Context) {
				if _, err := s.Payments.Refund(ctx, paymentID, amount, 0, ref, "Order cancelled before shipping"); err != nil {
					s.Log.Error("cancellation refund failed", "subOrder", id, "err", err)
				}
			}
		}
		_, err = outbox.Add(ctx, tx, TopicStatusChanged, id.String(), map[string]any{
			"subOrderId": id, "orderId": d.OrderID, "number": d.Number, "buyerId": d.BuyerID, "sellerId": d.SellerID,
			"status": store.SubStatusCANCELLED, "by": by.Role, "reason": reason})
		return err
	})
	if err == nil && refund != nil && s.Payments != nil {
		refund(context.WithoutCancel(ctx))
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
