// Package m4http serves the M4 endpoints: buyer reviews and feedback,
// discovery, creators, demos, creator reviews, reward pools, cash-out and
// promoters (LLD 7.6).
package m4http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/whsig"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/product"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/programmes"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/reviews"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Handler serves M4.
type Handler struct {
	Reviews    *reviews.Service
	Programmes *programmes.Service
	Products   *product.Service
}

// PublicRoutes need no session.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/products/{id}/reviews", h.listReviews)
	r.Get("/products/{id}/promoters", h.promoters)
	r.Get("/products/{id}/bought-together", h.boughtTogether)
	r.Get("/home/products-of-week", h.week)
	r.Post("/webhooks/payout", h.payoutWebhook)
}

// ViewerRoutes read the session when present.
func (h *Handler) ViewerRoutes(r chi.Router) {
	r.Get("/products/{id}/creator-reviews", h.creatorReviews)
}

// Routes need a session and CSRF.
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleBuyer))
		r.Post("/uploads/review-images", h.uploadReviewImage)
		r.Post("/order-items/{id}/review", h.createReview)
		r.Get("/me/reviewed-items", h.reviewedItems)
		r.Post("/reviews/{id}/helpful", h.helpful)
		r.Post("/reviews/{id}/report", h.report)
		r.Post("/products/{id}/feedback", h.feedback)
		r.Post("/creator-reviews/{id}/vote", h.vote)
		r.Get("/home/for-you", h.forYou)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleCreator))
		r.Get("/creator/application", h.getCreator)
		r.Put("/creator/application", h.saveCreator)
		r.Post("/creator/application/submit", h.submitCreator)
		r.Post("/creator/kyc", h.kyc)
		r.Post("/creator/demo-requests", h.requestDemo)
		r.Get("/creator/demo-requests", h.creatorDemos)
		r.Post("/creator/reviews", h.postCreatorReview)
		r.Patch("/creator/reviews/{id}", h.editCreatorReview)
		r.Get("/creator/reviews", h.myCreatorReviews)
		r.Get("/creator/earnings", h.earnings)
		r.Post("/creator/cashouts", h.cashout)
		r.Get("/creator/cashouts", h.cashouts)
		r.Delete("/creator/cashouts/{id}", h.cancelCashout)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RolePromoter))
		r.Put("/promoter/application", h.savePromoter)
		r.Get("/promoter/dashboard", h.promoterDashboard)
		r.Post("/promoter/investments", h.addInvestment)
		r.Put("/promoter/products/{id}/promotion", h.promote(true))
		r.Delete("/promoter/products/{id}/promotion", h.promote(false))
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Get("/seller/demo-requests", h.sellerDemos)
		r.Post("/seller/demo-requests/{id}/decision", h.decideDemo)
		r.Get("/seller/promoters", h.sellerPromoters)
		r.Post("/seller/promoters/{id}/confirm", h.confirmPromoter)
		r.Put("/seller/products/{id}/promoter-reward", h.rewardRule)
		r.Get("/seller/feedback", h.sellerFeedback)
		r.Post("/seller/feedback/{id}/reply", h.replyFeedback)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(true))
		r.Get("/admin/approvals/{role}", h.queue)
		r.Post("/admin/approvals/{role}/{id}/decision", h.decide)
		r.Get("/admin/reviews/reported", h.reported)
		r.Post("/admin/reviews/{id}/moderate", h.moderate)
		r.Put("/admin/reward-pools", h.setPool)
		r.Get("/admin/reward-pools", h.pools)
	})
}

func subject(r *http.Request) uuid.UUID {
	c, _ := auth.ClaimsFrom(r.Context())
	return c.Subject
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
	}
	return id, err == nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("The request body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, status, v)
}

func items(v any) map[string]any { return map[string]any{"items": v} }

// ---- Buyer reviews -----------------------------------------------------------------

func (h *Handler) uploadReviewImage(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		httpx.WriteError(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", "Images can be up to 10 MB"))
		return
	}
	up, err := h.Products.UploadReviewImage(r.Context(), subject(r), data)
	reply(w, r, http.StatusCreated, up, err)
}

func (h *Handler) createReview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in reviews.Input
	if !ok || !decode(w, r, &in) {
		return
	}
	rid, err := h.Reviews.Create(r.Context(), subject(r), id, in)
	reply(w, r, http.StatusCreated, map[string]any{"id": rid}, err)
}

func (h *Handler) reviewedItems(w http.ResponseWriter, r *http.Request) {
	ids, err := h.Reviews.Q.MyReviewItems(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(ids), err)
}

func (h *Handler) listReviews(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	list, sum, err := h.Reviews.List(r.Context(), id, r.URL.Query().Get("sort"), int32(page))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list, "summary": sum})
}

func (h *Handler) helpful(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if ok {
		reply(w, r, http.StatusNoContent, nil, h.Reviews.Helpful(r.Context(), subject(r), id))
	}
}

func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Reason string `json:"reason"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Reviews.Report(r.Context(), subject(r), id, in.Reason))
	}
}

func (h *Handler) feedback(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Category string `json:"category"`
		Body     string `json:"body"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Reviews.Feedback(r.Context(), subject(r), id, in.Category, in.Body))
	}
}

func (h *Handler) sellerFeedback(w http.ResponseWriter, r *http.Request) {
	list, err := h.Reviews.Q.SellerFeedback(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) replyFeedback(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Reply string `json:"reply"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Reviews.Reply(r.Context(), subject(r), id, in.Reply))
	}
}

func (h *Handler) reported(w http.ResponseWriter, r *http.Request) {
	list, err := h.Reviews.Reported(r.Context())
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) moderate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Publish bool `json:"publish"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Reviews.Moderate(r.Context(), id, in.Publish))
	}
}

// ---- Discovery ------------------------------------------------------------------------

func (h *Handler) week(w http.ResponseWriter, r *http.Request) {
	list, err := h.Reviews.Week(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, items(list))
}

func (h *Handler) boughtTogether(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	list, err := h.Reviews.BoughtTogether(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, items(list))
}

func (h *Handler) forYou(w http.ResponseWriter, r *http.Request) {
	list, err := h.Reviews.ForYou(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(list), err)
}

// ---- Creators ----------------------------------------------------------------------------

func (h *Handler) getCreator(w http.ResponseWriter, r *http.Request) {
	a, err := h.Programmes.GetCreator(r.Context(), subject(r))
	reply(w, r, http.StatusOK, a, err)
}

func (h *Handler) saveCreator(w http.ResponseWriter, r *http.Request) {
	var in programmes.CreatorApplication
	if decode(w, r, &in) {
		a, err := h.Programmes.SaveCreator(r.Context(), subject(r), in)
		reply(w, r, http.StatusOK, a, err)
	}
}

func (h *Handler) submitCreator(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AcceptTerms bool `json:"acceptTerms"`
	}
	if decode(w, r, &in) {
		a, err := h.Programmes.SubmitCreator(r.Context(), subject(r), in.AcceptTerms)
		reply(w, r, http.StatusOK, a, err)
	}
}

func (h *Handler) kyc(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PAN     string `json:"pan"`
		Account string `json:"account"`
		IFSC    string `json:"ifsc"`
	}
	if decode(w, r, &in) {
		a, err := h.Programmes.VerifyKYC(r.Context(), subject(r), in.PAN, in.Account, in.IFSC)
		reply(w, r, http.StatusOK, a, err)
	}
}

func (h *Handler) requestDemo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID uuid.UUID `json:"productId"`
		Note      string    `json:"note"`
	}
	if decode(w, r, &in) {
		d, err := h.Programmes.RequestDemo(r.Context(), subject(r), in.ProductID, in.Note)
		reply(w, r, http.StatusCreated, d, err)
	}
}

func (h *Handler) creatorDemos(w http.ResponseWriter, r *http.Request) {
	me := subject(r)
	list, err := h.Programmes.Demos(r.Context(), &me, nil)
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) sellerDemos(w http.ResponseWriter, r *http.Request) {
	me := subject(r)
	list, err := h.Programmes.Demos(r.Context(), nil, &me)
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) decideDemo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.DecideDemo(r.Context(), subject(r), id, in.Approve, in.Reason))
	}
}

func (h *Handler) postCreatorReview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID uuid.UUID `json:"productId"`
		programmes.CreatorReviewInput
	}
	if decode(w, r, &in) {
		cr, err := h.Programmes.PostCreatorReview(r.Context(), subject(r), in.ProductID, in.CreatorReviewInput)
		reply(w, r, http.StatusCreated, cr, err)
	}
}

func (h *Handler) editCreatorReview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in programmes.CreatorReviewInput
	if ok && decode(w, r, &in) {
		cr, err := h.Programmes.EditCreatorReview(r.Context(), subject(r), id, in)
		reply(w, r, http.StatusOK, cr, err)
	}
}

func (h *Handler) myCreatorReviews(w http.ResponseWriter, r *http.Request) {
	list, err := h.Programmes.Q.MyCreatorReviews(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) creatorReviews(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var viewer *uuid.UUID
	if c, ok := auth.ClaimsFrom(r.Context()); ok {
		viewer = &c.Subject
	}
	list, err := h.Programmes.CreatorReviews(r.Context(), id, viewer)
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) vote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Value int32 `json:"value"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.Vote(r.Context(), subject(r), id, in.Value))
	}
}

func (h *Handler) earnings(w http.ResponseWriter, r *http.Request) {
	list, err := h.Programmes.Q.CreatorEarnings(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) cashout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Electrons int32 `json:"electrons"`
	}
	if decode(w, r, &in) {
		c, err := h.Programmes.RequestCashout(r.Context(), subject(r), in.Electrons)
		reply(w, r, http.StatusCreated, c, err)
	}
}

func (h *Handler) cashouts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Programmes.Q.ListCashouts(r.Context(), subject(r))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) cancelCashout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if ok {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.CancelCashout(r.Context(), subject(r), id))
	}
}

func (h *Handler) payoutWebhook(w http.ResponseWriter, r *http.Request) {
	if err := h.Programmes.HandlePayoutWebhook(r); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, whsig.ErrBadSignature) || errors.Is(err, payout.ErrMalformed) {
			status = http.StatusBadRequest
		}
		http.Error(w, `{"error":"`+http.StatusText(status)+`"}`, status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Admin --------------------------------------------------------------------------------

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	list, err := h.Programmes.Queue(r.Context(), roleParam(r))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if ok && decode(w, r, &in) {
		if roleParam(r) == "" {
			httpx.WriteError(w, r, httpx.ErrNotFound)
			return
		}
		reply(w, r, http.StatusNoContent, nil, h.Programmes.Decide(r.Context(), id, in.Approve, in.Reason))
	}
}

func (h *Handler) setPool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID uuid.UUID `json:"productId"`
		Period    string    `json:"period"`
		Size      int32     `json:"size"`
	}
	if decode(w, r, &in) {
		p, err := h.Programmes.SetPool(r.Context(), in.ProductID, in.Period, in.Size)
		reply(w, r, http.StatusOK, p, err)
	}
}

func (h *Handler) pools(w http.ResponseWriter, r *http.Request) {
	list, err := h.Programmes.Q.ListPools(r.Context())
	reply(w, r, http.StatusOK, items(list), err)
}

// ---- Promoters ------------------------------------------------------------------------------

func (h *Handler) savePromoter(w http.ResponseWriter, r *http.Request) {
	var in programmes.PromoterProfile
	if decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.SavePromoter(r.Context(), subject(r), in))
	}
}

func (h *Handler) promoterDashboard(w http.ResponseWriter, r *http.Request) {
	me := subject(r)
	ctx := r.Context()
	a, err := h.Programmes.Q.GetAccountApproval(ctx, me)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := map[string]any{"approval": a.Approval, "reason": a.ApprovalReason}
	if p, err := h.Programmes.Q.GetPromoterProfile(ctx, me); err == nil {
		out["profile"] = p
	}
	inv, err := h.Programmes.Q.ListInvestments(ctx, storeInvParams(&me, nil))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	prods, err := h.Programmes.Q.PromotableProducts(ctx, me)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out["investments"], out["products"] = inv, prods
	reply(w, r, http.StatusOK, out, nil)
}

func (h *Handler) addInvestment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SellerID uuid.UUID `json:"sellerId"`
		Proof    string    `json:"proof"`
	}
	if decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.AddInvestment(r.Context(), subject(r), in.SellerID, in.Proof))
	}
}

func (h *Handler) promote(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if ok {
			reply(w, r, http.StatusNoContent, nil, h.Programmes.SetPromotion(r.Context(), subject(r), id, on))
		}
	}
}

func (h *Handler) promoters(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	list, err := h.Programmes.Q.ProductPromoters(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	httpx.WriteJSON(w, http.StatusOK, items(list))
}

func (h *Handler) sellerPromoters(w http.ResponseWriter, r *http.Request) {
	me := subject(r)
	list, err := h.Programmes.Q.ListInvestments(r.Context(), storeInvParams(nil, &me))
	reply(w, r, http.StatusOK, items(list), err)
}

func (h *Handler) confirmPromoter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.DecideInvestment(r.Context(), subject(r), id, in.Confirm))
	}
}

func (h *Handler) rewardRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	var in struct {
		ElectronsPerUnit int32 `json:"electronsPerUnit"`
	}
	if ok && decode(w, r, &in) {
		reply(w, r, http.StatusNoContent, nil, h.Programmes.SetRewardRule(r.Context(), subject(r), id, in.ElectronsPerUnit))
	}
}

func storeInvParams(promoter, seller *uuid.UUID) store.ListInvestmentsParams {
	return store.ListInvestmentsParams{PromoterID: promoter, SellerID: seller}
}

// roleParam maps /admin/approvals/creators|promoters to the account role.
func roleParam(r *http.Request) string {
	switch chi.URLParam(r, "role") {
	case "creators":
		return "CREATOR"
	case "promoters":
		return "PROMOTER"
	}
	return ""
}
