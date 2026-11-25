// Package pdp serves the product page (UI-PUB-04..08, FR-PDP): a cacheable
// static part, a live block with price, stock, Electrons and the viewer's
// actions, and similar products.
package pdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/search"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Spec is one specification with its label, in schema order.
type Spec struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value any    `json:"value"`
	Unit  string `json:"unit,omitempty"`
	Main  bool   `json:"main,omitempty"`
}

// Variant is a buyable version (prices also appear in the live block).
type Variant struct {
	ID         uuid.UUID         `json:"id"`
	SKU        string            `json:"sku"`
	Attrs      map[string]string `json:"attrs"`
	MrpPaise   int64             `json:"mrpPaise"`
	PricePaise int64             `json:"pricePaise"`
}

// Image is one gallery photo.
type Image struct {
	URL    string `json:"url"`
	Alt    string `json:"alt"`
	Width  int32  `json:"width"`
	Height int32  `json:"height"`
}

// Category places the product in the tree.
type Category struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	ParentSlug string `json:"parentSlug,omitempty"`
	ParentName string `json:"parentName,omitempty"`
	Icon       string `json:"icon,omitempty"`
}

// Seller is the "about the company" block (UI-PUB-07).
type Seller struct {
	StoreName string `json:"storeName"`
	Verified  bool   `json:"verified"`
}

// Product is the static, cacheable part of the page.
type Product struct {
	ID            uuid.UUID `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Brand         string    `json:"brand"`
	Model         string    `json:"model"`
	Description   string    `json:"description"`
	Warranty      string    `json:"warranty"`
	OriginCountry string    `json:"originCountry"`
	IsLocal       bool      `json:"isLocal"`
	RatingAvg     float64   `json:"ratingAvg"`
	RatingCount   int32     `json:"ratingCount"`
	Category      Category  `json:"category"`
	Seller        Seller    `json:"seller"`
	Specs         []Spec    `json:"specs"`
	Variants      []Variant `json:"variants"`
	Images        []Image   `json:"images"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Service builds product pages.
type Service struct {
	Q      *store.Queries
	Search *search.Service
}

// Get returns the static product for a slug.
func (s *Service) Get(ctx context.Context, slug string) (Product, error) {
	row, err := s.Q.GetPublicProduct(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, httpx.ErrNotFound
	}
	if err != nil {
		return Product{}, err
	}
	p := row.Product
	out := Product{
		ID: p.ID, Slug: p.Slug, Name: p.Name, Brand: p.Brand, Model: p.Model, Description: p.Description,
		Warranty: p.Warranty, OriginCountry: p.OriginCountry, IsLocal: p.IsLocal, RatingCount: p.RatingCount,
		Seller: Seller{StoreName: row.StoreName, Verified: row.VerifiedTag}, UpdatedAt: p.UpdatedAt,
		Specs: []Spec{}, Variants: []Variant{}, Images: []Image{},
	}
	if f, err := p.RatingAvg.Float64Value(); err == nil && f.Valid {
		out.RatingAvg = f.Float64
	}
	cat, err := s.Q.GetCategory(ctx, p.CategoryID)
	if err != nil {
		return Product{}, err
	}
	out.Category = Category{Slug: cat.Slug, Name: cat.Name}
	if cat.ParentID != nil {
		if parent, err := s.Q.GetCategory(ctx, *cat.ParentID); err == nil {
			out.Category.ParentSlug, out.Category.ParentName, out.Category.Icon = parent.Slug, parent.Name, parent.Icon
		}
	}
	var schema catalogue.SpecSchema
	_ = json.Unmarshal(cat.SpecSchema, &schema)
	values := map[string]any{}
	_ = json.Unmarshal(p.Specs, &values)
	for _, f := range schema.Fields { // schema order; only filled fields
		if v, ok := values[f.Key]; ok {
			out.Specs = append(out.Specs, Spec{Key: f.Key, Label: f.Label, Value: v, Unit: f.Unit, Main: f.Main})
		}
	}
	vars, err := s.Q.ListVariants(ctx, p.ID)
	if err != nil {
		return Product{}, err
	}
	for _, v := range vars {
		attrs := map[string]string{}
		_ = json.Unmarshal(v.Attrs, &attrs)
		out.Variants = append(out.Variants, Variant{ID: v.ID, SKU: v.Sku, Attrs: attrs, MrpPaise: v.MrpPaise, PricePaise: v.PricePaise})
	}
	imgs, err := s.Q.ListProductImages(ctx, p.ID)
	if err != nil {
		return Product{}, err
	}
	for _, im := range imgs {
		out.Images = append(out.Images, Image{URL: "/api/v1/media/" + im.StorageKey, Alt: im.Alt, Width: im.Width, Height: im.Height})
	}
	return out, nil
}

// LiveVariant is the current price and stock of a variant.
type LiveVariant struct {
	ID                 uuid.UUID `json:"id"`
	PricePaise         int64     `json:"pricePaise"`
	MrpPaise           int64     `json:"mrpPaise"`
	Available          int32     `json:"available"`
	ElectronsNonMember int64     `json:"electronsNonMember"`
	ElectronsMember    int64     `json:"electronsMember"`
}

// Action is something the viewer can do here. Disabled actions say why, so
// the page can explain instead of hiding them.
type Action struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"` // SIGN_IN, COMING_M2, COMING_M3, COMING_M4, OUT_OF_STOCK, OTHER_ROLE
	Href    string `json:"href,omitempty"`
}

// Live is the per-request block (never cached).
type Live struct {
	Variants []LiveVariant `json:"variants"`
	Viewer   string        `json:"viewer"` // ANONYMOUS or the effective role
	IsMember bool          `json:"isMember"`
	Actions  []Action      `json:"actions"`
}

// ElectronRules are the earning rules from settings (BRD 4; config table).
type ElectronRules struct {
	NonMemberMinPaise, NonMemberBps, MemberMinPaise, MemberBps int64
}

// Earn returns Electrons (1 = ₹1) for a price, rounded up, or 0 below the minimum.
func (r ElectronRules) Earn(pricePaise int64, member bool) int64 {
	min, bps := r.NonMemberMinPaise, r.NonMemberBps
	if member {
		min, bps = r.MemberMinPaise, r.MemberBps
	}
	if pricePaise < min || bps <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(pricePaise) * float64(bps) / 10000 / 100))
}

func (s *Service) rules(ctx context.Context) ElectronRules {
	r := ElectronRules{NonMemberMinPaise: 250000, NonMemberBps: 50, MemberMinPaise: 80000, MemberBps: 200}
	for key, dst := range map[string]*int64{
		"electrons.nonmember.min_paise": &r.NonMemberMinPaise, "electrons.nonmember.rate_bps": &r.NonMemberBps,
		"electrons.member.min_paise": &r.MemberMinPaise, "electrons.member.rate_bps": &r.MemberBps,
	} {
		if c, err := s.Q.GetConfig(ctx, key); err == nil {
			var n int64
			if json.Unmarshal(c.Value, &n) == nil {
				*dst = n
			}
		}
	}
	return r
}

// LiveBlock returns price, stock, Electrons and actions for this viewer.
func (s *Service) LiveBlock(ctx context.Context, productID uuid.UUID, viewer *auth.Claims) (Live, error) {
	row, err := s.Q.GetPublicProductByID(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Live{}, httpx.ErrNotFound
	}
	if err != nil {
		return Live{}, err
	}
	vars, err := s.Q.ListVariants(ctx, productID)
	if err != nil {
		return Live{}, err
	}
	rules := s.rules(ctx)
	out := Live{Variants: []LiveVariant{}, Viewer: "ANONYMOUS"}
	anyStock := false
	for _, v := range vars {
		avail := v.Quantity - v.Reserved - v.Sold
		anyStock = anyStock || avail > 0
		out.Variants = append(out.Variants, LiveVariant{
			ID: v.ID, PricePaise: v.PricePaise, MrpPaise: v.MrpPaise, Available: avail,
			ElectronsNonMember: rules.Earn(v.PricePaise, false), ElectronsMember: rules.Earn(v.PricePaise, true),
		})
	}
	role := auth.Role("")
	if viewer != nil {
		role = viewer.EffectiveRole()
		out.Viewer = string(role)
	}
	out.Actions = actions(role, viewer, row.SellerAccount, productID, anyStock)
	return out, nil
}

// actions maps the viewer's role to the action area (UI-PUB-05). Cart and
// checkout arrive in M2, launch deals in M3, creators and promoters in M4.
func actions(role auth.Role, viewer *auth.Claims, sellerID, productID uuid.UUID, inStock bool) []Action {
	buy := func(reason string) []Action {
		if !inStock && reason == "" {
			reason = "OUT_OF_STOCK"
		}
		return []Action{
			{Key: "buy_now", Reason: reason},
			{Key: "add_to_cart", Reason: reason},
			{Key: "wishlist", Reason: "COMING_M2"},
		}
	}
	switch role {
	case "":
		return buy("SIGN_IN")
	case auth.RoleBuyer:
		return buy("COMING_M2")
	case auth.RoleSeller:
		if viewer.Subject == sellerID {
			return []Action{{Key: "edit", Enabled: true, Href: fmt.Sprintf("/seller/products/%s", productID)}}
		}
		return []Action{{Key: "buy_now", Reason: "OTHER_ROLE"}}
	case auth.RolePromoter:
		return []Action{{Key: "promote", Reason: "COMING_M4"}}
	case auth.RoleCreator:
		return []Action{{Key: "request_demo", Reason: "COMING_M4"}, {Key: "add_review", Reason: "COMING_M4"}}
	case auth.RoleAdmin:
		return []Action{{Key: "hide", Reason: "COMING_M5"}}
	}
	return []Action{}
}

// Handler serves the product routes.
type Handler struct{ Svc *Service }

// PublicRoutes mounts the cacheable routes.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/products/{slug}", h.get)
	r.Get("/products/{id}/similar", h.similar)
}

// ViewerRoutes mounts routes that read the (optional) session.
func (h *Handler) ViewerRoutes(r chi.Router) {
	r.Get("/products/{id}/live", h.live)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, err := h.Svc.Get(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	var viewer *auth.Claims
	if c, ok := auth.ClaimsFrom(r.Context()); ok {
		viewer = &c
	}
	live, err := h.Svc.LiveBlock(r.Context(), id, viewer)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, live)
}

func (h *Handler) similar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	items, err := h.Svc.Search.Similar(r.Context(), id, 8)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
