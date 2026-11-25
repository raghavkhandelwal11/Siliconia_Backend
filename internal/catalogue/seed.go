package catalogue

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

//go:embed seed/categories.json
var seedJSON []byte

// SeedCategory is one top-level category in seed/categories.json.
type SeedCategory struct {
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Icon     string    `json:"icon"`
	Children []SeedSub `json:"children"`
}

// SeedSub is one sub-category with its spec schema.
type SeedSub struct {
	Slug       string     `json:"slug"`
	Name       string     `json:"name"`
	Examples   string     `json:"examples"`
	SpecSchema SpecSchema `json:"specSchema"`
}

// SeedData returns the 12 default categories (product-catalogue doc section 2).
func SeedData() ([]SeedCategory, error) {
	var doc struct {
		Categories []SeedCategory `json:"categories"`
	}
	if err := json.Unmarshal(seedJSON, &doc); err != nil {
		return nil, fmt.Errorf("catalogue: seed json: %w", err)
	}
	return doc.Categories, nil
}

// Seed inserts the default categories once. Existing rows (matched by slug)
// are left as admins last saved them. Returns how many rows were inserted.
func Seed(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	cats, err := SeedData()
	if err != nil {
		return 0, err
	}
	inserted := 0
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		for i, c := range cats {
			parent, n, err := seedOne(ctx, q, nil, c.Slug, c.Name, c.Icon, "", i, SpecSchema{Fields: []SpecField{}})
			if err != nil {
				return err
			}
			inserted += n
			for j, s := range c.Children {
				if err := s.SpecSchema.Validate(); err != nil {
					return fmt.Errorf("catalogue: %s: %w", s.Slug, err)
				}
				_, n, err := seedOne(ctx, q, &parent, s.Slug, s.Name, "", s.Examples, j, s.SpecSchema)
				if err != nil {
					return err
				}
				inserted += n
			}
		}
		return nil
	})
	if err != nil {
		return 0, err // rolled back: nothing was inserted
	}
	return inserted, nil
}

func seedOne(ctx context.Context, q *store.Queries, parent *uuid.UUID, slug, name, icon, examples string, pos int, schema SpecSchema) (uuid.UUID, int, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return uuid.Nil, 0, err
	}
	id, _ := uuid.NewV7()
	row, err := q.SeedCategory(ctx, store.SeedCategoryParams{
		ID: id, ParentID: parent, Slug: slug, Name: name, Icon: icon, Examples: examples, Position: int32(pos), SpecSchema: raw,
	})
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("catalogue: seed %s: %w", slug, err)
	}
	if row.Inserted {
		return row.ID, 1, nil
	}
	return row.ID, 0, nil
}
