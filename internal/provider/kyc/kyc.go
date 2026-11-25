// Package kyc defines the KYC provider (PAN check and bank penny-drop, LLD 12)
// and the free sim-kyc adapter used in this build. Paid adapters are planned
// in paid-later/ only.
package kyc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Status is the outcome of a check.
type Status string

const (
	Verified     Status = "VERIFIED"
	Failed       Status = "FAILED"        // PAN not found, account closed, invalid
	NameMismatch Status = "NAME_MISMATCH" // found, but the name on record differs
)

// MinNameMatch is the fuzzy name score a check needs (LLD 12, sim-kyc rules).
const MinNameMatch = 0.8

// Result of a PAN check or penny-drop.
type Result struct {
	Status       Status  `json:"status"`
	Reference    string  `json:"reference"`
	NameOnRecord string  `json:"nameOnRecord,omitempty"`
	NameMatch    float64 `json:"nameMatch"`
	Reason       string  `json:"reason,omitempty"`
}

// Provider runs KYC checks.
type Provider interface {
	VerifyPAN(ctx context.Context, pan, name string) (Result, error)
	PennyDrop(ctx context.Context, account, ifsc, name string) (Result, error)
}

var (
	panRE  = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
	ifscRE = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`)
	acctRE = regexp.MustCompile(`^[0-9]{9,18}$`)
)

// ValidPAN reports whether s looks like a PAN (AAAAA9999A).
func ValidPAN(s string) bool { return panRE.MatchString(s) }

// ValidIFSC reports whether s looks like an IFSC code (AAAA0XXXXXX).
func ValidIFSC(s string) bool { return ifscRE.MatchString(s) }

// ValidAccount reports whether s is a 9–18 digit bank account number.
func ValidAccount(s string) bool { return acctRE.MatchString(s) }

// Sim calls the sim-kyc service. The simulator echoes the applicant's name as
// the name on record (except for its fixed mismatch cases); the score is
// computed here with NameScore, exactly as a real adapter would.
type Sim struct {
	BaseURL string
	Client  *http.Client
}

// NewSim creates the sim-kyc adapter.
func NewSim(baseURL string) *Sim {
	return &Sim{BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: 3 * time.Second}}
}

// VerifyPAN implements Provider.
func (s *Sim) VerifyPAN(ctx context.Context, pan, name string) (Result, error) {
	return s.call(ctx, "/internal/kyc/pan", map[string]string{"pan": pan, "name": name}, name)
}

// PennyDrop implements Provider.
func (s *Sim) PennyDrop(ctx context.Context, account, ifsc, name string) (Result, error) {
	return s.call(ctx, "/internal/kyc/bank", map[string]string{"account": account, "ifsc": ifsc, "name": name}, name)
}

func (s *Sim) call(ctx context.Context, path string, body map[string]string, name string) (Result, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("kyc: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("kyc: %s returned %d", path, res.StatusCode)
	}
	var out Result
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("kyc: decode: %w", err)
	}
	return Score(out, name), nil
}

// Score fills NameMatch from the name on record and downgrades a found record
// whose name does not match well enough.
func Score(r Result, name string) Result {
	if r.Status != Verified {
		return r
	}
	r.NameMatch = NameScore(name, r.NameOnRecord)
	if r.NameMatch < MinNameMatch {
		r.Status = NameMismatch
		r.Reason = "The name on record does not match the name you entered"
	}
	return r
}

var honorifics = map[string]bool{"MR": true, "MRS": true, "MS": true, "MISS": true, "DR": true, "SHRI": true, "SMT": true, "KUMARI": true, "M/S": true, "MS.": true}

// normalizeName upper-cases and drops punctuation and honorifics.
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	kept := words[:0]
	for _, w := range words {
		if !honorifics[w] {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}

// NameScore is a 0–1 similarity tolerant of case, punctuation, honorifics,
// word order and small typos: the better of a letter-pair (Sørensen–Dice)
// score and an edit-distance score over the words in sorted order.
func NameScore(a, b string) float64 {
	na, nb := normalizeName(a), normalizeName(b)
	if na == "" || nb == "" {
		return 0
	}
	return max(dice(na, nb), editScore(sortedWords(na), sortedWords(nb)))
}

func dice(a, b string) float64 {
	pa, pb := pairs(a), pairs(b)
	count := map[string]int{}
	for _, p := range pa {
		count[p]++
	}
	inter := 0
	for _, p := range pb {
		if count[p] > 0 {
			count[p]--
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(pa)+len(pb))
}

func sortedWords(s string) string {
	w := strings.Fields(s)
	slices.Sort(w)
	return strings.Join(w, " ")
}

// editScore is 1 − Levenshtein distance / longer length.
func editScore(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(max(len(ra), len(rb)))
}

func pairs(s string) []string {
	var out []string
	for _, w := range strings.Fields(s) {
		r := []rune(w)
		if len(r) == 1 {
			out = append(out, string(r))
		}
		for i := 0; i+1 < len(r); i++ {
			out = append(out, string(r[i:i+2]))
		}
	}
	return out
}

// ErrInvalid is returned for malformed input before calling a provider.
var ErrInvalid = errors.New("kyc: invalid input")
