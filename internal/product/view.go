package product

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
)

// MinDescription is the shortest description a published product may have.
const MinDescription = 30

// CategoryRef names the product's sub-category and its parent.
type CategoryRef struct {
	ID         uuid.UUID `json:"id"`
	Slug       string    `json:"slug"`
	Name       string    `json:"name"`
	ParentName string    `json:"parentName,omitempty"`
	ParentIcon string    `json:"parentIcon,omitempty"`
}

// VariantView is a variant with its seller stock.
type VariantView struct {
	ID         uuid.UUID         `json:"id"`
	SKU        string            `json:"sku"`
	Attrs      map[string]string `json:"attrs"`
	MrpPaise   int64             `json:"mrpPaise"`
	PricePaise int64             `json:"pricePaise"`
	Stock      int32             `json:"stock"`
	Reserved   int32             `json:"reserved"`
	Sold       int32             `json:"sold"`
}

// ImageView is one product image.
type ImageView struct {
	UploadID uuid.UUID `json:"uploadId"`
	URL      string    `json:"url"`
	Width    int32     `json:"width"`
	Height   int32     `json:"height"`
	Alt      string    `json:"alt"`
}

// View is the whole product as the editor shows it.
type View struct {
	ID            uuid.UUID              `json:"id"`
	Slug          string                 `json:"slug"`
	Status        string                 `json:"status"`
	Category      CategoryRef            `json:"category"`
	SpecSchema    catalogue.SpecSchema   `json:"specSchema"`
	Name          string                 `json:"name"`
	Brand         string                 `json:"brand"`
	Model         string                 `json:"model"`
	Description   string                 `json:"description"`
	Warranty      string                 `json:"warranty"`
	OriginCountry string                 `json:"originCountry"`
	IsLocal       bool                   `json:"isLocal"`
	Specs         map[string]any         `json:"specs"`
	Variants      []VariantView          `json:"variants"`
	Images        []ImageView            `json:"images"`
	Missing       []string               `json:"missing"`
	SpecProblems  []catalogue.FieldError `json:"specProblems"`
	Version       int32                  `json:"version"`
	UpdatedAt     time.Time              `json:"updatedAt"`
	PublishedAt   *time.Time             `json:"publishedAt,omitempty"`
}

// MediaURL is where the web app loads a stored image.
func MediaURL(key string) string { return "/api/v1/media/" + key }

// Get returns one of the seller's products.
func (s *Service) Get(ctx context.Context, sellerID, productID uuid.UUID) (View, error) {
	p, err := owned(ctx, s.Q, sellerID, productID, false)
	if err != nil {
		return View{}, err
	}
	cat, err := s.Q.GetCategory(ctx, p.CategoryID)
	if err != nil {
		return View{}, err
	}
	ref := CategoryRef{ID: cat.ID, Slug: cat.Slug, Name: cat.Name}
	if cat.ParentID != nil {
		if parent, err := s.Q.GetCategory(ctx, *cat.ParentID); err == nil {
			ref.ParentName, ref.ParentIcon = parent.Name, parent.Icon
		}
	}
	var schema catalogue.SpecSchema
	_ = json.Unmarshal(cat.SpecSchema, &schema)
	if schema.Fields == nil {
		schema.Fields = []catalogue.SpecField{}
	}
	specs := map[string]any{}
	_ = json.Unmarshal(p.Specs, &specs)

	rows, err := s.Q.ListVariants(ctx, productID)
	if err != nil {
		return View{}, err
	}
	variants := make([]VariantView, 0, len(rows))
	for _, r := range rows {
		attrs := map[string]string{}
		_ = json.Unmarshal(r.Attrs, &attrs)
		variants = append(variants, VariantView{
			ID: r.ID, SKU: r.Sku, Attrs: attrs, MrpPaise: r.MrpPaise, PricePaise: r.PricePaise,
			Stock: r.Quantity, Reserved: r.Reserved, Sold: r.Sold,
		})
	}
	imgs, err := s.Q.ListProductImages(ctx, productID)
	if err != nil {
		return View{}, err
	}
	images := make([]ImageView, 0, len(imgs))
	for _, im := range imgs {
		images = append(images, ImageView{UploadID: im.UploadID, URL: MediaURL(im.StorageKey), Width: im.Width, Height: im.Height, Alt: im.Alt})
	}

	v := View{
		ID: p.ID, Slug: p.Slug, Status: p.Status, Category: ref, SpecSchema: schema, Name: p.Name, Brand: p.Brand,
		Model: p.Model, Description: p.Description, Warranty: p.Warranty, OriginCountry: p.OriginCountry,
		IsLocal: p.IsLocal, Specs: specs, Variants: variants, Images: images, Version: p.Version,
		UpdatedAt: p.UpdatedAt, PublishedAt: p.PublishedAt,
	}
	_, v.SpecProblems = schema.Normalize(specs)
	if v.SpecProblems == nil {
		v.SpecProblems = []catalogue.FieldError{}
	}
	v.Missing = missing(v)
	return v, nil
}

// missing lists what publishing still needs, as keys the web app translates.
func missing(v View) []string {
	out := []string{}
	if v.Brand == "" {
		out = append(out, "brand")
	}
	if len([]rune(v.Description)) < MinDescription {
		out = append(out, "description")
	}
	if v.Warranty == "" {
		out = append(out, "warranty")
	}
	if len(v.SpecProblems) > 0 {
		out = append(out, "specs")
	}
	if len(v.Variants) == 0 {
		out = append(out, "variants")
	}
	if len(v.Images) == 0 {
		out = append(out, "images")
	}
	return out
}

// Summary is a row of the seller's product list.
type Summary struct {
	ID            uuid.UUID `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Brand         string    `json:"brand"`
	Status        string    `json:"status"`
	CategoryName  string    `json:"categoryName"`
	MinPricePaise int64     `json:"minPricePaise"`
	MaxPricePaise int64     `json:"maxPricePaise"`
	VariantCount  int32     `json:"variantCount"`
	Available     int32     `json:"available"`
	CoverURL      string    `json:"coverUrl,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// List returns the seller's products, most recently changed first.
func (s *Service) List(ctx context.Context, sellerID uuid.UUID) ([]Summary, error) {
	rows, err := s.Q.ListSellerProducts(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(rows))
	for _, r := range rows {
		sm := Summary{
			ID: r.ID, Slug: r.Slug, Name: r.Name, Brand: r.Brand, Status: r.Status, CategoryName: r.CategoryName,
			MinPricePaise: r.MinPricePaise, MaxPricePaise: r.MaxPricePaise, VariantCount: r.VariantCount,
			Available: r.Available, UpdatedAt: r.UpdatedAt,
		}
		if r.CoverKey != "" {
			sm.CoverURL = MediaURL(r.CoverKey)
		}
		out = append(out, sm)
	}
	return out, nil
}
