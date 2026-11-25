package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Handler serves M5.
type Handler struct{ Svc *Service }

// PublicRoutes need no session.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/legal/{kind}", h.legal)
}

// Routes need a session and CSRF.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/me/legal/pending", h.pendingLegal)
	r.Post("/me/legal/accept", h.acceptLegal)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole(auth.RoleSeller))
		r.Get("/seller/payouts", h.sellerPayouts)
		r.Get("/seller/payouts/{id}/statement.csv", h.statement(false))
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin(true))
		r.Get("/admin/overview", h.overview)
		r.Get("/admin/accounts", h.accounts)
		r.Post("/admin/accounts/{id}/status", h.accountStatus)
		r.Get("/admin/orders", h.orders)
		r.Get("/admin/returns", h.returns)
		r.Get("/admin/payout-batches", h.batches)
		r.Post("/admin/payout-batches", h.createBatch)
		r.Get("/admin/payout-batches/{id}", h.batch)
		r.Post("/admin/payout-batches/{id}/{action}", h.batchAction)
		r.Get("/admin/payouts/{id}/statement.csv", h.statement(true))
		r.Get("/admin/config", h.config)
		r.Put("/admin/config/{key}", h.setConfig)
		r.Get("/admin/config/{key}/history", h.configHistory)
		r.Get("/admin/categories", h.categories)
		r.Put("/admin/categories/{id}/rates", h.setRates)
		r.Get("/admin/legal", h.legalDocs)
		r.Post("/admin/legal", h.publishLegal)
		r.Get("/admin/audit", h.audit)
		r.Get("/admin/reports/{name}.csv", h.report)
		r.Get("/admin/ledger-adjustments", h.adjustments)
		r.Post("/admin/ledger-adjustments", h.requestAdjustment)
		r.Post("/admin/ledger-adjustments/{id}/decision", h.decideAdjustment)
		r.Get("/admin/health", h.health)
		r.Get("/admin/recon-issues", h.recon)
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
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

type list[T any] struct {
	Items []T `json:"items"`
}

func csvReply(w http.ResponseWriter, r *http.Request, name string, b []byte, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(b)
}

func (h *Handler) legal(w http.ResponseWriter, r *http.Request) {
	d, err := h.Svc.Q.LatestLegalOf(r.Context(), chi.URLParam(r, "kind"))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (h *Handler) pendingLegal(w http.ResponseWriter, r *http.Request) {
	ds, err := h.Svc.Q.PendingLegal(r.Context(), subject(r))
	reply(w, r, http.StatusOK, list[store.LegalDoc]{ds}, err)
}

func (h *Handler) acceptLegal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DocIDs []uuid.UUID `json:"docIds"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.DocIDs) == 0 || len(in.DocIDs) > 10 {
		httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Accept 1 to 10 documents"))
		return
	}
	reply(w, r, http.StatusNoContent, nil, h.Svc.AcceptLegal(r.Context(), subject(r), in.DocIDs, httpx.ClientIP(r)))
}

func (h *Handler) sellerPayouts(w http.ResponseWriter, r *http.Request) {
	ps, err := h.Svc.Q.SellerPayouts(r.Context(), subject(r))
	reply(w, r, http.StatusOK, list[store.Payout]{ps}, err)
}

func (h *Handler) statement(isAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		b, err := h.Svc.Statement(r.Context(), subject(r), id, isAdmin)
		csvReply(w, r, "payout-"+id.String(), b, err)
	}
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	o, err := h.Svc.Q.AdminOverview(r.Context())
	reply(w, r, http.StatusOK, o, err)
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	p := store.SearchAccountsParams{Search: r.URL.Query().Get("q")}
	if role := r.URL.Query().Get("role"); role != "" {
		p.Role = store.NullAccountRole{AccountRole: store.AccountRole(role), Valid: true}
		if !map[string]bool{"BUYER": true, "SELLER": true, "CREATOR": true, "PROMOTER": true, "ADMIN": true}[role] {
			httpx.WriteError(w, r, httpx.ErrValidation.WithDetail("Unknown role"))
			return
		}
	}
	rows, err := h.Svc.Q.SearchAccounts(r.Context(), p)
	reply(w, r, http.StatusOK, list[store.SearchAccountsRow]{rows}, err)
}

func (h *Handler) accountStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Suspend bool   `json:"suspend"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, http.StatusNoContent, nil, h.Svc.SetAccountStatus(r.Context(), subject(r), id, in.Suspend, in.Reason))
}

func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.AdminOrders(r.Context(), r.URL.Query().Get("q"))
	reply(w, r, http.StatusOK, list[store.AdminOrdersRow]{rows}, err)
}

func (h *Handler) returns(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.AdminReturns(r.Context())
	reply(w, r, http.StatusOK, list[store.AdminReturnsRow]{rows}, err)
}

func (h *Handler) batches(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ListBatches(r.Context())
	reply(w, r, http.StatusOK, list[store.ListBatchesRow]{rows}, err)
}

func (h *Handler) createBatch(w http.ResponseWriter, r *http.Request) {
	admin := subject(r)
	id, err := h.Svc.CreateBatch(r.Context(), &admin)
	reply(w, r, http.StatusCreated, map[string]uuid.UUID{"id": id}, err)
}

// BatchPayout is a payout row for the admin view; bank details stay sealed.
type BatchPayout struct {
	ID              uuid.UUID `json:"id"`
	SellerID        uuid.UUID `json:"sellerId"`
	StoreName       string    `json:"storeName"`
	GrossPaise      int64     `json:"grossPaise"`
	CommissionPaise int64     `json:"commissionPaise"`
	TdsPaise        int64     `json:"tdsPaise"`
	NetPaise        int64     `json:"netPaise"`
	Status          string    `json:"status"`
	BankRef         *string   `json:"bankRef"`
	FailureReason   *string   `json:"failureReason"`
}

func (h *Handler) batch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := h.Svc.Q.BatchPayouts(r.Context(), id)
	out := make([]BatchPayout, 0, len(rows))
	for _, p := range rows {
		out = append(out, BatchPayout{p.ID, p.SellerID, p.StoreName, p.GrossPaise, p.CommissionPaise, p.TdsPaise, p.NetPaise, p.Status, p.BankRef, p.FailureReason})
	}
	reply(w, r, http.StatusOK, list[BatchPayout]{out}, err)
}

func (h *Handler) batchAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var err error
	switch chi.URLParam(r, "action") {
	case "approve":
		err = h.Svc.Approve(r.Context(), subject(r), id)
	case "release":
		err = h.Svc.Release(r.Context(), subject(r), id)
	case "cancel":
		err = h.Svc.Cancel(r.Context(), subject(r), id)
	default:
		err = httpx.ErrNotFound
	}
	reply(w, r, http.StatusNoContent, nil, err)
}

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ListConfig(r.Context())
	reply(w, r, http.StatusOK, list[store.Config]{rows}, err)
}

func (h *Handler) setConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Value   json.RawMessage `json:"value"`
		Version int32           `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, http.StatusNoContent, nil, h.Svc.SetConfig(r.Context(), subject(r), chi.URLParam(r, "key"), in.Value, in.Version))
}

func (h *Handler) configHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ConfigHistory(r.Context(), chi.URLParam(r, "key"))
	reply(w, r, http.StatusOK, list[store.ConfigHistoryRow]{rows}, err)
}

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.AdminCategories(r.Context())
	reply(w, r, http.StatusOK, list[store.AdminCategoriesRow]{rows}, err)
}

func (h *Handler) setRates(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in CategoryRates
	if !decode(w, r, &in) {
		return
	}
	out, err := h.Svc.SetCategoryRates(r.Context(), subject(r), id, in)
	reply(w, r, http.StatusOK, out, err)
}

func (h *Handler) legalDocs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.LatestLegal(r.Context())
	reply(w, r, http.StatusOK, list[store.LegalDoc]{rows}, err)
}

func (h *Handler) publishLegal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind   string `json:"kind"`
		Title  string `json:"title"`
		BodyMd string `json:"bodyMd"`
	}
	if !decode(w, r, &in) {
		return
	}
	d, err := h.Svc.PublishLegal(r.Context(), subject(r), in.Kind, in.Title, in.BodyMd)
	reply(w, r, http.StatusCreated, d, err)
}

func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ListAudit(r.Context(), r.URL.Query().Get("targetType"))
	reply(w, r, http.StatusOK, list[store.ListAuditRow]{rows}, err)
}

func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	b, err := h.Svc.Report(r.Context(), name)
	csvReply(w, r, name+"-"+time.Now().Format("20060102"), b, err)
}

func (h *Handler) adjustments(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ListAdjustments(r.Context())
	reply(w, r, http.StatusOK, list[store.ListAdjustmentsRow]{rows}, err)
}

func (h *Handler) requestAdjustment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID uuid.UUID `json:"accountId"`
		Amount    int32     `json:"amount"`
		Reason    string    `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := h.Svc.RequestAdjustment(r.Context(), subject(r), in.AccountID, in.Amount, in.Reason)
	reply(w, r, http.StatusCreated, out, err)
}

func (h *Handler) decideAdjustment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Approve bool `json:"approve"`
	}
	if !decode(w, r, &in) {
		return
	}
	reply(w, r, http.StatusNoContent, nil, h.Svc.DecideAdjustment(r.Context(), subject(r), id, in.Approve))
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	checks, err := h.Svc.Q.HealthSummary(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	inc, err := h.Svc.Q.OpenIncidents(r.Context())
	reply(w, r, http.StatusOK, map[string]any{"checks": checks, "incidents": inc}, err)
}

func (h *Handler) recon(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Svc.Q.ReconIssues(r.Context())
	reply(w, r, http.StatusOK, list[store.ReconIssue]{rows}, err)
}
