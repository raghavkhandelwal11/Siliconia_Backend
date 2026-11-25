// Package search serves product listings for search and category pages
// (UI-PUB-03, FR-CAT-04): full-text and fuzzy search, global and category
// spec filters, facets with counts, sorting and pagination.
package search

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/catalogue"
)

// Sorts offered on listings.
var Sorts = []string{"relevance", "price_asc", "price_desc", "newest", "rating", "popular"}

// Page sizes.
const (
	DefaultPageSize = 24
	MaxPageSize     = 48
	MaxPage         = 100
)

// Params is a parsed listing request.
type Params struct {
	Q            string
	Category     string // slug; a top-level category includes its sub-categories
	Brands       []string
	PriceMin     int64 // paise
	PriceMax     int64
	MinRating    float64
	Local        bool
	InStock      bool
	Verified     bool
	Sort         string
	Page         int
	PageSize     int
	Specs        map[string][]string // spec.<key>=a,b (choices) or true/false
	SpecRanges   map[string][2]*float64
	unknownSpecs []string
}

// SpecPrefix marks spec filters in the query string: spec.ram_gb=4..8, spec.os=Android.
const SpecPrefix = "spec."

// ParseParams reads the query string. Spec filters are checked later against
// the category schema, because only sub-categories have them.
func ParseParams(v url.Values) (Params, error) {
	p := Params{
		Q:          strings.TrimSpace(v.Get("q")),
		Category:   strings.TrimSpace(v.Get("category")),
		Sort:       v.Get("sort"),
		Page:       1,
		PageSize:   DefaultPageSize,
		Specs:      map[string][]string{},
		SpecRanges: map[string][2]*float64{},
	}
	if len([]rune(p.Q)) > 100 {
		return p, fmt.Errorf("search text is up to 100 characters")
	}
	for _, b := range splitList(v.Get("brand")) {
		if len(p.Brands) < 20 {
			p.Brands = append(p.Brands, b)
		}
	}
	var err error
	if p.PriceMin, err = paise(v.Get("priceMin")); err != nil {
		return p, err
	}
	if p.PriceMax, err = paise(v.Get("priceMax")); err != nil {
		return p, err
	}
	if s := v.Get("rating"); s != "" {
		r, err := strconv.ParseFloat(s, 64)
		if err != nil || r < 0 || r > 5 {
			return p, fmt.Errorf("rating is a number from 0 to 5")
		}
		p.MinRating = r
	}
	p.Local, p.InStock, p.Verified = v.Get("local") == "true", v.Get("inStock") == "true", v.Get("verified") == "true"
	if p.Sort == "" {
		p.Sort = "relevance"
	}
	if !slices.Contains(Sorts, p.Sort) {
		return p, fmt.Errorf("sort is one of %s", strings.Join(Sorts, ", "))
	}
	if s := v.Get("page"); s != "" {
		if p.Page, err = strconv.Atoi(s); err != nil || p.Page < 1 || p.Page > MaxPage {
			return p, fmt.Errorf("page is between 1 and %d", MaxPage)
		}
	}
	if s := v.Get("pageSize"); s != "" {
		if p.PageSize, err = strconv.Atoi(s); err != nil || p.PageSize < 1 || p.PageSize > MaxPageSize {
			return p, fmt.Errorf("pageSize is between 1 and %d", MaxPageSize)
		}
	}
	for k, vals := range v {
		key, ok := strings.CutPrefix(k, SpecPrefix)
		if !ok || len(vals) == 0 {
			continue
		}
		val := vals[0]
		if lo, hi, isRange := strings.Cut(val, ".."); isRange {
			r := [2]*float64{}
			for i, part := range []string{lo, hi} {
				if part == "" {
					continue
				}
				f, err := strconv.ParseFloat(part, 64)
				if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
					return p, fmt.Errorf("%s is a range like 4..8", k)
				}
				r[i] = &f
			}
			p.SpecRanges[key] = r
		} else {
			p.Specs[key] = splitList(val)
		}
	}
	return p, nil
}

// checkSpecs drops spec filters the category does not have and validates
// the rest against their field types.
func (p *Params) checkSpecs(schema catalogue.SpecSchema) error {
	fields := map[string]catalogue.SpecField{}
	for _, f := range schema.Fields {
		if f.Filter {
			fields[f.Key] = f
		}
	}
	for k, vals := range p.Specs {
		f, ok := fields[k]
		switch {
		case !ok:
			p.unknownSpecs = append(p.unknownSpecs, k)
			delete(p.Specs, k)
		case f.Type == catalogue.TypeNumber:
			return fmt.Errorf("%s%s takes a range like 4..8", SpecPrefix, k)
		case f.Type == catalogue.TypeBoolean:
			if len(vals) != 1 || (vals[0] != "true" && vals[0] != "false") {
				return fmt.Errorf("%s%s is true or false", SpecPrefix, k)
			}
		}
	}
	for k := range p.SpecRanges {
		if f, ok := fields[k]; !ok || f.Type != catalogue.TypeNumber {
			p.unknownSpecs = append(p.unknownSpecs, k)
			delete(p.SpecRanges, k)
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" && len([]rune(x)) <= 60 {
			out = append(out, x)
		}
	}
	return out
}

func paise(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("prices are whole numbers of paise")
	}
	return n, nil
}

// builder collects SQL conditions with numbered arguments.
type builder struct {
	conds []string
	args  []any
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *builder) where(cond string) { b.conds = append(b.conds, cond) }

func (b *builder) clause() string {
	if len(b.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(b.conds, " AND ")
}

func (b *builder) clone() *builder {
	return &builder{conds: slices.Clone(b.conds), args: slices.Clone(b.args)}
}
