package catalogue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Handler serves the public category endpoints.
type Handler struct{ Q *store.Queries }

// Routes mounts GET /categories and GET /categories/{slug} under /api/v1.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/categories", h.tree)
	r.Get("/categories/{slug}", h.one)
}

// Node is a category in the tree (menus, the categories page).
type Node struct {
	ID       uuid.UUID `json:"id"`
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Icon     string    `json:"icon,omitempty"`
	Examples string    `json:"examples,omitempty"`
	Children []Node    `json:"children,omitempty"`
}

// Detail is one category with its parent, children and spec schema.
type Detail struct {
	ID         uuid.UUID  `json:"id"`
	Slug       string     `json:"slug"`
	Name       string     `json:"name"`
	Icon       string     `json:"icon,omitempty"`
	Examples   string     `json:"examples,omitempty"`
	Parent     *Node      `json:"parent,omitempty"`
	Children   []Node     `json:"children"`
	SpecSchema SpecSchema `json:"specSchema"`
}

// BuildTree turns the flat active rows (parents first) into a two-level tree.
func BuildTree(rows []store.Category) []Node {
	index := map[uuid.UUID]int{}
	var roots []Node
	for _, c := range rows {
		if c.ParentID == nil {
			index[c.ID] = len(roots)
			roots = append(roots, Node{ID: c.ID, Slug: c.Slug, Name: c.Name, Icon: c.Icon, Children: []Node{}})
		}
	}
	for _, c := range rows {
		if c.ParentID == nil {
			continue
		}
		if i, ok := index[*c.ParentID]; ok { // children of retired parents are hidden too
			roots[i].Children = append(roots[i].Children, Node{ID: c.ID, Slug: c.Slug, Name: c.Name, Examples: c.Examples})
		}
	}
	if roots == nil {
		roots = []Node{}
	}
	return roots
}

func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Q.ListActiveCategories(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeCached(w, r, map[string]any{"categories": BuildTree(rows)})
}

func (h *Handler) one(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := h.Q.GetCategoryBySlug(ctx, chi.URLParam(r, "slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var schema SpecSchema
	if err := json.Unmarshal(c.SpecSchema, &schema); err != nil {
		httpx.WriteError(w, r, fmt.Errorf("catalogue: schema of %s: %w", c.Slug, err))
		return
	}
	if schema.Fields == nil {
		schema.Fields = []SpecField{}
	}
	d := Detail{ID: c.ID, Slug: c.Slug, Name: c.Name, Icon: c.Icon, Examples: c.Examples, Children: []Node{}, SpecSchema: schema}
	rows, err := h.Q.ListActiveCategories(ctx)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	for _, row := range rows {
		if c.ParentID != nil && row.ID == *c.ParentID {
			d.Parent = &Node{ID: row.ID, Slug: row.Slug, Name: row.Name, Icon: row.Icon}
		}
		if row.ParentID != nil && *row.ParentID == c.ID {
			d.Children = append(d.Children, Node{ID: row.ID, Slug: row.Slug, Name: row.Name, Examples: row.Examples})
		}
	}
	if c.ParentID != nil && d.Parent == nil { // parent retired
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	writeCached(w, r, d)
}

// writeCached sends JSON with a content ETag. Browsers and the Next.js server
// revalidate cheaply; categories change rarely (admin edits).
func writeCached(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
