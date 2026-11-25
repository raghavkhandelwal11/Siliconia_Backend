// Package kyc is the sim-kyc service: a free stand-in for PAN verification and
// bank penny-drop (LLD 12). Its rules are fixed so testers can pick outcomes:
//
//   - PAN AAAAA0000A is not found; any other well-formed PAN is found, and the
//     name on record is the name the tester sent.
//   - Accounts ending 0000 belong to someone else (name mismatch); accounts
//     ending 9999 are closed; any other account is verified.
package kyc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
)

// Server handles /internal/kyc/* on the internal network only (Caddy blocks /internal).
type Server struct{}

// Handler returns the HTTP handler.
func (Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/kyc/pan", pan)
	mux.HandleFunc("POST /internal/kyc/bank", bank)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

type request struct {
	PAN     string `json:"pan"`
	Account string `json:"account"`
	IFSC    string `json:"ifsc"`
	// Name is the applicant's name; the simulator echoes it as the name on record.
	Name string `json:"name"`
}

func pan(w http.ResponseWriter, r *http.Request) {
	var req request
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil || !provider.ValidPAN(req.PAN) {
		reply(w, provider.Result{Status: provider.Failed, Reference: ref(), Reason: "PAN format is not valid"})
		return
	}
	if req.PAN == "AAAAA0000A" {
		reply(w, provider.Result{Status: provider.Failed, Reference: ref(), Reason: "No record found for this PAN"})
		return
	}
	reply(w, provider.Result{Status: provider.Verified, Reference: ref(), NameOnRecord: onRecord(req.Name)})
}

func bank(w http.ResponseWriter, r *http.Request) {
	var req request
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil ||
		!provider.ValidAccount(req.Account) || !provider.ValidIFSC(req.IFSC) {
		reply(w, provider.Result{Status: provider.Failed, Reference: ref(), Reason: "Account number or IFSC is not valid"})
		return
	}
	switch {
	case strings.HasSuffix(req.Account, "9999"):
		reply(w, provider.Result{Status: provider.Failed, Reference: ref(), Reason: "This account is closed"})
	case strings.HasSuffix(req.Account, "0000"):
		reply(w, provider.Result{Status: provider.Verified, Reference: ref(), NameOnRecord: "RAMESH KUMAR VERMA"})
	default:
		reply(w, provider.Result{Status: provider.Verified, Reference: ref(), NameOnRecord: onRecord(req.Name)})
	}
}

// onRecord is the name the simulated registry holds: the applicant's own name.
func onRecord(name string) string { return strings.ToUpper(strings.TrimSpace(name)) }

func reply(w http.ResponseWriter, res provider.Result) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func ref() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "simkyc_" + hex.EncodeToString(b)
}
