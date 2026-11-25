// Package cart implements the cart and wishlist (UI-BUY-01, UI-BUY-10,
// FR-CHK-01, FR-CHK-10). Guests get a cart keyed by a device cookie that is
// merged into the buyer's cart after sign-in (FR-AUTH-09).
package cart

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/pdp"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// MaxQty is the most of one variant a cart can hold.
const MaxQty = 10

var (
	ErrNotBuyer     = httpx.NewError(http.StatusForbidden, "OTHER_ROLE", "Carts are for shoppers. Sign in with a buyer account to buy")
	ErrUnavailable  = httpx.NewError(http.StatusConflict, "PRODUCT_UNAVAILABLE", "This product is not available right now")
	ErrNotEnough    = httpx.NewError(http.StatusConflict, "NOT_ENOUGH_STOCK", "Not enough stock for that quantity")
	ErrSignInToSave = httpx.NewError(http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in to save products to your wishlist")
)

// Owner says whose cart this is: a buyer account or a guest device.
type Owner struct {
	AccountID *uuid.UUID
	DeviceID  string
}

// Line is one product in the cart with what the shopper should know about it.
type Line struct {
	VariantID       uuid.UUID         `json:"variantId"`
	ProductID       uuid.UUID         `json:"productId"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Brand           string            `json:"brand"`
	Attrs           map[string]string `json:"attrs"`
	CoverURL        string            `json:"coverUrl,omitempty"`
	Qty             int32             `json:"qty"`
	PricePaise      int64             `json:"pricePaise"`
	MrpPaise        int64             `json:"mrpPaise"`
	PriceAtAddPaise int64             `json:"priceAtAddPaise"`
	Available       int32             `json:"available"`
	// Problem: UNAVAILABLE, OUT_OF_STOCK or LOW_STOCK (fewer left than in the cart).
	Problem string `json:"problem,omitempty"`
}

// Group is one seller's lines; each becomes a sub-order at checkout.
type Group struct {
	StoreName     string `json:"storeName"`
	Lines         []Line `json:"lines"`
	SubtotalPaise int64  `json:"subtotalPaise"`
}

// Merged reports what happened to a guest cart after sign-in (UI-AUT-06).
type Merged struct {
	Lines    int  `json:"lines"`
	Adjusted bool `json:"adjusted"`
}

// Cart is the whole cart page.
type Cart struct {
	Groups             []Group `json:"groups"`
	ItemCount          int32   `json:"itemCount"`
	SubtotalPaise      int64   `json:"subtotalPaise"`
	MrpTotalPaise      int64   `json:"mrpTotalPaise"`
	SavingsPaise       int64   `json:"savingsPaise"`
	ElectronsNonMember int64   `json:"electronsNonMember"`
	ElectronsMember    int64   `json:"electronsMember"`
	CanCheckout        bool    `json:"canCheckout"`
	Merged             *Merged `json:"merged,omitempty"`
}

// Service runs carts and wishlists.
type Service struct {
	Pool *pgxpool.Pool
	Q    *store.Queries
}

// NewDeviceID makes a guest cart cookie value.
func NewDeviceID() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// find returns the owner's cart, or nil when it has none yet.
func find(ctx context.Context, q *store.Queries, o Owner) (*store.Cart, error) {
	var c store.Cart
	var err error
	switch {
	case o.AccountID != nil:
		c, err = q.GetCartByAccount(ctx, o.AccountID)
	case o.DeviceID != "":
		c, err = q.GetCartByDevice(ctx, &o.DeviceID)
	default:
		return nil, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func ensure(ctx context.Context, q *store.Queries, o Owner) (store.Cart, error) {
	if c, err := find(ctx, q, o); err != nil || c != nil {
		if c == nil {
			return store.Cart{}, err
		}
		return *c, err
	}
	id, _ := uuid.NewV7()
	var dev *string
	if o.AccountID == nil {
		dev = &o.DeviceID
	}
	if _, err := q.CreateCart(ctx, store.CreateCartParams{ID: id, AccountID: o.AccountID, DeviceID: dev}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.Cart{}, err
	}
	c, err := find(ctx, q, o) // also covers a concurrent create
	if err != nil || c == nil {
		return store.Cart{}, errors.Join(err, errors.New("cart: not created"))
	}
	return *c, nil
}

// Get returns the cart. A buyer with a guest cart on this device gets it
// merged first; merged reports the result and guestDone says the device
// cookie can be cleared.
func (s *Service) Get(ctx context.Context, o Owner, guestDevice string) (Cart, bool, error) {
	var merged *Merged
	if o.AccountID != nil && guestDevice != "" {
		m, err := s.merge(ctx, *o.AccountID, guestDevice)
		if err != nil {
			return Cart{}, false, err
		}
		merged = m
	}
	c, err := s.view(ctx, o)
	c.Merged = merged
	return c, merged != nil || (o.AccountID != nil && guestDevice != ""), err
}

func (s *Service) view(ctx context.Context, o Owner) (Cart, error) {
	out := Cart{Groups: []Group{}}
	cart, err := find(ctx, s.Q, o)
	if err != nil || cart == nil {
		return out, err
	}
	rows, err := s.Q.CartLines(ctx, cart.ID)
	if err != nil {
		return out, err
	}
	ok := len(rows) > 0
	for _, r := range rows {
		l := Line{
			VariantID: r.VariantID, ProductID: r.ProductID, Slug: r.Slug, Name: r.Name, Brand: r.Brand, Qty: r.Qty,
			PricePaise: r.PricePaise, MrpPaise: r.MrpPaise, PriceAtAddPaise: r.PriceAtAddPaise, Available: r.Available,
			Attrs: map[string]string{},
		}
		_ = json.Unmarshal(r.Attrs, &l.Attrs)
		if r.Cover != "" {
			l.CoverURL = "/api/v1/media/" + r.Cover
		}
		switch {
		case r.Buyable == nil || !*r.Buyable:
			l.Problem = "UNAVAILABLE"
		case r.Available <= 0:
			l.Problem = "OUT_OF_STOCK"
		case r.Available < r.Qty:
			l.Problem = "LOW_STOCK"
		}
		if l.Problem != "" {
			ok = false
		} else {
			out.SubtotalPaise += l.PricePaise * int64(l.Qty)
			out.MrpTotalPaise += l.MrpPaise * int64(l.Qty)
			out.ItemCount += l.Qty
		}
		if n := len(out.Groups); n == 0 || out.Groups[n-1].StoreName != r.StoreName {
			out.Groups = append(out.Groups, Group{StoreName: r.StoreName, Lines: []Line{}})
		}
		g := &out.Groups[len(out.Groups)-1]
		g.Lines = append(g.Lines, l)
		if l.Problem == "" {
			g.SubtotalPaise += l.PricePaise * int64(l.Qty)
		}
	}
	out.SavingsPaise = out.MrpTotalPaise - out.SubtotalPaise
	rules := pdp.Rules(ctx, s.Q)
	out.ElectronsNonMember = rules.Earn(out.SubtotalPaise, false)
	out.ElectronsMember = rules.Earn(out.SubtotalPaise, true)
	out.CanCheckout = ok
	return out, nil
}

// SetQty sets a line's quantity (0 removes it), capped by stock.
func (s *Service) SetQty(ctx context.Context, o Owner, variantID uuid.UUID, qty int32) (Cart, error) {
	if qty < 0 || qty > MaxQty {
		return Cart{}, httpx.ErrValidation.WithDetail("Choose a quantity from 1 to 10")
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		c, err := ensure(ctx, q, o)
		if err != nil {
			return err
		}
		if qty == 0 {
			return q.DeleteCartItem(ctx, store.DeleteCartItemParams{CartID: c.ID, VariantID: variantID})
		}
		v, err := q.GetBuyableVariant(ctx, variantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if v.Available < qty {
			if v.Available <= 0 {
				return ErrNotEnough.WithDetail("This product is out of stock")
			}
			return ErrNotEnough.WithDetail(plural(v.Available))
		}
		if err := q.SetCartItem(ctx, store.SetCartItemParams{CartID: c.ID, VariantID: variantID, Qty: qty, PriceAtAddPaise: v.PricePaise}); err != nil {
			return err
		}
		return q.TouchCart(ctx, c.ID)
	})
	if err != nil {
		return Cart{}, err
	}
	return s.view(ctx, o)
}

func plural(n int32) string {
	if n == 1 {
		return "Only 1 left"
	}
	return fmt.Sprintf("Only %d left", n)
}

// merge moves a guest cart into the buyer's cart (quantities add up, capped by
// 10 and stock) and deletes the guest cart.
func (s *Service) merge(ctx context.Context, accountID uuid.UUID, device string) (*Merged, error) {
	var res *Merged
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		guest, err := find(ctx, q, Owner{DeviceID: device})
		if err != nil || guest == nil {
			return err
		}
		items, err := q.ListCartItems(ctx, guest.ID)
		if err != nil {
			return err
		}
		mine, err := ensure(ctx, q, Owner{AccountID: &accountID})
		if err != nil {
			return err
		}
		existing, err := q.ListCartItems(ctx, mine.ID)
		if err != nil {
			return err
		}
		have := map[uuid.UUID]int32{}
		for _, it := range existing {
			have[it.VariantID] = it.Qty
		}
		res = &Merged{}
		for _, it := range items {
			v, err := q.GetBuyableVariant(ctx, it.VariantID)
			if errors.Is(err, pgx.ErrNoRows) {
				res.Adjusted = true
				continue
			}
			if err != nil {
				return err
			}
			want := have[it.VariantID] + it.Qty
			got := min(want, MaxQty, max(v.Available, 0))
			if got != want {
				res.Adjusted = true
			}
			if got <= 0 {
				continue
			}
			if err := q.SetCartItem(ctx, store.SetCartItemParams{CartID: mine.ID, VariantID: it.VariantID, Qty: got, PriceAtAddPaise: it.PriceAtAddPaise}); err != nil {
				return err
			}
			res.Lines++
		}
		if err := q.DeleteCart(ctx, guest.ID); err != nil {
			return err
		}
		return q.TouchCart(ctx, mine.ID)
	})
	if res != nil && res.Lines == 0 && !res.Adjusted {
		res = nil // the guest cart was empty
	}
	return res, err
}

// WishItem is a saved product with its current state.
type WishItem struct {
	VariantID       uuid.UUID         `json:"variantId"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Brand           string            `json:"brand"`
	Attrs           map[string]string `json:"attrs"`
	CoverURL        string            `json:"coverUrl,omitempty"`
	PricePaise      int64             `json:"pricePaise"`
	MrpPaise        int64             `json:"mrpPaise"`
	PriceAtAddPaise int64             `json:"priceAtAddPaise"`
	PriceDropped    bool              `json:"priceDropped"`
	InStock         bool              `json:"inStock"`
	Buyable         bool              `json:"buyable"`
	AddedAt         time.Time         `json:"addedAt"`
}

// Wishlist returns the buyer's saved products, newest first.
func (s *Service) Wishlist(ctx context.Context, accountID uuid.UUID) ([]WishItem, error) {
	rows, err := s.Q.WishlistLines(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]WishItem, 0, len(rows))
	for _, r := range rows {
		w := WishItem{
			VariantID: r.VariantID, Slug: r.Slug, Name: r.Name, Brand: r.Brand, PricePaise: r.PricePaise, MrpPaise: r.MrpPaise,
			PriceAtAddPaise: r.PriceAtAddPaise, PriceDropped: r.PricePaise < r.PriceAtAddPaise, InStock: r.Available > 0,
			Buyable: r.Buyable != nil && *r.Buyable, AddedAt: r.CreatedAt, Attrs: map[string]string{},
		}
		_ = json.Unmarshal(r.Attrs, &w.Attrs)
		if r.Cover != "" {
			w.CoverURL = "/api/v1/media/" + r.Cover
		}
		out = append(out, w)
	}
	return out, nil
}

// Save adds a variant to the wishlist (any live product, even out of stock).
func (s *Service) Save(ctx context.Context, accountID, variantID uuid.UUID) error {
	v, err := s.Q.GetBuyableVariant(ctx, variantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	return s.Q.AddWishlist(ctx, store.AddWishlistParams{AccountID: accountID, VariantID: variantID, PriceAtAddPaise: v.PricePaise})
}

// Unsave removes a variant from the wishlist.
func (s *Service) Unsave(ctx context.Context, accountID, variantID uuid.UUID) error {
	return s.Q.RemoveWishlist(ctx, store.RemoveWishlistParams{AccountID: accountID, VariantID: variantID})
}

// Saved reports which of these variants the buyer has saved (for the product page).
func (s *Service) Saved(ctx context.Context, accountID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	return s.Q.WishlistHas(ctx, store.WishlistHasParams{AccountID: accountID, Ids: ids})
}
