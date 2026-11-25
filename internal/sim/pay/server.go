package pay

import (
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

//go:embed checkout.html
var pages embed.FS

var checkoutTmpl = template.Must(template.ParseFS(pages, "checkout.html"))

// Handler serves the internal API (/v1, bearer key) and the hosted checkout
// page (/pay, public through Caddy).
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /pay/checkout/{id}", s.page)
	mux.HandleFunc("POST /pay/checkout/{id}", s.submit)
	api := http.NewServeMux()
	api.HandleFunc("POST /v1/orders", s.apiCreateOrder)
	api.HandleFunc("GET /v1/orders/{id}", s.apiGetOrder)
	api.HandleFunc("POST /v1/refunds", s.apiRefund)
	api.HandleFunc("POST /v1/mandates/{id}/charges", s.apiCharge)
	api.HandleFunc("POST /v1/mandates/{id}/revoke", s.apiRevoke)
	api.HandleFunc("GET /v1/settlements", s.apiSettle)
	mux.Handle("/v1/", s.requireKey(api))
	return mux
}

func (s *Sim) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.cfg.APIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case errors.Is(err, ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	case errors.Is(err, ErrConflict):
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"conflict"}`))
	case errors.Is(err, ErrBadInput):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad_input"}`))
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Sim) apiCreateOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ref       string `json:"ref"`
		Amount    int64  `json:"amount"`
		Mandate   bool   `json:"mandate"`
		ReturnURL string `json:"returnUrl"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in) != nil {
		reply(w, nil, ErrBadInput)
		return
	}
	o, err := s.CreateOrder(in.Ref, in.Amount, in.Mandate, in.ReturnURL)
	reply(w, o, err)
}

func (s *Sim) apiGetOrder(w http.ResponseWriter, r *http.Request) {
	o, err := s.GetOrder(r.PathValue("id"))
	reply(w, o, err)
}

func (s *Sim) apiRefund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PaymentID string `json:"paymentId"`
		Amount    int64  `json:"amount"`
		Ref       string `json:"ref"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in) != nil {
		reply(w, nil, ErrBadInput)
		return
	}
	rf, err := s.CreateRefund(in.PaymentID, in.Amount, in.Ref)
	reply(w, rf, err)
}

func (s *Sim) apiCharge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount int64  `json:"amount"`
		Ref    string `json:"ref"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in) != nil {
		reply(w, nil, ErrBadInput)
		return
	}
	o, err := s.ChargeMandate(r.PathValue("id"), in.Amount, in.Ref)
	reply(w, o, err)
}

func (s *Sim) apiRevoke(w http.ResponseWriter, r *http.Request) {
	err := s.RevokeMandate(r.PathValue("id"))
	reply(w, map[string]string{"status": "REVOKED"}, err)
}

func (s *Sim) apiSettle(w http.ResponseWriter, r *http.Request) {
	st, err := s.Settle(r.URL.Query().Get("date"))
	if err != nil || r.URL.Query().Get("format") != "csv" {
		reply(w, st, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="settlement-%s.csv"`, st.Date))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"type", "id", "ref", "amount_paise", "time"})
	for _, p := range st.Payments {
		_ = cw.Write([]string{"payment", p.PaymentID, p.Ref, strconv.FormatInt(p.Amount, 10), p.CapturedAt.Format("2006-01-02T15:04:05Z07:00")})
	}
	for _, rf := range st.Refunds {
		_ = cw.Write([]string{"refund", rf.ID, rf.Ref, strconv.FormatInt(-rf.Amount, 10), rf.DoneAt.Format("2006-01-02T15:04:05Z07:00")})
	}
	_ = cw.Write([]string{"net", "", "", strconv.FormatInt(st.Net, 10), ""})
	cw.Flush()
}

type pageData struct {
	Order  Order
	Rupees string
	Banks  []struct{ Code, Name string }
	Error  string
	Paid   bool
}

func rupees(paise int64) string {
	r := strconv.FormatInt(paise/100, 10)
	// Indian grouping: 1,23,45,678
	if len(r) > 3 {
		head, tail := r[:len(r)-3], r[len(r)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		r = strings.Join(parts, ",") + "," + tail
	}
	if p := paise % 100; p != 0 {
		r += fmt.Sprintf(".%02d", p)
	}
	return "₹" + r
}

func (s *Sim) render(w http.ResponseWriter, status int, o Order, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' http: https:; base-uri 'none'")
	w.WriteHeader(status)
	_ = checkoutTmpl.Execute(w, pageData{Order: o, Rupees: rupees(o.Amount), Banks: Banks, Error: msg, Paid: o.Status == Pending || o.Status == Captured})
}

func (s *Sim) page(w http.ResponseWriter, r *http.Request) {
	o, err := s.GetOrder(r.PathValue("id"))
	if err != nil {
		http.Error(w, "This checkout link is not valid.", http.StatusNotFound)
		return
	}
	s.render(w, http.StatusOK, o, "")
}

func (s *Sim) submit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	method := r.PostForm.Get("method")
	cred := map[string]string{"UPI": r.PostForm.Get("vpa"), "CARD": r.PostForm.Get("card"), "NETBANKING": r.PostForm.Get("bank")}[method]
	o, err := s.Pay(id, method, cred)
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "This checkout link is not valid.", http.StatusNotFound)
		return
	case errors.Is(err, ErrBadInput):
		cur, _ := s.GetOrder(id)
		s.render(w, http.StatusUnprocessableEntity, cur, "Use one of the test details shown on this page.")
		return
	case errors.Is(err, ErrConflict):
		// Already paid or in progress: send the shopper back to the shop.
	}
	status := "processing"
	if o.Status == Failed {
		status = "failed"
	}
	if o.ReturnURL == "" {
		s.render(w, http.StatusOK, o, "")
		return
	}
	u, perr := url.Parse(o.ReturnURL)
	if perr != nil {
		s.render(w, http.StatusOK, o, "")
		return
	}
	q := u.Query()
	q.Set("ref", o.Ref)
	q.Set("status", status)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
