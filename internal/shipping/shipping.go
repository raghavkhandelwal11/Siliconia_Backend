// Package shipping books shipments with the shipping provider, applies its
// tracking webhooks to sub-orders and answers PIN serviceability (LLD 8.5).
package shipping

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// TopicStatusChanged is published whenever a sub-order's status moves.
const TopicStatusChanged = "suborder.status_changed"

var (
	ErrNotReady       = httpx.NewError(http.StatusConflict, "NOT_READY_TO_SHIP", "Only confirmed orders can be shipped")
	ErrNoPickup       = httpx.NewError(http.StatusConflict, "PICKUP_ADDRESS_MISSING", "Add a pickup address to your seller profile first")
	ErrNotServiceable = httpx.NewError(http.StatusConflict, "NOT_SERVICEABLE", "No courier delivers to this PIN code right now")
	ErrShippingDown   = httpx.ErrUnavailable.WithDetail("The courier service is not responding. Try again in a minute")
)

// Service books shipments and tracks them.
type Service struct {
	Pool     *pgxpool.Pool
	Q        *store.Queries
	Provider provider.Provider
	Log      *slog.Logger

	cache sync.Map // pincode → cached
}

type cached struct {
	sv  provider.Serviceability
	exp time.Time
}

// Serviceability is what the shop tells buyers about a PIN code.
type Serviceability struct {
	Pincode     string `json:"pincode"`
	Deliverable bool   `json:"deliverable"`
	COD         bool   `json:"cod"`
	ETADays     int    `json:"etaDays"`
	Known       bool   `json:"-"` // false when the courier service did not answer
}

// Check answers serviceability for a PIN, cached for 10 minutes. When the
// courier service is down it returns Known=false so callers can decide.
func (s *Service) Check(ctx context.Context, pin string) Serviceability {
	if c, ok := s.cache.Load(pin); ok && time.Now().Before(c.(cached).exp) {
		sv := c.(cached).sv
		return Serviceability{Pincode: pin, Deliverable: sv.Deliverable, COD: sv.COD, ETADays: sv.ETADays, Known: true}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sv, err := s.Provider.Serviceability(ctx, pin)
	if err != nil {
		s.Log.Warn("serviceability check failed", "err", err)
		return Serviceability{Pincode: pin}
	}
	s.cache.Store(pin, cached{sv: sv, exp: time.Now().Add(10 * time.Minute)})
	return Serviceability{Pincode: pin, Deliverable: sv.Deliverable, COD: sv.COD, ETADays: sv.ETADays, Known: true}
}

// Shipment is a booked shipment as sellers and buyers see it.
type Shipment struct {
	AWB            string     `json:"awb"`
	Status         string     `json:"status"`
	ETA            *time.Time `json:"eta,omitempty"`
	CODAmountPaise int64      `json:"codAmountPaise"`
	Courier        string     `json:"courier"`
}

func view(sh store.Shipment) Shipment {
	return Shipment{AWB: sh.Awb, Status: sh.Status, ETA: sh.Eta, CODAmountPaise: sh.CodAmountPaise, Courier: sh.Provider}
}

type orderAddress struct {
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Line1   string `json:"line1"`
	Line2   string `json:"line2"`
	City    string `json:"city"`
	State   string `json:"state"`
	Pincode string `json:"pincode"`
}

// Ship books the forward shipment for a seller's sub-order (CONFIRMED or
// PACKED) and moves it to SHIPPED. Calling it again returns the same shipment.
func (s *Service) Ship(ctx context.Context, seller, subOrderID uuid.UUID, weightGrams int) (Shipment, error) {
	if weightGrams <= 0 || weightGrams > 50000 {
		weightGrams = 500
	}
	var out Shipment
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		so, err := q.GetSubOrderForShipping(ctx, subOrderID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && so.SellerID != seller) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if existing, err := q.GetForwardShipment(ctx, subOrderID); err == nil {
			out = view(existing)
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if so.Status != store.SubStatusCONFIRMED && so.Status != store.SubStatusPACKED {
			return ErrNotReady
		}
		var pickup struct {
			Line1, Line2, City, State, Pincode, Phone string
		}
		if len(so.PickupAddress) == 0 || json.Unmarshal(so.PickupAddress, &pickup) != nil || pickup.Pincode == "" {
			return ErrNoPickup
		}
		var to orderAddress
		_ = json.Unmarshal(so.Address, &to)
		phone := pickup.Phone
		if phone == "" && so.SellerPhone != nil {
			phone = *so.SellerPhone
		}
		var cod int64
		if so.PayMethod == "COD" {
			cod = so.SubtotalPaise
		}
		req := provider.ShipmentRequest{
			Ref: subOrderID.String(), Kind: provider.Forward,
			From:      provider.Address{Name: so.StoreName, Phone: phone, Line1: pickup.Line1, Line2: pickup.Line2, City: pickup.City, State: pickup.State, Pincode: pickup.Pincode},
			To:        provider.Address{Name: to.Name, Phone: to.Phone, Line1: to.Line1, Line2: to.Line2, City: to.City, State: to.State, Pincode: to.Pincode},
			Parcels:   []provider.Parcel{{WeightGrams: weightGrams, Description: "Electronics"}},
			CODAmount: cod,
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		booked, err := s.Provider.CreateShipment(pctx, req)
		switch {
		case errors.Is(err, provider.ErrNotServiceable):
			return ErrNotServiceable
		case errors.Is(err, provider.ErrRejected):
			return httpx.ErrValidation.WithDetail("The courier could not accept this shipment. Check both addresses")
		case err != nil:
			s.Log.Error("create shipment", "err", err)
			return ErrShippingDown
		}
		id, _ := uuid.NewV7()
		var eta *time.Time
		if !booked.ETA.IsZero() {
			eta = &booked.ETA
		}
		sh, err := q.CreateShipment(ctx, store.CreateShipmentParams{ID: id, SubOrderID: subOrderID, Kind: provider.Forward, Ref: req.Ref,
			Provider: s.Provider.Name(), Awb: booked.AWB, Status: booked.Status, Eta: eta, CodAmountPaise: cod})
		if err != nil {
			return err
		}
		if err := q.SetActor(ctx, store.SetActorParams{Actor: "SELLER", Note: "AWB " + sh.Awb}); err != nil {
			return err
		}
		if _, err := q.TransitionSubOrder(ctx, store.TransitionSubOrderParams{ID: subOrderID, ToStatus: store.SubStatusSHIPPED,
			FromStatus: []string{"CONFIRMED", "PACKED"}}); err != nil {
			return err
		}
		out = view(sh)
		_, err = outbox.Add(ctx, tx, TopicStatusChanged, subOrderID.String(), map[string]any{
			"subOrderId": subOrderID, "orderId": so.OrderID, "number": so.Number, "status": store.SubStatusSHIPPED, "awb": sh.Awb})
		return err
	})
	return out, err
}

// Label returns the label PDF for a seller's shipped sub-order.
func (s *Service) Label(ctx context.Context, seller, subOrderID uuid.UUID) ([]byte, string, error) {
	so, err := s.Q.GetSubOrder(ctx, subOrderID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && so.SellerID != seller) {
		return nil, "", httpx.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	sh, err := s.Q.GetForwardShipment(ctx, subOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", httpx.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	pdf, err := s.Provider.Label(ctx, sh.Awb)
	if err != nil {
		s.Log.Error("label", "err", err)
		return nil, "", ErrShippingDown
	}
	return pdf, sh.Awb, nil
}

// subStatus maps a forward tracking status to the sub-order status it means.
func subStatus(tracking string) (store.SubStatus, bool) {
	switch tracking {
	case "PICKED_UP", "IN_TRANSIT":
		return store.SubStatusSHIPPED, true
	case "OUT_FOR_DELIVERY", "FAILED_ATTEMPT":
		return store.SubStatusOUTFORDELIVERY, true
	case "DELIVERED":
		return store.SubStatusDELIVERED, true
	case "RTO_DELIVERED":
		return store.SubStatusRTO, true
	}
	return "", false
}

// HandleWebhook verifies a tracking webhook and applies it once.
func (s *Service) HandleWebhook(r *http.Request) error {
	e, err := s.Provider.VerifyWebhook(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if _, err := q.RecordWebhook(ctx, store.RecordWebhookParams{Provider: s.Provider.Name(), EventID: e.ID, Payload: e.Raw}); errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate delivery
		} else if err != nil {
			return err
		}
		if err := s.apply(ctx, tx, q, e); err != nil {
			return err
		}
		return q.MarkWebhookProcessed(ctx, store.MarkWebhookProcessedParams{Provider: s.Provider.Name(), EventID: e.ID})
	})
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, q *store.Queries, e provider.Event) error {
	sh, err := q.GetShipmentByAWBForUpdate(ctx, e.AWB)
	if errors.Is(err, pgx.ErrNoRows) {
		s.Log.Warn("tracking for unknown shipment", "awb", e.AWB)
		return nil
	}
	if err != nil {
		return err
	}
	id, _ := uuid.NewV7()
	if err := q.AddTrackingEvent(ctx, store.AddTrackingEventParams{ID: id, ShipmentID: sh.ID, Status: e.Status, Location: e.Location, At: e.At, Raw: e.Raw}); err != nil {
		return err
	}
	// Webhooks can arrive out of order: only a newer scan moves the status.
	if sh.LastEventAt != nil && !e.At.After(*sh.LastEventAt) {
		return nil
	}
	var eta *time.Time
	if !e.ETA.IsZero() {
		eta = &e.ETA
	}
	if err := q.UpdateShipmentStatus(ctx, store.UpdateShipmentStatusParams{ID: sh.ID, Status: e.Status, Eta: eta, LastEventAt: &e.At}); err != nil {
		return err
	}
	if sh.Kind != provider.Forward {
		return nil // return pickups are handled by returns (T2.6)
	}
	next, ok := subStatus(e.Status)
	if !ok {
		return nil
	}
	so, err := q.GetSubOrder(ctx, sh.SubOrderID)
	if err != nil {
		return err
	}
	if so.Status == next || so.Status == store.SubStatusDELIVERED || so.Status == store.SubStatusRTO || so.Status == store.SubStatusCANCELLED {
		return nil
	}
	if err := q.SetActor(ctx, store.SetActorParams{Actor: "COURIER", Note: e.Location}); err != nil {
		return err
	}
	if next == store.SubStatusDELIVERED {
		days := returnDays(ctx, q)
		if _, err := q.MarkSubOrderDelivered(ctx, store.MarkSubOrderDeliveredParams{ID: so.ID, DeliveredAt: &e.At,
			ReturnWindowEndsAt: ptr(e.At.Add(time.Duration(days) * 24 * time.Hour)), CodCollectedPaise: sh.CodAmountPaise}); err != nil {
			return err
		}
	} else if _, err := q.SetSubOrderStatus(ctx, store.SetSubOrderStatusParams{ID: so.ID, Status: next}); err != nil {
		return err
	}
	_, err = outbox.Add(ctx, tx, TopicStatusChanged, so.ID.String(), map[string]any{
		"subOrderId": so.ID, "orderId": so.OrderID, "status": next, "awb": sh.Awb, "tracking": e.Status, "at": e.At})
	return err
}

func returnDays(ctx context.Context, q *store.Queries) int64 {
	if c, err := q.GetConfig(ctx, "returns.window_days"); err == nil {
		var n int64
		if json.Unmarshal(c.Value, &n) == nil && n > 0 {
			return n
		}
	}
	return 7
}

func ptr[T any](v T) *T { return &v }
