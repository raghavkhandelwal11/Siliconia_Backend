package ship

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Handler serves the internal API (/v1, bearer key). Nothing is public: the
// shop shows tracking from its own database.
//
//	GET  /v1/serviceability/{pincode}
//	POST /v1/shipments                 (kind FORWARD or RETURN)
//	GET  /v1/shipments/{awb}
//	GET  /v1/shipments/{awb}/label     (PDF)
//	POST /v1/shipments/{awb}/cancel
//	POST /v1/shipments/{awb}/advance   (next tracking step now, for testers)
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/serviceability/{pincode}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, Check(r.PathValue("pincode")), nil)
	})
	api.HandleFunc("POST /v1/shipments", s.apiCreate)
	api.HandleFunc("GET /v1/shipments/{awb}", func(w http.ResponseWriter, r *http.Request) {
		sh, err := s.Get(r.PathValue("awb"))
		reply(w, sh, err)
	})
	api.HandleFunc("GET /v1/shipments/{awb}/label", s.apiLabel)
	api.HandleFunc("POST /v1/shipments/{awb}/cancel", func(w http.ResponseWriter, r *http.Request) {
		sh, err := s.Cancel(r.PathValue("awb"))
		reply(w, sh, err)
	})
	api.HandleFunc("POST /v1/shipments/{awb}/advance", func(w http.ResponseWriter, r *http.Request) {
		sh, err := s.Advance(r.PathValue("awb"))
		reply(w, sh, err)
	})
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
	code := ""
	switch {
	case errors.Is(err, ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		code = "not_found"
	case errors.Is(err, ErrConflict):
		w.WriteHeader(http.StatusConflict)
		code = "conflict"
	case errors.Is(err, ErrNotServiceable):
		w.WriteHeader(http.StatusUnprocessableEntity)
		code = "not_serviceable"
	case errors.Is(err, ErrBadInput):
		w.WriteHeader(http.StatusBadRequest)
		code = "bad_input"
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
		return
	default:
		_ = json.NewEncoder(w).Encode(v)
		return
	}
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}

func (s *Sim) apiCreate(w http.ResponseWriter, r *http.Request) {
	var in ShipmentRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil {
		reply(w, nil, ErrBadInput)
		return
	}
	sh, err := s.CreateShipment(in)
	reply(w, sh, err)
}

func (s *Sim) apiLabel(w http.ResponseWriter, r *http.Request) {
	sh, err := s.Get(r.PathValue("awb"))
	if err != nil {
		reply(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="label-`+sh.AWB+`.pdf"`)
	_, _ = w.Write(Label(sh))
}
