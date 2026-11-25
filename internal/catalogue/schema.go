// Package catalogue holds the category tree and the per-sub-category
// specification schemas that drive the seller product form, the product
// page's main specifications and listing filters (TRS FR-CAT; LLD 5.3).
package catalogue

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// FieldType is the kind of value a spec field holds.
type FieldType string

const (
	TypeNumber  FieldType = "number"
	TypeText    FieldType = "text"
	TypeEnum    FieldType = "enum"    // one of Options
	TypeMulti   FieldType = "multi"   // any subset of Options
	TypeBoolean FieldType = "boolean" // yes / no
)

// SpecField describes one specification of a sub-category.
type SpecField struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Type     FieldType `json:"type"`
	Unit     string    `json:"unit,omitempty"`
	Options  []string  `json:"options,omitempty"`
	Required bool      `json:"required,omitempty"`
	Filter   bool      `json:"filter,omitempty"` // offered as a listing filter
	Main     bool      `json:"main,omitempty"`   // shown in "main specifications"
}

// SpecSchema is stored in categories.spec_schema.
type SpecSchema struct {
	Fields []SpecField `json:"fields"`
}

var keyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// MaxMainFields is how many "main specifications" the product page shows.
const MaxMainFields = 3

// Validate checks a schema before it is saved (seed or admin edit).
func (s SpecSchema) Validate() error {
	var errs []error
	seen := map[string]bool{}
	main := 0
	for i, f := range s.Fields {
		where := fmt.Sprintf("field %d (%s)", i+1, f.Key)
		if !keyRE.MatchString(f.Key) {
			errs = append(errs, fmt.Errorf("%s: key must be lower_snake_case, up to 40 characters", where))
		}
		if seen[f.Key] {
			errs = append(errs, fmt.Errorf("%s: duplicate key", where))
		}
		seen[f.Key] = true
		if strings.TrimSpace(f.Label) == "" {
			errs = append(errs, fmt.Errorf("%s: label is required", where))
		}
		switch f.Type {
		case TypeEnum, TypeMulti:
			if len(f.Options) < 2 {
				errs = append(errs, fmt.Errorf("%s: %s needs at least 2 options", where, f.Type))
			}
			if dup := firstDuplicate(f.Options); dup != "" {
				errs = append(errs, fmt.Errorf("%s: option %q is listed twice", where, dup))
			}
		case TypeNumber, TypeText, TypeBoolean:
			if len(f.Options) > 0 {
				errs = append(errs, fmt.Errorf("%s: only enum and multi fields have options", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: unknown type %q", where, f.Type))
		}
		if f.Filter && f.Type == TypeText {
			// Free text cannot be filtered with checkboxes or ranges.
			errs = append(errs, fmt.Errorf("%s: text fields cannot be filters", where))
		}
		if f.Main {
			main++
		}
	}
	if main > MaxMainFields {
		errs = append(errs, fmt.Errorf("at most %d fields can be main specifications, found %d", MaxMainFields, main))
	}
	return errors.Join(errs...)
}

// FieldError is one problem with a product's spec values.
type FieldError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// Normalize checks product spec values against the schema and returns them in
// canonical form (numbers as float64, multi values in option order). Unknown
// keys are rejected so typos do not silently disappear.
func (s SpecSchema) Normalize(values map[string]any) (map[string]any, []FieldError) {
	out := make(map[string]any, len(values))
	var errs []FieldError
	byKey := make(map[string]SpecField, len(s.Fields))
	for _, f := range s.Fields {
		byKey[f.Key] = f
	}
	for k := range values {
		if _, ok := byKey[k]; !ok {
			errs = append(errs, FieldError{k, "Not a specification of this category"})
		}
	}
	for _, f := range s.Fields {
		v, present := values[f.Key]
		if !present || v == nil || v == "" {
			if f.Required {
				errs = append(errs, FieldError{f.Key, f.Label + " is required"})
			}
			continue
		}
		nv, msg := normalizeValue(f, v)
		if msg != "" {
			errs = append(errs, FieldError{f.Key, msg})
			continue
		}
		out[f.Key] = nv
	}
	slices.SortFunc(errs, func(a, b FieldError) int { return strings.Compare(a.Key, b.Key) })
	return out, errs
}

func normalizeValue(f SpecField, v any) (any, string) {
	switch f.Type {
	case TypeNumber:
		n, ok := v.(float64) // encoding/json decodes numbers as float64
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, f.Label + " must be a number of 0 or more"
		}
		return n, ""
	case TypeText:
		t, ok := v.(string)
		if !ok {
			return nil, f.Label + " must be text"
		}
		t = strings.TrimSpace(t)
		if len([]rune(t)) > 200 {
			return nil, f.Label + " must be at most 200 characters"
		}
		return t, ""
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, f.Label + " must be yes or no"
		}
		return b, ""
	case TypeEnum:
		t, ok := v.(string)
		if !ok || !slices.Contains(f.Options, t) {
			return nil, f.Label + " must be one of: " + strings.Join(f.Options, ", ")
		}
		return t, ""
	case TypeMulti:
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return nil, f.Label + " must be a list of options"
		}
		chosen := map[string]bool{}
		for _, item := range list {
			t, ok := item.(string)
			if !ok || !slices.Contains(f.Options, t) {
				return nil, f.Label + " must only contain: " + strings.Join(f.Options, ", ")
			}
			chosen[t] = true
		}
		ordered := make([]string, 0, len(chosen))
		for _, o := range f.Options {
			if chosen[o] {
				ordered = append(ordered, o)
			}
		}
		return ordered, ""
	}
	return nil, "Unsupported field type"
}

func firstDuplicate(xs []string) string {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}
