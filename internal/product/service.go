// Package product implements the seller product editor (UI-SEL-03, FR-SEL-02):
// products with category spec fields, variants with stock, and images.
package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/storage"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Limits on a product.
const (
	MaxVariants = 50
	MaxImages   = 10
	MaxStock    = 100000
)

var (
	ErrNotApproved   = httpx.NewError(http.StatusForbidden, "SELLER_NOT_APPROVED", "Your seller account must be approved before you add products")
	ErrConflict      = httpx.NewError(http.StatusConflict, "VERSION_CONFLICT", "This product was changed in another window. Reload to see the latest version")
	ErrCategoryLock  = httpx.NewError(http.StatusConflict, "CATEGORY_LOCKED", "The category can only change while the product is a draft")
	ErrVariantInUse  = httpx.NewError(http.StatusConflict, "VARIANT_IN_USE", "A variant with orders cannot be removed")
	ErrStockLow      = httpx.NewError(http.StatusConflict, "STOCK_BELOW_COMMITTED", "Stock cannot go below units already reserved or sold")
	ErrDuplicateSKU  = httpx.NewError(http.StatusConflict, "DUPLICATE_SKU", "Each variant needs a different SKU")
	ErrNotReady      = httpx.NewError(http.StatusUnprocessableEntity, "PRODUCT_INCOMPLETE", "Finish the product before publishing")
	ErrBadTransition = httpx.NewError(http.StatusConflict, "STATUS_NOT_ALLOWED", "That status change is not allowed")
)

// SpecError lists spec fields with problems (422 with details).
type SpecError struct{ Fields []catalogue.FieldError }

func (e *SpecError) Error() string { return "invalid specifications" }

// Service runs the product editor.
type Service struct {
	Pool    *pgxpool.Pool
	Q       *store.Queries
	Store   storage.Store
	Limiter *ratelimit.Limiter
	Log     *slog.Logger
}

// requireSeller lets only approved sellers manage products.
func (s *Service) requireSeller(ctx context.Context, q *store.Queries, sellerID uuid.UUID) error {
	acc, err := q.GetAccount(ctx, sellerID)
	if err != nil {
		return err
	}
	if acc.Role != store.AccountRoleSELLER || acc.Approval != store.ApprovalStatusAPPROVED || acc.Status != "ACTIVE" {
		return ErrNotApproved
	}
	return nil
}

// owned loads a product and checks it belongs to the seller (404 otherwise, so
// other sellers' product IDs are not confirmed).
func owned(ctx context.Context, q *store.Queries, sellerID, productID uuid.UUID, lock bool) (store.Product, error) {
	var p store.Product
	var err error
	if lock {
		p, err = q.GetProductForUpdate(ctx, productID)
	} else {
		p, err = q.GetProduct(ctx, productID)
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.SellerID != sellerID || p.Status == "HIDDEN")) {
		return store.Product{}, httpx.ErrNotFound
	}
	return p, err
}

// CreateInput starts a draft.
type CreateInput struct {
	CategoryID uuid.UUID `json:"categoryId"`
	Name       string    `json:"name"`
	Brand      string    `json:"brand"`
}

// Create makes a draft product in a sub-category.
func (s *Service) Create(ctx context.Context, sellerID uuid.UUID, in CreateInput) (View, error) {
	in.Name, in.Brand = clean(in.Name), clean(in.Brand)
	if err := checkText(in.Name, in.Brand, "", "", ""); err != nil {
		return View{}, err
	}
	if len([]rune(in.Name)) < 3 {
		return View{}, httpx.ErrValidation.WithDetail("Give the product a name of at least 3 characters")
	}
	var id uuid.UUID
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.requireSeller(ctx, q, sellerID); err != nil {
			return err
		}
		if _, err := subCategory(ctx, q, in.CategoryID); err != nil {
			return err
		}
		id, _ = uuid.NewV7()
		_, err := q.CreateProduct(ctx, store.CreateProductParams{
			ID: id, SellerID: sellerID, CategoryID: in.CategoryID, Slug: Slug(in.Name, id), Name: in.Name, Brand: in.Brand,
			OriginCountry: "IN", Specs: []byte("{}"),
		})
		return err
	})
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, sellerID, id)
}

// subCategory loads an active category that has a parent (products live in sub-categories).
func subCategory(ctx context.Context, q *store.Queries, id uuid.UUID) (catalogue.SpecSchema, error) {
	c, err := q.GetActiveCategory(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.ParentID == nil) {
		return catalogue.SpecSchema{}, httpx.ErrValidation.WithDetail("Choose a sub-category, such as Smartphones")
	}
	if err != nil {
		return catalogue.SpecSchema{}, err
	}
	var schema catalogue.SpecSchema
	if err := json.Unmarshal(c.SpecSchema, &schema); err != nil {
		return catalogue.SpecSchema{}, fmt.Errorf("product: schema of %s: %w", c.Slug, err)
	}
	return schema, nil
}

// UpdateInput is the details and specifications form.
type UpdateInput struct {
	CategoryID    uuid.UUID      `json:"categoryId"`
	Name          string         `json:"name"`
	Brand         string         `json:"brand"`
	Model         string         `json:"model"`
	Description   string         `json:"description"`
	Warranty      string         `json:"warranty"`
	OriginCountry string         `json:"originCountry"`
	IsLocal       bool           `json:"isLocal"`
	Specs         map[string]any `json:"specs"`
	Version       int32          `json:"version"`
}

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// Update saves details and specs. Specs are type-checked against the
// category schema; required fields are only enforced when publishing.
func (s *Service) Update(ctx context.Context, sellerID, productID uuid.UUID, in UpdateInput) (View, error) {
	in.Name, in.Brand, in.Model, in.Warranty = clean(in.Name), clean(in.Brand), clean(in.Model), clean(in.Warranty)
	in.Description = strings.TrimSpace(in.Description)
	in.OriginCountry = strings.ToUpper(strings.TrimSpace(in.OriginCountry))
	if err := checkText(in.Name, in.Brand, in.Model, in.Description, in.Warranty); err != nil {
		return View{}, err
	}
	if len([]rune(in.Name)) < 3 {
		return View{}, httpx.ErrValidation.WithDetail("Give the product a name of at least 3 characters")
	}
	if !countryRE.MatchString(in.OriginCountry) {
		return View{}, httpx.ErrValidation.WithDetail("Country of origin is a 2-letter code such as IN")
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.requireSeller(ctx, q, sellerID); err != nil {
			return err
		}
		p, err := owned(ctx, q, sellerID, productID, true)
		if err != nil {
			return err
		}
		if p.Version != in.Version {
			return ErrConflict
		}
		if in.CategoryID != p.CategoryID && p.Status != "DRAFT" {
			return ErrCategoryLock
		}
		schema, err := subCategory(ctx, q, in.CategoryID)
		if err != nil {
			return err
		}
		specs, fieldErrs := optional(schema).Normalize(in.Specs)
		if len(fieldErrs) > 0 {
			return &SpecError{Fields: fieldErrs}
		}
		raw, _ := json.Marshal(specs)
		_, err = q.UpdateProduct(ctx, store.UpdateProductParams{
			ID: productID, CategoryID: in.CategoryID, Name: in.Name, Brand: in.Brand, Model: in.Model,
			Description: in.Description, Warranty: in.Warranty, OriginCountry: in.OriginCountry, IsLocal: in.IsLocal,
			Specs: raw, Version: in.Version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, sellerID, productID)
}

// optional copies a schema with nothing required, for saving drafts.
func optional(s catalogue.SpecSchema) catalogue.SpecSchema {
	fields := make([]catalogue.SpecField, len(s.Fields))
	for i, f := range s.Fields {
		f.Required = false
		fields[i] = f
	}
	return catalogue.SpecSchema{Fields: fields}
}

// VariantInput is one row of the variants table.
type VariantInput struct {
	ID         *uuid.UUID        `json:"id"`
	SKU        string            `json:"sku"`
	Attrs      map[string]string `json:"attrs"`
	MrpPaise   int64             `json:"mrpPaise"`
	PricePaise int64             `json:"pricePaise"`
	Stock      int32             `json:"stock"`
}

var skuRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

// SaveVariants replaces the variant set (with stock) in one transaction.
func (s *Service) SaveVariants(ctx context.Context, sellerID, productID uuid.UUID, in []VariantInput) (View, error) {
	if len(in) == 0 || len(in) > MaxVariants {
		return View{}, httpx.ErrValidation.WithDetail(fmt.Sprintf("Add between 1 and %d variants", MaxVariants))
	}
	seen := map[string]bool{}
	for i := range in {
		v := &in[i]
		v.SKU = strings.TrimSpace(v.SKU)
		row := fmt.Sprintf("Variant %d: ", i+1)
		switch {
		case !skuRE.MatchString(v.SKU):
			return View{}, httpx.ErrValidation.WithDetail(row + "SKU uses letters, digits, dot, dash or underscore (up to 40)")
		case seen[strings.ToUpper(v.SKU)]:
			return View{}, ErrDuplicateSKU
		case v.PricePaise <= 0 || v.MrpPaise < v.PricePaise:
			return View{}, httpx.ErrValidation.WithDetail(row + "price must be above zero and not more than MRP")
		case v.MrpPaise > 100000000000:
			return View{}, httpx.ErrValidation.WithDetail(row + "MRP is too high")
		case v.Stock < 0 || v.Stock > MaxStock:
			return View{}, httpx.ErrValidation.WithDetail(row + fmt.Sprintf("stock is between 0 and %d", MaxStock))
		case len(v.Attrs) > 5:
			return View{}, httpx.ErrValidation.WithDetail(row + "use at most 5 options such as Colour or Storage")
		}
		seen[strings.ToUpper(v.SKU)] = true
		for k, val := range v.Attrs {
			if clean(k) == "" || len([]rune(k)) > 20 || clean(val) == "" || len([]rune(val)) > 40 {
				return View{}, httpx.ErrValidation.WithDetail(row + "option names are up to 20 characters and values up to 40")
			}
		}
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.requireSeller(ctx, q, sellerID); err != nil {
			return err
		}
		if _, err := owned(ctx, q, sellerID, productID, true); err != nil {
			return err
		}
		keep := make([]uuid.UUID, 0, len(in))
		for _, v := range in {
			if v.ID != nil {
				keep = append(keep, *v.ID)
			}
		}
		inUse, err := q.CountVariantsInUse(ctx, store.CountVariantsInUseParams{ProductID: productID, Keep: keep})
		if err != nil {
			return err
		}
		if inUse > 0 {
			return ErrVariantInUse
		}
		if _, err := q.DeleteVariantsExcept(ctx, store.DeleteVariantsExceptParams{ProductID: productID, Keep: keep}); err != nil {
			return err
		}
		for i, v := range in {
			var id uuid.UUID
			if v.ID != nil {
				id = *v.ID
			} else {
				id, _ = uuid.NewV7()
			}
			attrs := map[string]string{}
			for k, val := range v.Attrs {
				attrs[clean(k)] = clean(val)
			}
			raw, _ := json.Marshal(attrs)
			if _, err := q.UpsertVariant(ctx, store.UpsertVariantParams{
				ID: id, ProductID: productID, Sku: v.SKU, Attrs: raw, MrpPaise: v.MrpPaise, PricePaise: v.PricePaise, Position: int32(i),
			}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return httpx.ErrNotFound
				}
				if pgCode(err) == "23505" {
					return ErrDuplicateSKU
				}
				return err
			}
			lotID, _ := uuid.NewV7()
			if err := q.SetSellerStock(ctx, store.SetSellerStockParams{ID: lotID, VariantID: id, OwnerSellerID: sellerID, Quantity: v.Stock}); err != nil {
				if pgCode(err) == "23514" {
					return ErrStockLow
				}
				return err
			}
		}
		return q.TouchProduct(ctx, productID)
	})
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, sellerID, productID)
}

// ImageInput is one image in display order (the first is the cover).
type ImageInput struct {
	UploadID uuid.UUID `json:"uploadId"`
	Alt      string    `json:"alt"`
}

// SaveImages replaces the image list; only the seller's own uploads can be used.
func (s *Service) SaveImages(ctx context.Context, sellerID, productID uuid.UUID, in []ImageInput) (View, error) {
	if len(in) > MaxImages {
		return View{}, httpx.ErrValidation.WithDetail(fmt.Sprintf("Use up to %d images", MaxImages))
	}
	ids := make([]uuid.UUID, 0, len(in))
	seen := map[uuid.UUID]bool{}
	for _, im := range in {
		if seen[im.UploadID] {
			return View{}, httpx.ErrValidation.WithDetail("The same image is listed twice")
		}
		if len([]rune(clean(im.Alt))) > 150 {
			return View{}, httpx.ErrValidation.WithDetail("Image descriptions are up to 150 characters")
		}
		seen[im.UploadID] = true
		ids = append(ids, im.UploadID)
	}
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.requireSeller(ctx, q, sellerID); err != nil {
			return err
		}
		if _, err := owned(ctx, q, sellerID, productID, true); err != nil {
			return err
		}
		ups, err := q.GetOwnUploads(ctx, store.GetOwnUploadsParams{OwnerID: sellerID, Ids: ids})
		if err != nil {
			return err
		}
		if len(ups) != len(ids) {
			return httpx.ErrValidation.WithDetail("Upload the images again; some were not found")
		}
		if err := q.DeleteProductImages(ctx, productID); err != nil {
			return err
		}
		for i, im := range in {
			if err := q.InsertProductImage(ctx, store.InsertProductImageParams{
				ProductID: productID, UploadID: im.UploadID, Position: int32(i), Alt: clean(im.Alt),
			}); err != nil {
				return err
			}
		}
		return q.TouchProduct(ctx, productID)
	})
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, sellerID, productID)
}

// SetStatus publishes, pauses or resumes a product.
func (s *Service) SetStatus(ctx context.Context, sellerID, productID uuid.UUID, to string) (View, error) {
	v, err := s.Get(ctx, sellerID, productID)
	if err != nil {
		return View{}, err
	}
	allowed := map[string][]string{"DRAFT": {"ACTIVE"}, "ACTIVE": {"PAUSED"}, "PAUSED": {"ACTIVE"}}
	ok := false
	for _, x := range allowed[v.Status] {
		ok = ok || x == to
	}
	if !ok {
		return View{}, ErrBadTransition
	}
	if to == "ACTIVE" && len(v.Missing) > 0 {
		return View{}, ErrNotReady.WithDetail("Still to do: " + strings.Join(v.Missing, ", "))
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := s.requireSeller(ctx, q, sellerID); err != nil {
			return err
		}
		if _, err := owned(ctx, q, sellerID, productID, true); err != nil {
			return err
		}
		_, err := q.SetProductStatus(ctx, store.SetProductStatusParams{ID: productID, Status: to})
		return err
	})
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, sellerID, productID)
}

// Slug builds a readable, unique URL slug: "galaxy-m35-5g-1a2b3c".
func Slug(name string, id uuid.UUID) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "product"
	}
	hex := strings.ReplaceAll(id.String(), "-", "")
	return base + "-" + hex[len(hex)-6:]
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

func checkText(name, brand, model, description, warranty string) error {
	switch {
	case len([]rune(name)) > 150:
		return httpx.ErrValidation.WithDetail("The name can be up to 150 characters")
	case len([]rune(brand)) > 60:
		return httpx.ErrValidation.WithDetail("The brand can be up to 60 characters")
	case len([]rune(model)) > 80:
		return httpx.ErrValidation.WithDetail("The model can be up to 80 characters")
	case len([]rune(description)) > 5000:
		return httpx.ErrValidation.WithDetail("The description can be up to 5,000 characters")
	case len([]rune(warranty)) > 200:
		return httpx.ErrValidation.WithDetail("The warranty can be up to 200 characters")
	}
	return nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
