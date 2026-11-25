// Package reviews holds buyer reviews with photos, helpful votes and
// reports (T4.1), product feedback to the manufacturer, and discovery:
// products of the week and recommendations (T4.7).
package reviews

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

var (
	ErrNotYours      = httpx.NewError(http.StatusForbidden, "NOT_PURCHASED", "You can review items you bought once they are delivered")
	ErrAlreadyReview = httpx.NewError(http.StatusConflict, "ALREADY_REVIEWED", "You've already reviewed this item")
)

// Service runs reviews, feedback and discovery.
type Service struct {
	Pool *pgxpool.Pool
	Q    *store.Queries
}

func media(key string) string { return "/api/v1/media/" + key }

// Input is a buyer review.
type Input struct {
	Rating   int32       `json:"rating"`
	Title    string      `json:"title"`
	Body     string      `json:"body"`
	PhotoIDs []uuid.UUID `json:"photoIds"`
}

// Create reviews a delivered item (one review per item; LLD 5.3).
func (s *Service) Create(ctx context.Context, buyer, itemID uuid.UUID, in Input) (uuid.UUID, error) {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Rating < 1 || in.Rating > 5 || len([]rune(in.Title)) > 120 || len([]rune(in.Body)) > 3000 || len(in.PhotoIDs) > 5 {
		return uuid.Nil, httpx.ErrValidation.WithDetail("Rate 1 to 5 stars; title up to 120 and review up to 3000 characters; up to 5 photos")
	}
	id, _ := uuid.NewV7()
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		it, err := q.ReviewableItem(ctx, itemID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && it.BuyerID != buyer) {
			return ErrNotYours
		}
		if err != nil {
			return err
		}
		if it.SubStatus != store.SubStatusDELIVERED || (it.Status != "ACTIVE" && it.Status != "REPLACED") {
			return ErrNotYours
		}
		r, err := q.CreateReview(ctx, store.CreateReviewParams{ID: id, OrderItemID: itemID, ProductID: it.ProductID, AccountID: buyer,
			Rating: in.Rating, Title: in.Title, Body: in.Body})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrAlreadyReview
		}
		if err != nil {
			return err
		}
		for i, p := range in.PhotoIDs {
			if err := q.AddReviewPhoto(ctx, store.AddReviewPhotoParams{ReviewID: r.ID, Position: int32(i), UploadID: p, OwnerID: buyer}); err != nil {
				return err
			}
		}
		return q.RefreshProductRating(ctx, it.ProductID)
	})
	return id, err
}

// Review is a published review.
type Review struct {
	ID       uuid.UUID `json:"id"`
	Rating   int32     `json:"rating"`
	Title    string    `json:"title,omitempty"`
	Body     string    `json:"body,omitempty"`
	Author   string    `json:"author"`
	Helpful  int32     `json:"helpful"`
	Photos   []string  `json:"photos"`
	Verified bool      `json:"verifiedPurchase"`
	At       time.Time `json:"at"`
}

// Summary is the star breakdown.
type Summary struct {
	Count   int32    `json:"count"`
	Average float64  `json:"average"`
	Stars   [5]int32 `json:"stars"` // 5★ … 1★
}

// List returns 10 published reviews per page and the summary.
func (s *Service) List(ctx context.Context, productID uuid.UUID, sort string, page int32) ([]Review, Summary, error) {
	if sort != "helpful" {
		sort = "recent"
	}
	rows, err := s.Q.ListProductReviews(ctx, store.ListProductReviewsParams{ProductID: productID, Sort: sort, Off: max(page, 0) * 10})
	if err != nil {
		return nil, Summary{}, err
	}
	out := make([]Review, len(rows))
	for i, r := range rows {
		out[i] = Review{ID: r.ID, Rating: r.Rating, Title: r.Title, Body: r.Body, Author: firstName(r.Author), Helpful: r.HelpfulCount,
			Photos: []string{}, Verified: true, At: r.CreatedAt}
		for _, k := range r.Photos {
			out[i].Photos = append(out[i].Photos, media(k))
		}
	}
	sm, err := s.Q.ReviewSummary(ctx, productID)
	if err != nil {
		return nil, Summary{}, err
	}
	return out, Summary{Count: sm.N, Average: sm.Avg, Stars: [5]int32{sm.S5, sm.S4, sm.S3, sm.S2, sm.S1}}, nil
}

func firstName(n string) string {
	if f := strings.Fields(n); len(f) > 0 {
		return f[0]
	}
	return "Buyer"
}

// Helpful counts one vote per account (not for your own review).
func (s *Service) Helpful(ctx context.Context, acc, id uuid.UUID) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		n, err := q.AddHelpful(ctx, store.AddHelpfulParams{ReviewID: id, AccountID: acc})
		if err != nil || n == 0 {
			return err
		}
		return q.BumpHelpful(ctx, id)
	})
}

// Report flags a review; three reports hide it pending moderation.
func (s *Service) Report(ctx context.Context, acc, id uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 3 || len([]rune(reason)) > 300 {
		return httpx.ErrValidation.WithDetail("Tell us what's wrong (3 to 300 characters)")
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		n, err := q.AddReport(ctx, store.AddReportParams{ReviewID: id, AccountID: acc, Reason: reason})
		if err != nil || n == 0 {
			return err
		}
		r, err := q.BumpReport(ctx, id)
		if err != nil {
			return err
		}
		if r.Status == "HIDDEN" {
			return q.RefreshProductRating(ctx, r.ProductID)
		}
		return nil
	})
}

// Moderate publishes or hides a review (admin).
func (s *Service) Moderate(ctx context.Context, id uuid.UUID, publish bool) error {
	status := "HIDDEN"
	if publish {
		status = "PUBLISHED"
	}
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		pid, err := q.SetReviewStatus(ctx, store.SetReviewStatusParams{ID: id, Status: status})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		return q.RefreshProductRating(ctx, pid)
	})
}

// Reported lists reviews with reports for moderation.
func (s *Service) Reported(ctx context.Context) ([]store.ReportedReviewsRow, error) {
	return s.Q.ReportedReviews(ctx)
}

// ---- Feedback to the manufacturer ------------------------------------------------

var feedbackCategories = map[string]bool{"QUALITY": true, "FEATURE": true, "PACKAGING": true, "DOCUMENTATION": true, "OTHER": true}

// Feedback sends a buyer's product suggestion to the seller.
func (s *Service) Feedback(ctx context.Context, buyer, productID uuid.UUID, category, body string) error {
	body = strings.TrimSpace(body)
	if !feedbackCategories[category] || len([]rune(body)) < 5 || len([]rune(body)) > 2000 {
		return httpx.ErrValidation.WithDetail("Pick a category and write 5 to 2000 characters")
	}
	if ok, err := s.Q.HasDeliveredPurchase(ctx, store.HasDeliveredPurchaseParams{BuyerID: buyer, ProductID: productID}); err != nil {
		return err
	} else if !ok {
		return ErrNotYours
	}
	id, _ := uuid.NewV7()
	_, err := s.Q.CreateFeedback(ctx, store.CreateFeedbackParams{ID: id, ProductID: productID, BuyerID: buyer, Category: category, Body: body})
	return err
}

// Reply answers feedback (the product's seller only).
func (s *Service) Reply(ctx context.Context, seller, id uuid.UUID, reply string) error {
	reply = strings.TrimSpace(reply)
	if len([]rune(reply)) < 2 || len([]rune(reply)) > 2000 {
		return httpx.ErrValidation.WithDetail("Write a reply (2 to 2000 characters)")
	}
	n, err := s.Q.ReplyFeedback(ctx, store.ReplyFeedbackParams{ID: id, SellerID: seller, SellerReply: &reply})
	if err == nil && n == 0 {
		return httpx.ErrNotFound
	}
	return err
}

// ---- Discovery ---------------------------------------------------------------------

// Card is a product tile.
type Card struct {
	ID          uuid.UUID `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Brand       string    `json:"brand"`
	PricePaise  int64     `json:"pricePaise"`
	RatingAvg   float64   `json:"ratingAvg"`
	RatingCount int32     `json:"ratingCount"`
	CoverURL    string    `json:"coverUrl,omitempty"`
}

func card(id uuid.UUID, slug, name, brand string, price int64, avg float64, n int32, cover string) Card {
	c := Card{ID: id, Slug: slug, Name: name, Brand: brand, PricePaise: price, RatingAvg: avg, RatingCount: n}
	if cover != "" {
		c.CoverURL = media(cover)
	}
	return c
}

// ComputeWeek ranks this week's products (Monday job, IST).
func (s *Service) ComputeWeek(ctx context.Context, now time.Time) error {
	ist := now.In(time.FixedZone("IST", 19800))
	monday := ist.AddDate(0, 0, -((int(ist.Weekday()) + 6) % 7))
	start := time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
	return s.Q.ComputeProductsOfWeek(ctx, pgtype.Date{Time: start, Valid: true})
}

// Week lists the current products of the week.
func (s *Service) Week(ctx context.Context) ([]Card, error) {
	rows, err := s.Q.ProductsOfWeek(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Card, len(rows))
	for i, r := range rows {
		out[i] = card(r.ID, r.Slug, r.Name, r.Brand, r.PricePaise, r.RatingAvg, r.RatingCount, r.Cover)
	}
	return out, nil
}

// BoughtTogether recommends products often ordered with this one.
func (s *Service) BoughtTogether(ctx context.Context, productID uuid.UUID) ([]Card, error) {
	rows, err := s.Q.BoughtTogether(ctx, productID)
	if err != nil {
		return nil, err
	}
	out := make([]Card, len(rows))
	for i, r := range rows {
		out[i] = card(r.ID, r.Slug, r.Name, r.Brand, r.PricePaise, r.RatingAvg, r.RatingCount, r.Cover)
	}
	return out, nil
}

// ForYou recommends popular products from categories the buyer shops in.
func (s *Service) ForYou(ctx context.Context, buyer uuid.UUID) ([]Card, error) {
	rows, err := s.Q.ForYou(ctx, buyer)
	if err != nil {
		return nil, err
	}
	out := make([]Card, len(rows))
	for i, r := range rows {
		out[i] = card(r.ID, r.Slug, r.Name, r.Brand, r.PricePaise, r.RatingAvg, r.RatingCount, r.Cover)
	}
	return out, nil
}
