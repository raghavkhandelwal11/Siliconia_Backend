// Package checkout quotes the cart, places orders with stock reservations and
// idempotency keys, hands online payments to the payment provider, confirms
// orders when payment is captured and expires unpaid orders (LLD 8.3,
// UI-BUY-02..04, FR-CHK, FR-ORD-01).
package checkout

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/electrons"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/launch"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/membership"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/shipping"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Pay methods a buyer can choose at checkout.
const (
	PayOnline        = "ONLINE"
	PayCOD           = "COD"
	PayElectronsOnly = "ELECTRONS_ONLY"
)

var (
	ErrCartEmpty     = httpx.NewError(http.StatusConflict, "CART_EMPTY", "Your cart is empty")
	ErrCartProblems  = httpx.NewError(http.StatusConflict, "CART_PROBLEMS", "Some items in your cart need attention first")
	ErrQuoteChanged  = httpx.NewError(http.StatusConflict, "QUOTE_CHANGED", "Prices or stock changed. Review your order again")
	ErrStockChanged  = httpx.NewError(http.StatusConflict, "STOCK_INSUFFICIENT", "Someone just bought the last units of an item. Review your order again")
	ErrCODNotAllowed = httpx.NewError(http.StatusUnprocessableEntity, "COD_NOT_ALLOWED", "Cash on delivery is not available for this order")
	ErrNoDelivery    = httpx.NewError(http.StatusConflict, "NOT_SERVICEABLE", "We don't deliver to this PIN code yet. Choose another address")
	ErrPhoneRequired = httpx.NewError(http.StatusUnprocessableEntity, "PHONE_REQUIRED", "Verify your mobile number before placing an order")
	ErrIdemConflict  = httpx.NewError(http.StatusUnprocessableEntity, "IDEMPOTENCY_CONFLICT", "This request key was already used for a different order")
	ErrIdemPending   = httpx.NewError(http.StatusConflict, "IN_PROGRESS", "This order is still being placed")
	ErrNotPending    = httpx.NewError(http.StatusConflict, "NOT_AWAITING_PAYMENT", "This order is not waiting for payment")
)

// PinChecker answers PIN serviceability (the shipping module).
type PinChecker interface {
	Check(ctx context.Context, pin string) shipping.Serviceability
}

// Service runs checkout.
type Service struct {
	Shipping  PinChecker // nil: every PIN is served with COD (unit tests)
	Launches  *launch.Service
	Pool      *pgxpool.Pool
	Q         *store.Queries
	Payments  *payment.Service
	PublicURL string // where the payment page sends shoppers back to
	Log       *slog.Logger
}

// Rules are the settings an order snapshots (LLD 4.7).
type Rules struct {
	Electrons   pdp.ElectronRules `json:"electrons"`
	CODMaxPaise int64             `json:"codMaxPaise"`
	HoldMinutes int64             `json:"holdMinutes"`
	ReturnDays  int64             `json:"returnDays"`
}

func (s *Service) rules(ctx context.Context, q *store.Queries) Rules {
	r := Rules{Electrons: pdp.Rules(ctx, q), CODMaxPaise: 800000, HoldMinutes: 10, ReturnDays: 7}
	for key, dst := range map[string]*int64{"cod.max_order_paise": &r.CODMaxPaise, "checkout.hold_minutes": &r.HoldMinutes, "returns.window_days": &r.ReturnDays} {
		if c, err := q.GetConfig(ctx, key); err == nil {
			var n int64
			if json.Unmarshal(c.Value, &n) == nil && n > 0 {
				*dst = n
			}
		}
	}
	return r
}

func (s *Service) pin(ctx context.Context, pin string) shipping.Serviceability {
	if s.Shipping == nil {
		return shipping.Serviceability{Pincode: pin, Deliverable: true, COD: true, ETADays: 4, Known: true}
	}
	return s.Shipping.Check(ctx, pin)
}

// Delivery is the delivery estimate for the chosen address.
type Delivery struct {
	Deliverable bool       `json:"deliverable"`
	ETADays     int        `json:"etaDays,omitempty"`
	EstimatedBy *time.Time `json:"estimatedBy,omitempty"`
}

// QuoteLine is one product at checkout.
type QuoteLine struct {
	VariantID      uuid.UUID         `json:"variantId"`
	Name           string            `json:"name"`
	Slug           string            `json:"slug"`
	CoverURL       string            `json:"coverUrl,omitempty"`
	Attrs          map[string]string `json:"attrs"`
	Qty            int32             `json:"qty"`
	UnitPricePaise int64             `json:"unitPricePaise"`
	UnitMrpPaise   int64             `json:"unitMrpPaise"`
	LineTotalPaise int64             `json:"lineTotalPaise"`
	Problem        string            `json:"problem,omitempty"`
}

// QuoteGroup is one seller's part of the order (a sub-order).
type QuoteGroup struct {
	StoreName     string      `json:"storeName"`
	Lines         []QuoteLine `json:"lines"`
	SubtotalPaise int64       `json:"subtotalPaise"`
}

// Quote is the checkout summary the buyer confirms.
type Quote struct {
	Groups             []QuoteGroup `json:"groups"`
	ItemCount          int32        `json:"itemCount"`
	ItemTotalPaise     int64        `json:"itemTotalPaise"`
	MrpTotalPaise      int64        `json:"mrpTotalPaise"`
	SavingsPaise       int64        `json:"savingsPaise"`
	ShippingPaise      int64        `json:"shippingPaise"`
	PayablePaise       int64        `json:"payablePaise"`
	CODEligible        bool         `json:"codEligible"`
	CODLimitPaise      int64        `json:"codLimitPaise"`
	CODReason          string       `json:"codReason,omitempty"` // NOT_MEMBER, LAUNCH_DEAL, OVER_LIMIT, PIN_NO_COD
	IsMember           bool         `json:"isMember"`
	ElectronsAvailable int32        `json:"electronsAvailable"`
	ElectronsMax       int32        `json:"electronsMax"`
	ElectronsUsed      int32        `json:"electronsUsed"`
	ElectronsEarn      int64        `json:"electronsEarn"`
	ElectronsOnly      bool         `json:"electronsOnly"`
	LaunchDeal         bool         `json:"launchDeal"`
	Delivery           *Delivery    `json:"delivery,omitempty"`
	ElectronsNonMember int64        `json:"electronsNonMember"`
	ElectronsMember    int64        `json:"electronsMember"`
	QuoteHash          string       `json:"quoteHash"`
	CanPlace           bool         `json:"canPlace"`
	Address            *Address     `json:"address,omitempty"`
}

type built struct {
	quote Quote
	rows  []store.CheckoutLinesRow
}

// build prices the cart lines. Shipping is free in the prototype (the BRD
// sets no delivery fee); serviceability arrives with sim-ship (T2.4).
// opts are the buyer-specific inputs to a quote.
type opts struct {
	member     bool
	balance    int32
	electrons  int32
	launchDeal bool
	claim      string // claim or launch id, part of the quote hash
}

// ErrTooManyElectrons means the buyer asked to use more than allowed.
var ErrTooManyElectrons = httpx.NewError(http.StatusUnprocessableEntity, "ELECTRONS_TOO_MANY", "You can use up to your available Electrons and the order total")

func (s *Service) build(ctx context.Context, q *store.Queries, rows []store.CheckoutLinesRow, addr *store.Address, payMethod string, rules Rules, o opts) built {
	var sv *shipping.Serviceability
	if addr != nil {
		v := s.pin(ctx, addr.Pincode)
		sv = &v
	}
	out := Quote{Groups: []QuoteGroup{}, CODLimitPaise: rules.CODMaxPaise}
	ok := len(rows) > 0
	h := sha256.New()
	var lastSeller uuid.UUID
	for _, r := range rows {
		l := QuoteLine{VariantID: r.VariantID, Name: r.Name, Slug: r.Slug, Qty: r.Qty, UnitPricePaise: r.PricePaise, UnitMrpPaise: r.MrpPaise,
			LineTotalPaise: r.PricePaise * int64(r.Qty), Attrs: map[string]string{}}
		_ = json.Unmarshal(r.Attrs, &l.Attrs)
		if r.Cover != "" {
			l.CoverURL = "/api/v1/media/" + r.Cover
		}
		switch {
		case r.Buyable == nil || !*r.Buyable || r.LotID == nil:
			l.Problem = "UNAVAILABLE"
		case r.Available <= 0:
			l.Problem = "OUT_OF_STOCK"
		case r.Available < r.Qty:
			l.Problem = "LOW_STOCK"
		}
		if l.Problem != "" {
			ok = false
		}
		if len(out.Groups) == 0 || r.SellerID != lastSeller {
			out.Groups = append(out.Groups, QuoteGroup{StoreName: r.StoreName, Lines: []QuoteLine{}})
			lastSeller = r.SellerID
		}
		g := &out.Groups[len(out.Groups)-1]
		g.Lines = append(g.Lines, l)
		g.SubtotalPaise += l.LineTotalPaise
		out.ItemTotalPaise += l.LineTotalPaise
		out.MrpTotalPaise += r.MrpPaise * int64(r.Qty)
		out.ItemCount += r.Qty
		h.Write([]byte(r.VariantID.String() + ":" + strconv.Itoa(int(r.Qty)) + ":" + strconv.FormatInt(r.PricePaise, 10) + ";"))
	}
	out.SavingsPaise = out.MrpTotalPaise - out.ItemTotalPaise
	out.IsMember, out.LaunchDeal, out.ElectronsAvailable = o.member, o.launchDeal, o.balance
	out.ElectronsMax = int32(min(int64(o.balance), out.ItemTotalPaise/100))
	out.ElectronsUsed = min(o.electrons, out.ElectronsMax)
	out.PayablePaise = out.ItemTotalPaise - int64(out.ElectronsUsed)*100 + out.ShippingPaise
	out.ElectronsOnly = out.PayablePaise == 0 && out.ItemTotalPaise > 0
	out.ElectronsEarn = rules.Electrons.Earn(out.ItemTotalPaise-int64(out.ElectronsUsed)*100, o.member)
	// COD (LLD 9.3): members only, within the limit, no launch deals, and the PIN must allow it.
	switch {
	case !o.member:
		out.CODReason = "NOT_MEMBER"
	case o.launchDeal:
		out.CODReason = "LAUNCH_DEAL"
	case out.PayablePaise <= 0 || out.PayablePaise > rules.CODMaxPaise:
		out.CODReason = "OVER_LIMIT"
	default:
		out.CODEligible = true
	}
	deliverable := true
	if sv != nil && sv.Known {
		d := &Delivery{Deliverable: sv.Deliverable}
		if sv.Deliverable {
			d.ETADays = sv.ETADays
			by := time.Now().In(ist).AddDate(0, 0, sv.ETADays)
			d.EstimatedBy = &by
		}
		out.Delivery, deliverable = d, sv.Deliverable
		if out.CODEligible && !sv.COD {
			out.CODEligible, out.CODReason = false, "PIN_NO_COD"
		}
	} else if sv != nil && out.CODEligible {
		// The courier service did not answer: no promise of cash on delivery.
		out.CODEligible, out.CODReason = false, "PIN_NO_COD"
	}
	out.ElectronsNonMember = rules.Electrons.Earn(out.ItemTotalPaise, false)
	out.ElectronsMember = rules.Electrons.Earn(out.ItemTotalPaise, true)
	if addr != nil {
		a := addressView(*addr)
		out.Address = &a
		h.Write([]byte(addr.ID.String()))
	}
	h.Write([]byte(payMethod + "|" + strconv.Itoa(int(out.ElectronsUsed)) + "|" + o.claim))
	out.QuoteHash = hex.EncodeToString(h.Sum(nil))[:24]
	out.CanPlace = ok && addr != nil && deliverable && (out.ElectronsOnly || payMethod != PayCOD || out.CODEligible)
	return built{quote: out, rows: rows}
}

// QuoteInput chooses the address and how to pay. With a launch claim (deal
// units) or a launch id and quantity (launch stock at the normal price) the
// order is that item alone instead of the cart.
type QuoteInput struct {
	AddressID     *uuid.UUID `json:"addressId"`
	PayMethod     string     `json:"payMethod"`
	Electrons     int32      `json:"electrons"`
	LaunchClaimID *uuid.UUID `json:"launchClaimId"`
	LaunchID      *uuid.UUID `json:"launchId"`
	Qty           int32      `json:"qty"`
}

// source is where an order's lines come from.
type source struct {
	rows     []store.CheckoutLinesRow
	cartID   *uuid.UUID
	launchID *uuid.UUID
	claim    *store.LaunchClaim
	deal     bool
	key      string
}

var (
	ErrClaimGone   = httpx.NewError(http.StatusConflict, "CLAIM_EXPIRED", "Your reserved deal has expired. Reserve again if units are left")
	ErrLaunchState = httpx.NewError(http.StatusConflict, "LAUNCH_NOT_ON_SALE", "Launch units are not on sale right now")
)

// lines loads the cart, a held launch claim, or launch-normal units.
func (s *Service) lines(ctx context.Context, q *store.Queries, buyer uuid.UUID, in QuoteInput, lock bool) (source, error) {
	launchRow := func(id uuid.UUID, deal bool, qty int32) (store.CheckoutLinesRow, error) {
		l, err := q.LaunchLine(ctx, id)
		if err != nil {
			return store.CheckoutLinesRow{}, err
		}
		price, lot, avail := l.NormalPricePaise, l.NormalLotID, l.NormalAvailable
		if deal {
			price, lot, avail = l.DealPricePaise, l.DealLotID, qty
		}
		yes := true
		return store.CheckoutLinesRow{VariantID: l.VariantID, Qty: qty, PricePaise: price, MrpPaise: l.MrpPaise, Attrs: l.Attrs, ProductID: l.ProductID,
			Name: l.Name, Slug: l.Slug, SellerID: l.SellerID, StoreName: l.StoreName, GstRateBps: l.GstRateBps, Hsn: l.Hsn, CommissionBps: l.CommissionBps,
			Buyable: &yes, LotID: lot, Available: avail, Cover: l.Cover}, nil
	}
	switch {
	case in.LaunchClaimID != nil:
		c, err := q.GetClaimForUpdate(ctx, *in.LaunchClaimID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.AccountID != buyer) {
			return source{}, httpx.ErrNotFound
		}
		if err != nil {
			return source{}, err
		}
		if c.Status != "HELD" || time.Now().After(c.HeldUntil) {
			return source{}, ErrClaimGone
		}
		row, err := launchRow(c.LaunchID, true, c.Qty)
		if err != nil {
			return source{}, err
		}
		return source{rows: []store.CheckoutLinesRow{row}, launchID: &c.LaunchID, claim: &c, deal: true, key: "claim:" + c.ID.String()}, nil
	case in.LaunchID != nil:
		if in.Qty < 1 || in.Qty > 10 {
			return source{}, httpx.ErrValidation.WithDetail("Quantity must be 1 to 10")
		}
		l, err := q.GetLaunch(ctx, *in.LaunchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return source{}, httpx.ErrNotFound
		}
		if err != nil {
			return source{}, err
		}
		if l.Status != store.LaunchStatusNORMALONSALE {
			return source{}, ErrLaunchState
		}
		row, err := launchRow(l.ID, false, in.Qty)
		if err != nil {
			return source{}, err
		}
		return source{rows: []store.CheckoutLinesRow{row}, launchID: &l.ID, key: "launch:" + l.ID.String() + ":" + strconv.Itoa(int(in.Qty))}, nil
	}
	var cart store.Cart
	var err error
	if lock {
		cart, err = q.LockCart(ctx, &buyer)
	} else {
		cart, err = q.GetCartByAccount(ctx, &buyer)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return source{}, ErrCartEmpty
	}
	if err != nil {
		return source{}, err
	}
	rows, err := q.CheckoutLines(ctx, cart.ID)
	if err != nil {
		return source{}, err
	}
	if len(rows) == 0 {
		return source{}, ErrCartEmpty
	}
	return source{rows: rows, cartID: &cart.ID}, nil
}

func (s *Service) buyerOpts(ctx context.Context, q *store.Queries, buyer uuid.UUID, src source, electronsWanted int32) (opts, error) {
	if electronsWanted < 0 {
		return opts{}, ErrTooManyElectrons
	}
	bal, err := q.Balance(ctx, buyer)
	if err != nil {
		return opts{}, err
	}
	if electronsWanted > bal.Available {
		return opts{}, ErrTooManyElectrons
	}
	return opts{member: membership.IsMember(ctx, q, buyer), balance: bal.Available, electrons: electronsWanted, launchDeal: src.deal, claim: src.key}, nil
}

func checkMethod(m string) (string, error) {
	switch m {
	case "", PayOnline:
		return PayOnline, nil
	case PayCOD:
		return PayCOD, nil
	}
	return "", httpx.ErrValidation.WithDetail("Choose online payment or cash on delivery")
}

// Quote prices the buyer's cart for an address and pay method.
func (s *Service) Quote(ctx context.Context, buyer uuid.UUID, in QuoteInput) (Quote, error) {
	method, err := checkMethod(in.PayMethod)
	if err != nil {
		return Quote{}, err
	}
	var addr *store.Address
	if in.AddressID != nil {
		a, err := s.Q.GetAddress(ctx, store.GetAddressParams{ID: *in.AddressID, AccountID: buyer})
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, httpx.ErrValidation.WithDetail("Choose one of your saved addresses")
		}
		if err != nil {
			return Quote{}, err
		}
		addr = &a
	}
	src, err := s.lines(ctx, s.Q, buyer, in, false)
	if err != nil {
		return Quote{}, err
	}
	o, err := s.buyerOpts(ctx, s.Q, buyer, src, in.Electrons)
	if err != nil {
		return Quote{}, err
	}
	return s.build(ctx, s.Q, src.rows, addr, method, s.rules(ctx, s.Q), o).quote, nil
}

// PlaceInput is what the buyer confirms.
type PlaceInput struct {
	AddressID     uuid.UUID  `json:"addressId"`
	PayMethod     string     `json:"payMethod"`
	QuoteHash     string     `json:"quoteHash"`
	Electrons     int32      `json:"electrons"`
	LaunchClaimID *uuid.UUID `json:"launchClaimId"`
	LaunchID      *uuid.UUID `json:"launchId"`
	Qty           int32      `json:"qty"`
}

// Buyer is who places the order.
type Buyer struct {
	ID        uuid.UUID
	Synthetic bool // an admin viewing as a buyer: test orders only (LLD 4.2)
}

const placeRoute = "POST /orders"

// Place creates the order. The same Idempotency-Key returns the same order.
func (s *Service) Place(ctx context.Context, b Buyer, key string, in PlaceInput) (OrderView, error) {
	method, err := checkMethod(in.PayMethod)
	if err != nil {
		return OrderView{}, err
	}
	if l := len(key); l < 8 || l > 100 {
		return OrderView{}, httpx.ErrValidation.WithDetail("Send an Idempotency-Key header (8 to 100 characters)")
	}
	acc, err := s.Q.GetAccount(ctx, b.ID)
	if err != nil {
		return OrderView{}, err
	}
	if acc.PhoneVerifiedAt == nil {
		return OrderView{}, ErrPhoneRequired
	}
	reqHash := sha256.Sum256([]byte(in.AddressID.String() + "|" + method + "|" + in.QuoteHash + "|" + strconv.Itoa(int(in.Electrons))))
	qin := QuoteInput{AddressID: &in.AddressID, PayMethod: method, Electrons: in.Electrons, LaunchClaimID: in.LaunchClaimID, LaunchID: in.LaunchID, Qty: in.Qty}

	var orderID uuid.UUID
	var replay bool
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		// Idempotency: the key row commits or rolls back with the order.
		if _, err := q.BeginIdempotency(ctx, store.BeginIdempotencyParams{AccountID: b.ID, Route: placeRoute, Key: key, RequestHash: reqHash[:]}); errors.Is(err, pgx.ErrNoRows) {
			prev, err := q.GetIdempotency(ctx, store.GetIdempotencyParams{AccountID: b.ID, Route: placeRoute, Key: key})
			if err != nil {
				return err
			}
			if string(prev.RequestHash) != string(reqHash[:]) {
				return ErrIdemConflict
			}
			var stored struct {
				OrderID uuid.UUID `json:"orderId"`
			}
			if prev.Response == nil || json.Unmarshal(prev.Response, &stored) != nil {
				return ErrIdemPending
			}
			orderID, replay = stored.OrderID, true
			return nil
		} else if err != nil {
			return err
		}

		addr, err := q.GetAddress(ctx, store.GetAddressParams{ID: in.AddressID, AccountID: b.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrValidation.WithDetail("Choose one of your saved addresses")
		}
		if err != nil {
			return err
		}
		src, err := s.lines(ctx, q, b.ID, qin, true)
		if err != nil {
			return err
		}
		rows := src.rows
		bo, err := s.buyerOpts(ctx, q, b.ID, src, in.Electrons)
		if err != nil {
			return err
		}
		rules := s.rules(ctx, q)
		bq := s.build(ctx, q, rows, &addr, method, rules, bo)
		if bq.quote.ElectronsUsed != in.Electrons {
			return ErrTooManyElectrons
		}
		switch {
		case bq.quote.Delivery != nil && !bq.quote.Delivery.Deliverable:
			return ErrNoDelivery
		case !bq.quote.CanPlace && method == PayCOD && !bq.quote.CODEligible:
			return ErrCODNotAllowed
		case !bq.quote.CanPlace:
			return ErrCartProblems
		case bq.quote.QuoteHash != in.QuoteHash:
			return ErrQuoteChanged
		}
		for _, r := range rows { // never oversell
			if _, err := q.ReserveStock(ctx, store.ReserveStockParams{ID: *r.LotID, Qty: r.Qty}); errors.Is(err, pgx.ErrNoRows) {
				return ErrStockChanged
			} else if err != nil {
				return err
			}
		}

		orderID, _ = uuid.NewV7()
		now := time.Now()
		status, subStatus := "PENDING_PAYMENT", store.SubStatusPENDINGPAYMENT
		var confirmed *time.Time
		if bq.quote.ElectronsOnly {
			method = PayElectronsOnly
		}
		if method == PayCOD || method == PayElectronsOnly {
			status, subStatus, confirmed = "CONFIRMED", store.SubStatusCONFIRMED, &now
		}
		if err := q.SetActor(ctx, store.SetActorParams{Actor: "BUYER", Note: ""}); err != nil {
			return err
		}
		addrJSON, _ := json.Marshal(addressView(addr))
		rulesJSON, _ := json.Marshal(rules)
		order, err := q.CreateOrder(ctx, store.CreateOrderParams{
			ID: orderID, Number: orderNumber(now), BuyerID: b.ID, Address: addrJSON, Status: status,
			ItemTotalPaise: bq.quote.ItemTotalPaise, MrpTotalPaise: bq.quote.MrpTotalPaise, ShippingPaise: bq.quote.ShippingPaise,
			PayablePaise: bq.quote.PayablePaise, PayMethod: method, RulesSnapshot: rulesJSON,
			ExpiresAt: now.Add(time.Duration(rules.HoldMinutes) * time.Minute), ConfirmedAt: confirmed, IsSynthetic: b.Synthetic,
		})
		if err != nil {
			return err
		}
		if err := q.SetOrderExtras(ctx, store.SetOrderExtrasParams{ID: orderID, ElectronsUsed: bq.quote.ElectronsUsed,
			IsMemberAtOrder: bo.member, ContainsLaunchDeal: src.deal}); err != nil {
			return err
		}
		if err := electrons.Spend(ctx, q, b.ID, bq.quote.ElectronsUsed, "SPEND_ORDER", electrons.RefOrder, orderID); err != nil {
			return err
		}
		weights := make([]int64, len(rows))
		for i, r := range rows {
			weights[i] = r.PricePaise * int64(r.Qty)
		}
		spent := electrons.Split(int64(bq.quote.ElectronsUsed), weights)
		subs := map[uuid.UUID]uuid.UUID{}
		for i, r := range rows {
			subID, ok := subs[r.SellerID]
			if !ok {
				subID, _ = uuid.NewV7()
				var subtotal int64
				for _, x := range rows {
					if x.SellerID == r.SellerID {
						subtotal += x.PricePaise * int64(x.Qty)
					}
				}
				if _, err := q.CreateSubOrder(ctx, store.CreateSubOrderParams{ID: subID, OrderID: orderID, SellerID: r.SellerID, Status: subStatus, SubtotalPaise: subtotal}); err != nil {
					return err
				}
				subs[r.SellerID] = subID
			}
			itemID, _ := uuid.NewV7()
			commission := int32(0) // per category once agreed (TRS Q-01)
			if r.CommissionBps != nil {
				commission = *r.CommissionBps
			}
			if err := q.CreateOrderItem(ctx, store.CreateOrderItemParams{
				ID: itemID, SubOrderID: subID, VariantID: r.VariantID, LotID: *r.LotID, ProductName: r.Name, ProductSlug: r.Slug,
				VariantAttrs: r.Attrs, CoverKey: r.Cover, Qty: r.Qty, UnitMrpPaise: r.MrpPaise, UnitPricePaise: r.PricePaise,
				LineTotalPaise: r.PricePaise * int64(r.Qty), GstRateBps: r.GstRateBps, Hsn: r.Hsn, CommissionBps: commission,
			}); err != nil {
				return err
			}
			if spent[i] > 0 {
				if err := q.SetItemElectrons(ctx, store.SetItemElectronsParams{ID: itemID, ElectronsSpent: int32(spent[i])}); err != nil {
					return err
				}
			}
			if src.launchID != nil {
				priceType := "LAUNCH_NORMAL"
				var claimID *uuid.UUID
				if src.claim != nil {
					priceType, claimID = "LAUNCH_DEAL", &src.claim.ID
				}
				if err := q.SetItemLaunch(ctx, store.SetItemLaunchParams{ID: itemID, PriceType: priceType, LaunchID: src.launchID, LaunchClaimID: claimID}); err != nil {
					return err
				}
			}
			if status == "CONFIRMED" {
				if err := q.CommitReservedStock(ctx, store.CommitReservedStockParams{ID: *r.LotID, Qty: r.Qty}); err != nil {
					return err
				}
			}
		}
		if src.cartID != nil {
			if err := q.ClearCart(ctx, *src.cartID); err != nil {
				return err
			}
		}
		if src.claim != nil {
			if err := q.SetClaimOrdered(ctx, store.SetClaimOrderedParams{ID: src.claim.ID, OrderID: &orderID, HeldUntil: order.ExpiresAt}); err != nil {
				return err
			}
		}
		if status == "CONFIRMED" {
			if err := confirmExtras(ctx, q, orderID); err != nil {
				return err
			}
		}
		evt := map[string]any{"orderId": orderID, "number": order.Number, "buyerId": b.ID, "payMethod": method, "payablePaise": order.PayablePaise}
		if _, err := outbox.Add(ctx, tx, "order.placed", order.Number, evt); err != nil {
			return err
		}
		if status == "CONFIRMED" {
			if _, err := outbox.Add(ctx, tx, "order.confirmed", order.Number, evt); err != nil {
				return err
			}
		}
		resp, _ := json.Marshal(map[string]any{"orderId": orderID})
		return q.FinishIdempotency(ctx, store.FinishIdempotencyParams{AccountID: b.ID, Route: placeRoute, Key: key, StatusCode: ptr32(201), Response: resp})
	})
	if err != nil {
		return OrderView{}, err
	}
	_ = replay
	if method == PayOnline {
		if _, err := s.StartPayment(ctx, b.ID, orderID); err != nil {
			s.Log.Warn("payment hand-off failed; the buyer can retry", "order", orderID, "err", err)
		}
	}
	return s.Order(ctx, b.ID, orderID)
}

// StartPayment creates (or re-uses) the payment for an unpaid order.
func (s *Service) StartPayment(ctx context.Context, buyer, orderID uuid.UUID) (string, error) {
	o, err := s.Q.GetOrder(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && o.BuyerID != buyer) {
		return "", httpx.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if o.Status != "PENDING_PAYMENT" || time.Now().After(o.ExpiresAt) {
		return "", ErrNotPending
	}
	ret := strings.TrimRight(s.PublicURL, "/") + "/checkout/result?order=" + o.ID.String()
	co, err := s.Payments.Start(ctx, "ORDER", o.Number, &buyer, o.PayablePaise, false, ret)
	if err != nil {
		return "", err
	}
	if o.PaymentID == nil {
		if err := s.Q.SetOrderPayment(ctx, store.SetOrderPaymentParams{ID: o.ID, PaymentID: &co.PaymentID}); err != nil {
			return "", err
		}
	}
	return co.CheckoutURL, nil
}

// OnPayment is the payment hook for orders: a capture confirms the order in
// the webhook's transaction; a capture for an expired order is refunded.
func (s *Service) OnPayment(ctx context.Context, tx pgx.Tx, q *store.Queries, p store.Payment) (func(context.Context), error) {
	if p.Status != "CAPTURED" {
		return nil, nil // a failed attempt can be retried until the order expires
	}
	o, err := q.GetOrderByNumberForUpdate(ctx, p.Ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch o.Status {
	case "PENDING_PAYMENT":
		if err := q.SetActor(ctx, store.SetActorParams{Actor: "SYSTEM", Note: "Payment received"}); err != nil {
			return nil, err
		}
		if err := q.ConfirmOrder(ctx, o.ID); err != nil {
			return nil, err
		}
		if err := q.SetSubOrdersStatus(ctx, store.SetSubOrdersStatusParams{OrderID: o.ID, Status: store.SubStatusCONFIRMED}); err != nil {
			return nil, err
		}
		items, err := q.ListOrderItems(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if err := q.CommitReservedStock(ctx, store.CommitReservedStockParams{ID: it.LotID, Qty: it.Qty}); err != nil {
				return nil, err
			}
		}
		if err := confirmExtras(ctx, q, o.ID); err != nil {
			return nil, err
		}
		_, err = outbox.Add(ctx, tx, "order.confirmed", o.Number, map[string]any{"orderId": o.ID, "number": o.Number, "buyerId": o.BuyerID, "payMethod": o.PayMethod, "payablePaise": o.PayablePaise})
		return nil, err
	case "EXPIRED": // money arrived too late: give it back automatically (LLD 8.3)
		paymentID, amount, ref := p.ID, p.AmountPaise, o.Number+"-LATE"
		return func(ctx context.Context) {
			if _, err := s.Payments.Refund(ctx, paymentID, amount, 0, ref, "Payment arrived after the order expired"); err != nil {
				s.Log.Error("late payment refund failed", "order", o.Number, "err", err)
			}
		}, nil
	}
	return nil, nil
}

// Sweep expires unpaid orders past their hold and releases their stock.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	n := 0
	var afters []func(context.Context)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		ids, err := q.DueForExpiry(ctx)
		if err != nil {
			return err
		}
		if err := q.SetActor(ctx, store.SetActorParams{Actor: "SYSTEM", Note: "Payment not completed in time"}); err != nil {
			return err
		}
		for _, id := range ids {
			o, err := q.GetOrder(ctx, id)
			if err != nil {
				return err
			}
			if err := q.ExpireOrder(ctx, id); err != nil {
				return err
			}
			if err := q.SetSubOrdersStatus(ctx, store.SetSubOrdersStatusParams{OrderID: id, Status: store.SubStatusEXPIRED}); err != nil {
				return err
			}
			items, err := q.ListOrderItems(ctx, id)
			if err != nil {
				return err
			}
			for _, it := range items {
				if err := q.ReleaseReservedStock(ctx, store.ReleaseReservedStockParams{ID: it.LotID, Qty: it.Qty}); err != nil {
					return err
				}
			}
			if _, err := electrons.Restore(ctx, q, o.BuyerID, electrons.RefOrder, id, o.ElectronsUsed, "Order not paid in time"); err != nil {
				return err
			}
			if s.Launches != nil {
				fn, err := s.Launches.ReleaseForOrder(ctx, q, id)
				if err != nil {
					return err
				}
				afters = append(afters, fn)
			}
			if _, err := outbox.Add(ctx, tx, "order.expired", o.Number, map[string]any{"orderId": id, "number": o.Number, "buyerId": o.BuyerID}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err == nil {
		for _, fn := range afters {
			fn(ctx)
		}
	}
	return n, err
}

// confirmExtras runs when an order is confirmed: Electrons are earned
// (pending until the return window closes) and launch claims are paid.
func confirmExtras(ctx context.Context, q *store.Queries, orderID uuid.UUID) error {
	if err := electrons.EarnForOrder(ctx, q, orderID); err != nil {
		return err
	}
	return q.SetClaimsPaid(ctx, &orderID)
}

var ist = time.FixedZone("IST", 19800)

// orderNumber is short and readable: SX-261126-K7Q2M (no 0/O/1/I).
func orderNumber(t time.Time) string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	b := make([]byte, 5)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return "SX-" + t.In(ist).Format("060102") + "-" + string(b)
}

func ptr32(n int32) *int32 { return &n }

// ItemView is an order line from the snapshot.
type ItemView struct {
	ID             uuid.UUID         `json:"id"`
	Status         string            `json:"status"`
	Return         *ItemReturn       `json:"return,omitempty"`
	CanReturn      bool              `json:"canReturn"`
	Name           string            `json:"name"`
	Slug           string            `json:"slug"`
	CoverURL       string            `json:"coverUrl,omitempty"`
	Attrs          map[string]string `json:"attrs"`
	Qty            int32             `json:"qty"`
	UnitPricePaise int64             `json:"unitPricePaise"`
	UnitMrpPaise   int64             `json:"unitMrpPaise"`
	LineTotalPaise int64             `json:"lineTotalPaise"`
}

// SubOrderView is one seller's part of the order.
type SubOrderView struct {
	ID                 uuid.UUID     `json:"id"`
	StoreName          string        `json:"storeName"`
	Status             string        `json:"status"`
	SubtotalPaise      int64         `json:"subtotalPaise"`
	Items              []ItemView    `json:"items"`
	DeliveredAt        *time.Time    `json:"deliveredAt,omitempty"`
	ReturnWindowEndsAt *time.Time    `json:"returnWindowEndsAt,omitempty"`
	Shipment           *ShipmentView `json:"shipment,omitempty"`
	Events             []Event       `json:"events"`
	CancelReason       string        `json:"cancelReason,omitempty"`
	CancelledBy        string        `json:"cancelledBy,omitempty"`
	CanCancel          bool          `json:"canCancel"`
	HasInvoice         bool          `json:"hasInvoice"`
	IsReplacement      bool          `json:"isReplacement"`
}

// ItemReturn is the latest return or replacement asked for an item.
type ItemReturn struct {
	ID     uuid.UUID `json:"id"`
	Number string    `json:"number"`
	Type   string    `json:"type"`
	Status string    `json:"status"`
}

// Event is one step in a sub-order's history (the buyer timeline).
type Event struct {
	Status string    `json:"status"`
	Actor  string    `json:"actor"`
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"`
}

// ShipmentView is the forward shipment with its tracking scans.
type ShipmentView struct {
	AWB     string          `json:"awb"`
	Courier string          `json:"courier"`
	Status  string          `json:"status"`
	ETA     *time.Time      `json:"eta,omitempty"`
	Events  []TrackingEvent `json:"events"`
}

// TrackingEvent is one courier scan.
type TrackingEvent struct {
	Status   string    `json:"status"`
	Location string    `json:"location"`
	At       time.Time `json:"at"`
}

// PaymentView is the payment state shown on the order.
type PaymentView struct {
	Status      string `json:"status"`
	FailureCode string `json:"failureCode,omitempty"`
	CheckoutURL string `json:"checkoutUrl,omitempty"`
}

// OrderView is the order as the buyer sees it.
type OrderView struct {
	ID             uuid.UUID      `json:"id"`
	Number         string         `json:"number"`
	Status         string         `json:"status"`
	PayMethod      string         `json:"payMethod"`
	PlacedAt       time.Time      `json:"placedAt"`
	ExpiresAt      time.Time      `json:"expiresAt"`
	Address        Address        `json:"address"`
	ItemTotalPaise int64          `json:"itemTotalPaise"`
	MrpTotalPaise  int64          `json:"mrpTotalPaise"`
	ShippingPaise  int64          `json:"shippingPaise"`
	PayablePaise   int64          `json:"payablePaise"`
	SubOrders      []SubOrderView `json:"subOrders"`
	Payment        *PaymentView   `json:"payment,omitempty"`
}

// Order returns one of the buyer's orders.
func (s *Service) Order(ctx context.Context, buyer, id uuid.UUID) (OrderView, error) {
	o, err := s.Q.GetOrder(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && o.BuyerID != buyer) {
		return OrderView{}, httpx.ErrNotFound
	}
	if err != nil {
		return OrderView{}, err
	}
	v := OrderView{ID: o.ID, Number: o.Number, Status: o.Status, PayMethod: o.PayMethod, PlacedAt: o.PlacedAt, ExpiresAt: o.ExpiresAt,
		ItemTotalPaise: o.ItemTotalPaise, MrpTotalPaise: o.MrpTotalPaise, ShippingPaise: o.ShippingPaise, PayablePaise: o.PayablePaise, SubOrders: []SubOrderView{}}
	_ = json.Unmarshal(o.Address, &v.Address)
	subs, err := s.Q.ListSubOrders(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	items, err := s.Q.ListOrderItems(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	shipments, err := s.Q.ListOrderShipments(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	tracking, err := s.Q.ListOrderTracking(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	events, err := s.Q.ListSubOrderEvents(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	rets, err := s.Q.ListOrderReturns(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	for _, sub := range subs {
		sv := SubOrderView{ID: sub.ID, StoreName: sub.StoreName, Status: string(sub.Status), SubtotalPaise: sub.SubtotalPaise, Items: []ItemView{},
			DeliveredAt: sub.DeliveredAt, ReturnWindowEndsAt: sub.ReturnWindowEndsAt, Events: []Event{},
			CanCancel:     sub.Status == store.SubStatusCONFIRMED || sub.Status == store.SubStatusPACKED,
			IsReplacement: sub.ReplacesItemID != nil,
			HasInvoice: sub.ReplacesItemID == nil && (sub.Status == store.SubStatusSHIPPED || sub.Status == store.SubStatusOUTFORDELIVERY ||
				sub.Status == store.SubStatusDELIVERED || sub.Status == store.SubStatusRTO)}
		if sub.CancelReason != nil {
			sv.CancelReason = *sub.CancelReason
		}
		if sub.CancelledBy != nil {
			sv.CancelledBy = *sub.CancelledBy
		}
		for _, e := range events {
			if e.SubOrderID == sub.ID {
				// The courier's location is shown with its scans; keep notes for people.
				note := e.Note
				if e.Actor == "COURIER" {
					note = ""
				}
				sv.Events = append(sv.Events, Event{Status: string(e.Status), Actor: e.Actor, Note: note, At: e.At})
			}
		}
		for _, sh := range shipments {
			if sh.SubOrderID == nil || *sh.SubOrderID != sub.ID || sh.Kind != "FORWARD" || sh.Status == "CANCELLED" {
				continue
			}
			view := &ShipmentView{AWB: sh.Awb, Courier: sh.Provider, Status: sh.Status, ETA: sh.Eta, Events: []TrackingEvent{}}
			for _, t := range tracking {
				if t.ShipmentID == sh.ID {
					view.Events = append(view.Events, TrackingEvent{Status: t.Status, Location: t.Location, At: t.At})
				}
			}
			sv.Shipment = view
		}
		for _, it := range items {
			if it.SubOrderID != sub.ID {
				continue
			}
			iv := ItemView{ID: it.ID, Status: it.Status, Name: it.ProductName, Slug: it.ProductSlug, Qty: it.Qty, UnitPricePaise: it.UnitPricePaise, UnitMrpPaise: it.UnitMrpPaise,
				LineTotalPaise: it.LineTotalPaise, Attrs: map[string]string{}}
			_ = json.Unmarshal(it.VariantAttrs, &iv.Attrs)
			for _, rr := range rets { // newest last wins
				if rr.OrderItemID == it.ID {
					iv.Return = &ItemReturn{ID: rr.ID, Number: rr.Number, Type: rr.Type, Status: rr.Status}
				}
			}
			open := iv.Return != nil && iv.Return.Status != "REJECTED"
			iv.CanReturn = it.Status == "ACTIVE" && !open && sub.Status == store.SubStatusDELIVERED &&
				sub.ReturnWindowEndsAt != nil && time.Now().Before(*sub.ReturnWindowEndsAt)
			if it.CoverKey != "" {
				iv.CoverURL = "/api/v1/media/" + it.CoverKey
			}
			sv.Items = append(sv.Items, iv)
		}
		v.SubOrders = append(v.SubOrders, sv)
	}
	if o.PaymentID != nil {
		p, err := s.Q.GetPayment(ctx, *o.PaymentID)
		if err == nil {
			pv := &PaymentView{Status: p.Status}
			if p.FailureCode != nil {
				pv.FailureCode = *p.FailureCode
			}
			if o.Status == "PENDING_PAYMENT" && time.Now().Before(o.ExpiresAt) && p.CheckoutUrl != nil {
				pv.CheckoutURL = *p.CheckoutUrl
			}
			v.Payment = pv
		}
	}
	return v, nil
}

// OrderSummary is a row of the order list.
type OrderSummary struct {
	ID           uuid.UUID `json:"id"`
	Number       string    `json:"number"`
	Status       string    `json:"status"`
	PayablePaise int64     `json:"payablePaise"`
	PlacedAt     time.Time `json:"placedAt"`
	ItemCount    int32     `json:"itemCount"`
	CoverURL     string    `json:"coverUrl,omitempty"`
}

// Orders lists the buyer's orders, newest first.
func (s *Service) Orders(ctx context.Context, buyer uuid.UUID) ([]OrderSummary, error) {
	rows, err := s.Q.ListOrders(ctx, buyer)
	if err != nil {
		return nil, err
	}
	out := make([]OrderSummary, len(rows))
	for i, r := range rows {
		out[i] = OrderSummary{ID: r.ID, Number: r.Number, Status: r.Status, PayablePaise: r.PayablePaise, PlacedAt: r.PlacedAt, ItemCount: r.ItemCount}
		if r.CoverKey != "" {
			out[i].CoverURL = "/api/v1/media/" + r.CoverKey
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PlacedAt.After(out[j].PlacedAt) })
	return out, nil
}
