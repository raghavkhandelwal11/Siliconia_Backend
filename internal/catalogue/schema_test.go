package catalogue

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func TestSeedDataIsValid(t *testing.T) {
	cats, err := SeedData()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != 12 {
		t.Fatalf("want the 12 categories from the catalogue doc, got %d", len(cats))
	}
	slugs := map[string]bool{}
	for _, c := range cats {
		for _, slug := range append([]string{c.Slug}, childSlugs(c)...) {
			if slugs[slug] {
				t.Errorf("slug %q used twice", slug)
			}
			slugs[slug] = true
		}
		if c.Icon == "" || len(c.Children) == 0 {
			t.Errorf("%s: needs an icon and sub-categories", c.Slug)
		}
		for _, s := range c.Children {
			if err := s.SpecSchema.Validate(); err != nil {
				t.Errorf("%s: %v", s.Slug, err)
			}
			var required, filters, main int
			for _, f := range s.SpecSchema.Fields {
				if f.Required {
					required++
				}
				if f.Filter {
					filters++
				}
				if f.Main {
					main++
				}
			}
			if required == 0 || filters == 0 || main == 0 {
				t.Errorf("%s: want at least one required, filter and main field (got %d/%d/%d)", s.Slug, required, filters, main)
			}
		}
	}
}

func childSlugs(c SeedCategory) []string {
	out := make([]string, len(c.Children))
	for i, s := range c.Children {
		out[i] = s.Slug
	}
	return out
}

func TestValidateRejectsBadSchemas(t *testing.T) {
	cases := map[string]SpecSchema{
		"bad key":        {Fields: []SpecField{{Key: "RAM GB", Label: "RAM", Type: TypeNumber}}},
		"duplicate key":  {Fields: []SpecField{{Key: "ram", Label: "RAM", Type: TypeNumber}, {Key: "ram", Label: "RAM 2", Type: TypeNumber}}},
		"no label":       {Fields: []SpecField{{Key: "ram", Type: TypeNumber}}},
		"enum 1 option":  {Fields: []SpecField{{Key: "os", Label: "OS", Type: TypeEnum, Options: []string{"Android"}}}},
		"dup option":     {Fields: []SpecField{{Key: "os", Label: "OS", Type: TypeEnum, Options: []string{"A", "A"}}}},
		"number options": {Fields: []SpecField{{Key: "ram", Label: "RAM", Type: TypeNumber, Options: []string{"1", "2"}}}},
		"unknown type":   {Fields: []SpecField{{Key: "ram", Label: "RAM", Type: "date"}}},
		"text filter":    {Fields: []SpecField{{Key: "cpu", Label: "CPU", Type: TypeText, Filter: true}}},
		"four main": {Fields: []SpecField{
			{Key: "a", Label: "A", Type: TypeBoolean, Main: true}, {Key: "b", Label: "B", Type: TypeBoolean, Main: true},
			{Key: "c", Label: "C", Type: TypeBoolean, Main: true}, {Key: "d", Label: "D", Type: TypeBoolean, Main: true}}},
	}
	for name, s := range cases {
		if s.Validate() == nil {
			t.Errorf("%s: schema accepted", name)
		}
	}
}

var phone = SpecSchema{Fields: []SpecField{
	{Key: "ram_gb", Label: "RAM", Type: TypeNumber, Required: true},
	{Key: "os", Label: "OS", Type: TypeEnum, Options: []string{"Android", "iOS"}, Required: true},
	{Key: "has_5g", Label: "5G", Type: TypeBoolean},
	{Key: "ports", Label: "Ports", Type: TypeMulti, Options: []string{"USB-C", "USB-A", "Wireless"}},
	{Key: "chip", Label: "Chip", Type: TypeText},
}}

func TestNormalizeAcceptsAndCanonicalises(t *testing.T) {
	got, errs := phone.Normalize(map[string]any{
		"ram_gb": 8.0, "os": "Android", "has_5g": true, "ports": []any{"Wireless", "USB-C"}, "chip": "  Dimensity 7300 ",
	})
	if errs != nil {
		t.Fatal(errs)
	}
	want := map[string]any{"ram_gb": 8.0, "os": "Android", "has_5g": true, "ports": []string{"USB-C", "Wireless"}, "chip": "Dimensity 7300"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeReportsEveryProblem(t *testing.T) {
	_, errs := phone.Normalize(map[string]any{
		"os": "Symbian", "has_5g": "yes", "ports": []any{"Serial"}, "colour": "red", "ram_gb": -1.0,
	})
	keys := make([]string, len(errs))
	for i, e := range errs {
		keys[i] = e.Key
	}
	if strings.Join(keys, ",") != "colour,has_5g,os,ports,ram_gb" {
		t.Fatalf("errors: %+v", errs)
	}
	if _, errs := phone.Normalize(map[string]any{"os": "iOS"}); len(errs) != 1 || errs[0].Key != "ram_gb" {
		t.Fatalf("missing required: %+v", errs)
	}
}

func TestBuildTreeNestsChildrenAndHidesOrphans(t *testing.T) {
	a, b, gone := uuid.New(), uuid.New(), uuid.New()
	rows := []store.Category{
		{ID: a, Slug: "audio", Name: "Audio"},
		{ID: b, ParentID: &a, Slug: "headphones", Name: "Headphones"},
		{ID: uuid.New(), ParentID: &gone, Slug: "orphan", Name: "Orphan"},
	}
	tree := BuildTree(rows)
	if len(tree) != 1 || len(tree[0].Children) != 1 || tree[0].Children[0].Slug != "headphones" {
		t.Fatalf("tree: %+v", tree)
	}
	if got := BuildTree(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty tree should be [] not null: %#v", got)
	}
}
